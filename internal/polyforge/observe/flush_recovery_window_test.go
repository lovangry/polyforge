package observe

// flush_recovery_window_test.go: the F9 semantic boundary of the flush
// attempt-id fence across the recovery-adoption window (memo item (a)).
//
// The hub announces a successor instance (RTRecovered emission-time
// registration — NoteInstance) BEFORE the monitor adopts it. In that
// window a flush carrying the announced (for the monitor: mismatched)
// attempt_id is a legitimate pre-adoption instance: it must be QUEUED,
// never refused with the STALE_ATTEMPT rejection. The complements are
// pinned with it:
//
//   - at adoption under a SUPERSEDING id, the queued deferred flush is
//     released and, still mismatched, answered with the stale refusal
//     (the DAEMON-409 STALE_ATTEMPT shape — "STALE_ATTEMPT: flush
//     carries attempt_id X; the current instance is Y");
//   - a second flush arriving within the window AFTER adoption, with a
//     mismatched attempt_id, is refused DAEMON-409 STALE_ATTEMPT at the
//     fence, while a matching one is accepted.
//
// Two levels: the monitor's own queue/release contract (fake handle,
// deterministic via the flushCh FIFO), and the HTTP face's fence
// grading end-to-end.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// TestFlushRecoveryWindowQueuesMismatchedThenRefusesAtAdoption pins the
// monitor's recovery-window contract on the fake handle:
//
//  1. a flush naming a not-yet-adopted instance is parked in the
//     adoption window (no answer reaches its sender — in particular no
//     STALE_ATTEMPT refusal);
//  2. when adoption lands under a SUPERSEDING id, the parked call is
//     released and, still mismatched, answered with the STALE_ATTEMPT
//     refusal shape;
//  3. the refusal sealed nothing: no pending question was minted from
//     the refused call.
func TestFlushRecoveryWindowQueuesMismatchedThenRefusesAtAdoption(t *testing.T) {
	h1 := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h1, nil)

	// (1) pre-adoption: the flush names an instance the monitor has not
	// adopted (the hub's latest known announcement). It must be queued.
	queued := FlushCall{
		Req: protocol.FlushRequest{
			WorkItem: "wi_test", AttemptID: "att_2",
			Reason: protocol.FlushNeedsInput, QuestionID: "q_win", Question: "which?", Step: "code",
		},
		Resp: make(chan protocol.FlushAck, 1),
	}
	m.flushCh <- queued
	// Sentinel on the SAME channel: flushCh is FIFO, so once the
	// sentinel (carrying the CURRENT id) settles, the queued call has
	// already been processed — and parked, since its id is ahead.
	sentinel := FlushCall{
		Req: protocol.FlushRequest{
			WorkItem: "wi_test", AttemptID: "att_1",
			Reason: protocol.FlushNeedsInput, QuestionID: "q_fence", Question: "fence?", Step: "code",
		},
		Resp: make(chan protocol.FlushAck, 1),
	}
	m.flushCh <- sentinel
	select {
	case ack := <-sentinel.Resp:
		if !ack.OK {
			t.Fatalf("sentinel (current id) must take the ordinary path: %+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sentinel flush never settled")
	}
	// Queued, not refused: no answer — in particular no STALE_ATTEMPT
	// error — was surfaced to the parked call's sender.
	select {
	case ack := <-queued.Resp:
		t.Fatalf("pre-adoption mismatched flush must be queued, not answered: %+v", ack)
	case <-time.After(150 * time.Millisecond):
	}

	// (2) adoption lands under a SUPERSEDING id (a second, newer
	// announcement won the race): the parked flush's id can never become
	// current, so the release answers the stale refusal.
	h3 := newFakeHandle("att_3")
	recoverMonitor(m, h3, "att_3")
	var ack protocol.FlushAck
	select {
	case ack = <-queued.Resp:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred flush was never released at adoption")
	}
	if ack.OK {
		t.Fatalf("a still-mismatched deferred flush must be refused, not accepted: %+v", ack)
	}
	if !bytes.Contains([]byte(ack.Error), []byte("STALE_ATTEMPT")) {
		t.Fatalf("the refusal must carry the STALE_ATTEMPT shape: %q", ack.Error)
	}
	if !bytes.Contains([]byte(ack.Error), []byte("att_2")) {
		t.Fatalf("the refusal must name the flush's own attempt id: %q", ack.Error)
	}

	// (3) the refusal sealed nothing: the only pending question is the
	// sentinel's, and no needs_input event carries the refused id.
	m.stateMu.Lock()
	pending := m.pending
	m.stateMu.Unlock()
	if pending == nil || pending.ID != "q_fence" {
		t.Fatalf("the refused flush must not mint a pending; want q_fence only: %+v", pending)
	}
	for _, e := range eventsOf(m, EvNeedsInput) {
		if bytes.Contains(e.Body, []byte("q_win")) {
			t.Fatalf("the refused flush must not record its question: %+v", e)
		}
	}
}

// TestHTTPFlushRecoveryWindowMismatchedRefused walks the whole episode
// through the UDS fence (the DAEMON-409 grading face):
//
//  1. inside the pre-adoption window, a flush carrying the announced id
//     passes the fence and is QUEUED — the sender sees no instant
//     STALE_ATTEMPT 409;
//  2. adoption under a superseding id releases the queued flush and the
//     sender receives the stale refusal in its ack (the flush contract:
//     the ack — even an OK=false one — is the response, never an HTTP
//     error);
//  3. after adoption, a second flush within the window with a
//     mismatched id is refused DAEMON-409 STALE_ATTEMPT at the fence,
//     while the matching id is accepted.
func TestHTTPFlushRecoveryWindowMismatchedRefused(t *testing.T) {
	h := newFakeHandle("att_cur")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// The successor instance is ANNOUNCED (emission-time registration,
	// as the daemon bridge does at RTRecovered emission) but not yet
	// adopted by the monitor.
	hs.o.NoteInstance("wi_http", "att_next")

	// (1) the window flush: it names the announced id, so the fence
	// must not refuse it — the monitor queues it. Its HTTP caller stays
	// open until the call settles.
	type result struct {
		code int
		body []byte
	}
	windowRes := make(chan result, 1)
	go func() {
		code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
			WorkItem: "wi_http", AttemptID: "att_next",
			Reason: protocol.FlushNeedsInput, QuestionID: "q_win", Question: "which?",
		}, nil)
		windowRes <- result{code, body}
	}()
	time.Sleep(100 * time.Millisecond) // let the window flush reach the monitor
	// Sentinel through the same fence + flushCh FIFO: when it settles,
	// the window flush has been processed — and parked.
	code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_cur",
		Reason: protocol.FlushNeedsInput, QuestionID: "q_fence", Question: "fence?",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("sentinel (current id) flush: %d %s", code, body)
	}
	select {
	case r := <-windowRes:
		t.Fatalf("pre-adoption window flush answered too early — it must be queued, not refused: %d %s", r.code, r.body)
	default:
	}

	// (2) adoption lands under a SUPERSEDING id: the parked call is
	// released still-mismatched and its sender receives the stale
	// refusal (in the ack, per the flush contract).
	h2 := newFakeHandle("att_next2")
	hs.rt <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_http", AttemptID: "att_next2"},
		Handle: h2,
		WI: WIInfo{
			WorkItem: "wi_http", Project: "proj", CapsuleDir: hs.capsuleDir,
			BundleName: "bundle-x",
		},
	}
	select {
	case r := <-windowRes:
		if r.code != http.StatusOK {
			t.Fatalf("the released flush's answer is its ack (never an HTTP error): %d %s", r.code, r.body)
		}
		var ack protocol.FlushAck
		if err := json.Unmarshal(r.body, &ack); err != nil {
			t.Fatalf("released-flush ack decode: %v (%s)", err, r.body)
		}
		if ack.OK {
			t.Fatalf("a still-mismatched deferred flush must be refused: %+v (%s)", ack, r.body)
		}
		if !bytes.Contains(r.body, []byte("STALE_ATTEMPT")) {
			t.Fatalf("the refusal must carry the STALE_ATTEMPT shape: %s", r.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred flush was never released at adoption")
	}

	// Adoption observable: the view now carries the adopted instance id.
	soon(t, 2*time.Second, "adoption never landed", func() bool {
		_, body := hs.get(t, "/jobs/wi_http/observe")
		var view protocol.JobView
		return json.Unmarshal(body, &view) == nil && view.AttemptID == "att_next2"
	})

	// (3) a SECOND flush within the window AFTER adoption: every
	// mismatched id — the superseded announcement and the
	// pre-recovery current id included — is refused DAEMON-409
	// STALE_ATTEMPT and never reaches the monitor.
	for _, stale := range []string{"att_next", "att_cur"} {
		code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
			WorkItem: "wi_http", AttemptID: stale,
			Reason: protocol.FlushNeedsInput, QuestionID: "q_z_" + stale, Question: "which?",
		}, nil)
		if code != http.StatusConflict || !bytes.Contains(body, []byte("STALE_ATTEMPT")) {
			t.Fatalf("post-adoption mismatched id %q: %d %s, want 409 STALE_ATTEMPT", stale, code, body)
		}
	}
	// The refused flushes minted nothing; the MATCHING id is accepted.
	_, body = hs.get(t, "/jobs/wi_http/observe")
	var view protocol.JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("view decode: %v (%s)", err, body)
	}
	if view.Pending != nil && view.Pending.ID == "q_win" {
		t.Fatalf("the refused window flush must not mint a pending: %+v", view.Pending)
	}
	code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_next2",
		Reason: protocol.FlushNeedsInput, QuestionID: "q_live", Question: "which?",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("post-adoption matching flush: %d %s", code, body)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(body, &ack); err != nil || !ack.OK {
		t.Fatalf("post-adoption matching flush ack: %v (%s)", err, body)
	}
}
