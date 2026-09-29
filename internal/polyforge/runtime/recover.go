// recover.go: the recovery engine (v1-spec-full §七) and the recovery
// lifecycle entry (§一 Recover). Discriminator = transcript availability
// (NOT same-machine/cross-machine — cross-machine recovery gets the
// transcript for free through capsule git):
//
//	transcript readable → resume (attach the harness to the recorded
//	  session; seconds)
//	resume failing twice consecutively → fresh derivation (a brand-new
//	  agent that may read the capsule's old transcripts for context)
//	fresh failing → bounded fail: the cumulative recovery budget
//	  (default 2) is exhausted and the WI terminates RTFinished(blocked)
//	  — the storm guard: never a third relaunch.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// cleanupBound bounds one teardown's waits inside the six-layer cleanup.
const cleanupBound = 10 * time.Second

// recoverOp runs one recovery event from the message loop. Callers have
// already verified the attempt-id fence and the RUNNING state; opMu
// serializes it against Start/Stop/Restart.
func (r *Runtime) recoverOp(ctx context.Context, j *job, cause string) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.recoverCore(ctx, j, cause)
}

// recoverCore is the recovery sequence; the caller holds opMu.
func (r *Runtime) recoverCore(ctx context.Context, j *job, cause string) {
	if err := j.transition(StateRecovering); err != nil {
		return // lost the race to SEALING/DONE: the fence did its job
	}
	j.mu.Lock()
	j.recoveries++
	rec := j.recoveries
	j.mu.Unlock()
	if perr := r.persist(); perr != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
	}

	// Kill the old tree first (§一 Recover): six layers, no writer may
	// survive next to the re-derivation.
	r.killAgentTree(j)

	// commit-dirty-on-recover (§六): the dead agent's unflushed capsule
	// state is committed and pushed BEFORE the new instance can touch the
	// worktree. Best-effort by design: a failed push surfaces at the next
	// boundary; the local commit already keeps the bytes.
	commitDirty(r, j)

	// Storm guard: the cumulative budget is spent — bounded fail, no
	// relaunch, human attention.
	if rec >= r.cfg.MaxRecoveries {
		r.boundedFail(ctx, j, fmt.Sprintf("recovery budget exhausted (%d recoveries, cause %q)", rec, cause))
		return
	}
	r.cascade(ctx, j, cause)
}

// cascade runs the resume→fresh ladder for one recovery event.
func (r *Runtime) cascade(ctx context.Context, j *job, cause string) {
	j.mu.Lock()
	resumeDead := j.resumeDead
	j.mu.Unlock()
	if !resumeDead && transcriptReadable(j.transcript) {
		for {
			h, err := r.spawnAgent(ctx, j, true)
			if err == nil {
				r.recovered(j, h, cause, true)
				return
			}
			fmt.Fprintf(os.Stderr, "runtime: %s: resume derivation failed: %v\n", j.wi, err)
			j.mu.Lock()
			j.resumeFails++
			if j.resumeFails >= r.cfg.MaxResumeFails {
				j.resumeDead = true
			}
			dead := j.resumeDead
			j.mu.Unlock()
			if dead {
				break // two consecutive resume failures: switch to fresh
			}
		}
	}
	h, err := r.spawnAgent(ctx, j, false)
	if err == nil {
		r.recovered(j, h, cause, false)
		return
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: fresh derivation failed: %v\n", j.wi, err)
	r.boundedFail(ctx, j, "fresh derivation failed: "+err.Error())
}

// recovered finishes a successful re-derivation.
func (r *Runtime) recovered(j *job, h AgentHandle, cause string, resumed bool) {
	j.mu.Lock()
	if resumed {
		j.resumeFails = 0 // a successful resume breaks the failure streak
	}
	inst := j.instance
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
		AttemptID: inst,
		WorkItem:  j.wi,
		AgentPID:  pid,
		Detail:    cause,
	})
}

// boundedFail terminates the WI without a relaunch: end-generation blocked,
// RTFinished(blocked). The aihub side then shows blocked and the restart
// reconciliation leaves the WI untouched (§十一).
func (r *Runtime) boundedFail(ctx context.Context, j *job, detail string) {
	if err := r.hub.EndGeneration(j.wi, endGenerationReq(j, protocol.FinishBlocked)); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: bounded-fail end-generation failed (restart will reconcile): %v\n", j.wi, err)
	}
	if err := j.transition(StateDone); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: bounded fail could not reach DONE: %v\n", j.wi, err)
	}
	r.dropJob(j)
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	r.send(protocol.RTMsg{
		Kind:      protocol.RTFinished,
		AttemptID: j.instance,
		WorkItem:  j.wi,
		Reason:    protocol.FinishBlocked,
		Detail:    detail,
	})
}

func endGenerationReq(j *job, status string) aihubclient.EndGenerationRequest {
	return aihubclient.EndGenerationRequest{
		AttemptID:  j.attempt,
		Generation: j.generation,
		Status:     status,
	}
}

// commitDirty is the commit-dirty-on-recover step (§六): stage everything,
// commit with the seq trailer, push. A commit that landed locally but
// failed to reach the server is pushed once more directly (the standing
// HEAD push), so the next boundary sees an honest remote state. Errors
// are the caller's to log; the LOCAL commit is the durable half and it
// stands on its own.
func commitDirty(r *Runtime, j *job) {
	sha, pushed, perr := gitops.CommitPush(j.git, r.nextSeq(j), "recover: commit dirty capsule before re-derivation")
	if perr == nil && sha != "" && !pushed {
		perr = pushStanding(j.git)
	}
	if perr != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: commit-dirty-on-recover push failed (local commit %q stands): %v\n", j.wi, sha, perr)
	}
}

// transcriptReadable is the recovery engine's ONLY discriminator (§七):
// the recorded harness transcript exists, is non-empty and its first line
// parses as a JSON record. Anything else — missing, empty, truncated to
// zero bytes by the crash — reads as "no transcript" and routes to a
// fresh derivation.
func transcriptReadable(path string) bool {
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return false
	}
	first := raw
	if i := strings.IndexByte(string(raw), '\n'); i >= 0 {
		first = raw[:i]
	}
	first = []byte(strings.TrimSpace(string(first)))
	if len(first) == 0 {
		return false
	}
	var probe map[string]any
	return json.Unmarshal(first, &probe) == nil
}

// spawnAgent derives one agent instance (resume or fresh), journals the
// spawn identity and installs the handle under the job lock, minting the
// next lineage id (the attempt-id fence).
func (r *Runtime) spawnAgent(ctx context.Context, j *job, resume bool) (AgentHandle, error) {
	j.mu.Lock()
	spawnN := j.spawnN
	capsuleDir := j.capsuleDir
	j.mu.Unlock()
	// the lineage id this derivation will carry once installed — the
	// journal must name the process that is live when Spawn returns
	nextInst := j.attempt
	if spawnN >= 1 {
		nextInst = fmt.Sprintf("%s#%d", j.attempt, spawnN)
	}
	p := SpawnParams{
		WorkItemID:     j.wi,
		CapsuleDir:     capsuleDir,
		SessionID:      j.sessionID,
		TranscriptPath: j.transcript,
		Resume:         resume,
		Generation:     j.generation,
		InitialPrompt:  r.buildPrompt(j, resume),
		CgroupPath:     r.cgroupPathFor(j.wi),
	}
	p.Journal = func(pid int, startedAt string) error {
		return writeSpawnJournal(p, pid, startedAt, nextInst)
	}
	h, err := r.spawner.Spawn(ctx, p)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	j.spawnN++
	j.agent = h
	j.agentPID = h.PID()
	j.agentStartedAt = h.StartedAt()
	j.instance = j.newInstanceLocked()
	j.mu.Unlock()
	return h, nil
}

// buildPrompt assembles the WI agent's initial instruction: the executor
// preamble (from the daemon's configured prompt file when set) plus the
// WI/capsule identity and the resume-or-fresh framing.
func (r *Runtime) buildPrompt(j *job, resume bool) string {
	var b strings.Builder
	if r.cfg.ExecutorPromptFile != "" {
		if raw, err := os.ReadFile(r.cfg.ExecutorPromptFile); err == nil {
			b.Write(raw)
			b.WriteString("\n\n")
		}
	}
	fmt.Fprintf(&b, "Work item %s (project %s, generation %d). Capsule root: %s. ",
		j.wi, j.project, j.generation, j.capsuleDir)
	if j.bundleName != "" {
		fmt.Fprintf(&b, "Workflow bundle %s (digest %s). ", j.bundleName, j.bundleDigest)
	}
	if resume {
		fmt.Fprintf(&b, "This is a crash continuation: resume your session from the transcript at %s and continue the work item.", j.transcript)
	} else {
		fmt.Fprintf(&b, "This is a fresh derivation. Earlier attempt transcripts under the capsule session directory describe the previous agent's reasoning; read them for context before starting.")
	}
	return b.String()
}

// killAgentTree tears the current agent's whole tree down through the six
// layers and clears the handle.
func (r *Runtime) killAgentTree(j *job) {
	j.mu.Lock()
	pid := j.agentPID
	start := j.agentStartedAt
	j.mu.Unlock()
	if pid <= 0 {
		return
	}
	rep, err := Cleanup(CleanupTarget{
		PID:        pid,
		Pgid:       pid, // the agent spawns as its own group leader
		StartedAt:  start,
		WIID:       j.wi,
		CapsuleDir: j.capsuleDir,
		CgroupPath: r.cgroupPathFor(j.wi),
	}, cleanupBound)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: agent tree cleanup incomplete: %v (warnings: %v)\n", j.wi, err, rep.Warnings)
	}
	j.mu.Lock()
	j.agent = nil
	j.mu.Unlock()
}
