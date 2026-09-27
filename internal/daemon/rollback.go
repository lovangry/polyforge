package daemon

// rollback.go: the 04 §7.3 rollback path — a dedicated scene-isolation
// sequence that shares NOTHING with the 04 §7.1 crash-recovery criteria
// (different trigger, different goal: §7.1 CONTINUES the same execution
// scene after an accidental process death; this path ISOLATES the scene
// because the server-side rollback invalidated its identity for good).
//
// Trigger: a generation-token-bearing WRITE request (block upload, manifest
// submit, end-generation) answered with HTTP 403 whose error body carries
// reason=secret_rolled_back (02 §6.6 mode="rollback": the token's epoch was
// retired with NO grace window; the same reason reaches us as CORE-403 on
// the execution face and SYNC-403 on the publish face — the code differs by
// face, the reason string does not). Judgment material comes entirely from
// the failing write's response body; there is no whoami endpoint to consult.
//
// On trigger the Daemon MUST:
//   1. NOT call reauthorize (that is the grace-window re-issue path; a
//      rolled-back credential is unrecoverable) and NOT treat the
//      invalidation as a takeover race to forward anywhere.
//   2. Terminate the WI's persistent WI Agent process group: SIGTERM to the
//      whole group, SIGKILL after the bound (04 §7.3 step 2).
//   3. Quarantine the local Capsule (mark + move, never delete) and drop
//      the jobs[] entry from daemon/state.json.
//   4. NOT re-claim, re-queue or otherwise auto-resume the WI: the only
//      arbitration path left is an admin force-takeover, which is a HUMAN
//      decision (04 §7.3 step 4; recovery consumes the takeover response's
//      new-generation credentials directly — never a second claim).

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/sync/aihub"
)

// ReasonSecretRolledBack is the 02 §6.6 CORE-403/SYNC-403 reason body value
// that selects this path.
const ReasonSecretRolledBack = "secret_rolled_back"

// ErrSecretRolledBack is the daemon-side error shape after the rollback
// sequence has run: the caller's write failed, the scene is quarantined,
// and recovery is an admin force-takeover — never a retry.
var ErrSecretRolledBack = errors.New("daemon: generation credential invalidated by rollback (secret_rolled_back); capsule quarantined, recovery is admin force-takeover only")

// isSecretRolledBack classifies a write-path error against the 04 §7.3
// trigger. Both CORE-403 (end-generation) and SYNC-403 (publish fencing)
// carry the same reason; anything else — including reason=stale_generation
// (a takeover race, handled by local sealing, NOT this path) and
// secret_rotated_expired (the grace re-authorize path) — is deliberately
// excluded.
func isSecretRolledBack(err error) bool {
	var ae *aihub.APIError
	return errors.As(err, &ae) && ae.Reason == ReasonSecretRolledBack
}

// quarantineRolledBack executes the 04 §7.3 isolation sequence for one WI.
// It is idempotent: a second call for a WI whose job is already gone is a
// no-op. Errors of the individual steps are collected but never abort the
// sequence — a half-isolated scene must still lose its jobs[] record.
func (d *Daemon) quarantineRolledBack(wi string) error {
	d.mu.Lock()
	jr, ok := d.jobs[wi]
	d.mu.Unlock()
	if !ok {
		return nil
	}
	// Monitoring/log signal FIRST (04 §9): a ring event for any live
	// observer plus a stderr line for the daemon log — both before the
	// bookkeeping below makes the job invisible.
	jr.ring.Append(EvJobStopped, map[string]any{
		"cause": ReasonSecretRolledBack, "work_item_id": wi,
		"recovery": "admin force-takeover (no auto re-claim)",
	})
	fmt.Fprintf(os.Stderr, "pf-daemon: %s for %s: stopping the WI Agent process group and quarantining the local capsule; no auto re-claim — recovery is admin force-takeover\n",
		ReasonSecretRolledBack, wi)

	// Step 2: stop the persistent WI Agent and collect its whole process
	// group (step agents / command children included): SIGTERM, then
	// SIGKILL after the bound.
	if jr.watcher != nil {
		jr.watcher.Stop()
	}
	if jr.agent != nil {
		_ = jr.agent.Terminate(10 * time.Second)
	}
	// Step 3: quarantine the Capsule (mark + move, never delete) and drop
	// the jobs[] record. A failed move is escalated — the directory stays
	// where it was, but the job record still goes so no supervisor path
	// keeps driving a dead generation.
	var firstErr error
	if _, qerr := jr.cap.Quarantine(ReasonSecretRolledBack); qerr != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: capsule quarantine move failed for %s: %v (directory left in place, job record still removed)\n", wi, qerr)
		firstErr = qerr
	}
	if err := d.state.RemoveJob(d.st, wi); err != nil && firstErr == nil {
		firstErr = err
	}
	d.mu.Lock()
	delete(d.jobs, wi)
	d.mu.Unlock()
	d.jobq.Complete(wi)
	// Step 4 (deliberate omission): NO re-claim, NO re-queue, NO retry.
	// The WI's only arbitration path is an admin force-takeover, which
	// consumes the takeover response's new-generation credentials directly
	// (02 §6.2: never a second claim).
	return firstErr
}

// classifyWriteErr runs the 04 §7.3 (secret_rolled_back) and the 05 §2.9
// takeover-fence (stale_generation, 04 §6.2 B) trigger checks on one
// failed write and, when either fires, executes its isolation sequence.
// Callers wrap the ORIGINAL error (the server's verdict) for their face
// and surface the typed daemon verdict alongside it.
func (d *Daemon) classifyWriteErr(wi string, err error) error {
	if err == nil {
		return err
	}
	if isSecretRolledBack(err) {
		if qerr := d.quarantineRolledBack(wi); qerr != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: rollback quarantine for %s partially failed: %v\n", wi, qerr)
		}
		return fmt.Errorf("%w: %s: %v", ErrSecretRolledBack, wi, err)
	}
	if isStaleGeneration(err) {
		if serr := d.isolateTakenOver(wi); serr != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: takeover isolation for %s partially failed: %v\n", wi, serr)
		}
		return fmt.Errorf("%w: %s: %v", ErrStaleGeneration, wi, err)
	}
	return err
}
