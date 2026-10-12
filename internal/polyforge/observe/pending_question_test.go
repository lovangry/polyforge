package observe

// pending_question_test.go: the pins that survive the polyforge#106 answer-
// relay trim. The legacy WI-Agent redelivery lane this file used to pin
// (delivered-but-unconsumed answers re-sent on the next instance
// attachment) is deleted — the driver consumes answers, and the durable
// QUESTION face re-presents the persisted question by itself (pinned
// unedited by the migrated s19 e2e suite). Two KEPT faces stay pinned
// here, byte-for-byte as before:
//
//   - the F9 needs_input same-id re-present: one pending, asked_at
//     refreshed (via the exec_committed-triggered execution.json read);
//   - the pending projection's execution.json reconcile: an invalid id
//     never clobbers the valid pending, and a consumed (nil) pending
//     clears the projection (the QUESTION face's D6/M4 half).

import (
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestSameIDRepresentRefreshesAskedAtWithoutNewEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)

	writePendingQuestionAt(t, m, "qF9", "again?", "code", "2026-10-01T00:00:01Z")
	// the same question id re-presented (a recovered derivation re-writing
	// its persisted pending): only asked_at moves.
	writePendingQuestionAt(t, m, "qF9", "again?", "code", "2026-10-01T00:00:02Z")
	m.stateMu.Lock()
	var pending *protocol.PendingView
	if m.pending != nil {
		cp := *m.pending
		pending = &cp
	}
	m.stateMu.Unlock()

	// One question id = one pending — the re-present refreshes the
	// projection's asked_at. The needs_input EVENT is the driver's (it
	// lands in the same log at the park transition): the supervisor's
	// projection must never add a second copy.
	if got := len(eventsOf(m, "needs_input")); got != 0 {
		t.Fatalf("the supervisor must not duplicate the driver's needs_input event: got %d", got)
	}
	if pending == nil || pending.ID != "qF9" || pending.AskedAt != "2026-10-01T00:00:02Z" {
		t.Fatalf("pending must stay the same single question with a refreshed asked_at: %+v", pending)
	}
}

func TestApplyPendingFromExecIgnoresInvalidID(t *testing.T) {
	m, _ := newTestMonitor(t, &fakeHandle{}, nil)
	// a valid pending already recorded
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
