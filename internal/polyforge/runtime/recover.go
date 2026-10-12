// recover.go: the recovery engine (v1-spec-full §七) and the recovery
// lifecycle entry (§一 Recover). Discriminator = transcript availability
// (NOT same-machine/cross-machine — cross-machine recovery gets the
// transcript for free through capsule git):
//
//	transcript readable → resume (attach the harness to the recorded
//	  session; seconds)
//	resume failing twice consecutively → fresh derivation (a brand-new
//	  agent that may read the capsule's old transcripts for context)
//	fresh failing → one FAILED recovery: the budget counts failures and
//	  a successful recovery RESETS it. After MaxRecoveries (default 2)
//	  consecutive failed recoveries the WI terminates
//	  RTFinished(blocked) — the storm guard: never a third relaunch
//	  attempt after two failed ones, and never a poisoned budget after
//	  a healthy relaunch.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/common/workflow"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// cleanupBound bounds one teardown's waits inside the six-layer cleanup.
const cleanupBound = 10 * time.Second

// recoverOp runs one recovery event from the message loop. Callers have
// already verified the attempt-id fence and the RUNNING state; opMu
// serializes it against Start/Stop/Restart.
func (r *Runtime) recoverOp(ctx context.Context, j *job, cause, detail string) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.recoverCore(ctx, j, cause, detail)
}

// recoverCore is the recovery sequence; the caller holds opMu. cause
// names the recovery trigger (the storm guard's ledger key and the
// interrupted-dispatch archive's class input); detail is the observe
// lane's free-text elaboration ("" when none).
func (r *Runtime) recoverCore(ctx context.Context, j *job, cause, detail string) {
	if err := j.transition(StateRecovering); err != nil {
		return // lost the race to SEALING/DONE: the fence did its job
	}
	// Mid-flight vanish-after-start guard (#103): the engine below
	// re-materializes capsule residue (work-item file, bundle set) and
	// re-derives agents — against an ABSENT capsule that is a silent
	// rebuild, exactly what the contract forbids for unpublished local
	// state. Refuse instead with the loud bounded-fail verdict naming
	// the WI and the missing path: the operator restores the capsule,
	// then unblocks (M2 — the human is the only remediation door).
	if err := r.requireMidflightCapsule(j.wi, j.project, j.capsuleDir); err != nil {
		if r.cfg.EventTap != nil {
			r.cfg.EventTap(j.wi, "missing_midflight_capsule", map[string]any{
				"capsule_dir": j.capsuleDir,
				"cause":       cause,
				"note":        "mid-flight capsule absent at the active workspace; no rebuild from remote — restore the capsule, then unblock",
			})
		}
		fmt.Fprintf(os.Stderr, "runtime: %s: %v\n", j.wi, err)
		r.boundedFail(ctx, j, err.Error())
		return
	}
	// v2 terminal wiring (the #99 finale): Run returning nil IS the
	// driver's completion signal, and the
	// monitor's onExit rightly reports "agent process exited" without
	// knowing the difference. Before any recovery machinery runs, the
	// capsule's terminal workflow_status is translated into the WI's
	// close: completed wraps, failed fails, blocked hands the WI to its
	// owner (the hub's blocked verdict is the unblock door). Without
	// this the terminal exit bounced through death-recovery into the
	// storm guard and closed the WI as blocked instead of wrapped —
	// within one second, exactly as the guard was designed to.
	switch capsuleWorkflowStatus(j.capsuleDir) {
	case capsule.WorkflowStatusCompleted:
		fmt.Fprintf(os.Stderr, "runtime: %s: driver workflow completed — wrapping (recovery cause %q is the completion signal, not a death)\n", j.wi, cause)
		// alreadyPushed: the C3 engine already committed and pushed the
		// terminal execution.json version (every transition bumps Version
		// and publishes); the final push here would only race it.
		if err := r.stopOp(ctx, j, protocol.FinishWrapped, false, true); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: terminal wrap failed (record kept for the next restart): %v\n", j.wi, err)
		}
		return
	case capsule.WorkflowStatusFailedGate, capsule.WorkflowStatusFailedRewindExhausted:
		fmt.Fprintf(os.Stderr, "runtime: %s: driver workflow failed (%s) — closing failed (recovery cause %q)\n", j.wi, capsuleWorkflowStatus(j.capsuleDir), cause)
		if err := r.stopOp(ctx, j, protocol.FinishFailed, false, true); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: terminal fail close failed (record kept for the next restart): %v\n", j.wi, err)
		}
		return
	case capsule.WorkflowStatusBlocked:
		fmt.Fprintf(os.Stderr, "runtime: %s: driver workflow blocked on an incomplete result — the owner holds the next move (cause %q)\n", j.wi, cause)
		// The parked verdict's chain durability (polyforge#116): the bounded
		// close below drops the job with NO seal — unlike the completed and
		// failed branches, nothing publishes the blocked execution.json — so
		// the verdict would reach the chain only through the monitor's next
		// tick, a race the owner's resume face must not inherit (a later
		// re-materialization would clone pre-blocked state and silently
		// discard the verdict — the workspace where the same re-claim both
		// wedges and escapes depending on a timing race). Seal the parked
		// capsule explicitly: kill the (already returned) driver's tree, then
		// commit+push the parked state — journal, step output and the blocked
		// verdict together. The §6.4 owner moves (resume / wrap --fail) and
		// every re-materialization read exactly this state.
		r.boundedFail(ctx, j, "workflow blocked: the owner holds the next move (resume / rewind / wrap --fail)")
		return
	}
	// The v2 entry-rate storm guard (the #97 lesson): a relaunch that
	// SUCCEEDS as a spawn but dies within seconds used to reset the
	// failure budget on every iteration — a poisoned capsule (a rejected
	// envelope, a wedged driver) then recovered forever at ~1 Hz, each
	// iteration spawning a real step process and pushing a commitDirty.
	// The budget's reset-on-success cannot see a fast death, so the RATE
	// of re-entry is the missing signal: a recoverCore that fires again
	// inside the stability window (default 60s) is one storm unit,
	// counted on its own ledger; a re-entry after a stable gap resets it
	// (the previous relaunch survived — a fresh episode). The counter is
	// deliberately separate from recoverFails: ladder-internal retries
	// and their reset-on-success semantics (§三) are unchanged.
	now := time.Now()
	j.mu.Lock()
	if !j.lastRecoveryAt.IsZero() && now.Sub(j.lastRecoveryAt) < r.recoveryStabilityWindow() {
		j.stormFails++
	} else {
		j.stormFails = 0
	}
	j.lastRecoveryAt = now
	storm := j.stormFails
	j.mu.Unlock()
	if storm >= r.cfg.MaxRecoveries {
		r.boundedFail(ctx, j, fmt.Sprintf(
			"recovery storm: %d recoveries re-fired inside the %.0fs stability window (latest cause %q) — the relaunch is dying faster than it can stabilize",
			storm, r.recoveryStabilityWindow().Seconds(), cause))
		return
	}
	// Kill the old tree first (§一 Recover): six layers, no writer may
	// survive next to the re-derivation.
	r.killAgentTree(j)

	// commit-dirty-on-recover (§六): the dead agent's unflushed capsule
	// state is committed and pushed BEFORE the new instance can touch the
	// worktree. Best-effort by design: a failed push surfaces at the next
	// boundary; the local commit already keeps the bytes.
	commitDirty(r, j)

	// F1 repair: a capsule without runtime-state/work-item.json (a pre-F1
	// pre-F1 scene) gets it re-materialized before the next derivation
	// wakes up blind to its own goal. Best-effort, idempotent.
	r.ensureWorkItemFile(j)

	// F7 repair: a capsule whose materialized bundle set is missing its
	// completion marker (a crash-torn snapshot) — or predates the product
	// fix entirely — gets the set idempotently rebuilt, marker
	// included, before the next derivation wakes up. The
	// rebuild never breaks a scene the dead agents already read (staged
	// renames, identical content). Best-effort, never blocks recovery.
	r.ensureMaterialized(j)

	// v2 bundle freshness (the stale-capsule lesson): the operator may
	// have evolved the workflow bundle under workflows-dir AFTER this
	// WI's claim; recovery is the heal point where the capsule is
	// re-pinned to the checkout's current content and re-materialized,
	// so the relaunched driver validates against the CURRENT graph
	// instead of the claim-era one. Best-effort, loud on failure.
	r.refreshBundle(j)

	// The engine binding is the driver or NOTHING (polyforge#106): the
	// legacy in-package WI-Agent spawn is deleted, so a nil starter is a
	// misassembled runtime, not an engine choice — refused loudly, never
	// fallen back from.
	starter := r.cfg.AgentStarter
	if starter == nil {
		r.boundedFail(ctx, j, "runtime misassembly: no execution engine bound (Config.AgentStarter is nil) — the legacy in-package WI-Agent engine was deleted (polyforge#106); the daemon binds the driver bridge starter unconditionally at assembly")
		return
	}

	// The storm guard (§三): each FAILED recovery ladder spends one unit
	// of the budget; a successful recovery RESETS it (a healthy relaunch
	// must never inherit earlier failures). Only when MaxRecoveries
	// recoveries have FAILED outright does the WI bounded-fail — never
	// merely because it has recovered before.
	//
	// polyforge#79: the effective lists this re-derivation dispatches
	// under (rehydrated or re-derived — see
	// effectiveCandidatesForRecovery) ride every relaunch attempt, and
	// the interrupted dispatch (the death that forced this recovery)
	// is archived by the driver before the re-dispatch: the
	// recomputation then advances on provider-class causes alone. The
	// archive signal is the raw cause — the bridge classifies it and
	// filters operator-shaped stops (a deliberate stop is not a model
	// verdict).
	stepCandidates := r.effectiveCandidatesForRecovery(j)
	interruptedCause := cause
	for {
		// Driver WI: restart the Go Driver via the bound engine starter
		// (it reads execution.json and continues from the current step;
		// the recovery steps above — killTree, commitDirty — already ran).
		// The relaunch is a fresh derivation (B2): advance the counter.
		// The seed is the CAPSULE SPAWN JOURNAL max, not the job's counter
		// (the #99 verification lesson): the driver's internal step ordinals
		// run ahead of the relaunch counter without ever writing back, so a
		// job-counter seed re-derives an ordinal the dead driver already
		// used — two step processes on one drop path, the fence defeated.
		// The journal is the one ledger every derivation appends to;
		// seeding past its max restores the fence.
		j.mu.Lock()
		if m := maxSpawnNFromJournal(j.capsuleDir); m > j.spawnN {
			j.spawnN = m
		}
		j.spawnN++
		ordinal := int64(j.spawnN)
		j.mu.Unlock()
		h, err := starter(ctx, AgentStartInput{
			WI:            j.wi,
			Project:       j.project,
			CapsuleDir:    j.capsuleDir,
			BundleName:    j.bundleName,
			BundleDigest:  j.bundleDigest,
			SessionHandle: j.sessionFingerprint,
			SpawnN:        ordinal,
			Spawner:       r.spawner,
			// polyforge#79: the effective lists this re-derivation
			// dispatches under, and the archive signal for the dispatch
			// the recovery cause interrupted.
			StepCandidates:   stepCandidates,
			InterruptedCause: interruptedCause,
		})
		if err == nil {
			// A bridge handle fronts no child process group (its PID()
			// answers the hosting daemon's own pid — never signalable), so
			// the recorded pid is zeroed for it; a scripted test engine's
			// real child pid is recorded verbatim for the tree killer and
			// the persisted record.
			pid := h.PID()
			if pid == os.Getpid() {
				pid = 0
			}
			j.mu.Lock()
			j.agent = h
			j.agentPID = pid
			j.agentStartedAt = h.StartedAt()
			j.mu.Unlock()
			// persisted with the advanced counter (crash-safe)
			if perr := r.persist(); perr != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
			}
			r.recovered(j, h, cause)
			return
		}
		fmt.Fprintf(os.Stderr, "runtime: %s: engine relaunch failed: %v\n", j.wi, err)
		j.mu.Lock()
		j.recoverFails++
		fails := j.recoverFails
		j.mu.Unlock()
		if perr := r.persist(); perr != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
		}
		if fails >= r.cfg.MaxRecoveries {
			r.boundedFail(ctx, j, fmt.Sprintf("recovery budget exhausted (%d consecutive failed recoveries, cause %q)", fails, cause))
			return
		}
		fmt.Fprintf(os.Stderr, "runtime: %s: recovery failed (%d/%d), retrying the ladder\n", j.wi, fails, r.cfg.MaxRecoveries)
	}
}

// effectiveCandidatesForRecovery answers the per-step effective
// candidate lists a recovery re-derivation dispatches under
// (polyforge#79):
//
//   - a LIVE job carries its claim-time snapshot — an in-process
//     recovery keeps the claim-time list (an edit governs the NEXT
//     claim, never an in-flight job's recomputation);
//   - a restart-reconstructed job carries no snapshot: the effective
//     lists re-derive from the workspace yaml FRESH through the same
//     compose, so the archived-attempt recomputation indexes the
//     re-derived list (the mid-flight-edit restart shape);
//   - a yaml that fails to re-read falls back to the capsule.json
//     audit — the claim-time snapshot materialize wrote;
//   - an audit-less capsule rehydrates to nil (bundle-only, today's
//     behavior).
func (r *Runtime) effectiveCandidatesForRecovery(j *job) map[string][]string {
	j.mu.Lock()
	snap := j.stepCandidates
	j.mu.Unlock()
	if snap != nil {
		return snap
	}
	if ws, werr := r.cfg.Mapping.WorkspaceFor(j.project); werr == nil {
		wcfg, cerr := workspace.Load(ws)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: workspace yaml unreadable at recovery (%v); the claim-time audit stands\n", j.wi, cerr)
		} else {
			return workspace.ComposeCandidates(r.readCapsuleBundleFrontmatter(j), wcfg)
		}
	}
	return readCapsuleEffectiveCandidates(j.capsuleDir)
}

// readCapsuleBundleFrontmatter reads the job's MATERIALIZED bundle
// frontmatter from the capsule — the claim-era graph the driver
// validates against (never the daemon checkout, which the operator may
// have evolved).
func (r *Runtime) readCapsuleBundleFrontmatter(j *job) *workflow.Frontmatter {
	if j.bundleName == "" || j.capsuleDir == "" {
		return nil
	}
	fm, err := workflow.ReadFrontmatter(filepath.Join(j.capsuleDir, capsule.DirWorkflows, j.bundleName, "workflow.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: materialized bundle frontmatter unreadable (treating as absent): %v\n", j.wi, err)
		return nil
	}
	return fm
}

// readCapsuleEffectiveCandidates reads the effective_candidates audit
// of the capsule.json on disk (nil when absent — an audit-less capsule
// rehydrates to nil, bundle-only behavior).
func readCapsuleEffectiveCandidates(capsuleDir string) map[string][]string {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.CapsuleJSON))
	if err != nil {
		return nil
	}
	var idn capsule.Identity
	if json.Unmarshal(raw, &idn) != nil {
		return nil
	}
	return idn.EffectiveCandidates
}

// recovered finishes a successful re-derivation. A successful recovery
// RESETS the failure budget — the counter is spent by failures only.
// v1.5: RTRecovered carries SpawnN (the B2 fence key) and the in-process
// session secret (the observe lane's M4 revalidation reads it); identity
// otherwise changes NOT AT ALL — no new attempt, no generation bump. (The
// resume-failure bookkeeping died with the legacy resume→fresh cascade —
// polyforge#106: every relaunch is a fresh driver derivation.)
func (r *Runtime) recovered(j *job, h AgentHandle, cause string) {
	j.mu.Lock()
	j.recoverFails = 0
	spawnN := j.spawnN
	pid := j.agentPID
	j.mu.Unlock()
	if err := j.transition(StateRunning); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: recovery could not return to RUNNING: %v\n", j.wi, err)
	}
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	r.send(protocol.RTMsg{
		Kind:      protocol.RTRecovered,
		SpawnN:    spawnN,
		WorkItem:  j.wi,
		AgentPID:  pid,
		Detail:    cause,
		SessionID: j.sessionID,
	})
}

// capsuleWorkflowStatus answers the capsule's execution.json
// workflow_status ("" when absent or unreadable — a legacy WI-Agent
// capsule or a pre-init scene; the caller treats that as "no terminal
// verdict" and recovers as before).
func capsuleWorkflowStatus(capsuleDir string) string {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.ExecutionJSON))
	if err != nil {
		return ""
	}
	var exec capsule.Execution
	if json.Unmarshal(raw, &exec) != nil {
		return ""
	}
	return exec.WorkflowStatus
}

func (r *Runtime) recoveryStabilityWindow() time.Duration {
	if r.cfg.RecoveryStabilityWindow > 0 {
		return r.cfg.RecoveryStabilityWindow
	}
	return 60 * time.Second
}

// boundedFail terminates the WI without a relaunch: SessionEnd(blocked)
// — the hub clears the session row and marks the WI blocked, which the
// claim gate excludes until a human intervenes (M2). The storm guard's
// "never a third relaunch" contract is preserved: restart sees blocked
// and leaves the record untouched. RTFinished(blocked) follows — on
// sendRelease, so a full observe channel defers rather than drops it
// (NEW-2: every RTFinished is a terminal release). Before the verdict the
// agent tree is killed and the dirty capsule committed+pushed best-effort
// (polyforge#116: the parked verdict must reach the chain; v2-converge §6
// bug 4: every branch, not just the blocked one). A SessionEnd the hub does
// not acknowledge keeps the record end_pending and retries (§6 bug 5,
// endpending.go) — the WI is never orphaned running at aihub.
func (r *Runtime) boundedFail(ctx context.Context, j *job, detail string) {
	// No writer may outlive the verdict, and the last local work must
	// reach aihub before the WI is handed to its owner (v2-converge §6
	// bug 4): every bounded-fail branch — missing capsule, storm guard,
	// budget exhaustion — kills the driver/agent tree
	// and commits+pushes best-effort here, not in the callers. The head
	// the capsule ends at rides the blocked verdict (cross-daemon continue
	// check, §7).
	r.killAgentTree(j)
	head := ""
	if dirExists(j.capsuleDir) {
		head = commitDirty(r, j)
	}
	r.finishBlocked(j, detail, head)
	_ = ctx
}

// commitDirty is the commit-dirty-on-recover step (§六): stage everything,
// commit with the seq trailer, push. A commit that landed locally but
// failed to reach the server is pushed once more directly (the standing
// HEAD push), so the next boundary sees an honest remote state. Errors
// are the caller's to log; the LOCAL commit is the durable half and it
// stands on its own.
func commitDirty(r *Runtime, j *job) string {
	// F3: the dead agent's live transcript rides this dirty commit too.
	r.syncTranscriptBeforeSeal(j)
	sha, _, _, perr := gitops.CommitPush(j.git, "recover: commit dirty capsule before re-derivation")
	if perr != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: commit-dirty-on-recover push failed (local commit %q stands): %v\n", j.wi, sha, perr)
	}
	return sha
}

// killAgentTree tears the current agent's whole tree down through the six
// layers and clears the handle.
func (r *Runtime) killAgentTree(j *job) {
	j.mu.Lock()
	pid := j.agentPID
	start := j.agentStartedAt
	h := j.agent
	j.agent = nil
	j.mu.Unlock()
	// v2 driver shape (the #99 verification lesson): a bridge handle
	// carries no process id (agentPID 0 — the handle's PID is the daemon
	// itself, which must never be signaled), so the pid-keyed cleanup
	// below is a no-op and the old driver goroutine SURVIVED the
	// recovery: it kept collecting envelopes at its own ordinals while
	// the relaunch dispatched new ones — two writers on one
	// execution.json. The handle's own Terminate is the kill switch:
	// canceling the driver context unwinds Run, which terminates its
	// live step process on the way out.
	if h != nil && pid <= 0 {
		if err := h.Terminate(cleanupBound); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: driver handle teardown incomplete: %v\n", j.wi, err)
		}
		r.reapOrphans(j.wi, j.capsuleDir)
		return
	}
	if pid <= 0 {
		// driver job with no live handle (the daemon crashed and the
		// record carries pid 0): the journal + env-marker reap is the
		// only way to find the step processes.
		r.reapOrphans(j.wi, j.capsuleDir)
		return
	}
	rep, err := Cleanup(CleanupTarget{
		PID:        pid,
		Pgid:       pid, // the agent spawns as its own group leader
		StartedAt:  start,
		WIID:       j.wi,
		DaemonID:   r.cfg.MachineID,
		CapsuleDir: j.capsuleDir,
		CgroupPath: r.cgroupPathFor(j.wi),
	}, cleanupBound)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: agent tree cleanup incomplete: %v (warnings: %v)\n", j.wi, err, rep.Warnings)
	}
}
