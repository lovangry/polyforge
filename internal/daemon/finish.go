package daemon

// finish.go: the 04 §6.2 A-path close, shared by exactly two callers:
// the graceful shutdown sequence (04 §6.5 step 4, daemon.go Shutdown)
// and the pause/wrap routes (00 §5.1 — the two user verbs the split table
// routes to the daemon local face because their closing flush_evidence
// must be Daemon-generated, 02 §6.4). ONE close path, no duplication:
// boundary notice → final flush (the daemon publishes on the WI Agent's
// behalf, 05 §6.1) → Core end-generation with the Daemon-generated
// flush_evidence (02 §5.1 row 9 / 02 §6.4) → seal.
//
// Pause is NOT a local-only suspension: 04 §6.2 names "WI 到达 pause /
// wrap，或优雅停机中断收尾" one and the same normal-close path, and
// 04 §5.1's resume row is built on "pause 已结束该 WI 当前活跃的
// generation" — resume is always a fresh claim (generation+1). A pause
// that left the WI claimed server-side would make resume impossible
// (claim from status running is CORE-409 ALREADY_CLAIMED), so the pause
// route ends the generation exactly like the shutdown interrupt does.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/silgrid/polyforge/internal/sync/aihub"
)

// ErrNoLocalJob is the DAEMON-404 shape for a pause/wrap against a work
// item with no live local job (never claimed here, or already closed and
// sealed).
var ErrNoLocalJob = errors.New("daemon: no local job")

// ErrCloseInProgress is the DAEMON-409 shape for a close already running
// for this WI (a second pause/wrap racing the first, or a wrap landing
// while the graceful shutdown sequence is closing the same job).
var ErrCloseInProgress = errors.New("daemon: close already in progress (DAEMON-409)")

// FinishOutcome is the pause/wrap route answer: the close-out facts of the
// 04 §6.2 A-path sequence, including the flush_evidence the daemon
// generated and submitted with the end-generation call (02 §6.4).
type FinishOutcome struct {
	WorkItemID    string               `json:"work_item_id"`
	Reason        string               `json:"reason"`
	HeadSeq       int64                `json:"head_seq"`
	ManifestSha   string               `json:"manifest_sha"`
	FlushEvidence *aihub.FlushEvidence `json:"flush_evidence,omitempty"`
}

// wrapReasons are the two wrap-route reasons (the terminal outcomes; the
// CLI's --fail flag selects "fail").
var wrapReasons = map[string]bool{"wrap": true, "fail": true}

// PauseJob closes one active local job with reason "pause" (00 §5.1 pause
// verb). The WI becomes paused server-side; resume is a fresh claim
// (generation+1) that goes through the ordinary claim sequence — this path
// never re-claims anything itself.
func (d *Daemon) PauseJob(ctx context.Context, wi string) (*FinishOutcome, error) {
	return d.finishJob(ctx, wi, "pause")
}

// WrapJob closes one active local job with reason wrap|fail (00 §5.1
// wrap verb): terminal success or terminal failure. An empty reason
// defaults to "wrap" (the CLI's non---fail default); anything else is a
// DAEMON-400 shape.
func (d *Daemon) WrapJob(ctx context.Context, wi, reason string) (*FinishOutcome, error) {
	if reason == "" {
		reason = "wrap"
	}
	if !wrapReasons[reason] {
		return nil, fmt.Errorf("daemon: unknown wrap reason %q (DAEMON-400)", reason)
	}
	return d.finishJob(ctx, wi, reason)
}

// finishJob is the route entry into the shared close: job lookup, the
// one-close-at-a-time guard, then closeJob.
func (d *Daemon) finishJob(ctx context.Context, wi, reason string) (*FinishOutcome, error) {
	d.mu.Lock()
	jr, ok := d.jobs[wi]
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w for %s", ErrNoLocalJob, wi)
	}
	if d.closing[wi] {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrCloseInProgress, wi)
	}
	d.closing[wi] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.closing, wi)
		d.mu.Unlock()
	}()
	return d.closeJob(ctx, wi, jr, reason, reason)
}

// closeJob runs the single 04 §6.2 A-path close for one active local job:
//
//  1. ring event + the interrupting bookkeeping mark (04 §4.1);
//  2. stop-advancing notice and the safe-boundary wait (04 §6.5 step 3);
//  3. final flush via the SAME PublishRequest path every close uses
//     (05 §6.1; the end reason makes it produce flush_evidence);
//  4. Core end-generation (02 §5.1 row 9) signed with the daemon-held
//     generation token and carrying that evidence (02 §6.4);
//  5. sealJob: watcher stopped, agent recycled, capsule sealed read-only,
//     jobs[] record dropped.
//
// A rolled-back credential isolates through the shared 04 §7.3 classifier
// (quarantine, never a retry) at either Core-writing step. endReason is
// the Core reason (pause|wrap|fail); ringCause is what live observers see
// on the ring (the shutdown sequence passes "shutdown").
func (d *Daemon) closeJob(ctx context.Context, wi string, jr *jobRuntime, endReason, ringCause string) (*FinishOutcome, error) {
	jr.ring.Append(EvJobStopped, map[string]any{"cause": ringCause})
	// Bookkeeping mark: the close is under way (04 §4.1 interrupting), so
	// a crash mid-close leaves an honest record, not a stale running row.
	d.markJobStatus(wi, JobInterrupting)
	// Boundary notice: the daemon cannot pick or skip steps for the agent
	// (00 §3.6) — it only asks it to stop advancing at the next boundary.
	if jr.agent != nil && jr.agent.Alive() {
		if err := jr.agent.NotifyStop(ctx); err == nil {
			_ = jr.agent.Quiesce(ctx)
		}
	}
	// Final flush on the agent's behalf. PublishRequest classifies a
	// rolled-back credential through the shared 04 §7.3 helper already.
	outcome, err := d.PublishRequest(ctx, wi, endReason)
	if err != nil || outcome == nil || outcome.FlushEvidence == nil {
		if err == nil {
			err = fmt.Errorf("daemon: final flush produced no flush_evidence")
		}
		return nil, err
	}
	// End the generation with the Daemon-generated evidence. On a
	// rolled-back token the 04 §7.3 path (quarantine, no retry) runs via
	// the same shared classifier.
	if err := d.client.EndGeneration(wi, endReason, "", jr.token, outcome.FlushEvidence); err != nil {
		return nil, d.classifyWriteErr(wi, err)
	}
	d.sealJob(wi, jr)
	return &FinishOutcome{
		WorkItemID: wi, Reason: endReason,
		HeadSeq: outcome.HeadSeq, ManifestSha: outcome.ManifestSha,
		FlushEvidence: outcome.FlushEvidence,
	}, nil
}

// markJobStatus persists a jobs[] status transition (04 §4.1). Best-effort
// by design: a failed persist is logged and never blocks the close itself.
func (d *Daemon) markJobStatus(wi, status string) {
	j := d.st.FindJob(wi)
	if j == nil {
		return
	}
	j.Status = status
	if err := d.state.UpsertJob(d.st, *j); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: job status persist failed for %s: %v\n", wi, err)
	}
}
