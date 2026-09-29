package daemon

// stale.go: the takeover-fence local isolation (04 §6.2 path B / 05 §2.9 /
// 02 §6.4 "the fenced old generation may seal-and-exit locally without a
// successful publish"). Trigger: any generation-token-bearing WRITE
// (block upload, manifest submit, end-generation) answered CORE-403 or
// SYNC-403 whose reason is stale_generation — the generation was taken
// over (or otherwise fenced). The old owner MUST NOT retry, MUST NOT
// re-authorize (that is the secret_rotated_expired path, 02 §6.6) and
// MUST stop writing immediately: 05 §7.3 — "the old Daemon thereupon
// immediately stops publishing for that WI, notifies the old WI Agent to
// stop driving, and seals the local scene (no further publish attempts)".
//
// This is 04 §6.2 path B, deliberately a SEPARATE sequence from the
// secret_rolled_back quarantine (rollback.go / 04 §7.3): different trigger,
// different recovery (takeover: whoever holds authority starts a fresh
// generation via the ordinary claim/force-takeover sequence — possibly this
// very machine; rollback: admin force-takeover only, never an automatic
// re-claim). The mechanical steps are nevertheless the §6.2 B list:
// stop the watcher, terminate the agent process group (no boundary wait —
// the old generation's output has lost publish eligibility), quarantine the
// capsule for audit, drop the jobs[] record. No re-queue: recovery is a
// fresh claim/takeover decision, not this daemon's to make here.

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/sync/aihub"
)

// ReasonStaleGeneration is the 05 §2.9 CORE-403/SYNC-403 reason body value
// meaning the generation was taken over: retry_hint "reclaim_required".
const ReasonStaleGeneration = "stale_generation"

// ErrStaleGeneration is the daemon-side error shape after the takeover
// isolation has run: the write failed, the local scene is sealed (agent
// stopped, capsule quarantined), and the WI's future is a fresh
// claim/force-takeover by whoever holds authority — never a retry here.
var ErrStaleGeneration = errors.New("daemon: generation taken over (stale_generation); local scene sealed and quarantined, recovery is a fresh claim/force-takeover — never a retry")

// isStaleGeneration classifies a write-path error against the takeover
// fence. Both faces carry the same reason (CORE-403 on the execution face,
// SYNC-403 on the publish face); secret_rolled_back (04 §7.3 quarantine)
// and secret_rotated_expired (02 §6.6 grace re-authorize) are different
// paths and deliberately do not match.
func isStaleGeneration(err error) bool {
	var ae *aihub.APIError
	return errors.As(err, &ae) && ae.Reason == ReasonStaleGeneration
}

// isolateTakenOver executes the 04 §6.2 B isolation sequence for one WI
// whose generation was fenced away. Idempotent: a second call for a WI
// whose job is already gone is a no-op. Individual step errors are
// collected but never abort the sequence — the jobs[] record must still go.
func (d *Daemon) isolateTakenOver(wi string) error {
	d.mu.Lock()
	jr, ok := d.jobs[wi]
	d.mu.Unlock()
	if !ok {
		return nil
	}
	jr.ring.Append(EvJobStopped, map[string]any{
		"cause": ReasonStaleGeneration, "work_item_id": wi,
		"recovery": "fresh claim/force-takeover (no retry on this scene)",
	})
	fmt.Fprintf(os.Stderr, "pf-daemon: %s for %s: the generation was taken over; stopping the WI Agent, quarantining the local capsule and making no further publish attempts\n",
		ReasonStaleGeneration, wi)

	// 04 §6.2 B step 1: stop the watcher.
	if jr.watcher != nil {
		jr.watcher.Stop()
	}
	// Step 2: terminate the whole process group directly — no boundary
	// notice, no wait: the old generation's output is no longer eligible
	// for publication.
	if jr.agent != nil {
		_ = jr.agent.Terminate(10 * time.Second)
	}
	// Step 3: quarantine the Capsule (marked + moved, never deleted) so the
	// old scene stays available for audit while the new generation (wherever
	// it is derived) rebuilds from the last published HEAD (05 §7.4).
	var firstErr error
	if _, qerr := jr.cap.Quarantine(ReasonStaleGeneration); qerr != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: capsule quarantine move failed for %s: %v (directory left in place, job record still removed)\n", wi, qerr)
		firstErr = qerr
	}
	// Step 4: drop the jobs[] record. Step 5 is not ours: a new generation's
	// derivation walks the ordinary §6.1 sequence wherever it is claimed.
	if err := d.state.RemoveJob(d.st, wi); err != nil && firstErr == nil {
		firstErr = err
	}
	d.mu.Lock()
	delete(d.jobs, wi)
	d.mu.Unlock()
	d.jobq.Complete(wi)
	return firstErr
}
