package observe

// redeliver_test.go: the pins that survive the polyforge#106 answer-
// relay trim. The legacy WI-Agent redelivery lane this file used to pin
// (delivered-but-unconsumed answers re-sent on the next instance
// attachment) is deleted — the driver consumes answers, and the durable
// QUESTION face re-presents the persisted question by itself (pinned
// unedited by the migrated s19 e2e suite). Two KEPT faces stay pinned
// here, byte-for-byte as before:
//
//   - the F9 needs_input same-id re-present: one pending, one event,
//     asked_at refreshed (the flush face — batch 2a kept it);
//   - the pending projection's execution.json reconcile: an invalid id
//     never clobbers the valid pending, and a consumed (nil) pending
//     clears the projection (the QUESTION face's D6/M4 half).

import (
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestFlushSameIDRefreshesAskedAtWithoutNewEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)

	call := func() FlushCall {
		return FlushCall{
			Req:  protocol.FlushRequest{SpawnN: 1, Reason: protocol.FlushNeedsInput, QuestionID: "qF9", Question: "again?", Step: "code"},
			Resp: make(chan protocol.FlushAck, 1),
		}
	}
	if ack := doFlush(m, call()); !ack.OK {
		t.Fatalf("first needs_input ack: %+v", ack)
	}
	m.stateMu.Lock()
	first := m.pending.AskedAt
	m.stateMu.Unlock()

	time.Sleep(5 * time.Millisecond) // asked_at is wall-clock derived
	if ack := doFlush(m, call()); !ack.OK {
		t.Fatalf("same-id re-present ack: %+v", ack)
	}
	m.stateMu.Lock()
	second := m.pending.AskedAt
	pending := m.pending
	m.stateMu.Unlock()

	// One question id = one needs_input event, one pending — but the
	// re-present refreshes the projection's asked_at.
	if got := len(eventsOf(m, EvNeedsInput)); got != 1 {
		t.Fatalf("needs_input events: got %d, want 1", got)
	}
	if pending == nil || pending.ID != "qF9" {
		t.Fatalf("pending must stay the same single question: %+v", pending)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("asked_at must refresh on the same-id re-present: %q vs %q", first, second)
	}
}

func TestApplyPendingFromExecIgnoresInvalidID(t *testing.T) {
	m, _ := newTestMonitor(t, &fakeHandle{}, nil)
	// flush face recorded a valid pending
	m.applyPending(&protocol.PendingView{ID: "q_good", Step: "grill", Question: "valid", AskedAt: "t1"})
	// execution.json carries an invalid pending (id missing)
	exec := &capsule.Execution{CurrentStep: "grill", Steps: map[string]*capsule.Step{
		"grill": {PendingQuestion: &capsule.PendingQuestion{Step: "grill", Question: "invalid"}},
	}}
	m.applyPendingFromExec(exec)
	m.stateMu.Lock()
	got := m.pending
	m.stateMu.Unlock()
	if got == nil || got.ID != "q_good" {
		t.Fatalf("invalid exec pending must not clobber the valid one; got %+v", got)
	}
	// a nil pending in exec (consumed) still clears quietly
	exec.Steps["grill"].PendingQuestion = nil
	m.applyPendingFromExec(exec)
	m.stateMu.Lock()
	got = m.pending
	m.stateMu.Unlock()
	if got != nil {
		t.Fatalf("consumed pending must clear the projection; got %+v", got)
	}
}
