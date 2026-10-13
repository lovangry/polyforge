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
//	  attach) — UNLESS the record carries a pending terminal close
//	  (stop_reason wrapped|failed|blocked, the NEW-3 crash window: the
//	  daemon died between the fromPaused persist and SessionEnd): the
//	  close is replayed so the wrap/fail intent is never forgotten.
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
//	aihub unreachable → keep the record (shown verify_pending) and retry
//	  the verify in-process with backoff until aihub answers, then proceed
//	  as above — never guess a verdict.
//
// A mid-flight record whose capsule directory is missing, or whose project
// fell out of the [workspace] mapping, is NOT a startup failure (v2-converge
// §6): that ONE work item goes through the ordinary bounded-fail close
// (blockUnrecoverable — SessionEnd(blocked) with a reason naming the missing
// path or the unmapped project, a capsule_missing / project_unmapped event
// for `pf watch`), the daemon starts and every other work item reconciles as
// above. The WI is then plain `blocked` at aihub and nothing stays parked
// locally; the owner fixes the cause if any and `pf resume`s (a fresh
// re-claim from the capsule chain and the task-repo carrier), or cancels it
// through the ordinary blocked exits. An end_pending record (a bounded-fail
// verdict the hub has not yet heard) is retried first, needing no capsule.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// MissingCapsuleError is the typed mid-flight failure (#103): a recorded
// non-terminal work item's capsule directory is absent under the active
// workspace — the operator has not moved it to the new base (or it was
// lost). The daemon never rebuilds such a capsule from the remote: the
// capsule git chain IS the revision chain, and a re-clone would silently
// discard unpublished local state. At restart it blocks ONLY that work item
// (v2-converge §6; it is not a startup failure); `pf resume <wi>` re-claims
// it from the capsule chain.
type MissingCapsuleError struct {
	WI  string
	Dir string
}

func (e *MissingCapsuleError) Error() string {
	return fmt.Sprintf("work item %s: mid-flight capsule missing at %s — it is NOT recoverable from the remote (unpublished local state would be lost); restore the capsule to that path manually, then `pf resume %s` (or restart the daemon); or give up with `pf cancel %s`", e.WI, e.Dir, e.WI, e.WI)
}

// UnmappedMidflightError is the #113 mid-flight failure posture: a
// recorded work item's project has no workspace in the CURRENT mapping
// (the operator's config edit dropped its section). The capsule is NOT
// relocated; at restart ONLY that work item is blocked (v2-converge §6) —
// the operator re-adds the workspace section, then `pf resume <wi>`; the
// existing no-rebuild rule stands.
type UnmappedMidflightError struct {
	WI      string
	Project string
}

func (e *UnmappedMidflightError) Error() string {
	return fmt.Sprintf("work item %s: mid-flight project %q is not in the [workspace] mapping — the capsule is NOT relocated and the work item is not recoverable until its workspace is re-added to the mapping in the config, then `pf resume %s` (or restart the daemon) (`pf doctor` checks the mapping)", e.WI, e.Project, e.WI)
}

// requireMidflightCapsule enforces the no-silent-rebuild contract for a
// work item this daemon proceeds with (the #103 guard, carried onto the
// mapping): the record's project MUST be mapped (an unmapped mid-flight
// project fails loudly, naming it — never relocated, never rebuilt), and
// its capsule directory must exist. The persisted record's CapsuleDir is
// the truth (in-flight capsules keep their homes across mapping
// re-adds); a record predating the field falls back to the record's
// project routed through the current mapping.
func (r *Runtime) requireMidflightCapsule(wi, project, recordedDir string) error {
	ws, merr := r.cfg.Mapping.WorkspaceFor(project)
	if merr != nil {
		return &UnmappedMidflightError{WI: wi, Project: project}
	}
	dir := recordedDir
	if dir == "" {
		dir = filepath.Join(ws, "capsules", wi)
	}
	if dirExists(dir) {
		return nil
	}
	return &MissingCapsuleError{WI: wi, Dir: dir}
}

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
			// a torn write: no session to verify,
			// nothing to reconcile — drop it rather than guess.
			fmt.Fprintf(os.Stderr, "runtime: %s: record without a session dropped\n", rec.WorkItemID)
			continue
		}
		r.mu.Lock()
		r.jobs[rec.WorkItemID] = r.restoredJob(rec)
		r.mu.Unlock()
	}
	for _, rec := range st.Jobs {
		if rec.SessionID == "" {
			continue
		}
		// One bad record never aborts the daemon (v2 §6): a missing
		// capsule / unmapped project parks that WI alone; everything else
		// keeps its record-kept posture. The error return is the
		// aihub-unreachable verdict only, which Restart logs and moves on.
		_ = r.restartOne(ctx, rec)
	}
	return r.persist()
}

// retryVerify is the background half of a failed restart verify
// (verify_pending): re-run the whole restartOne reconciliation on the
// end_pending backoff until aihub gives a verdict, so the WI proceeds
// without waiting for the next daemon restart. One goroutine per WI.
func (r *Runtime) retryVerify(j *job) {
	r.goRetry("verify:"+j.wi, func() bool {
		r.opMu.Lock()
		defer r.opMu.Unlock()
		if cur := r.jobOf(j.wi); cur != j {
			return true // dropped or replaced meanwhile: nothing to verify
		}
		j.mu.Lock()
		pending := j.verifyPending
		j.mu.Unlock()
		if !pending {
			return true
		}
		if err := r.restartOne(context.Background(), j.record()); err != nil {
			return false
		}
		if err := r.persist(); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
		}
		return true
	})
}

// restartRelease is the superseded-at-restart path: the WI is held by
// another session (currentFingerprint names the public holder). The local
// tree is a zombie writer and the capsule a foreign scene — kill,
// quarantine in place, drop the record, leave a durable trace.
func (r *Runtime) restartRelease(rec *jobRecord, currentFingerprint string) {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		j = r.restoredJob(rec)
	}
	r.killAgentTree(j) // pid 0 (driver lane) reaps via journal + env marker
	if err := quarantineCapsule(j.capsuleDir, j.git.GitDir); err != nil {
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
	// Guard BEFORE the hub call (#103/#113): a missing capsule must not
	// first flip the WI to running server-side and only then fail.
	if err := r.requireMidflightCapsule(rec.WorkItemID, rec.Project, rec.CapsuleDir); err != nil {
		return err
	}
	if _, err := r.hub.SessionAttach(rec.WorkItemID, api.SessionAttachRequest{
		WorkItemID: rec.WorkItemID,
		SessionID:  rec.SessionID,
		MachineID:  r.cfg.MachineID,
		Owner:      r.cfg.MachineID,
	}); err != nil {
		return err
	}
	return r.restartResume(ctx, rec, j, causeOperatorAttach, "session attach after a graceful shutdown")
}

// restartDrop handles aihub's terminal verdicts: no revival. A local
// agent still alive is a writer of a dead session — its tree is collected
// and the record dropped.
func (r *Runtime) restartDrop(rec *jobRecord) {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		j = r.restoredJob(rec)
	}
	// dead or alive: either way the tree's orphans must not survive
	// (killAgentTree's identity guard refuses to signal a reused pid)
	r.killAgentTree(j)
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
	return r.restartResume(ctx, rec, j, causeDaemonRestart, "daemon restart (crash mid-run)")
}

// isTerminalStopReason reports whether a persisted stop_reason is a
// TERMINAL close verdict (wrapped|failed|blocked) — as opposed to the
// retained operator pause's own "paused" reason or an unset one. The
// NEW-3 crash window (a pending close of a retained Paused record) is
// told apart from a plain retained pause exactly here.
func isTerminalStopReason(reason string) bool {
	return reason == protocol.FinishWrapped ||
		reason == protocol.FinishFailed ||
		reason == protocol.FinishBlocked
}

// restartResume is the shared tail of the attach and crash-resume
// paths. recoveryCause names the trigger the recovery engine logs (and,
// polyforge#79, the interrupted-dispatch archive's classification input:
// an operator-shaped attach cause archives nothing — a deliberate stop
// is not a model verdict — while a daemon_restart re-derivation archives
// the dispatch the crash interrupted).
func (r *Runtime) restartResume(ctx context.Context, rec *jobRecord, j *job, recoveryCause, detail string) error {
	// The resume/recovery entry's own guard (#103/#113): whatever route
	// led here, a mid-flight record whose project is unmapped or whose
	// capsule is absent at its mapped base is the typed hard failure —
	// the recovery engine must never see it and re-materialize or
	// re-derive against a scene that is not there.
	if err := r.requireMidflightCapsule(rec.WorkItemID, rec.Project, rec.CapsuleDir); err != nil {
		return err
	}
	// crash orphans die BEFORE anything is re-dispatched (single writer)
	r.reapOrphans(rec.WorkItemID, j.capsuleDir)
	state, _ := j.snapshot()
	if state == StateSealing {
		// finish the interrupted close — a RECORDED verdict passes through
		// verbatim (wrapped|failed|blocked). A reason the crashed stop never
		// wrote is NOBODY's verdict: finishing it wrapped would report a
		// success no one decided, so the close finishes BLOCKED — with a
		// note naming the interrupted, cause-unknown seal — and a human
		// decides (audit B2).
		reason := j.stopReason
		note := ""
		if !isTerminalStopReason(reason) {
			reason = protocol.FinishBlocked
			note = "seal interrupted by a daemon crash before any verdict was recorded (cause unknown) — finished blocked for a human to decide"
			fmt.Fprintf(os.Stderr, "runtime: %s: interrupted seal with no recorded verdict finished blocked (the human decides)\n", rec.WorkItemID)
			if r.cfg.EventTap != nil {
				r.cfg.EventTap(rec.WorkItemID, "interrupted_seal", map[string]any{"note": note})
			}
		}
		j.mu.Lock()
		sealed := j.sealedPush
		j.mu.Unlock()
		return r.stopOp(ctx, j, reason, true, sealed, note)
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
	r.recoverCore(ctx, j, recoveryCause, detail)
	return nil
}

// restartOne reconciles ONE recorded job with aihub (the body Restart
// runs per record). The error return is the aihub-unreachable verdict only
// — a bad capsule / unmapped project blocks the WI (blockUnrecoverable) and
// returns nil.
func (r *Runtime) restartOne(ctx context.Context, rec *jobRecord) error {
	if rec.SessionID == "" {
		return nil
	}
	if j := r.jobOf(rec.WorkItemID); j != nil && j.liveAgent() {
		return nil // already running under this daemon: not a restart case
	}
	// end_pending (v2-converge §6 bug 5): a bounded-fail whose
	// SessionEnd(blocked) never reached the hub. The record carries the
	// only copy of the session secret — retry the close now (and in the
	// background) before anything else; never verify-and-guess. The
	// close needs no capsule, so a missing capsule never blocks it.
	if rec.State == string(StateEndPending) {
		if j := r.jobOf(rec.WorkItemID); j != nil {
			r.restartEndPending(j)
		}
		return nil
	}
	v, err := r.hub.SessionVerify(rec.WorkItemID, rec.SessionID)
	if err != nil {
		// aihub unreachable for this WI: keep the record and retry the
		// verify in-process with backoff (like end_pending) until it
		// yields a verdict — never guess one, never wait for the next
		// daemon restart.
		fmt.Fprintf(os.Stderr, "runtime: %s: session verify failed, record kept, retrying: %v\n", rec.WorkItemID, err)
		if j := r.jobOf(rec.WorkItemID); j != nil {
			j.mu.Lock()
			j.verifyPending = true
			j.mu.Unlock()
			r.retryVerify(j)
		}
		return fmt.Errorf("runtime: %s: session verify failed: %w", rec.WorkItemID, err)
	}
	if j := r.jobOf(rec.WorkItemID); j != nil {
		j.mu.Lock()
		j.verifyPending = false
		j.mu.Unlock()
	}
	if v.Matches {
		switch v.Status {
		case api.StatusRunning:
			// crash-mid-run under this session: recover-resume the
			// local capsule (a mid-seal record finishes its stop).
			// The mid-flight capsule must be present at its mapped
			// base (#103/#113): a miss BLOCKS this WI alone at
			// aihub (v2 §6) — never a rebuild-from-remote, never a daemon
			// abort.
			if err := r.requireMidflightCapsule(rec.WorkItemID, rec.Project, rec.CapsuleDir); err != nil {
				r.blockUnrecoverable(ctx, rec, err)
				return nil
			}
			if serr := r.restartRunning(ctx, rec); serr != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: restart recovery failed: %v\n", rec.WorkItemID, serr)
			}
		case api.StatusPaused:
			if v.CleanPause {
				// NEW-3 (crash window): a TERMINAL stop_reason on the retained
				// record is a pending close — stopOp's fromPaused branch
				// persists the wrap/fail/blocked intent on the record BEFORE
				// the terminal SessionEnd goes out, and a daemon crash in that
				// window leaves the hub still answering paused+clean under our
				// (matching) session. Replay the close instead of reconciling
				// back to a plain retained pause: the SessionEnd is fenced on
				// the session secret the verify just proved matches, so the
				// terminal verdict lands (Paused → Done is a legal edge — the
				// same alreadySealing=true shape the StateSealing finisher
				// uses). A replay failure keeps the record and its intent —
				// the next restart re-verifies and retries.
				if j := r.jobOf(rec.WorkItemID); j != nil && isTerminalStopReason(rec.StopReason) {
					if r.cfg.EventTap != nil {
						r.cfg.EventTap(rec.WorkItemID, "pending_close_replay", map[string]any{
							"reason":              rec.StopReason,
							"session_fingerprint": j.fingerprint(),
							"note":                "crash between the retained-record close persist and SessionEnd; terminal close replayed at restart",
						})
					}
					fmt.Fprintf(os.Stderr, "runtime: %s: pending %s close of the retained paused record — replaying (crash window, NEW-3)\n", rec.WorkItemID, rec.StopReason)
					// The close replay proceeds against the local capsule
					// (#103): absent at the active base, the wrap/fail
					// intent must not be "replayed" out of thin air — the
					// WI is blocked instead.
					if err := r.requireMidflightCapsule(rec.WorkItemID, rec.Project, rec.CapsuleDir); err != nil {
						r.blockUnrecoverable(ctx, rec, err)
						return nil
					}
					j.mu.Lock()
					sealed := j.sealedPush
					j.mu.Unlock()
					if err := r.stopOp(ctx, j, rec.StopReason, true, sealed, ""); err != nil {
						fmt.Fprintf(os.Stderr, "runtime: %s: pending close replay failed, record kept: %v\n", rec.WorkItemID, err)
					}
					return nil
				}
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
				// re-verifies with a fresh verdict. A missing mid-flight
				// capsule / unmapped project (typed, #103/#113) blocks the
				// WI: the resume must never proceed without the local
				// scene, and never rebuild it from the remote.
				var miss *MissingCapsuleError
				var unm *UnmappedMidflightError
				if errors.As(aerr, &miss) || errors.As(aerr, &unm) {
					r.blockUnrecoverable(ctx, rec, aerr)
					return nil
				}
				fmt.Fprintf(os.Stderr, "runtime: %s: session attach failed, record kept: %v\n", rec.WorkItemID, aerr)
			}
		case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
			r.restartDrop(rec)
			r.clearManualModels(rec.WorkItemID)
		default:
			// queued/blocked while our session still matches (a torn
			// server state): treat as not-ours, drop the record.
			fmt.Fprintf(os.Stderr, "runtime: %s: hub says %s with our session; record dropped\n", rec.WorkItemID, v.Status)
			r.forgetRecord(rec)
		}
		return nil
	}
	// verify does NOT match: our session is no longer the holder (or
	// the row is gone entirely).
	switch v.Status {
	case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
		// the close settled server-side before the crash: collect any
		// local residue and drop the record — no quarantine, the
		// terminal scene is the preserved artifact.
		r.restartDrop(rec)
		r.clearManualModels(rec.WorkItemID)
	case api.StatusBlocked:
		// M2 storm guard: the bounded-fail verdict stands — no
		// revival, no kill, no quarantine; the record is kept so the
		// next boot re-checks (the unblock path re-claims through the
		// normal start, which quarantines the old scene itself).
		// Crash orphans are the one exception: no live step process
		// is legitimate for a blocked WI, and they would keep writing
		// the scene — reap only (no session action).
		r.reapOrphans(rec.WorkItemID, rec.CapsuleDir)
	case api.StatusQueued:
		// back in the pool: our claim is gone. A local tree (if any)
		// is a writer of a session nobody holds — collect it and drop
		// the record; the capsule stays for the next claim's
		// quarantine-or-clone decision.
		fmt.Fprintf(os.Stderr, "runtime: %s: session ended server-side (queued); record dropped\n", rec.WorkItemID)
		r.restartDrop(rec)
	default:
		// running or paused under ANOTHER session (a force-end
		// + re-claim happened while we were down): release — kill any local
		// tree, quarantine the capsule, drop the record. NO hub
		// transition: the new owner owns the work.
		r.restartRelease(rec, v.CurrentFingerprint)
	}
	return nil
}

// Restart-time block reason kinds — also the durable event kinds.
const (
	EventCapsuleMissing  = "capsule_missing"
	EventProjectUnmapped = "project_unmapped"
)

// blockUnrecoverable is the restart posture for ONE mid-flight record whose
// capsule is missing or whose project is unmapped: it takes the ordinary
// bounded-fail path (SessionEnd(blocked) with a reason naming the cause, the
// orphan sweep, the durable end_pending retry), so the work item shows
// `blocked` at aihub — the owner's `pf resume` re-claims it from the capsule
// chain and the task-repo carrier — and nothing stays parked locally. The
// daemon starts and every other work item reconciles as usual. A durable
// event names the cause for `pf watch`.
func (r *Runtime) blockUnrecoverable(ctx context.Context, rec *jobRecord, cause error) {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		j = r.restoredJob(rec)
		r.mu.Lock()
		r.jobs[rec.WorkItemID] = j
		r.mu.Unlock()
	}
	kind := EventCapsuleMissing
	body := map[string]any{"work_item": rec.WorkItemID, "project": rec.Project}
	var detail string
	var miss *MissingCapsuleError
	var unm *UnmappedMidflightError
	switch {
	case errors.As(cause, &unm):
		kind = EventProjectUnmapped
		body["project"] = unm.Project
		detail = fmt.Sprintf("project %q is not in the daemon's [workspace] mapping — re-add it to the config, then `pf resume %s`", unm.Project, rec.WorkItemID)
	case errors.As(cause, &miss):
		body["path"] = miss.Dir
		detail = fmt.Sprintf("capsule missing at %s — it is never rebuilt from the remote; `pf resume %s` re-claims the work item from its capsule chain", miss.Dir, rec.WorkItemID)
	default:
		detail = cause.Error()
	}
	body["reason"] = detail
	if r.cfg.EventTap != nil {
		r.cfg.EventTap(rec.WorkItemID, kind, body)
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: blocked at restart: %s\n", rec.WorkItemID, detail)
	// the record is a restart-reconciled job: enter the recovery state so
	// boundedFail's closing edge (-> done | end_pending) is a legal one
	j.mu.Lock()
	j.state = StateRecovering
	j.mu.Unlock()
	r.boundedFail(ctx, j, detail)
}

// Recovery causes that are lifecycle events, exempt from the storm ledger.
const (
	causeDaemonRestart  = "daemon_restart"
	causeOperatorAttach = "operator daemon shutdown"
)
