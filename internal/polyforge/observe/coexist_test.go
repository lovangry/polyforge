package observe

// coexist_test.go: pins the CONFIRMED no-priority coexistence semantics
// of pf/answer and pf/stop_notice over the same stdio control handle
// (memo item (b); grill ruling A). There is no priority mechanism between
// the two delivery paths and that absence IS the contract:
//
//   - pf/answer is delivered by monitor.deliverVia through
//     handle.SendControl with its own answer-scale ack window
//     (10 minutes);
//   - pf/stop_notice is written by daemon/shutdown.go through the SAME
//     handle.SendControl with its own 30-second deadline;
//   - both are delivered serially in arrival order, each waits for its
//     ack inside its OWN window, and neither preempts, cancels, delays
//     or settles the other: the two deliveries remain in flight
//     simultaneously.
//
// The tests drive both writes through the observe package's fake handle
// (the daemon's shutdown lane writes the notice exactly this way), so
// this file deliberately adds NO new mechanism — it fixes the current
// semantics in place.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// The two production-shaped ack bounds (04-local-runtime.md: pf/answer's
// ack window is answer-scale — 10 minutes, the same scale as the hang
// window; pf/stop_notice answers from boundary bookkeeping — 30
// seconds).
const (
	answerAckWindowProd = 10 * time.Minute
	stopNoticeAckBound  = 30 * time.Second
)

// allSends snapshots every control message the handle has taken, in
// arrival order (each SendControl entry is one serialized append).
func allSends(h *fakeHandle) []protocol.ControlMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]protocol.ControlMsg, len(h.sends))
	copy(out, h.sends)
	return out
}

// TestStopNoticeDoesNotWaitForPendingAnswer: a pf/answer awaiting its ack
// inside its 10-minute window neither delays, preempts nor cancels the
// daemon's pf/stop_notice write — the notice goes out over the same
// handle immediately, inside its own 30-second deadline, both deliveries
// stay in flight simultaneously, and the answer afterwards settles by its
// OWN window's semantics (delivered, consumption pending).
func TestStopNoticeDoesNotWaitForPendingAnswer(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.AnswerAckWindow = answerAckWindowProd
	})
	armQuestion(t, m, "q1")

	// The agent takes the pf/answer but has not acked yet: the delivery
	// sits inside its 10-minute window. The fake records the ctx bound
	// the monitor gave the send and holds it open until released.
	var (
		mu             sync.Mutex
		answerCtx      context.Context
		answerDeadline time.Time
		answerSendAt   time.Time
	)
	release := make(chan struct{})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		if msg.Method == protocol.ControlAnswer {
			dl, _ := ctx.Deadline()
			mu.Lock()
			answerCtx, answerDeadline, answerSendAt = ctx, dl, time.Now()
			mu.Unlock()
			<-release // ack not arrived yet: legitimately pending inside the window
			return protocol.ControlAck{}, ErrAnswerAckPending
		}
		return protocol.ControlAck{Method: msg.Method, Seq: msg.Seq, OK: true}, nil
	})
	answerDone := make(chan error, 1)
	go func() { answerDone <- submitAnswer(m, relayFor("q1", "a1")) }()
	soon(t, 2*time.Second, "pf/answer never reached the control channel", func() bool {
		s := allSends(h)
		return len(s) == 1 && s[0].Method == protocol.ControlAnswer
	})

	// The daemon's shutdown write: pf/stop_notice over the SAME handle
	// with its own 30-second bound (daemon/shutdown.go's shape).
	noticeStart := time.Now()
	nctx, ncancel := context.WithTimeout(context.Background(), stopNoticeAckBound)
	defer ncancel()
	nack, nerr := h.SendControl(nctx, protocol.ControlMsg{Method: protocol.ControlStopNotice})
	// No delay, no preemption: the pending answer did not hold the
	// notice back — the write settles immediately, far inside its own
	// 30-second deadline.
	if nerr != nil || !nack.OK {
		t.Fatalf("stop_notice delivery blocked by the pending answer: ack=%+v err=%v", nack, nerr)
	}
	if el := time.Since(noticeStart); el >= stopNoticeAckBound {
		t.Fatalf("stop_notice did not settle inside its own 30s deadline: %v", el)
	}
	// Serial arrival order on the one control channel: the answer
	// (first to arrive), then the notice.
	sends := allSends(h)
	if len(sends) != 2 ||
		sends[0].Method != protocol.ControlAnswer ||
		sends[1].Method != protocol.ControlStopNotice {
		var got []string
		for _, s := range sends {
			got = append(got, s.Method)
		}
		t.Fatalf("arrival order must be [pf/answer, pf/stop_notice], got %v", got)
	}

	// Independent ack windows, asserted separately: the answer carries
	// its own ~10-minute bound, the notice its own 30-second one —
	// neither derived from the other.
	mu.Lock()
	remAnswer := answerDeadline.Sub(answerSendAt)
	actx := answerCtx
	mu.Unlock()
	if remAnswer < 9*time.Minute || remAnswer > answerAckWindowProd {
		t.Fatalf("pf/answer must carry its own 10-minute ack window, got %v", remAnswer)
	}
	nDeadline, _ := nctx.Deadline()
	if remNotice := time.Until(nDeadline); remNotice <= 0 || remNotice > stopNoticeAckBound {
		t.Fatalf("pf/stop_notice must carry its own 30-second deadline, got %v", remNotice)
	}

	// No cancellation, both still in flight: the notice's write neither
	// cancelled the answer's context nor settled its submission.
	if actx.Err() != nil {
		t.Fatalf("the stop_notice must not cancel the pending pf/answer: %v", actx.Err())
	}
	select {
	case err := <-answerDone:
		t.Fatalf("the pending pf/answer must stay in flight after the stop_notice: settled %v", err)
	default:
	}

	// Released: the answer settles by its OWN window's semantics — the
	// two-phase pending outcome (delivered, consumption not observed) —
	// never by the notice's.
	close(release)
	select {
	case err := <-answerDone:
		if !errors.Is(err, ErrAnswerAckPending) {
			t.Fatalf("the answer must settle as ack-pending by its own window's semantics: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pf/answer never settled after its release")
	}
}

// TestAnswerDoesNotWaitForPendingStopNotice: the mirror direction — a
// pf/stop_notice awaiting its ack inside its 30-second window neither
// delays, preempts nor cancels a pf/answer delivery: the answer goes out
// immediately and settles by its own semantics while the notice is still
// pending, both in flight simultaneously.
func TestAnswerDoesNotWaitForPendingStopNotice(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.AnswerAckWindow = answerAckWindowProd
	})
	armQuestion(t, m, "q1")

	// The daemon's shutdown write first: the agent holds the notice
	// un-acked inside its 30-second window.
	release := make(chan struct{})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		if msg.Method == protocol.ControlStopNotice {
			<-release // ack not arrived yet: pending inside the 30s window
		}
		return protocol.ControlAck{Method: msg.Method, Seq: msg.Seq, OK: true}, nil
	})
	noticeDone := make(chan struct{})
	go func() {
		nctx, ncancel := context.WithTimeout(context.Background(), stopNoticeAckBound)
		defer ncancel()
		if _, err := h.SendControl(nctx, protocol.ControlMsg{Method: protocol.ControlStopNotice}); err != nil {
			t.Errorf("stop_notice delivery: %v", err)
		}
		close(noticeDone)
	}()
	soon(t, 2*time.Second, "pf/stop_notice never reached the control channel", func() bool {
		s := allSends(h)
		return len(s) == 1 && s[0].Method == protocol.ControlStopNotice
	})

	// The answer delivery, submitted while the notice is pending: it
	// must go out at once (serially, after the notice) and settle by its
	// own semantics — NOT wait out the notice's 30-second window.
	start := time.Now()
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("pf/answer delivery must not wait for the pending stop_notice: %v", err)
	}
	if el := time.Since(start); el >= stopNoticeAckBound {
		t.Fatalf("the answer must settle well inside its own path, not after the notice's window: %v", el)
	}
	// Serial arrival order: notice (first to arrive), then answer.
	sends := allSends(h)
	if len(sends) != 2 ||
		sends[0].Method != protocol.ControlStopNotice ||
		sends[1].Method != protocol.ControlAnswer {
		var got []string
		for _, s := range sends {
			got = append(got, s.Method)
		}
		t.Fatalf("arrival order must be [pf/stop_notice, pf/answer], got %v", got)
	}
	// No preemption, both in flight simultaneously: the answer's
	// settling did not settle the still-pending notice.
	select {
	case <-noticeDone:
		t.Fatal("the pending pf/stop_notice must stay in flight after the pf/answer settled")
	default:
	}

	// The notice then settles inside its own window, untouched.
	close(release)
	select {
	case <-noticeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the pf/stop_notice never settled after its release")
	}
}
