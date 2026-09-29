package observe

// flush_test.go: the three flush(reason) semantics through the monitor —
// needs_input (idempotent by question id, immediate ack), wrap (ack
// distinguishes local commit from server push; 3 failures → blocked →
// WICompleted), shutdown (ack whatever happened, never hold the agent).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

func flushReq(reason string) FlushCall {
	return FlushCall{
		Req:  protocol.FlushRequest{WorkItem: "wi_test", AttemptID: "att_1", Reason: reason},
		Resp: make(chan protocol.FlushAck, 1),
	}
}

func doFlush(m *Monitor, call FlushCall) protocol.FlushAck {
	m.flushCh <- call
	select {
	case ack := <-call.Resp:
		return ack
	case <-time.After(3 * time.Second):
		return protocol.FlushAck{OK: false, Error: "test: flush never settled"}
	}
}

func TestFlushWrapAckSeparatesLocalCommitAndPush(t *testing.T) {
	h := newFakeHandle("att_1")

	// Case 1: commit landed AND reached the server.
	m1, ob1 := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			return "sha_full", true, nil
		}
	})
	ack := doFlush(m1, flushReq(protocol.FlushWrap))
	if !ack.OK || ack.LocalCommit != "sha_full" || !ack.Pushed {
		t.Fatalf("ack: %+v", ack)
	}
	msg := waitOB(t, ob1, "WICompleted(wrapped)")
	if msg.Kind != protocol.OBCompleted || msg.Reason != protocol.FinishWrapped {
		t.Fatalf("WICompleted: %+v", msg)
	}

	// Case 2: commit landed locally, push did not (no error — e.g.
	// nothing to commit / server deferral). The two fields are asserted
	// SEPARATELY: LocalCommit set, Pushed false.
	m2, ob2 := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			return "sha_local", false, nil
		}
	})
	ack2 := doFlush(m2, flushReq(protocol.FlushWrap))
	if !ack2.OK {
		t.Fatalf("a push-less success is still an ack success: %+v", ack2)
	}
	if ack2.LocalCommit != "sha_local" {
		t.Fatalf("local commit must be reported: %+v", ack2)
	}
	if ack2.Pushed {
		t.Fatalf("pushed must be false: %+v", ack2)
	}
	if msg := waitOB(t, ob2, "WICompleted"); msg.Kind != protocol.OBCompleted {
		t.Fatalf("WICompleted: %+v", msg)
	}
}

func TestFlushWrapRetriesThreeThenBlocks(t *testing.T) {
	h := newFakeHandle("att_1")
	attempts := 0
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			attempts++
			return "sha_partial", false, errors.New("GIT-500: upstream refused")
		}
	})
	ack := doFlush(m, flushReq(protocol.FlushWrap))
	if ack.OK {
		t.Fatalf("three failed seals must not ack OK: %+v", ack)
	}
	if ack.Pushed {
		t.Fatal("pushed must be false on failure")
	}
	if ack.LocalCommit != "sha_partial" {
		t.Fatalf("the last local commit is still reported: %+v", ack)
	}
	if attempts != 3 {
		t.Fatalf("seal attempts: got %d, want 3", attempts)
	}
	// The work item transitions to blocked and the Runtime hears
	// WICompleted(blocked).
	msg := waitOB(t, ob, "WICompleted(blocked)")
	if msg.Kind != protocol.OBCompleted || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("WICompleted: %+v", msg)
	}
	if len(eventsOf(m, EvWrapBlocked)) != 1 {
		t.Fatalf("wrap_blocked events: got %d, want 1", len(eventsOf(m, EvWrapBlocked)))
	}
	// And the monitor's own status says done (blocked is terminal here).
	if got := m.JobView().Status; got != protocol.JobDone {
		t.Fatalf("job status after blocked wrap: %s", got)
	}
}

func TestFlushWrapSecondCallWhileSealingRefused(t *testing.T) {
	h := newFakeHandle("att_1")
	release := make(chan struct{})
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			<-release
			return "sha_w", true, nil
		}
	})
	first := flushReq(protocol.FlushWrap)
	m.flushCh <- first
	time.Sleep(30 * time.Millisecond) // the first seal is now in flight
	second := flushReq(protocol.FlushWrap)
	ack := doFlush(m, second)
	if ack.OK || ack.Error == "" {
		t.Fatalf("a second wrap during sealing must be refused: %+v", ack)
	}
	close(release)
	if ack := <-first.Resp; !ack.OK {
		t.Fatalf("first seal must complete: %+v", ack)
	}
}

func TestFlushNeedsInputIdempotentByQuestionID(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)

	call := FlushCall{Req: protocol.FlushRequest{Reason: protocol.FlushNeedsInput, QuestionID: "q9", Question: "which?", Step: "code"}, Resp: make(chan protocol.FlushAck, 1)}
	if ack := doFlush(m, call); !ack.OK {
		t.Fatalf("first needs_input ack: %+v", ack)
	}
	// The SAME question id again: acked OK again, but only ONE
	// needs_input event exists (idempotent by question id).
	if ack := doFlush(m, call); !ack.OK {
		t.Fatalf("duplicate needs_input ack: %+v", ack)
	}
	if got := len(eventsOf(m, EvNeedsInput)); got != 1 {
		t.Fatalf("needs_input events: got %d, want 1", got)
	}
	// A malformed flush is the only refusal on this path.
	bad := FlushCall{Req: protocol.FlushRequest{Reason: protocol.FlushNeedsInput}, Resp: make(chan protocol.FlushAck, 1)}
	if ack := doFlush(m, bad); ack.OK {
		t.Fatalf("needs_input without question_id must be refused: %+v", ack)
	}
}

func TestFlushShutdownAcksFastAndReportsFailure(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			return "sha_s", true, nil
		}
	})
	start := time.Now()
	ack := doFlush(m, flushReq(protocol.FlushShutdown))
	if !ack.OK || !ack.Pushed || ack.LocalCommit != "sha_s" {
		t.Fatalf("shutdown ack: %+v", ack)
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown flush must settle immediately-ish")
	}

	m2, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			return "", false, errors.New("GIT-500: refused")
		}
	})
	ack2 := doFlush(m2, flushReq(protocol.FlushShutdown))
	// §五: a failed shutdown flush does not hold the agent — the failure
	// is reported in the ack and the data stays in the local commit.
	if ack2.OK || ack2.Error == "" {
		t.Fatalf("failed shutdown ack must carry the error: %+v", ack2)
	}
}
