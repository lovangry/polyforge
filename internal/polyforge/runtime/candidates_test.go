package runtime

// candidates_test.go: the F4/I2 candidate chain — spawn-time pinning of the
// bundle's defaults model_candidates, provider-error-only advancement
// (INV-1g), bounded wrap-around, crash-safe ordinal persistence across a
// restart, and the D5 claiming journal (durable claim idempotency +
// post-claim failure compensation). Candidate ids in fixtures are
// provider-qualified placeholders; no real model or vendor names appear
// anywhere here.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

var testCandidates = []string{"prov-a/model-a1", "prov-a/model-a2", "prov-b/model-b1"}

func seedCandidatedJob(r *Runtime, wi string, cands []string) *job {
	j := seedJob(r, wi)
	j.mu.Lock()
	j.candidates = cands
	j.mu.Unlock()
	return j
}

// A provider-class recovery advances the effective candidate; the second
// provider failure advances again; a hang/death recovery in between does
// NOT; exhausting the list wraps to 0 (bounded: exhaustion is never a
// terminal condition — the storm guard owns terminality).
func TestProviderErrorAdvancesCandidateOrdinal(t *testing.T) {
	sp := &countingSpawner{}
	r, _, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_cand"
	j := seedCandidatedJob(r, wi, testCandidates)
	installOldAgent(t, sp, j)

	// 1st provider error: derivation pinned to candidates[1]
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverProviderError))
	if m := sp.lastModel(); m != testCandidates[1] {
		t.Fatalf("after 1st provider error the pin must be %q, got %q", testCandidates[1], m)
	}

	// a hang death on the new instance: the ordinal must NOT move
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverHang))
	if m := sp.lastModel(); m != testCandidates[1] {
		t.Fatalf("a hang recovery must not advance the pin, got %q", m)
	}

	// 2nd provider error: candidates[2]
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverProviderError))
	if m := sp.lastModel(); m != testCandidates[2] {
		t.Fatalf("after 2nd provider error the pin must be %q, got %q", testCandidates[2], m)
	}

	// 3rd provider error: the list is exhausted — wrap to candidates[0],
	// never a terminal state
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverProviderError))
	if m := sp.lastModel(); m != testCandidates[0] {
		t.Fatalf("an exhausted chain must wrap to %q, got %q", testCandidates[0], m)
	}

	// a plain death never advances either
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if m := sp.lastModel(); m != testCandidates[0] {
		t.Fatalf("a death recovery must not advance the pin, got %q", m)
	}

	// the advanced ordinal is crash-safe bookkeeping: state.json carries it
	j.mu.Lock()
	ord := j.candidateOrdinal
	j.mu.Unlock()
	if ord != 0 {
		t.Fatalf("three advances over a 3-list wrap to 0, got ordinal %d", ord)
	}
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].CandidateOrdinal != 0 || len(recs[0].Candidates) != 3 {
		t.Fatalf("state.json must persist the chain and ordinal, got %+v", recs)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// A job without a candidate chain stays unpinned on every recovery cause:
// the harness adapter's default model remains in effect (no invented
// candidate, no argv pin).
func TestNoCandidateChainLeavesSpawnUnpinned(t *testing.T) {
	sp := &countingSpawner{}
	r, _, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_nocand"
	j := seedJob(r, wi) // no candidates
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverProviderError))
	if m := sp.lastModel(); m != "" {
		t.Fatalf("no chain must leave the spawn unpinned, got %q", m)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// The ordinal survives a daemon restart: a fresh Runtime over the same
// DataDir re-reads state.json and re-derives the SAME effective candidate
// (a daemon-restart recovery is not provider-class and never advances).
func TestRestartRestoresCandidateOrdinal(t *testing.T) {
	hub := newFakeHub()
	dataDir := filepath.Join(t.TempDir(), "data")
	sp := &countingSpawner{}
	r, _, _ := newTestRuntimeAt(t, hub, sp, dataDir)
	wi := "wi_ord"
	j := seedCandidatedJob(r, wi, testCandidates)
	installOldAgent(t, sp, j)

	// advance once: the next derivation is pinned to candidates[1]
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverProviderError))
	j.mu.Lock()
	oldAgent := j.agent
	oldPID := j.agentPID
	j.mu.Unlock()
	if m := sp.lastModel(); m != testCandidates[1] {
		t.Fatalf("pin after provider error = %q, want %q", m, testCandidates[1])
	}
	if oldAgent != nil {
		_ = oldAgent.Terminate(2e9)
		waitDead(t, oldPID)
	}

	// the fresh runtime re-reads the persisted ordinal and re-derives
	// under the same effective candidate (the record's session must be
	// the holder, or the restart releases instead of recovering)
	hub2 := newFakeHub()
	hub2.hold(wi, fakeSession(wi), api.StatusRunning)
	sp2 := &countingSpawner{}
	r2, _, _ := newTestRuntimeAt(t, hub2, sp2, dataDir)
	if err := r2.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if m := sp2.lastModel(); m != testCandidates[1] {
		t.Fatalf("restart must resume the persisted candidate %q, got %q", testCandidates[1], m)
	}
	if j2 := r2.jobOf(wi); j2 == nil {
		t.Fatal("the restarted job must be live")
	} else if h := j2.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// countingSpawner records the Model of every spawn (a thin wrapper over
// the scripted spawner's recorded params, plus a lastModel accessor).
type countingSpawner struct {
	scriptedSpawner
	models []string
}

func (s *countingSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	h, err := s.scriptedSpawner.Spawn(ctx, p)
	if err == nil {
		s.models = append(s.models, p.Model)
	}
	return h, err
}

func (s *countingSpawner) lastModel() string {
	if len(s.models) == 0 {
		return ""
	}
	return s.models[len(s.models)-1]
}

// materialize extracts the bundle's defaults model_candidates and the
// first derivation is pinned to candidate 0 of that chain.
func TestMaterializeExtractsCandidateChain(t *testing.T) {
	setGitIdentity(t)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "demo-bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	fm := `---
name: demo
defaults:
  model_candidates: [prov-a/model-a1, prov-a/model-a2]
steps:
  - id: one
    kind: author
    model_candidates: [prov-b/model-b9]
---

# body
`
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub() // name-derived digest: no verification, pin still read
	hub.digest = sentinelDigest()
	sp := &countingSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.WorkflowsDir = dir

	if err := r.Start(context.Background(), "wi_mdl"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = r.Stop(context.Background(), "wi_mdl", protocol.FinishWrapped) }()
	if m := sp.lastModel(); m != "prov-a/model-a1" {
		t.Fatalf("first derivation must pin defaults candidate 0, got %q", m)
	}
	// the chain (defaults, not the per-step override) is on the job and
	// rides state.json
	j := r.jobOf("wi_mdl")
	j.mu.Lock()
	cands, ord := j.candidates, j.candidateOrdinal
	j.mu.Unlock()
	if len(cands) != 2 || cands[0] != "prov-a/model-a1" || ord != 0 {
		t.Fatalf("job chain = %v ord=%d", cands, ord)
	}
}

// buildCmd carries the pinned candidate as spawn argv (--model), in the
// provider-qualified form the harness's model pattern accepts — and never
// invents a pin when the chain is absent.
func TestSpawnModelPinRidesArgv(t *testing.T) {
	sp := newPiSpawner(&Config{PiBin: "/nonexistent-pi"})

	pinned := sp.buildCmd(SpawnParams{
		WorkItemID: "wi_pin", CapsuleDir: "/caps", SessionHandle: "ab12", SpawnN: 1,
		Model: "prov-a/model-a2",
	})
	joined := joinArgs(pinned.Args)
	if !contains(joined, "--model prov-a/model-a2") {
		t.Fatalf("pinned candidate must ride argv: %v", pinned.Args)
	}
	// the pin sits after the isolation profile, never before the transport
	if idxOf(pinned.Args, "--mode") > idxOf(pinned.Args, "--model") {
		t.Fatalf("transport flag must precede the pin: %v", pinned.Args)
	}

	plain := sp.buildCmd(SpawnParams{
		WorkItemID: "wi_pin2", CapsuleDir: "/caps", SessionHandle: "ab12", SpawnN: 1,
	})
	if contains(joinArgs(plain.Args), "--model") {
		t.Fatalf("an absent chain must not invent a pin: %v", plain.Args)
	}
}

// A materialization failure (CAP-405 digest divergence) closes the fresh
// generation BLOCKED — never an orphaned running generation (D5) — and
// settles the claiming journal.
func TestMaterializeFailureBlocksGeneration(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	if err := os.WriteFile(filepath.Join(wfDir, "demo-bundle", "agents", "step.md"),
		[]byte("tampered after the pin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub()
	hub.digest = pinned
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir

	err := r.Start(context.Background(), "wi_cap405")
	if err == nil {
		t.Fatal("Start must refuse a diverged pinned bundle")
	}
	if ae, ok := err.(*api.Error); !ok || ae.Code != api.ErrCapCheckoutDiver {
		t.Fatalf("expected CAP-405, got %v", err)
	}
	if n := hub.endCount(); n != 1 {
		t.Fatalf("the failed start must close the generation exactly once, got %d", n)
	}
	end := hub.lastEnd()
	if end.Status != api.StatusBlocked || end.SessionID == "" || end.Note == "" {
		t.Fatalf("CAP-405 must close blocked on the session, got %+v", end)
	}
	if end.Note == "" {
		t.Fatalf("the blocked close must carry a note, got %+v", end)
	}
	// the claiming journal settled with it
	if items := r.loadClaiming(); len(items) != 0 {
		t.Fatalf("claiming journal must be empty after a compensated start, got %+v", items)
	}
	if r.jobOf("wi_cap405") != nil {
		t.Fatal("a compensated start leaves no live job")
	}
}

// A spawn failure after a successful claim is compensated the same way:
// the generation closes blocked, the journal settles.
func TestSpawnFailureBlocksGeneration(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	sp.failFresh = 1
	r, _, _ := newTestRuntime(t, hub, sp)

	if err := r.Start(context.Background(), "wi_spawnfail"); err == nil {
		t.Fatal("Start must fail when the derivation cannot spawn")
	}
	if n := hub.endCount(); n != 1 || hub.lastEnd().Status != api.StatusBlocked {
		t.Fatalf("a spawn failure must close the generation blocked once, got %d %+v",
			hub.endCount(), hub.lastEnd())
	}
	if items := r.loadClaiming(); len(items) != 0 {
		t.Fatalf("claiming journal must be empty, got %+v", items)
	}
}

// A crash between the claim call and the spawn leaves a durable claiming
// record; Restart verifies the journaled session, replays the claim with
// the SAME idempotency key (D4: the same answer), adopts the session and
// finishes the start.
func TestClaimCrashRestartReplaysSameKey(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &countingSpawner{}
	dataDir := filepath.Join(t.TempDir(), "data")
	r, _, _ := newTestRuntimeAt(t, hub, sp, dataDir)

	// the crash shape: the journal holds an unsettled claim intention
	// (the minted secret and the key), state.json holds no job for it
	// (the crash predates registration) — but the claim DID land on the
	// hub, so the hub reports the journaled session as the holder.
	key := randomKey()
	session := fakeSession("wi_crashd")
	if err := r.noteClaiming("wi_crashd", session, key); err != nil {
		t.Fatal(err)
	}
	hub.hold("wi_crashd", session, api.StatusRunning)

	// a FRESH runtime over the same data dir replays it
	hub2 := newFakeHub()
	hub2.hold("wi_crashd", session, api.StatusRunning)
	sp2 := &countingSpawner{}
	r2, _, _ := newTestRuntimeAt(t, hub2, sp2, dataDir)
	if err := r2.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if n := hub2.claimCount(); n != 1 {
		t.Fatalf("the replay must issue exactly one claim, got %d", n)
	}
	if got := hub2.claims[0].IdempotencyKey; got != key {
		t.Fatalf("the replay must carry the journaled key %q, got %q", key, got)
	}
	if got := hub2.claims[0].SessionID; got != session {
		t.Fatalf("the replay must carry the journaled session secret, got %q", got)
	}
	j := r2.jobOf("wi_crashd")
	if j == nil {
		t.Fatal("the replayed claim must run to a live agent")
	}
	if j.sessionID != session {
		t.Fatalf("the replay must adopt the SAME session, got %q", j.sessionID)
	}
	if n := sp2.spawnCount(); n != 1 {
		t.Fatalf("the replay must derive exactly one agent, got %d", n)
	}
	// settled: the journal is empty again
	if items := r2.loadClaiming(); len(items) != 0 {
		t.Fatalf("claiming journal must be empty after the replay, got %+v", items)
	}
	if err := r2.Stop(context.Background(), "wi_crashd", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A stale journal entry (the session is no longer ours — e.g. the blocked
// close landed but the journal clear did not) is dropped, not replayed:
// SessionVerify answers matches=false and the entry goes.
func TestStaleClaimingEntryDropped(t *testing.T) {
	hub := newFakeHub()
	hub.status["wi_stalec"] = api.StatusBlocked
	sp := &countingSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)

	if err := r.noteClaiming("wi_stalec", fakeSession("wi_stalec"), randomKey()); err != nil {
		t.Fatal(err)
	}
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if n := hub.claimCount(); n != 0 {
		t.Fatalf("a stale entry must not be re-claimed, got %d claims", n)
	}
	if items := r.loadClaiming(); len(items) != 0 {
		t.Fatalf("the stale entry must be dropped, got %+v", items)
	}
	if r.jobOf("wi_stalec") != nil {
		t.Fatal("no job may be revived from a stale claiming entry")
	}
}

// ---------------------------------------------------------------------------
// helpers

// newTestRuntimeAt builds a runtime whose DataDir (and therefore the
// state.json and claiming-journal paths) is the CALLER's directory, so two
// runtimes can share one bookkeeping tree across a simulated restart.
func newTestRuntimeAt(t *testing.T, hub Aihub, sp Spawner, dataDir string) (*Runtime, chan protocol.RTMsg, chan protocol.OBMsg) {
	t.Helper()
	cfg := Config{
		MachineID:      "m-test",
		Workspace:  filepath.Join(t.TempDir(), "ws"),
		DataDir:        dataDir,
		MaxResumeFails: 2,
		MaxRecoveries:  2,
		SendBound:      2 * time.Second,
		// episodes, not wall time (see newTestRuntime)
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := make(chan protocol.RTMsg, 64)
	fromOB := make(chan protocol.OBMsg, 64)
	r := New(cfg, hub, sp, toOB, fromOB)
	return r, toOB, fromOB
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func idxOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func (s *countingSpawner) spawnCount() int { return len(s.models) }

// When the compensation's hub close itself fails (an aihub outage at
// the worst moment), the claiming entry is KEPT: a later restart replays
// the same key and retries the compensation — the dangling claim window
// stays closed even across a failing close.
func TestAbandonedClaimKeepsJournalWhenCloseFails(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	if err := os.WriteFile(filepath.Join(wfDir, "demo-bundle", "agents", "step.md"),
		[]byte("tampered after the pin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub()
	hub.digest = pinned
	hub.lookupErr["wi_closefail"] = errLookupBoom // the hub close fails
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir

	if err := r.Start(context.Background(), "wi_closefail"); err == nil {
		t.Fatal("Start must fail on the diverged bundle")
	}
	// the close never landed: the entry must survive for the next restart
	items := r.loadClaiming()
	if len(items) != 1 {
		t.Fatalf("a failed close must keep the claiming entry, got %+v", items)
	}
	if _, ok := items["wi_closefail"]; !ok {
		t.Fatalf("the entry must be keyed by the submitted spelling, got %+v", items)
	}
}
