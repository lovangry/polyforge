package runtime

// restart.go: daemon-startup reconciliation (v1-spec-full §十一, re-keyed
// onto the v1.5 session model — contract §1.2/§1.3). The Runtime never
// revives from state.json alone — every recorded job is reconciled
// against aihub's own verdict first, and the verdict is the SESSION
// verify (compare-only, B3: the hub stores only the hash of our secret):
//
//	claiming-journal replay runs FIRST (B1): an uncleared entry whose
//	  journaled session still holds the WI is adopted and its start
//	  finished; anything else is dropped.
//	verify matches + status=paused + clean_pause=false → SessionAttach
//	  and resume from the local capsule (the graceful-shutdown shape:
//	  the hub's paused-with-session + clean_pause=false IS "daemon shut
//	  down gracefully, will attach on restart", M3). The RETAINED
//	  operator-paused record (v1.5 review Finding 2) reaches the same
//	  branch once the operator clears clean_pause (pf resume): its state
//	  is Paused and restartResume transitions it back to RUNNING before
//	  the recovery engine resumes.
//	verify matches + status=paused + clean_pause=true → KEEP the
//	  record, do nothing (Finding 2: the retained Paused record is the
//	  pause's whole point — the secret survives locally for the later
//	  attach).
//	verify matches + status=running → crash-mid-run: the local capsule
//	  recover-resumes through the recovery engine (a record caught
//	  mid-seal finishes the interrupted stop instead; a Paused record
//	  whose pause close never reached the hub resumes the same way).
//	verify does NOT match (current_fingerprint differs, or the session
//	  row is gone and the WI is queued/running elsewhere) → release:
//	  kill any local tree, quarantine the capsule
//	  (capsules/<id>.quarantined.<ts>), drop the record. NO hub
//	  transition — superseded, not terminal.
//	verify does not match + terminal status → drop the record: the
//	  close settled server-side before the crash.
//	verify does not match + blocked → leave everything untouched (M2:
//	  the bounded-fail verdict stands; the storm guard's "never a third
//	  relaunch" is exactly this).
//	verify does not match + paused/clean_pause under another session →
//	  drop the record: restart must NOT auto-resume it.
//	aihub unreachable → keep the record, try again on the next restart —
//	  never guess a verdict.

import (
	"context"
	"fmt"
	"os"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Restart loads state.json, reconciles every recorded job with aihub and
// recovers what is still ours to run. Jobs aihub no longer knows as ours
// are dropped or released; a job with no local record needs no
// reconciliation (a running WI whose capsule lives on another machine is
// that machine's business).
func (r *Runtime) Restart(ctx context.Context) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	st, err := loadState(r.statePath)
	if err != nil {
		return err
	}
	// B1 journal replay FIRST, before the state.json records: a journaled
	// start (crash between the claim call and the spawn) must win over any
	// stale record of the same WI — the replay carries the secret the
	// record may predate. startClaimed's sessionIsRecorded guard keeps the
	// crash-past-registration window honest (see claiming.go).
	r.resumeClaiming(ctx)
	// Seed every recorded job into the live map first: persist() rewrites
	// state.json from the map, so records that must SURVIVE the
	// reconciliation (blocked, aihub-unreachable) are preserved by simply
	// staying in it. A record whose WI is already live (the journal replay
	// just adopted it, or Start ran before Restart) is left alone.
	for _, rec := range st.Jobs {
		if r.jobOf(rec.WorkItemID) != nil {
			continue
		}
		if rec.SessionID == "" {
			// a pre-v1.5 record (or a torn write): no session to verify,
			// nothing to reconcile — drop it rather than guess.
			fmt.Fprintf(os.Stderr, "runtime: %s: record without a session dropped\n", rec.WorkItemID)
			continue
		}
		r.mu.Lock()
		r.jobs[rec.WorkItemID] = jobFromRecord(rec)
		r.mu.Unlock()
	}
	for _, rec := range st.Jobs {
		if rec.SessionID == "" {
			continue
		}
		if j := r.jobOf(rec.WorkItemID); j != nil && j.liveAgent() {
			continue // already running under this daemon: not a restart case
		}
		v, err := r.hub.SessionVerify(rec.WorkItemID, rec.SessionID)
		if err != nil {
			// aihub unreachable for this WI: keep the record, try again on
			// the next restart — never guess a verdict.
			fmt.Fprintf(os.Stderr, "runtime: %s: session verify failed, record kept: %v\n", rec.WorkItemID, err)
			continue
		}
		if v.Matches {
			switch v.Status {
			case api.StatusRunning:
				// crash-mid-run under this session: recover-resume the
				// local capsule (a mid-seal record finishes its stop).
				if serr := r.restartRunning(ctx, rec); serr != nil {
					fmt.Fprintf(os.Stderr, "runtime: %s: restart recovery failed: %v\n", rec.WorkItemID, serr)
				}
			case api.StatusPaused:
				if v.CleanPause {
					// Operator pause (Finding 2, v1.5 review): the record is
					// RETAINED, never auto-resumed. Its state is reconciled to
					// Paused (a torn record from a crash window is the only way
					// it can disagree — the pause close persists Paused first)
					// and nothing else happens: no attach, no spawn, no hub
					// transition. The operator's `pf resume` clears the hub
					// flag and the NEXT restart attaches through the branch
					// below.
					if j := r.jobOf(rec.WorkItemID); j != nil {
						j.mu.Lock()
						if j.state != StatePaused {
							j.state = StatePaused // crash-window reconcile (direct set: the hub is the authority)
						}
						j.cleanPause = true
						j.mu.Unlock()
					}
					fmt.Fprintf(os.Stderr, "runtime: %s: cleanly paused; record retained (no auto-resume until the operator clears clean_pause)\n", rec.WorkItemID)
				} else if aerr := r.restartAttach(ctx, rec); aerr != nil {
					// attach refused (taken over in the window, or the hub
					// disagree): keep the record — the next restart
					// re-verifies with a fresh verdict.
					fmt.Fprintf(os.Stderr, "runtime: %s: session attach failed, record kept: %v\n", rec.WorkItemID, aerr)
				}
			case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
				r.restartDrop(rec)
			default:
				// queued/blocked while our session still matches (a torn
				// server state): treat as not-ours, drop the record.
				fmt.Fprintf(os.Stderr, "runtime: %s: hub says %s with our session; record dropped\n", rec.WorkItemID, v.Status)
				r.forgetRecord(rec)
			}
			continue
		}
		// verify does NOT match: our session is no longer the holder (or
		// the row is gone entirely).
		switch v.Status {
		case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
			// the close settled server-side before the crash: collect any
			// local residue and drop the record — no quarantine, the
			// terminal scene is the preserved artifact.
			r.restartDrop(rec)
		case api.StatusBlocked:
			// M2 storm guard: the bounded-fail verdict stands — no
			// revival, no kill, no quarantine; the record is kept so the
			// next boot re-checks (the unblock path re-claims through the
			// normal start, which quarantines the old scene itself).
		case api.StatusQueued:
			// back in the pool: our claim is gone. A local tree (if any)
			// is a writer of a session nobody holds — collect it and drop
			// the record; the capsule stays for the next claim's
			// quarantine-or-clone decision.
			fmt.Fprintf(os.Stderr, "runtime: %s: session ended server-side (queued); record dropped\n", rec.WorkItemID)
			r.restartDrop(rec)
		default:
			// running or paused under ANOTHER session (the takeover
			// happened while we were down): release — kill any local
			// tree, quarantine the capsule, drop the record. NO hub
			// transition: the new owner owns the work.
			r.restartRelease(rec, v.CurrentFingerprint)
		}
	}
	return r.persist()
}

// restartRelease is the superseded-at-restart path: the WI is held by
// another session (currentFingerprint names the public holder). The local
// tree is a zombie writer and the capsule a foreign scene — kill,
// quarantine in place, drop the record, leave a durable trace.
func (r *Runtime) restartRelease(rec *jobRecord, currentFingerprint string) {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		j = jobFromRecord(rec)
	}
	if j.agentPID > 0 {
		r.killAgentTree(j)
	}
	if err := quarantineCapsule(j.capsuleDir); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: release quarantine failed (scene left in place): %v\n", rec.WorkItemID, err)
	}
	r.forgetRecord(rec)
	if r.cfg.EventTap != nil {
		r.cfg.EventTap(rec.WorkItemID, "session_released", map[string]any{
			"session_fingerprint": j.sessionFingerprint,
			"current_fingerprint": currentFingerprint,
			"note":                "answers may be lost; question re-opened",
		})
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: held by session %q — local tree killed, capsule quarantined, record dropped (no hub transition)\n",
		rec.WorkItemID, currentFingerprint)
}

// restartAttach is the same-session paused resume (M3): SessionAttach
// flips the WI back to running under the SAME secret, then the recovery
// engine resumes the local capsule (its resume-first cascade continues
// the transcript the graceful shutdown synced; a mid-seal record finishes
// its interrupted stop instead).
func (r *Runtime) restartAttach(ctx context.Context, rec *jobRecord) error {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		return fmt.Errorf("runtime: %s: no seeded record", rec.WorkItemID)
	}
	if _, err := r.hub.SessionAttach(rec.WorkItemID, api.SessionAttachRequest{
		WorkItemID: rec.WorkItemID,
		SessionID:  rec.SessionID,
		MachineID:  r.cfg.MachineID,
		Owner:      r.cfg.MachineID,
	}); err != nil {
		return err
	}
	return r.restartResume(ctx, rec, j)
}

// restartDrop handles aihub's terminal verdicts: no revival. A local
// agent still alive is a writer of a dead session — its tree is collected
// and the record dropped.
func (r *Runtime) restartDrop(rec *jobRecord) {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		j = jobFromRecord(rec)
	}
	if j.agentPID > 0 {
		// dead or alive: either way the tree's orphans must not survive
		// (killAgentTree's identity guard refuses to signal a reused pid)
		r.killAgentTree(j)
	}
	r.forgetRecord(rec)
}

func (r *Runtime) forgetRecord(rec *jobRecord) {
	r.mu.Lock()
	delete(r.jobs, rec.WorkItemID)
	r.mu.Unlock()
}

// restartRunning handles the verify-matches running verdict for a locally
// recorded job (the crash-mid-run shape): kill the old tree and run the
// recovery engine. A record caught mid-seal (the daemon died between
// SEALING and SessionEnd) finishes the interrupted stop instead.
func (r *Runtime) restartRunning(ctx context.Context, rec *jobRecord) error {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		return fmt.Errorf("runtime: %s: no seeded record", rec.WorkItemID)
	}
	return r.restartResume(ctx, rec, j)
}

// restartResume is the shared tail of the attach and crash-resume paths.
func (r *Runtime) restartResume(ctx context.Context, rec *jobRecord, j *job) error {
	state, _ := j.snapshot()
	if state == StateSealing {
		// finish the interrupted close — the sealed reason passes through
		// verbatim (wrapped|failed|blocked); anything a crashed stop never
		// wrote reads as the safest default
		reason := j.stopReason
		if reason != protocol.FinishFailed && reason != protocol.FinishWrapped &&
			reason != protocol.FinishBlocked {
			reason = protocol.FinishWrapped
		}
		j.mu.Lock()
		sealed := j.sealedPush
		j.mu.Unlock()
		return r.stopOp(ctx, j, reason, true, sealed)
	}
	if state == StateDone {
		r.forgetRecord(rec)
		return nil
	}
	if state == StatePaused {
		// Finding 2 (v1.5 review): a RETAINED paused record returns to
		// RUNNING through the legal edge before the engine resumes it —
		// reached from the attach path (the hub said paused +
		// clean_pause=false after the operator's resume) and as the
		// self-heal of a pause close whose SessionEnd never reached the
		// hub (verify answered running: the hub is the authority and
		// still wants the work item run).
		if err := j.transition(StateRunning); err != nil {
			return err
		}
	}
	// RUNNING, the just-resumed Paused record, or a crash mid-RECOVERING:
	// all recover through the engine (recoverCore kills the old tree
	// itself — with the pid-identity guard when the recorded agent still
	// lives, or its orphaned group when it does not).
	j.mu.Lock()
	j.state = StateRunning
	j.mu.Unlock()
	r.recoverCore(ctx, j, "daemon_restart")
	return nil
}
