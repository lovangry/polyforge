package observe

// redeliver_test.go: the F9 answer-redelivery contract — an answer that
// was DELIVERED but never acked/consumed stays re-deliverable in the
// ledger, and the NEXT instance attachment (RTStarted/RTRecovered
// landing) automatically re-sends it when its question is still the
// open pending one. Failure keeps the entry; other questions are not
// resent; the same-id needs_input re-present stays idempotent (one
// pending, one event, asked_at refreshed).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// sentAnswers lists the pf/answer control messages one handle received.
func sentAnswers(h *fakeHandle) []protocol.ControlMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []protocol.ControlMsg
	for _, msg := range h.sends {
		if msg.Method == protocol.ControlAnswer {
			out = append(out, msg)
		}
	}
	return out
}

// recoverMonitor adopts a successor handle the way Runtime's RTRecovered
// attachment does.
func recoverMonitor(m *Monitor, h Handle, attempt string) {
	m.rtCh <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: m.wi.WorkItem, AttemptID: attempt},
		Handle: h,
		WI:     m.wi,
	}
}

func TestRedeliveryOnRecoveryResendsAwaitingAnswer(t *testing.T) {
	h1 := newFakeHandle("att_1")
	h1.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		// Delivered, never acked: the two-phase pending state.
		return protocol.ControlAck{}, ErrAnswerAckPending
	})
	m, _ := newTestMonitor(t, h1, nil)
	armQuestion(t, m, "q1")

	relay := relayFor("q1", "a1")
	if err := submitAnswer(m, relay); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("first submission must settle ack-pending, got %v", err)
	}
	if len(m.ledger.Awaiting(m.wi.WorkItem)) != 1 {
		t.Fatalf("ledger must hold the delivered-but-unconsumed answer")
	}

	// The successor instance attaches: the awaiting answer is re-sent
	// automatically, bound to the SAME question id.
	h2 := newFakeHandle("att_2")
	recoverMonitor(m, h2, "att_2")
	soon(t, 2*time.Second, "redelivery never reached the successor", func() bool {
		return len(sentAnswers(h2)) == 1
	})
	sent := sentAnswers(h2)
	if sent[0].ID != "q1" || sent[0].Answer != relay.AnswerText {
		t.Fatalf("redelivered relay wrong: %+v", sent[0])
	}

	// Consumption observed on the successor: the ledger finalized, the
	// awaiting set emptied, the pending projection cleared, and the
	// attempt + consumption are both on the events record.
	soon(t, 2*time.Second, "redelivery never settled consumed", func() bool {
		return len(m.ledger.Awaiting(m.wi.WorkItem)) == 0
	})
	if len(eventsOf(m, EvAnswerRedelivery)) < 1 {
		t.Fatalf("no answer_redelivery event recorded")
	}
	if len(eventsOf(m, EvAnswered)) < 1 {
		t.Fatalf("no answered event after the redelivery consumption")
	}
	m.stateMu.Lock()
	pending := m.pending
	m.stateMu.Unlock()
	if pending != nil {
		t.Fatalf("pending must clear after the redelivered consumption: %+v", pending)
	}

	// The finalized id replays its outcome verbatim and never re-delivers.
	h2Before := len(sentAnswers(h2))
	if err := submitAnswer(m, relay); err != nil {
		t.Fatalf("replay of the redelivered answer: %v", err)
	}
	if len(sentAnswers(h2)) != h2Before {
		t.Fatalf("a finalized answer_id must never re-deliver")
	}
}

func TestRedeliverySkipsAnswersBoundToOtherQuestions(t *testing.T) {
	h1 := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h1, nil)
	armQuestion(t, m, "q1")
	// An awaiting answer bound to an OLDER, superseded question: it is
	// not this attach's business — the open question is q1.
	m.ledger.noteAwaiting(m.wi.WorkItem, relayFor("q_old", "a_old"))

	h2 := newFakeHandle("att_2")
	recoverMonitor(m, h2, "att_2")
	// Give the monitor a moment to (wrongly) send, then assert it did not.
	time.Sleep(150 * time.Millisecond)
	if n := len(sentAnswers(h2)); n != 0 {
		t.Fatalf("answers of other questions must not be resent: %d", n)
	}
	if len(m.ledger.Awaiting(m.wi.WorkItem)) != 1 {
		t.Fatalf("the foreign awaiting entry must stay untouched")
	}
}

func TestRedeliveryFailureStaysInLedgerForNextAttach(t *testing.T) {
	h1 := newFakeHandle("att_1")
	h1.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, ErrAnswerAckPending
	})
	m, _ := newTestMonitor(t, h1, nil)
	armQuestion(t, m, "q1")
	if err := submitAnswer(m, relayFor("q1", "a1")); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("first submission: %v", err)
	}

	// The successor dies too: its redelivery fails, the entry stays.
	h2 := newFakeHandle("att_2")
	h2.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("agent dead pre-ack")
	})
	recoverMonitor(m, h2, "att_2")
	soon(t, 2*time.Second, "failed redelivery never attempted", func() bool {
		return len(sentAnswers(h2)) == 1
	})
	soon(t, 2*time.Second, "failed redelivery never recorded", func() bool {
		// the failure event carries the error field
		for _, e := range eventsOf(m, EvAnswerRedelivery) {
			if eventHasError(e) {
				return true
			}
		}
		return false
	})
	if len(m.ledger.Awaiting(m.wi.WorkItem)) != 1 {
		t.Fatalf("a failed redelivery must keep the ledger entry re-deliverable")
	}

	// The third instance attaches and consumes: the entry finalizes.
	h3 := newFakeHandle("att_3")
	recoverMonitor(m, h3, "att_3")
	soon(t, 2*time.Second, "second redelivery never reached the third instance", func() bool {
		return len(sentAnswers(h3)) == 1
	})
	soon(t, 2*time.Second, "second redelivery never settled consumed", func() bool {
		return len(m.ledger.Awaiting(m.wi.WorkItem)) == 0
	})
}

// eventHasError reports whether one event's payload carries an "error"
// field (the failed-redelivery record).
func eventHasError(e protocol.EventEntry) bool {
	var body struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(e.Body, &body) == nil && body.Error != ""
}

func TestFlushSameIDRefreshesAskedAtWithoutNewEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)

	call := func() FlushCall {
		return FlushCall{
			Req:  protocol.FlushRequest{Reason: protocol.FlushNeedsInput, QuestionID: "qF9", Question: "again?", Step: "code"},
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
