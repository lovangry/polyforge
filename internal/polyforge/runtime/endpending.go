package runtime

// endpending.go: the durable SessionEnd(blocked) lane (v2-converge §6 bugs
// 1 and 5). A blocked close that the hub has not heard is NOT a closed WI:
// aihub still shows it running under a secret only the local record (or the
// claiming journal) holds. So the local scene is kept in a durable state and
// the close is retried — in-process with exponential backoff, and at every
// daemon start — until the hub accepts it (or answers that the session is no
// longer ours). Only then is the local record dropped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
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
// settled (record dropped). See closeBlocked for the push-then-close
// order.
func (r *Runtime) endPendingAttempt(j *job) bool {
	if !r.closeBlocked(j, true) {
		return false
	}
	r.settleEndPending(j)
	return true
}

// closeBlocked sends one job's SessionEnd(blocked), pushing the local
// chain FIRST when push is set, with the session credential that dies
// with the close: boundedFail commits the dirty capsule before the
// verdict, and when that push failed (most often the same outage that
// fails the close) the commits would otherwise stay local while the hub's
// blocked WI is re-claimed from the remote chain.
// Reports whether the close is SETTLED. Push outcomes:
//
//   - landed → the close goes out;
//   - transient failure → not settled (retried on the lane), unless the
//     session no longer holds the WI (its credential is dead: nothing can
//     be pushed under it; settled, exactly as endBlockedAtHub settles);
//   - PERMANENT rejection (hook refusal, non-fast-forward, size limit) →
//     no retry can land it: stop pushing, raise push_failed with the
//     local head and the reason (the commits stay in the local capsule),
//     and close without it.
func (r *Runtime) closeBlocked(j *job, push bool) bool {
	j.mu.Lock()
	secret, note, head, chain, gaveUp := j.sessionID, j.endNote, j.endHead, j.git, j.endPushGaveUp
	j.mu.Unlock()
	if push && !gaveUp && dirExists(j.capsuleDir) && dirExists(chain.GitDir) {
		if _, perr := gitops.Push(chain); perr != nil {
			if !gitops.IsPermanentPushRejection(perr) {
				if v, verr := r.hub.SessionVerify(j.wi, secret); verr == nil && !v.Matches {
					fmt.Fprintf(os.Stderr, "runtime: %s: blocked-close chain push failed (%v) and the session no longer holds the work item (status %s) — settled without it\n", j.wi, perr, v.Status)
					return true
				}
				fmt.Fprintf(os.Stderr, "runtime: %s: blocked-close chain push failed (will retry before the close): %v\n", j.wi, perr)
				return false
			}
			j.mu.Lock()
			j.endPushGaveUp = true
			j.mu.Unlock()
			// durable: a restart's retry must not push again either
			if err := r.persist(); err != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
			}
			local, _ := gitops.Head(chain)
			fmt.Fprintf(os.Stderr, "runtime: %s: blocked-close chain push permanently rejected — closing without it (local head %s stays in the capsule): %v\n", j.wi, local, perr)
			if r.cfg.EventTap != nil {
				r.cfg.EventTap(j.wi, "push_failed", map[string]any{
					"phase":      "blocked-close",
					"permanent":  true,
					"local_head": local,
					"error":      perr.Error(),
					"note":       "the hub permanently refused the chain push; the blocked close goes out without it and the unpushed commits stay in the local capsule",
				})
			}
		}
	}
	return r.endBlockedAtHub(j.wi, secret, note, head)
}

// settleEndPending drops an end_pending job whose close is settled.
func (r *Runtime) settleEndPending(j *job) {
	if err := j.transition(StateDone); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: end_pending job could not reach DONE: %v\n", j.wi, err)
	}
	r.dropJob(j)
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
}

// retryEndPending starts the background retry of an end_pending job.
func (r *Runtime) retryEndPending(j *job) {
	r.goRetry("job:"+j.wi, func() bool { return r.endPendingAttempt(j) })
}

// restartEndPending is the daemon-start half: it only SCHEDULES the
// background retry. Restart runs before the socket serves, and the
// attempt pushes the chain first (unbounded network work) — a synchronous
// attempt there could hang the daemon's startup on a hung hub.
func (r *Runtime) restartEndPending(j *job) {
	fmt.Fprintf(os.Stderr, "runtime: %s: end_pending record — retrying the blocked close in the background\n", j.wi)
	r.retryEndPending(j)
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
		r.clearClaimingSession(journalKey, secret)
		return true
	})
}

// finishBlocked is the locked tail of boundedFail, once the blocked
// close went out (settled reports whether the hub took it): success drops
// the job, failure parks it end_pending (durable) and retries. Observe is
// released either way — the driver is already dead.
func (r *Runtime) finishBlocked(j *job, detail string, settled bool) {
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

// retrySeal is the SEALING job's close retry: the terminal SessionEnd
// failed after the agent was torn down, so the job sits in SEALING with its
// verdict recorded. Each attempt takes opMu, checks the job is still the
// current SEALING one, and runs the very close the restart finisher runs —
// stopOp(alreadySealing) with the recorded reason and push state. A typed
// STALE_SESSION answer (the session no longer holds the work item: an
// earlier close landed with its answer lost, or an admin ended it) stops
// the retry and finishes the close locally. Shutdown stops the retry; the
// persisted SEALING record is finished by the next restart.
func (r *Runtime) retrySeal(j *job, note string) {
	r.goRetry("seal:"+j.wi, func() bool { return r.sealAttempt(j, note) })
}

// sealAttempt is one retry of a SEALING close; true = settled.
func (r *Runtime) sealAttempt(j *job, note string) bool {
	s, ok := r.tryLockWI(j.wi)
	if !ok {
		return false // another operation of the work item is under way
	}
	defer r.unlockWI(j.wi, s)
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if r.jobOf(j.wi) != j {
		return true // settled or replaced meanwhile
	}
	if st, _ := j.snapshot(); st != StateSealing {
		return true
	}
	if j.isBusy() {
		return false // another door's network step is in flight (B-8)
	}
	j.mu.Lock()
	reason := j.stopReason
	j.mu.Unlock()
	err := r.stopOp(context.Background(), j, reason, true, note)
	if err == nil {
		return true
	}
	if isStaleSession(err) {
		r.finishSealLocally(j, reason, err)
		return true
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: seal retry failed (will retry): %v\n", j.wi, err)
	return false
}

// isStaleSession reports the hub's typed "this session no longer holds
// the work item" refusal (CORE-403 STALE_SESSION).
func isStaleSession(err error) bool {
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCoreForbidden {
		return false
	}
	var extra struct {
		Reason string `json:"reason"`
	}
	if len(ae.Extra) > 0 && json.Unmarshal(ae.Extra, &extra) == nil && extra.Reason == "STALE_SESSION" {
		return true
	}
	return strings.Contains(ae.Detail, "STALE_SESSION")
}

// finishSealLocally ends a SEALING job whose session the hub no longer
// holds: the hub's state stands (nothing more can be sent under this
// session), the scene is sealed read-only, the job is DONE and dropped,
// observe is released, and an event records that the local close never
// reached the hub under this session.
func (r *Runtime) finishSealLocally(j *job, reason string, cause error) {
	if err := sealReadOnly(j.capsuleDir); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: read-only seal incomplete: %v\n", j.wi, err)
	}
	if err := j.transition(StateDone); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: sealing job could not reach DONE: %v\n", j.wi, err)
	}
	r.dropJob(j)
	if reason != protocol.FinishBlocked {
		r.clearManualModels(j.wi) // as the ordinary terminal close does
	}
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: seal finished locally — the hub no longer holds this session: %v\n", j.wi, cause)
	if r.cfg.EventTap != nil {
		r.cfg.EventTap(j.wi, "seal_finished_locally", map[string]any{
			"reason": reason,
			"note":   "the hub no longer holds this session (an earlier close landed with its answer lost, or an admin ended it): the work item's hub state is whatever was set there, not necessarily " + reason,
			"error":  cause.Error(),
		})
	}
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: j.wi, Reason: reason})
}
