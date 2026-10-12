package runtime

// endpending.go: the durable SessionEnd(blocked) lane (v2-converge §6 bugs
// 1 and 5). A blocked close that the hub has not heard is NOT a closed WI:
// aihub still shows it running under a secret only the local record (or the
// claiming journal) holds. So the local scene is kept in a durable state and
// the close is retried — in-process with exponential backoff, and at every
// daemon start — until the hub accepts it (or answers that the session is no
// longer ours). Only then is the local record dropped.

import (
	"fmt"
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// endBlockedAtHub sends SessionEnd(blocked) fenced on the secret and
// reports whether the close is SETTLED: either the hub accepted it, or a
// verify shows the session no longer holds the WI (our earlier close landed
// and only its answer was lost, or another session took over) — in both
// cases nothing is left to end.
func (r *Runtime) endBlockedAtHub(wi, secret, note, head string) bool {
	err := r.hub.SessionEnd(wi, api.SessionEndRequest{
		SessionID: secret,
		Status:    api.StatusBlocked,
		Note:      note,
		HeadSHA:   head,
	})
	if err == nil {
		return true
	}
	if v, verr := r.hub.SessionVerify(wi, secret); verr == nil && !v.Matches {
		fmt.Fprintf(os.Stderr, "runtime: %s: blocked close failed (%v) but the session no longer holds the work item (status %s) — settled\n", wi, err, v.Status)
		return true
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: session-end(blocked) failed (will retry): %v\n", wi, err)
	return false
}

// goRetry runs attempt on a backoff until it reports true, the key's
// goroutine is already running (one per WI), or the runtime closes.
func (r *Runtime) goRetry(key string, attempt func() bool) {
	r.endMu.Lock()
	if r.endRetrying == nil {
		r.endRetrying = map[string]bool{}
	}
	if r.endRetrying[key] {
		r.endMu.Unlock()
		return
	}
	r.endRetrying[key] = true
	r.endMu.Unlock()
	go func() {
		defer func() {
			r.endMu.Lock()
			delete(r.endRetrying, key)
			r.endMu.Unlock()
		}()
		delay := r.cfg.EndRetryBase
		for {
			select {
			case <-r.endStop:
				return
			case <-time.After(delay):
			}
			if attempt() {
				return
			}
			if delay *= 2; delay > r.cfg.EndRetryMax {
				delay = r.cfg.EndRetryMax
			}
		}
	}()
}

// endPendingAttempt is one retry of an end_pending job's close; true =
// settled (record dropped).
func (r *Runtime) endPendingAttempt(j *job) bool {
	j.mu.Lock()
	secret, note, head := j.sessionID, j.endNote, j.endHead
	j.mu.Unlock()
	if !r.endBlockedAtHub(j.wi, secret, note, head) {
		return false
	}
	if err := j.transition(StateDone); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: end_pending job could not reach DONE: %v\n", j.wi, err)
	}
	r.dropJob(j)
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	return true
}

// retryEndPending starts the background retry of an end_pending job.
func (r *Runtime) retryEndPending(j *job) {
	r.goRetry("job:"+j.wi, func() bool { return r.endPendingAttempt(j) })
}

// restartEndPending is the daemon-start half: one synchronous attempt, and
// the background retry when the hub is still not reachable.
func (r *Runtime) restartEndPending(j *job) {
	fmt.Fprintf(os.Stderr, "runtime: %s: end_pending record — retrying the blocked close\n", j.wi)
	if !r.endPendingAttempt(j) {
		r.retryEndPending(j)
	}
}

// retryClaimEnd keeps retrying the compensation SessionEnd(blocked) of a
// failed start (abandonClaim): the claiming-journal entry stays until the
// hub accepts the close, in-process here and — after a crash — through the
// journal replay at the next daemon start.
func (r *Runtime) retryClaimEnd(journalKey, wi, secret, note string) {
	r.goRetry("claim:"+wi, func() bool {
		if !r.endBlockedAtHub(wi, secret, note, "") {
			return false
		}
		r.clearClaiming(journalKey)
		return true
	})
}

// finishBlocked is the tail of boundedFail: the close verdict goes to the
// hub; success drops the job, failure parks it end_pending (durable) and
// retries. Observe is released either way — the driver is already dead.
func (r *Runtime) finishBlocked(j *job, detail, head string) {
	j.mu.Lock()
	j.endNote, j.endHead = detail, head
	secret := j.sessionID
	j.mu.Unlock()
	settled := r.endBlockedAtHub(j.wi, secret, detail, head)
	next := StateDone
	if !settled {
		next = StateEndPending
	}
	if err := j.transition(next); err != nil {
		// The verdict (or the pending close) is authoritative whatever the
		// edge table says: record it rather than leave a stale state.
		fmt.Fprintf(os.Stderr, "runtime: %s: bounded fail: %v — forcing %s\n", j.wi, err, next)
		j.mu.Lock()
		j.state = next
		j.mu.Unlock()
	}
	if settled {
		r.dropJob(j)
	}
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	r.send(protocol.RTMsg{
		Kind:     protocol.RTFinished,
		WorkItem: j.wi,
		Reason:   protocol.FinishBlocked,
		Detail:   detail,
	})
	if !settled {
		fmt.Fprintf(os.Stderr, "runtime: %s: blocked close not acknowledged by the hub — record kept end_pending, retrying\n", j.wi)
		r.retryEndPending(j)
	}
}
