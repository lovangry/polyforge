package runtime

// Recovery-engine regressions (v1-spec-full §七) driven with REAL child
// processes as the "agents": transcript present → resume; absent → fresh;
// two consecutive resume failures → fresh; storm guard / fresh failure →
// bounded fail (RTFinished(blocked)).

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func recoveryMsg(j *job, reason string) protocol.OBMsg {
	j.mu.Lock()
	spawn := j.spawnN
	j.mu.Unlock()
	return protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, SpawnN: spawn,
		WorkItem: j.wi, Reason: reason,
	}
}

func TestRecoverTranscriptPresentResumes(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_resume"
	j := seedJob(r, wi)
	writeTranscript(t, j)
	old := installOldAgent(t, sp, j)
	oldPID := old.PID()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 { // install + the resume derivation
		t.Fatalf("expected exactly the resume derivation, got %d calls", n)
	}
	if c := sp.call(1); !c.Resume || c.TranscriptPath != j.transcript {
		t.Fatalf("derivation must be a resume of the transcript, got %+v", c)
	}
	// The resume derivation is derivation #2 of the session: the spawn
	// ordinal (the B2 fence key) is 2.
	if n := sp.call(1).SpawnN; n != 2 {
		t.Fatalf("resume derivation spawn ordinal: got %d, want 2", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered || msg.WorkItem != wi {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	if msg.SpawnN != 2 {
		t.Fatalf("RTRecovered must announce spawn_n 2, got %d", msg.SpawnN)
	}
	if msg.AgentPID == oldPID || msg.AgentPID <= 0 {
		t.Fatalf("RTRecovered must carry the NEW handle: old=%d new=%d", oldPID, msg.AgentPID)
	}
	waitDead(t, oldPID)
	if st, _ := j.snapshot(); st != StateRunning {
		t.Fatalf("recovery must return to RUNNING, got %s", st)
	}
	j.mu.Lock()
	fails := j.resumeFails
	j.mu.Unlock()
	if fails != 0 {
		t.Fatalf("a successful resume must not count failures, got %d", fails)
	}
	// tidy: terminate the recovered sleeper
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func TestRecoverTranscriptAbsentGoesFresh(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_fresh"
	j := seedJob(r, wi) // no transcript file
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 {
		t.Fatalf("expected one fresh derivation, got %d calls", n)
	}
	if c := sp.call(1); c.Resume {
		t.Fatalf("absent transcript must route to a fresh derivation, got %+v", c)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	if c := sp.call(1); len(c.InitialPrompt) == 0 || !contains(c.InitialPrompt, "fresh derivation") {
		t.Fatal("the fresh prompt must frame the derivation and point at old transcripts")
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func TestRecoverGarbageTranscriptIsNotReadable(t *testing.T) {
	// the discriminator is READABILITY, not existence: a zero-byte or
	// non-JSON husk left by the crash routes to fresh
	if transcriptReadable("/nonexistent/nope.jsonl") {
		t.Fatal("missing transcript must not read as resumable")
	}
	dir := t.TempDir()
	p := dir + "/t.jsonl"
	if err := writeBytes(p, nil); err != nil {
		t.Fatal(err)
	}
	if transcriptReadable(p) {
		t.Fatal("empty transcript must not read as resumable")
	}
	if err := writeBytes(p, []byte("not json at all\n")); err != nil {
		t.Fatal(err)
	}
	if transcriptReadable(p) {
		t.Fatal("garbage transcript must not read as resumable")
	}
	if err := writeBytes(p, []byte("{\"type\":\"message_end\"}\n")); err != nil {
		t.Fatal(err)
	}
	if !transcriptReadable(p) {
		t.Fatal("a JSON record transcript must read as resumable")
	}
}

func TestRecoverResumeFailsTwiceThenFresh(t *testing.T) {
	sp := &scriptedSpawner{}
	sp.failResume = 2
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_2fail"
	j := seedJob(r, wi)
	writeTranscript(t, j)
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	// cascade: resume, resume, fresh — all within the one event
	if n := sp.callCount(); n != 4 {
		t.Fatalf("expected resume+resume+fresh, got %d calls", n)
	}
	if sp.call(1).Resume != true || sp.call(2).Resume != true {
		t.Fatal("the first two derivations must be resume attempts")
	}
	if sp.call(3).Resume {
		t.Fatal("after two consecutive resume failures the cascade must go fresh")
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered after the fresh fallback, got %+v", msg)
	}
	j.mu.Lock()
	fails, dead := j.resumeFails, j.resumeDead
	j.mu.Unlock()
	if fails != 2 || !dead {
		t.Fatalf("resume failure bookkeeping wrong: fails=%d dead=%v", fails, dead)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// TestRecoverBudgetTwoFailuresBoundedFail pins the ruled budget semantics
// (§三/§七): the budget counts FAILED recoveries; the ladder retries until
// MaxRecoveries (default 2) failures, then bounded-fails — never earlier,
// and never on the first failure alone.
func TestRecoverBudgetTwoFailuresBoundedFail(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_budget"
	j := seedJob(r, wi)
	installOldAgent(t, sp, j)
	sp.mu.Lock()
	sp.failFresh = 2 // no transcript → fresh; both fresh attempts fail
	sp.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	// two failed ladder attempts, then the budget is spent
	if n := sp.callCount(); n != 3 { // install + 2 failed fresh derivations
		t.Fatalf("expected two failed derivations before bounded fail, got %d calls", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("expected RTFinished(blocked), got %+v", msg)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishBlocked {
		t.Fatalf("bounded fail must end the generation blocked, got %+v", hub.lastEnd())
	}
	if r.jobOf(wi) != nil {
		t.Fatal("bounded-failed job must leave the live set")
	}
	if st, _ := j.snapshot(); st != StateDone {
		t.Fatalf("bounded fail must reach DONE, got %s", st)
	}
}

// TestRecoverBudgetReservesAcrossEpisodes: a pre-spent budget (a crash
// mid-recovery left one failure on the record) plus one new failed ladder
// reaches the bound — the storm guard never lets a poisoned WI relaunch
// forever.
func TestRecoverBudgetSpentAfterPersistedFailure(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_storm"
	j := seedJob(r, wi)
	j.mu.Lock()
	j.recoverFails = 1 // one failed recovery already on the record
	j.mu.Unlock()
	installOldAgent(t, sp, j)
	sp.mu.Lock()
	sp.failFresh = 1
	sp.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 { // install + ONE failed relaunch: the bound hit
		t.Fatalf("a pre-spent budget must bounded-fail after one new failure, got %d calls", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("expected RTFinished(blocked), got %+v", msg)
	}
}

// TestRecoverBudgetResetOnSuccess: one failed ladder attempt followed by
// a successful relaunch RESETS the budget — a later, independent failure
// episode starts from zero, not from the inherited count.
func TestRecoverBudgetResetOnSuccess(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_reset"
	j := seedJob(r, wi)
	installOldAgent(t, sp, j)
	sp.mu.Lock()
	sp.failFresh = 1 // first ladder attempt fails, the retry goes live
	sp.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 3 { // install + failed fresh + retry fresh
		t.Fatalf("expected one failed then one successful derivation, got %d calls", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	j.mu.Lock()
	fails := j.recoverFails
	j.mu.Unlock()
	if fails != 0 {
		t.Fatalf("a successful recovery must reset the budget, got %d", fails)
	}
	// a SECOND death episode starts from a clean budget: one scripted
	// failure is absorbed by a retry, not a bounded fail
	sp.mu.Lock()
	sp.failFresh = 1
	sp.mu.Unlock()
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	msg2 := recvRT(t, toOB)
	if msg2.Kind != protocol.RTRecovered {
		t.Fatalf("the reset budget must absorb an isolated failure, got %+v", msg2)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// TestRecoverSingleFreshFailureIsRetriedNotTerminal: ONE failed ladder
// attempt spends one unit of the budget and is RETRIED — a transient
// spawn failure is not a terminal verdict; only the exhausted budget is.
func TestRecoverSingleFreshFailureIsRetriedNotTerminal(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_freshfail"
	j := seedJob(r, wi) // no transcript → fresh; fresh is scripted to fail once
	installOldAgent(t, sp, j)
	sp.mu.Lock()
	sp.failFresh = 1
	sp.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("a single fresh failure must be retried, got %+v", msg)
	}
	if hub.endCount() != 0 {
		t.Fatalf("no generation may end on a retried failure, got %+v", hub.lastEnd())
	}
	j.mu.Lock()
	fails := j.recoverFails
	j.mu.Unlock()
	if fails != 0 {
		t.Fatalf("the successful retry must reset the budget, got %d", fails)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// TestRecoverStormGuardBlocksRapidRefire: the #97 shape — every relaunch
// SUCCEEDS as a spawn and dies within seconds, refiring recovery at ~1
// Hz. The legacy budget never saw these (each relaunch reset it); the
// entry-rate ledger counts rapid re-entries and bounded-fails the WI on
// the MaxRecoveries-th rapid refire.
func TestRecoverStormGuardBlocksRapidRefire(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	r.cfg.RecoveryStabilityWindow = 60 * time.Second // the production window
	wi := "wi_stormrate"
	j := seedJob(r, wi)
	installOldAgent(t, sp, j)

	// two back-to-back relaunches succeed (storm ledger 0 then 1 —
	// below the bound of MaxRecoveries=2)
	for i := 0; i < 2; i++ {
		r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
		if msg := recvRT(t, toOB); msg.Kind != protocol.RTRecovered {
			t.Fatalf("rapid recovery %d must still relaunch (ledger below the bound), got %+v", i+1, msg)
		}
	}
	// the third rapid refire reaches the bound: bounded fail, never a
	// fourth relaunch
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("the third rapid refire must bounded-fail, got %+v", msg)
	}
	if n := sp.callCount(); n != 3 { // install + two relaunches, NOT three
		t.Fatalf("the storm must stop at two relaunches, got %d spawns", n)
	}
}

// TestRecoverStormLedgerResetsAfterStabilityWindow: a recovery that
// arrives after a stable gap starts a fresh episode — an isolated later
// failure is never poisoned by an earlier storm.
func TestRecoverStormLedgerResetsAfterStabilityWindow(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	r.cfg.RecoveryStabilityWindow = 15 * time.Millisecond
	wi := "wi_stormgap"
	j := seedJob(r, wi)
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if msg := recvRT(t, toOB); msg.Kind != protocol.RTRecovered {
		t.Fatalf("first recovery must relaunch, got %+v", msg)
	}
	// inside the window: ledger 1 — still below the bound, relaunches
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if msg := recvRT(t, toOB); msg.Kind != protocol.RTRecovered {
		t.Fatalf("in-window refire must still relaunch below the bound, got %+v", msg)
	}
	// past the window: the ledger resets, the episode is fresh
	j.mu.Lock()
	j.lastRecoveryAt = time.Now().Add(-time.Hour)
	j.mu.Unlock()
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if msg := recvRT(t, toOB); msg.Kind != protocol.RTRecovered {
		t.Fatalf("a post-gap recovery must relaunch with a fresh ledger, got %+v", msg)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// TestRecoverDeadOldAgentWithOrphans drives the recovery against an old
// agent that DIED leaving orphaned children in its group: the six-layer
// cleanup must prove the tree empty before the re-derivation starts.
func TestRecoverDeadOldAgentWithOrphans(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_orphans"
	j := seedJob(r, wi)

	// an "agent" that forks a long-lived child and dies without reaping it
	old, err := spawnOrphaningSleeper(wi)
	if err != nil {
		t.Fatal(err)
	}
	orphan := old.orphanPID
	j.mu.Lock()
	j.agent = old.handle
	j.agentPID = old.handle.PID()
	j.agentStartedAt = old.handle.StartedAt()
	j.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	waitDead(t, orphan) // the orphan died with the tree, not just the leader
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
