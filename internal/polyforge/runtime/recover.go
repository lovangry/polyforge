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
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
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
	// Candidate-chain advancement (I2/INV-1g): ONLY a provider-class
	// recovery moves the ordinal — business FAIL, rewind, hang, plain
	// death and daemon restart never do (the mirror rule of
	// v0/internal/workflow/drive/drive.go:70 CandidateOrdinal: the count
	// advances on infra failures alone, gate-fail rewind resets it).
	// Bounded and crash-safe: the advance is persisted in the job record
	// BEFORE the re-derivation, and exhausting the list is NOT a terminal
	// condition — the ordinal wraps to 0 (the storm guard owns
	// terminality, not the candidate list).
	r.advanceCandidate(j, cause)

	// Kill the old tree first (§一 Recover): six layers, no writer may
	// survive next to the re-derivation.
	r.killAgentTree(j)

	// commit-dirty-on-recover (§六): the dead agent's unflushed capsule
	// state is committed and pushed BEFORE the new instance can touch the
	// worktree. Best-effort by design: a failed push surfaces at the next
	// boundary; the local commit already keeps the bytes.
	commitDirty(r, j)

	// F1 repair: a capsule without runtime-state/work-item.json (a legacy
	// pre-F1 scene) gets it re-materialized before the next derivation
	// wakes up blind to its own goal. Best-effort, idempotent.
	r.ensureWorkItemFile(j)

	// F7 repair: a capsule whose materialized bundle set is missing its
	// completion marker (a crash-torn snapshot) — or predates the product
	// fix entirely — gets the set idempotently rebuilt, discovery copies
	// and marker included, before the next derivation wakes up. The
	// rebuild never breaks a scene the dead agents already read (staged
	// renames, identical content). Best-effort, never blocks recovery.
	r.ensureMaterialized(j)

	// The storm guard (§三): each FAILED recovery ladder spends one unit
	// of the budget; a successful recovery RESETS it (a healthy relaunch
	// must never inherit earlier failures). Only when MaxRecoveries
	// recoveries have FAILED outright does the WI bounded-fail — never
	// merely because it has recovered before.
	for {
		if r.cascade(ctx, j, cause) {
			return // recovered (or otherwise settled) — budget already reset
		}
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

// cascade runs the resume→fresh ladder for one recovery attempt. It
// answers true when a re-derivation went live (the caller's budget is
// reset inside); false when the ladder failed outright and the caller
// must spend budget / retry.
//
// The resume discriminator (§七) is transcript readability of the DEAD
// instance's session record — with F3 there are two physical sources:
// the capsule's declared copy (session/session-<spawn-token>.jsonl, synced
// by the daemon at every commit boundary) and, when that is missing, the
// harness-store live file (the recorded live transcript, or a fresh
// scan keyed by the dead instance's spawn moment). The resume derivation
// hands the harness --session pointing at the capsule copy when it is
// readable — the resumed agent then appends straight into the capsule —
// and at the live file otherwise (v0's measured finding: the harness
// resumes from any .jsonl path).
func (r *Runtime) cascade(ctx context.Context, j *job, cause string) bool {
	j.mu.Lock()
	resumeDead := j.resumeDead
	j.mu.Unlock()
	if src := r.resumeSource(j); !resumeDead && src != "" {
		for {
			h, err := r.spawnAgent(ctx, j, true, src)
			if err == nil {
				r.recovered(j, h, cause, true)
				return true
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
	h, err := r.spawnAgent(ctx, j, false, "")
	if err == nil {
		r.recovered(j, h, cause, false)
		return true
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: fresh derivation failed: %v\n", j.wi, err)
	return false
}

// resumeSource answers the session file a resume derivation should
// continue from ("" = nothing resumable, go fresh): the capsule's
// declared copy first, the harness-store live file as the fallback.
func (r *Runtime) resumeSource(j *job) string {
	j.mu.Lock()
	transcript := j.transcript
	live := j.liveTranscript
	startedAt := j.agentStartedAt
	capsuleDir := j.capsuleDir
	j.mu.Unlock()
	if transcriptReadable(transcript) {
		return transcript
	}
	if live != "" && transcriptReadable(live) {
		return live
	}
	// Not recorded (the spawn-moment scan raced the harness's file
	// creation) — scan now: at recovery time the dead instance's live
	// file has long existed, so a hit here is its transcript.
	if spawnAt, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
		if scanned := r.locateLive(capsuleDir, spawnAt); scanned != "" && transcriptReadable(scanned) {
			return scanned
		}
	}
	return ""
}

// recovered finishes a successful re-derivation. A successful recovery
// RESETS the failure budget — the counter is spent by failures only.
// v1.5: RTRecovered carries SpawnN (the B2 fence key) and the in-process
// session secret (the observe lane's M4 revalidation reads it); identity
// otherwise changes NOT AT ALL — no new attempt, no generation bump.
func (r *Runtime) recovered(j *job, h AgentHandle, cause string, resumed bool) {
	j.mu.Lock()
	j.recoverFails = 0
	if resumed {
		j.resumeFails = 0 // a successful resume breaks the failure streak
	}
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

// advanceCandidate moves the effective model-candidate ordinal one
// step when — and only when — the recovery cause is provider-class.
// See recoverCore for the full rule statement.
func (r *Runtime) advanceCandidate(j *job, cause string) {
	j.mu.Lock()
	n := len(j.candidates)
	if n == 0 || cause != protocol.RecoverProviderError {
		j.mu.Unlock()
		return
	}
	j.candidateOrdinal = (j.candidateOrdinal + 1) % n
	ord := j.candidateOrdinal
	j.mu.Unlock()
	// crash-safe (I2): the advanced ordinal is durable before any
	// re-derivation is attempted, so a crash mid-recovery resumes the
	// SAME effective candidate.
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: provider failure advances the candidate chain to ordinal %d\n", j.wi, ord)
}

// effectiveModel answers the harness model id the next derivation of j
// must be pinned to ("" = no chain: the harness adapter's default).
func (j *job) effectiveModel() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := len(j.candidates)
	if n == 0 {
		return ""
	}
	return j.candidates[j.candidateOrdinal%n]
}

// boundedFail terminates the WI without a relaunch: SessionEnd(blocked)
// — the hub clears the session row and marks the WI blocked, which the
// claim gate excludes until a human intervenes (M2). The storm guard's
// "never a third relaunch" contract is preserved: restart sees blocked
// and leaves the record untouched. RTFinished(blocked) follows — on
// sendRelease, so a full observe channel defers rather than drops it
// (NEW-2: every RTFinished is a terminal release).
func (r *Runtime) boundedFail(ctx context.Context, j *job, detail string) {
	if err := r.hub.SessionEnd(j.wi, api.SessionEndRequest{
		SessionID: j.sessionID,
		Status:    api.StatusBlocked,
		Note:      detail,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: bounded-fail session-end failed (restart will reconcile): %v\n", j.wi, err)
	}
	if err := j.transition(StateDone); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: bounded fail could not reach DONE: %v\n", j.wi, err)
	}
	r.dropJob(j)
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	r.sendRelease(protocol.RTMsg{
		Kind:     protocol.RTFinished,
		WorkItem: j.wi,
		Reason:   protocol.FinishBlocked,
		Detail:   detail,
	})
	_ = ctx
}

// commitDirty is the commit-dirty-on-recover step (§六): stage everything,
// commit with the seq trailer, push. A commit that landed locally but
// failed to reach the server is pushed once more directly (the standing
// HEAD push), so the next boundary sees an honest remote state. Errors
// are the caller's to log; the LOCAL commit is the durable half and it
// stands on its own.
func commitDirty(r *Runtime, j *job) {
	// F3: the dead agent's live transcript rides this dirty commit too.
	r.syncTranscriptBeforeSeal(j)
	sha, _, _, perr := gitops.CommitPush(j.git, "recover: commit dirty capsule before re-derivation")
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
// spawn identity and installs the handle under the job lock.
//
// v1.5 identity rule (B2): the ONLY identity change across a recovery is
// spawn_n++ — no new attempt, no generation. The derivation ordinal is
// state.json's spawn_n + 1 (the fresh-clone seeding happened at
// startClaimed); the fence token for the transition protocol is the
// session fingerprint ("#N" appended for re-derivations, mirroring the
// v1 attempt#N shape), and the journal line records the ordinal so a
// later fresh clone continues the counter.
//
// transcript is the resume source (the file the resumed derivation
// continues); "" for a fresh derivation, which declares its own
// token-named capsule copy path instead (sessionPathFor). The live
// transcript — the file this derivation actually appends to — is
// recorded on the job: the resume source verbatim, or the located
// harness-store file for a fresh derivation ("" while the harness has
// not created it yet; observe re-locates at its sync points).
func (r *Runtime) spawnAgent(ctx context.Context, j *job, resume bool, transcript string) (AgentHandle, error) {
	j.mu.Lock()
	spawnN := j.spawnN
	capsuleDir := j.capsuleDir
	sessionFp := j.sessionFingerprint
	j.mu.Unlock()
	// the derivation ordinal this derivation will carry once installed
	// — the journal must name the process that is live when Spawn
	// returns. token is the LOCAL display name (the public fingerprint,
	// "#N"-suffixed after re-derivations) used only for the capsule-side
	// transcript path and the spawn journal; the protocol fence is the
	// counter itself.
	ordinal := spawnN + 1
	token := sessionFp
	if ordinal > 1 {
		token = fmt.Sprintf("%s#%d", sessionFp, ordinal-1)
	}
	var live string
	if resume {
		live = transcript
		// A resumed derivation CONTINUES the resume source: the declared
		// transcript stays that file (for a capsule-copy resume the agent
		// appends straight into the capsule — the C1 win — and a live-file
		// resume keeps the harness store as its session file).
		j.mu.Lock()
		j.transcript = transcript
		j.mu.Unlock()
	} else {
		j.mu.Lock()
		j.transcript = sessionPathFor(capsuleDir, token)
		j.mu.Unlock()
	}
	j.mu.Lock()
	declared := j.transcript
	j.mu.Unlock()
	p := SpawnParams{
		WorkItemID:     j.wi,
		CapsuleDir:     capsuleDir,
		SessionHandle:  sessionFp,
		SpawnN:         ordinal,
		TranscriptPath: declared,
		Resume:         resume,
		InitialPrompt:  r.buildPrompt(j, resume),
		CgroupPath:     r.cgroupPathFor(j.wi),
		// F4: the spawn is pinned to the effective candidate of the
		// persisted chain (spawn.go documents why this rides argv).
		Model: j.effectiveModel(),
	}
	p.Journal = func(pid int, startedAt string) error {
		return writeSpawnJournal(p, pid, startedAt, token)
	}
	h, err := r.spawner.Spawn(ctx, p)
	if err != nil {
		return nil, err
	}
	// F3: record the live transcript after the derivation is live. For a
	// fresh derivation this is a best-effort one-shot scan — the harness
	// may not have created the file yet (the first turn creates it), in
	// which case the record stays empty and every consumer re-locates on
	// demand. Never fatal, never blocking.
	if !resume {
		if spawnAt, perr := time.Parse(time.RFC3339Nano, h.StartedAt()); perr == nil {
			live = r.locateLive(capsuleDir, spawnAt)
		}
	}
	j.mu.Lock()
	j.spawnN = ordinal
	j.agent = h
	j.agentPID = h.PID()
	j.agentStartedAt = h.StartedAt()
	j.liveTranscript = live
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
	fmt.Fprintf(&b, "Work item %s (project %s, session %s). Capsule root: %s. ",
		j.wi, j.project, j.sessionFingerprint, j.capsuleDir)
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
