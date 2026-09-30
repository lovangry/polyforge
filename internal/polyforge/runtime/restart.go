// restart.go: daemon-startup reconciliation (v1-spec-full §十一). The
// Runtime never revives from state.json alone — every recorded job is
// reconciled against aihub's own verdict first:
//
//	aihub says wrapped/failed/canceled → do NOT revive; collect any local
//	  residue and drop the record.
//	aihub says running → the old agent tree is killed and the WI runs
//	  through the recovery engine (RTRecovered), unless the local record
//	  was mid-seal, in which case the interrupted stop is finished.
//	aihub says blocked → leave everything untouched (human attention).
package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Restart loads state.json, reconciles every recorded job with aihub and
// recovers what is still ours to run. Jobs aihub no longer knows as ours
// are dropped; a job with no local record needs no reconciliation (a
// running WI whose capsule lives on another machine is that machine's
// business).
func (r *Runtime) Restart(ctx context.Context) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	st, err := loadState(r.statePath)
	if err != nil {
		return err
	}
	// Seed every recorded job into the live map first: persist() rewrites
	// state.json from the map, so records that must SURVIVE the
	// reconciliation (blocked, aihub-unreachable) are preserved by simply
	// staying in it. A record whose WI is already live (Start ran before
	// Restart) is left entirely alone.
	for _, rec := range st.Jobs {
		if r.jobOf(rec.WorkItemID) != nil {
			continue
		}
		r.mu.Lock()
		r.jobs[rec.WorkItemID] = jobFromRecord(rec)
		r.mu.Unlock()
	}
	// D5, after seeding: replay every claim that crashed between the
	// aihub call and the spawn (a claiming-journal entry predates any
	// state.json record for that work item, but the replay's
	// already-active check must see the seeded records). The SAME
	// idempotency key hands back the SAME attempt, so the replay either
	// finishes the start or closes the generation blocked.
	r.resumeClaiming(ctx)
	for _, rec := range st.Jobs {
		if j := r.jobOf(rec.WorkItemID); j != nil && j.liveAgent() {
			continue // already running under this daemon: not a restart case
		}
		wi, err := r.hub.GetWorkItem(rec.WorkItemID)
		if err != nil {
			// aihub unreachable for this WI: keep the record, try again on
			// the next restart — never guess a verdict.
			fmt.Fprintf(os.Stderr, "runtime: %s: aihub lookup failed, record kept: %v\n", rec.WorkItemID, err)
			continue
		}
		// Pre-F2 residue: a record keyed by a slug ("<project>#<seq>") or
		// any other non-canonical id form predates the canonical-id
		// ruling. Migrate it in place — re-key by the canonical id, fix
		// the push remote (the aihub bare repo is canonical-named; the
		// slug-built remote URL was a 404) and rename the on-disk capsule
		// and git-dir when both exist — so the reconciliation below and
		// every later push run on the canonical id.
		if wi.ID != "" && wi.ID != rec.WorkItemID {
			rec = r.migrateRecord(rec, wi.ID)
		}
		switch wi.Status {
		case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
			r.restartDrop(rec)
		case api.StatusBlocked:
			// 不动: the bounded-fail verdict stands; no revival, no kill.
		case api.StatusRunning:
			if serr := r.restartRunning(ctx, rec); serr != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: restart recovery failed: %v\n", rec.WorkItemID, serr)
			}
		default:
			// queued/paused: the generation is no longer ours; drop the
			// bookkeeping (the capsule scene stays on disk).
			r.forgetRecord(rec)
		}
	}
	return r.persist()
}

// migrateRecord re-keys one pre-F2 record under its canonical work-item id
// (in-place restart migration, §十一 + F2):
//
//   - the push remote is ALWAYS re-derived from the canonical id — the
//     aihub bare repo is canonical-named and a slug-built remote URL was
//     a 404 (the "#" fragment separator), so this one fix is what makes
//     a resumed seal push reach the server at all;
//   - the on-disk capsule and external git-dir are renamed onto their
//     canonical-derived paths when the old ones exist and the new ones
//     do not (best-effort: a rename failure keeps the old paths, which
//     stay valid — the remote URL and the local git-dir name are
//     independent);
//   - the capsule's .git pointer is rewritten when the git-dir moved;
//   - the record's session/transcript paths are re-derived from the
//     (possibly renamed) capsule root;
//   - the job map entry is re-keyed and the old spelling registered as
//     an alias, so route-level lookups by the operator's spelling still
//     resolve.
//
// The returned record is the migrated one; the caller's switch runs on it.
func (r *Runtime) migrateRecord(rec *jobRecord, canonical string) *jobRecord {
	old := rec.WorkItemID
	out := *rec
	capDir, gitDir := rec.CapsuleDir, rec.GitDir
	n := func(err error, what string) {
		fmt.Fprintf(os.Stderr, "runtime: %s: %s migration kept the old path: %v\n", old, what, err)
	}
	if rec.CapsuleDir != "" {
		newCapsule := r.capsuleDir(canonical)
		if dirExists(rec.CapsuleDir) && !dirExists(newCapsule) {
			if err := os.Rename(rec.CapsuleDir, newCapsule); err != nil {
				n(err, "capsule dir")
			} else {
				capDir = newCapsule
			}
		}
	}
	if rec.GitDir != "" {
		newGit := r.gitDirFor(canonical)
		if dirExists(rec.GitDir) && !dirExists(newGit) {
			if err := os.Rename(rec.GitDir, newGit); err != nil {
				n(err, "git dir")
			} else {
				gitDir = newGit
				// the capsule's .git pointer names the git-dir absolutely;
				// keep it honest after the move (gitops works from GIT_DIR env
				// anyway — the pointer is for humans and tools in the scene).
				if capDir != rec.CapsuleDir || dirExists(capDir) {
					_ = os.WriteFile(filepath.Join(capDir, ".git"),
						[]byte("gitdir: "+gitDir+"\n"), 0o644)
				}
			}
		}
	}
	out.WorkItemID = canonical
	out.CapsuleDir = capDir
	out.GitDir = gitDir
	out.RemoteURL = r.remoteURLFor(canonical)
	out.Username = canonical
	out.SessionID = fmt.Sprintf("sess_%s_%d", canonical, rec.Generation)
	if capDir != "" && rec.AttemptID != "" {
		out.Transcript = sessionPathFor(capDir, rec.AttemptID)
	}
	fmt.Fprintf(os.Stderr, "runtime: migrated pre-canonical record %q → %q (capsule %s, remote %s)\n",
		old, canonical, out.CapsuleDir, out.RemoteURL)
	r.mu.Lock()
	delete(r.jobs, old)
	r.jobs[canonical] = jobFromRecord(&out)
	r.mu.Unlock()
	r.NoteAlias(old, canonical)
	return &out
}

// restartDrop handles aihub's terminal verdicts: no revival. A local agent
// still alive is a writer of a dead generation — its tree is collected and
// the record dropped.
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

// restartRunning handles aihub's running verdict for a locally recorded
// job: kill the old tree and run the recovery engine. A record caught
// mid-seal (the daemon died between SEALING and EndGeneration) finishes
// the interrupted stop instead of reviving.
func (r *Runtime) restartRunning(ctx context.Context, rec *jobRecord) error {
	j := r.jobOf(rec.WorkItemID)
	if j == nil {
		return fmt.Errorf("runtime: %s: no seeded record", rec.WorkItemID)
	}
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
		return r.stopOp(ctx, j, reason, true)
	}
	if state == StateDone {
		r.forgetRecord(rec)
		return nil
	}
	// RUNNING or a crash mid-RECOVERING: both recover through the engine
	// (recoverCore kills the old tree itself — with the pid-identity guard
	// when the recorded agent still lives, or its orphaned group when it
	// does not).
	j.mu.Lock()
	j.state = StateRunning
	j.mu.Unlock()
	r.recoverCore(ctx, j, "daemon_restart")
	return nil
}
