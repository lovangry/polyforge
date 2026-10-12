package runtime

// Recovery-engine regressions (v1-spec-full §七) driven with REAL child
// processes as the "agents": transcript present → resume; absent → fresh;
// two consecutive resume failures → fresh; storm guard / fresh failure →
// bounded fail (RTFinished(blocked)).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
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

// recordingHandle is a minimal AgentHandle that records Terminate calls —
// the shape of the driver bridge handle (agentPID 0, teardown through the
// handle itself).
type recordingHandle struct {
	terminated bool
}

func (h *recordingHandle) PID() int                            { return 0 }
func (h *recordingHandle) StartedAt() string                   { return "" }
func (h *recordingHandle) Alive() bool                         { return !h.terminated }
func (h *recordingHandle) Done() bool                          { return h.terminated }
func (h *recordingHandle) Terminate(bound time.Duration) error { h.terminated = true; return nil }

// TestRecoverDriverTerminatesOldHandleAndSeedsFromJournal (the #99
// verification lesson): a driver-path recovery must (1) TERMINATE the old
// bridge handle — the pid-keyed cleanup is a no-op at agentPID 0 and the
// old driver goroutine otherwise survives as a second envelope collector
// on execution.json — and (2) seed the relaunch ordinal past the CAPSULE
// SPAWN JOURNAL max, not the job counter, so the new driver's step
// ordinals never collide with the dead driver's.
func TestRecoverDriverTerminatesOldHandleAndSeedsFromJournal(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_drvreseed"
	j := seedJob(r, wi)
	// journal max 7 > job counter 1: the dead driver derived step
	// ordinals ahead of the relaunch counter
	journal := filepath.Join(j.capsuleDir, "runtime-state", "spawn-journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := ""
	for n := 1; n <= 7; n++ {
		lines += fmt.Sprintf(`{"spawn_n":%d,"transcript":"t%d"}%s`, n, n, "\n")
	}
	if err := os.WriteFile(journal, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	old := &recordingHandle{}
	j.mu.Lock()
	j.agent = old
	j.agentPID = 0 // the driver shape
	j.mu.Unlock()
	var gotSpawn int64
	r.cfg.AgentStarter = func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
		gotSpawn = in.SpawnN
		return &recordingHandle{}, nil
	}

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if msg := recvRT(t, toOB); msg.Kind != protocol.RTRecovered {
		t.Fatalf("driver recovery must relaunch, got %+v", msg)
	}
	if !old.terminated {
		t.Fatal("the old bridge handle must be terminated on driver recovery")
	}
	if gotSpawn != 8 {
		t.Fatalf("relaunch must seed past the journal max (want 8, got %d)", gotSpawn)
	}
}

// TestRecoverTerminalWorkflowCloses is the #99-finale regression: a
// driver whose workflow reached a terminal state has Run returning nil
// as its COMPLETION signal — the monitor reports a plain exit, and the
// recovery entry must translate the capsule's terminal verdict into the
// WI's close instead of relaunching: completed wraps (RTFinished wrapped,
// SessionEnd wrapped, no relaunch), failed_* fails, blocked hands the
// owner the unblock door. Without the wiring the exit bounced through
// death-recovery into the storm guard and closed the WI blocked.
func TestRecoverTerminalWorkflowCloses(t *testing.T) {
	for _, tc := range []struct {
		status  string
		endKind protocol.RTMsgKind
		wantEnd string
	}{
		{capsule.WorkflowStatusCompleted, protocol.RTFinished, api.StatusWrapped},
		{capsule.WorkflowStatusFailedGate, protocol.RTFinished, api.StatusFailed},
		{capsule.WorkflowStatusBlocked, protocol.RTFinished, api.StatusBlocked},
	} {
		t.Run(tc.status, func(t *testing.T) {
			hub := newFakeHub()
			sp := &scriptedSpawner{}
			r, toOB, _ := newTestRuntime(t, hub, sp)
			wi := "wi_term_" + strings.ReplaceAll(tc.status, "_", "")
			j := seedJob(r, wi)
			execDir := filepath.Join(j.capsuleDir, "runtime-state")
			if err := os.MkdirAll(execDir, 0o755); err != nil {
				t.Fatal(err)
			}
			blob, _ := json.Marshal(map[string]string{"workflow_status": tc.status})
			if err := os.WriteFile(filepath.Join(execDir, "execution.json"), blob, 0o644); err != nil {
				t.Fatal(err)
			}
			j.mu.Lock()
			j.agent = &recordingHandle{} // the dead driver handle (pid-less shape)
			j.agentPID = 0
			j.mu.Unlock()
			starterCalls := 0
			r.cfg.AgentStarter = func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
				starterCalls++
				return &recordingHandle{}, nil
			}

			r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
			msg := recvRT(t, toOB)
			if msg.Kind != protocol.RTFinished || msg.Reason != tc.wantEnd {
				t.Fatalf("terminal workflow must close as %s, got %+v", tc.wantEnd, msg)
			}
			if starterCalls != 0 {
				t.Fatalf("a terminal workflow must NOT relaunch the driver (starter called %d times)", starterCalls)
			}
			if r.jobOf(wi) != nil {
				t.Fatal("the closed job must leave the live set")
			}
		})
	}
}

// TestRecoverNonTerminalWorkflowRecovers: "running" (and a missing
// execution.json — the legacy WI-Agent shape) fall through to the
// ordinary recovery machinery.
func TestRecoverNonTerminalWorkflowRecovers(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_term_running"
	j := seedJob(r, wi)
	j.mu.Lock()
	j.agent = &recordingHandle{}
	j.agentPID = 0
	j.mu.Unlock()
	r.cfg.AgentStarter = func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
		return &recordingHandle{}, nil
	}
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if msg := recvRT(t, toOB); msg.Kind != protocol.RTRecovered {
		t.Fatalf("a running workflow must recover normally, got %+v", msg)
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
