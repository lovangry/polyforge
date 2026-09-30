package observe

// answers_test.go: the answer relay's idempotent THREE-STATE ledger —
// consumed / delivered-pending / redeliverable — driven through the
// monitor (the real path), plus the binding and identity refusals.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func armQuestion(t *testing.T, m *Monitor, qid string) {
	t.Helper()
	fc := FlushCall{Req: protocol.FlushRequest{Reason: protocol.FlushNeedsInput, QuestionID: qid, Question: "pick one"}, Resp: make(chan protocol.FlushAck, 1)}
	m.flushCh <- fc
	if ack := <-fc.Resp; !ack.OK {
		t.Fatalf("needs_input ack: %+v", ack)
	}
}

func submitAnswer(m *Monitor, relay AnswerRelay) error {
	ac := AnswerCall{Relay: relay, Caller: relay.AnsweredBy, Resp: make(chan error, 1)}
	m.answerCh <- ac
	select {
	case err := <-ac.Resp:
		return err
	case <-time.After(3 * time.Second):
		return errors.New("test: answer never settled")
	}
}

func relayFor(qid, aid string) AnswerRelay {
	return AnswerRelay{
		QuestionID: qid, AnswerID: aid, AnswerText: "do it",
		AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "alice",
	}
}

func TestAnswerRelayConsumedIsIdempotent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("first submission: %v", err)
	}
	if h.sentCount() != 1 {
		t.Fatalf("deliveries: got %d, want 1", h.sentCount())
	}
	// A repeated answer_id replays the FIRST outcome verbatim and never
	// re-delivers.
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if h.sentCount() != 1 {
		t.Fatalf("replay must not re-deliver: deliveries=%d", h.sentCount())
	}
	// Consumption observed → the pending projection cleared.
	m.stateMu.Lock()
	pending := m.pending
	m.stateMu.Unlock()
	if pending != nil {
		t.Fatalf("pending must clear after consumption, got %+v", pending)
	}
	if len(eventsOf(m, EvAnswered)) != 1 {
		t.Fatalf("answered events: got %d, want 1", len(eventsOf(m, EvAnswered)))
	}
}

func TestAnswerRelayAckTimeoutStaysPending(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	// The agent takes longer than the ack window: DELIVERED, consumption
	// NOT observed — the two-phase pending state, never an error.
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		<-ctx.Done()
		return protocol.ControlAck{}, ErrAnswerAckPending
	})
	err := submitAnswer(m, relayFor("q1", "a1"))
	if !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("outcome: got %v, want ErrAnswerAckPending", err)
	}
	// The entry stays redeliverable: a re-submit DELIVERS AGAIN.
	h.setSend(nil)
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("re-delivery after pending: %v", err)
	}
	if h.sentCount() != 2 {
		t.Fatalf("deliveries: got %d, want 2 (pending never consumes the id)", h.sentCount())
	}
}

func TestAnswerRelayAgentDeadNeverConsumesID(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("control path closed: agent exited")
	})
	err := submitAnswer(m, relayFor("q1", "a1"))
	if err == nil || errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("dead-agent delivery must fail, got %v", err)
	}
	// FAILURE NEVER CONSUMES THE ID: the same answer_id re-delivers to a
	// healthy successor (e.g. the recovered attempt).
	h.setSend(nil)
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("re-delivery after agent death: %v", err)
	}
	if m.ledger.AwaitingQuestion("wi_test", "q1") {
		t.Fatal("awaiting set must clear on consumption")
	}
}

func TestAnswerRelayBindingAndIdentity(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	// Binding: a fresh question id never matches an open question.
	if err := submitAnswer(m, relayFor("q_other", "a_x")); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("binding refusal: got %v", err)
	}
	// A definitively refused id replays its refusal and never delivers.
	if err := submitAnswer(m, relayFor("q_other", "a_x")); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("refusal replay: got %v", err)
	}
	if h.sentCount() != 0 {
		t.Fatalf("no delivery may happen for a refused binding: %d", h.sentCount())
	}

	// Identity: answered_by must equal the caller.
	ac := AnswerCall{Relay: relayFor("q1", "a_y"), Caller: "mallory", Resp: make(chan error, 1)}
	m.answerCh <- ac
	if err := <-ac.Resp; !errors.Is(err, ErrAnswerIdentity) {
		t.Fatalf("identity refusal: got %v", err)
	}
}

func TestAnswerSurvivesAgentDeathForRecoveredAttempt(t *testing.T) {
	// The recovery projection (F9 semantics): an answer delivered but
	// never consumed before an agent death stays re-deliverable in the
	// ledger, and the monitor now re-sends it to the recovered handle
	// AUTOMATICALLY at the RTRecovered landing — the pending question
	// stays visible until that re-delivery is consumed. The manual CLI
	// re-submit path still works on top (idempotently).
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("agent died mid-delivery")
	})
	_ = submitAnswer(m, relayFor("q1", "a1")) // failure: stays redeliverable

	h2 := newFakeHandle("att_2")
	m.rtCh <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_test", AttemptID: "att_2"}, Handle: h2}
	soon(t, 500*time.Millisecond, "recovery adopted", func() bool { return m.currentAttempt() == "att_2" })

	// The awaiting answer was re-delivered to the NEW handle automatically
	// and consumed there: the pending question clears with it.
	soon(t, 2*time.Second, "auto re-delivery to the recovered handle", func() bool {
		return len(sentAnswers(h2)) == 1
	})
	if got := sentAnswers(h2); got[0].ID != "q1" {
		t.Fatalf("re-delivered relay bound to the wrong question: %+v", got[0])
	}
	soon(t, 2*time.Second, "re-delivered answer never settled consumed", func() bool {
		return len(m.ledger.Awaiting(m.wi.WorkItem)) == 0
	})
	view := m.JobView()
	if view.Pending != nil {
		t.Fatalf("pending must clear once the re-delivered answer is consumed: %+v", view.Pending)
	}

	// The manual re-submit path is unchanged: the finalized answer_id
	// replays its outcome verbatim and never re-delivers.
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("manual re-submit of the finalized answer: %v", err)
	}
	if len(sentAnswers(h2)) != 1 {
		t.Fatalf("a finalized answer_id must never re-deliver")
	}
}
