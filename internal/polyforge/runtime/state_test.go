package runtime

// State-machine regressions: every legal edge, every illegal edge refused,
// and the message-loop fencing rules (stale attempt-id dropped;
// same-attempt WINeedsRecovery dropped while SEALING; duplicate death
// signals dropped while RECOVERING).

import (
	"context"
	"testing"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestStateMachineLegalEdges(t *testing.T) {
	// RUNNING → SEALING → DONE (stop path)
	j := &job{wi: "wi_sm1", state: StateRunning}
	if err := j.transition(StateSealing); err != nil {
		t.Fatalf("running→sealing: %v", err)
	}
	if err := j.transition(StateDone); err != nil {
		t.Fatalf("sealing→done: %v", err)
	}
	// RUNNING → RECOVERING → RUNNING (recovery succeeded)
	j = &job{wi: "wi_sm1", state: StateRunning}
	if err := j.transition(StateRecovering); err != nil {
		t.Fatalf("running→recovering: %v", err)
	}
	if err := j.transition(StateRunning); err != nil {
		t.Fatalf("recovering→running: %v", err)
	}
	// RUNNING → RECOVERING → DONE (bounded fail)
	j = &job{wi: "wi_sm1", state: StateRunning}
	if err := j.transition(StateRecovering); err != nil {
		t.Fatalf("running→recovering: %v", err)
	}
	if err := j.transition(StateDone); err != nil {
		t.Fatalf("recovering→done: %v", err)
	}
}

func TestStateMachineIllegalEdges(t *testing.T) {
	cases := []struct {
		from, to JobState
	}{
		{StateRunning, StateRunning},
		{StateRunning, StateDone},    // stop must pass through SEALING
		{StateSealing, StateRunning}, // a seal is not reversible
		{StateSealing, StateSealing},
		{StateSealing, StateRecovering}, // §三: SEALING drops recovery
		{StateRecovering, StateSealing},
		{StateRecovering, StateRecovering},
		{StateDone, StateRunning},
		{StateDone, StateSealing},
		{StateDone, StateRecovering},
		{StateDone, StateDone},
	}
	for _, c := range cases {
		j := &job{wi: "wi_sm2", state: c.from}
		if err := j.transition(c.to); err == nil {
			t.Fatalf("illegal transition %s → %s must be refused", c.from, c.to)
		}
		if j.state != c.from {
			t.Fatalf("refused transition mutated the state: %s → %s", c.from, j.state)
		}
	}
}

func TestStaleAttemptIDDropped(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, fromOB := newTestRuntime(t, hub, sp)
	wi := "wi_fence"
	j := seedJob(r, wi)
	old := installOldAgent(t, sp, j)
	defer old.Terminate(2e9)
	baseline := sp.callCount() // the install itself is one derivation

	// stale attempt-id: Observe still watching a handle we replaced
	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, AttemptID: "ra_" + wi + "#9",
		WorkItem: wi, Reason: protocol.RecoverDeath,
	})
	state, _ := j.snapshot()
	if state != StateRunning {
		t.Fatalf("stale message must not move the state, got %s", state)
	}
	if sp.callCount() != baseline {
		t.Fatal("stale message must not trigger a re-derivation")
	}
	assertNoRT(t, toOB)

	// stale completion is equally fenced
	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBCompleted, AttemptID: "ra_old_handle", WorkItem: wi, Reason: "wrapped",
	})
	if hub.endCount() != 0 {
		t.Fatal("stale completion must not end the generation")
	}
	_ = fromOB
}

func TestSealingDropsSameAttemptNeedsRecovery(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_sealdrop"
	j := seedJob(r, wi)
	if err := j.transition(StateSealing); err != nil {
		t.Fatal(err)
	}
	// a duplicate death signal for the very handle being sealed
	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, AttemptID: j.instance, WorkItem: wi, Reason: protocol.RecoverDeath,
	})
	if st, _ := j.snapshot(); st != StateSealing {
		t.Fatalf("SEALING must drop same-attempt WINeedsRecovery, got %s", st)
	}
	if sp.callCount() != 0 {
		t.Fatal("SEALING must not re-derive")
	}
	assertNoRT(t, toOB)
}

func TestRecoveringDropsDuplicateDeathSignal(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_dup"
	j := seedJob(r, wi)
	if err := j.transition(StateRecovering); err != nil {
		t.Fatal(err)
	}
	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, AttemptID: j.instance, WorkItem: wi, Reason: protocol.RecoverDeath,
	})
	if st, _ := j.snapshot(); st != StateRecovering {
		t.Fatalf("RECOVERING must drop duplicate signals, got %s", st)
	}
	if sp.callCount() != 0 {
		t.Fatal("RECOVERING must not start a second cascade")
	}
	assertNoRT(t, toOB)
}

// TestRunLoopDrivesRecovery proves the loop wiring end-to-end: a WINeedsRecovery
// fed through the fromOB channel reaches the recovery engine and Observe
// hears RTRecovered.
func TestRunLoopDrivesRecovery(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, fromOB := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_loop"
	j := seedJob(r, wi)
	writeTranscript(t, j)
	installOldAgent(t, sp, j)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	fromOB <- protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, AttemptID: j.instance, WorkItem: wi, Reason: protocol.RecoverDeath,
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	// tidy: tear the recovered sleeper down too
	if j2 := r.jobOf(wi); j2 != nil {
		j2.mu.Lock()
		h := j2.agent
		j2.mu.Unlock()
		if h != nil {
			_ = h.Terminate(2e9)
		}
	}
	cancel()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("Run did not exit on ctx cancel")
	}
}

// TestOBCompletedDrivesStop: Observe's completion message (flush(wrap)
// landed) drives the seal path with the final status it carries.
func TestOBCompletedDrivesStop(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	wi := "wi_completed"
	j := seedJob(r, wi)
	initGitCapsule(t, j)
	installOldAgent(t, &scriptedSpawner{ /* separate spawner: no derivation here */ }, j)

	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBCompleted, AttemptID: j.instance, WorkItem: wi, Reason: "wrapped",
	})

	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishWrapped {
		t.Fatalf("completion must seal wrapped, got %+v", msg)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishWrapped {
		t.Fatalf("end-generation must be driven by the completion, got %+v", hub.lastEnd())
	}
	if r.jobOf(wi) != nil {
		t.Fatal("completed job must leave the live set")
	}
	unseal(t, r.cfg.WorkspaceRoot)
}

// TestOBCompletedFailedReason: the failed final status is carried through.
func TestOBCompletedFailedReason(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	wi := "wi_failed"
	j := seedJob(r, wi)
	initGitCapsule(t, j)

	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBCompleted, AttemptID: j.instance, WorkItem: wi, Reason: "failed",
	})
	msg := recvRT(t, toOB)
	if msg.Reason != protocol.FinishFailed {
		t.Fatalf("failed completion must seal failed, got %+v", msg)
	}
	if hub.lastEnd().Status != protocol.FinishFailed {
		t.Fatalf("end-generation status must be failed, got %+v", hub.lastEnd())
	}
	unseal(t, r.cfg.WorkspaceRoot)
}
