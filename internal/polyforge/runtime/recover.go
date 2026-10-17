// recover.go: the recovery engine (reference.md §6.2) and the recovery
// lifecycle entry (Recover). Every relaunch kills the agent tree, commits
// the dirty capsule and starts a fresh in-process driver derivation that
// continues from execution.json (no transcript resume→fresh cascade,
// polyforge#106; a step's own retry shape is the driver's business):
//
//	relaunch failing → one FAILED recovery: the budget counts failures and
//	  a successful recovery RESETS it. After MaxRecoveries (default 2)
//	  consecutive failed recoveries the WI terminates
//	  RTFinished(blocked) — the consecutive-failures ledger: never a
//	  third relaunch without an accepted step result
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
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// cleanupBound bounds one teardown's waits inside the six-layer cleanup.
const cleanupBound = 10 * time.Second

// recoverOp runs one recovery of j's current derivation (see recoverAt).
func (r *Runtime) recoverOp(ctx context.Context, j *job, cause, detail string) {
	_, spawn := j.snapshot()
	r.recoverAt(ctx, j, spawn, cause, detail)
}

// recoverAt runs one recovery event for derivation spawn of j, off the
// message loop: it waits for the work item's lock (another operation of
// it — a close, the claim's settle, a restart step — decides first) and
// re-checks the fence under it. A job that settled meanwhile, a later
// derivation (a duplicate death signal of one already recovered), a state
// other than RUNNING (a close or a pause took it) or a record the restart
// reconciliation has not reached yet drops the event.
func (r *Runtime) recoverAt(ctx context.Context, j *job, spawn int, cause, detail string) {
	s := r.lockWI(j.wi)
	defer r.unlockWI(j.wi, s)
	if r.jobOf(j.wi) != j || j.pendingRestart() {
		return
	}
	if st, n := j.snapshot(); st != StateRunning || n != spawn {
		return
	}
	r.recoverCore(ctx, j, cause, detail)
}

// recoverCore is the recovery sequence; the caller holds the work item's
// lock, across its pushes and the relaunch. cause
// names the recovery trigger (the interrupted-dispatch archive's class
// input, and the consecutive-failures ledger's exempt-cause key);
// detail is the observe
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
	// this the terminal exit bounced through death-recovery and closed
	// the WI as blocked instead of wrapped.
	switch capsuleWorkflowStatus(j.capsuleDir) {
	case capsule.WorkflowStatusCompleted:
		fmt.Fprintf(os.Stderr, "runtime: %s: driver workflow completed — wrapping (recovery cause %q is the completion signal, not a death)\n", j.wi, cause)
		// The ordinary seal push runs: the driver does no git, and
		// the observe flush that would carry the terminal execution.json
		// may have failed or timed out. On a clean tree CommitPush only
		// re-pushes HEAD; a failed push degrades the close to blocked like
		// every other terminal close.
		if err := r.stopOp(ctx, j, protocol.FinishWrapped, false, ""); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: terminal wrap failed (record kept for the next restart): %v\n", j.wi, err)
		}
		return
	case capsule.WorkflowStatusFailedGate, capsule.WorkflowStatusFailedRewindExhausted:
		fmt.Fprintf(os.Stderr, "runtime: %s: driver workflow failed (%s) — closing failed (recovery cause %q)\n", j.wi, capsuleWorkflowStatus(j.capsuleDir), cause)
		if err := r.stopOp(ctx, j, protocol.FinishFailed, false, ""); err != nil {
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
		// verdict together. The §6.4 owner moves (resume / cancel) and
		// every re-materialization read exactly this state.
		r.boundedFail(ctx, j, "workflow blocked: the owner holds the next move (resume / cancel)")
		return
	}
	// The single recovery ledger (C7): ONE counter of consecutive recovery
	// failures. A recovery whose relaunch made no real progress — no step
	// result accepted since — is a failure, counted at the NEXT re-entry
	// (that re-entry is its proof); a re-entry that follows an accepted
	// result resets the counter (real progress is the only reset). The
	// progress signal is the capsule execution state's accepted-result
	// attempts (the driver's R14 commit points — the same surface the
	// observe lane polls); a dead agent writes nothing, so a dying relaunch
	// can never fake its way out of the ledger. A daemon restart / operator
	// attach is NOT a failure (v2-converge §2.3): those causes are exempt
	// from the ledger entirely — neither counted nor resetting — so a deploy
	// retry or a systemd restart loop cannot bounded-fail every running WI.
	if !restartExemptCause(cause) {
		accepted := acceptedResultCount(j.capsuleDir) // disk read: outside the job's mutex
		j.mu.Lock()
		switch {
		case accepted > j.acceptedAtRelaunch:
			// real progress since the last relaunch (a step's result was
			// accepted): the consecutive ledger resets (C7 — real
			// progress is the only reset).
			j.recoverFails = 0
		case j.lastRelaunchRecovery:
			// no accepted result since the last RECOVERY relaunch: that
			// relaunch failed to stabilize the WI — one consecutive
			// unit. (The original claim's death is a trigger, not a
			// recovery failure; only a recovery's own relaunch can fail.)
			j.recoverFails++
		}
		j.acceptedAtRelaunch = accepted
		j.lastRelaunchRecovery = false // the current relaunch re-arms it on success
		fails := j.recoverFails
		j.mu.Unlock()
		if fails >= r.cfg.MaxRecoveries {
			r.boundedFail(ctx, j, fmt.Sprintf(
				"recovery budget exhausted (%d consecutive recovery failures without an accepted step result, latest cause %q)",
				fails, cause))
			return
		}
	}
	// Kill the old tree first (§一 Recover): six layers, no writer may
	// survive next to the re-derivation.
	r.killAgentTree(j)

	// commit-dirty-on-recover (§六): the dead agent's unflushed capsule
	// state is committed and pushed BEFORE the new instance can touch the
	// worktree. Best-effort by design: a failed push surfaces at the next
	// boundary; the local commit already keeps the bytes.
	//
	// F1 repair: a capsule without runtime-state/work-item.json (a pre-F1
	// scene) gets it re-materialized before the next derivation wakes up
	// blind to its own goal (an aihub read when the claim snapshot is
	// gone). Best-effort, idempotent.
	_, _ = commitDirty(r, j)
	r.ensureWorkItemFile(ctx, j)

	// The bundle is pinned at claim (v2-converge §2.4): recovery relaunches
	// from the capsule's own materialized snapshot and never re-pins or
	// re-materializes from the current checkout. A missing or untrusted
	// snapshot is a loud terminal condition, not a silent rebuild.
	if r.cfg.WorkflowsDir != "" && j.bundleName != "" && !r.materializationComplete(j) {
		r.boundedFail(ctx, j, fmt.Sprintf(
			"the capsule's materialized snapshot of bundle %s is missing or incomplete; recovery never re-pins to the current checkout (the bundle is pinned at claim)",
			j.bundleName))
		return
	}

	// The engine binding is the driver or NOTHING (polyforge#106): there
	// is no other engine, so a nil starter is a misassembled runtime, not
	// an engine choice — refused loudly, never fallen back from.
	starter := r.cfg.AgentStarter
	if starter == nil {
		r.boundedFail(ctx, j, "runtime misassembly: no execution engine bound (Config.AgentStarter is nil) — the legacy in-package WI-Agent engine was deleted (polyforge#106); the daemon binds the driver bridge starter unconditionally at assembly")
		return
	}

	// The ladder's own spend (§三): each FAILED relaunch attempt is one
	// consecutive recovery failure on the SAME single counter (C7); only
	// an accepted step result resets it (the entry block above). Only
	// when the counter reaches MaxRecoveries does the WI bounded-fail —
	// never merely because it has recovered before.
	//
	// polyforge#79: the effective lists this re-derivation dispatches
	// under (rehydrated or re-derived — see
	// effectiveCandidatesForRecovery) ride every relaunch attempt, and
	// the interrupted dispatch (the death that forced this recovery)
	// is archived by the driver before the re-dispatch: the
	// recomputation then advances on model-class causes alone
	// (driver.IsModelClass). The archive signal is the raw cause — the
	// bridge classifies it and filters operator-shaped stops (a
	// deliberate stop is not a model verdict).
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
		if !r.admit() {
			// the shutdown latched: no agent starts behind it. The job
			// stays RECOVERING with no agent; the next daemon start
			// recovers the persisted record.
			if perr := r.persist(); perr != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
			}
			fmt.Fprintf(os.Stderr, "runtime: %s: recovery stopped by the shutdown before its relaunch (the next start recovers it)\n", j.wi)
			return
		}
		m := spawnJournalMaxN(j.capsuleDir) // disk read: outside the job's mutex
		j.mu.Lock()
		if m > j.spawnN {
			j.spawnN = m
		}
		j.spawnN++
		ordinal := int64(j.spawnN)
		j.mu.Unlock()
		h, err := starter(ctx, AgentStartInput{
			WI:         j.wi,
			Project:    j.project,
			CapsuleDir: j.capsuleDir,
			BundleName: j.bundleName,
			SpawnN:     ordinal,
			Spawner:    r.spawner,
			StepEnded:  r.stepEnded(j.wi),
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
			if r.release() {
				// bound after the shutdown stopped waiting for it: not in
				// its live set — torn down here; the record stays for the
				// next start
				fmt.Fprintf(os.Stderr, "runtime: %s: relaunched agent bound after the shutdown stopped waiting for it — torn down\n", j.wi)
				r.killAgentTree(j)
				if perr := r.persist(); perr != nil {
					fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
				}
				return
			}
			// persisted with the advanced counter (crash-safe)
			if perr := r.persist(); perr != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
			}
			r.recovered(j, h, cause)
			return
		}
		r.release()
		fmt.Fprintf(os.Stderr, "runtime: %s: engine relaunch failed: %v\n", j.wi, err)
		j.mu.Lock()
		j.recoverFails++
		fails := j.recoverFails
		j.mu.Unlock()
		if perr := r.persist(); perr != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
		}
		if fails >= r.cfg.MaxRecoveries {
			r.boundedFail(ctx, j, fmt.Sprintf("recovery budget exhausted (%d consecutive recovery failures, cause %q)", fails, cause))
			return
		}
		fmt.Fprintf(os.Stderr, "runtime: %s: recovery failed (%d/%d), retrying the ladder\n", j.wi, fails, r.cfg.MaxRecoveries)
	}
}

// effectiveCandidatesForRecovery answers the per-step effective
// candidate lists a recovery re-derivation dispatches under
// (polyforge#79): the job's claim-time snapshot — live, or rehydrated
// from the daemon-private state.json record (never from capsule.json,
// which the agent can write). nil means bundle-only behavior. The yaml
// is never re-read here: an edit governs the NEXT claim, never an
// in-flight job.
func (r *Runtime) effectiveCandidatesForRecovery(j *job) map[string][]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stepCandidates
}

// recovered finishes a successful re-derivation. A successful recovery
// RESETS the failure budget — the counter is spent by failures only.
// v1.5: RTRecovered carries SpawnN (the B2 fence key) and the in-process
// session secret (the observe lane's M4 revalidation reads it); identity
// otherwise changes NOT AT ALL — no new attempt, no generation bump. (There
// is no resume-failure bookkeeping — polyforge#106: every relaunch is a
// fresh driver derivation.)
func (r *Runtime) recovered(j *job, h AgentHandle, cause string) {
	accepted := acceptedResultCount(j.capsuleDir) // disk read: outside the job's mutex
	j.mu.Lock()
	// C7: the ledger's reset is the accepted-result check at the next
	// entry, never the relaunch itself (a relaunch that dies before its
	// step's result is accepted must keep counting). The relaunch marks
	// the job recovery-stabilized-so-far and notes the accepted-result
	// count it starts from.
	j.lastRelaunchRecovery = true
	j.acceptedAtRelaunch = accepted
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

// boundedFail terminates the WI without a relaunch: SessionEnd(blocked)
// — the hub clears the session row and marks the WI blocked, which the
// claim gate excludes until a human intervenes (M2). The ledger's
// "never a third relaunch without progress" contract is preserved:
// restart sees blocked and leaves the record untouched. RTFinished(blocked) follows. Before the verdict the
// agent tree is killed and the dirty capsule committed+pushed best-effort
// (polyforge#116: the parked verdict must reach the chain; v2-converge §6
// bug 4: every branch, not just the blocked one). A SessionEnd the hub does
// not acknowledge keeps the record end_pending and retries (§6 bug 5,
// endpending.go) — the WI is never orphaned running at aihub.
func (r *Runtime) boundedFail(ctx context.Context, j *job, detail string) {
	// No writer may outlive the verdict, and the last local work must
	// reach aihub before the WI is handed to its owner (v2-converge §6
	// bug 4): every bounded-fail branch — missing capsule, budget
	// exhaustion — kills the driver/agent tree
	// and commits+pushes best-effort here, not in the callers. The head
	// the capsule ends at rides the blocked verdict (cross-daemon continue
	// check, §7).
	r.killAgentTree(j)
	// The pushes and the blocked close run under the work item's lock
	// like the rest of the verdict.
	head := ""
	chainPushed := true // nothing local to push without a capsule
	if dirExists(j.capsuleDir) {
		var perr error
		head, perr = commitDirty(r, j)
		chainPushed = perr == nil
		// The re-claim the owner's resume runs restores the task repos
		// from the carrier: their uncommitted work rides refs/pf-wip
		// (polyforge#167). Before the verdict — the session credential
		// dies with it.
		r.saveTaskWIP(j, true, chainPushed)
	}
	j.mu.Lock()
	j.endNote, j.endHead = detail, head
	j.mu.Unlock()
	// When boundedFail's own push did not land, the close goes through
	// closeBlocked's push-then-close, so a hub blip never lets
	// SessionEnd(blocked) land with unpushed commits.
	settled := r.closeBlocked(ctx, j, !chainPushed)
	r.finishBlocked(j, detail, settled)
}

// commitDirty is the commit-dirty-on-recover step (§六): stage everything,
// commit with the seq trailer, push. A commit that landed locally but
// failed to reach the server is pushed once more directly (the standing
// HEAD push), so the next boundary sees an honest remote state. Errors
// are the caller's to log; the LOCAL commit is the durable half and it
// stands on its own.
func commitDirty(r *Runtime, j *job) (string, error) {
	r.syncLiveTranscript(j.capsuleDir)
	sha, _, _, perr := gitops.CommitPush(j.git, "recover: commit dirty capsule before re-derivation")
	if perr != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: commit-dirty-on-recover push failed (local commit %q stands): %v\n", j.wi, sha, perr)
	}
	return sha, perr
}

// saveTaskWIP runs the task-repo WIP lane (polyforge#167) at a close
// point where the step process is already gone: keep snapshots each
// dirty task repo to refs/pf-wip/<name> (a clean one drops its stale
// WIP ref); keep=false — the wrapped close — only drops stale WIP refs.
// chainOK reports whether the close's chain push landed (the carrier
// alert gate). Best-effort: never fails the close.
func (r *Runtime) saveTaskWIP(j *job, keep, chainOK bool) {
	if !dirExists(j.capsuleDir) {
		return
	}
	pushTaskWIPFn(j.git, keep, chainOK)
}

// pushTaskWIPFn is the close's task-repo WIP push (a seam for the
// hung-call tests).
var pushTaskWIPFn = gitops.PushTaskWIP

// SaveTaskWIP snapshots wi's task-repo uncommitted work to refs/pf-wip
// once its step process is gone (the shutdown, after terminating the
// agent tree; the driver, at each step end), so a re-claim restores it.
// ctx bounds the lane (gitops.Opts.Ctx): when it ends — its deadline or
// a cancellation — the running git call's process group is killed and
// the rest skipped, so the per-WI git lock is released promptly. The
// caller is released at ctx's end at the latest. No runtime lock is held across
// the push; a failure is logged and never fails the caller.
func (r *Runtime) SaveTaskWIP(ctx context.Context, wi string) {
	j := r.jobOf(wi)
	if j == nil || !dirExists(j.capsuleDir) {
		return
	}
	o := j.git
	o.Ctx = ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		pushTaskWIPFn(o, true, true)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// stepWIPBound bounds the step-end WIP push: it holds the step's result
// write and the next dispatch.
const stepWIPBound = 30 * time.Second

// stepEnded is wi's driver step-end hook: the WIP push of the task-repo
// work the step left uncommitted, so a kill -9 after a step end loses
// none of it on a re-claim.
func (r *Runtime) stepEnded(wi string) func(context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, stepWIPBound)
		defer cancel()
		r.SaveTaskWIP(ctx, wi)
	}
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
	}, cleanupBound)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: agent tree cleanup incomplete: %v (warnings: %v)\n", j.wi, err, rep.Warnings)
	}
}

// restartExemptCause reports whether a recovery cause is a daemon
// restart, an operator attach or an operator resume.
//
// Destination: the consecutive-recovery-failures budget is spent only by
// failures — a process-lifecycle event neither counts nor resets it, so a
// deploy or a restart loop cannot bounded-fail a healthy work item.
//
// The bridge's interruptedFromCause is a different list with a different
// destination (every derivation that ended without a result is recorded
// in attempts[] as audit); a daemon restart is exempt here and recorded
// there, so the two do not merge.
func restartExemptCause(cause string) bool {
	return cause == causeDaemonRestart || cause == causeOperatorAttach || cause == causeOperatorResume
}

// acceptedResultCount reads the capsule's execution state and counts the
// accepted-result attempts across steps (the driver's R14 commit
// points): the runtime's real-progress signal for the consecutive-
// failures ledger (C7 — the counter resets when a step's result is
// accepted). 0 when the file is absent or unparsable: nothing proven,
// never a reset. The file is agent-writable, but the ledger's adversary
// is death — a dead relaunch writes nothing, so the signal cannot be
// gamed by the failure mode the ledger bounds.
func acceptedResultCount(capsuleDir string) int64 {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.ExecutionJSON))
	if err != nil {
		return 0
	}
	var exec capsule.Execution
	if json.Unmarshal(raw, &exec) != nil {
		return 0
	}
	var n int64
	for _, st := range exec.Steps {
		if st == nil {
			continue
		}
		for _, at := range st.Attempts {
			if at.Cause == "accepted" {
				n++
			}
		}
	}
	return n
}
