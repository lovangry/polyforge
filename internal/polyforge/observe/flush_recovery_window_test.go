package observe

// flush_recovery_window_test.go: the semantic boundary of the flush
// spawn fence (v1.5 B2) across the recovery-adoption window.
//
// The hub announces a successor derivation (RTRecovered emission-time
// registration — NoteSpawn) BEFORE the monitor adopts it. In that
// window a flush carrying the announced (for the monitor: mismatched)
// spawn_n is a legitimate pre-adoption derivation: it must be QUEUED,
// never refused with the STALE_SPAWN rejection. The complements are
// pinned with it:
//
//   - at adoption under a SUPERSEDING counter, the queued deferred
//     flush is released and, still mismatched, answered with the stale
//     refusal (the STALE_SPAWN shape — "STALE_SPAWN: flush carries
//     spawn_n X; the current derivation is Y");
//   - a second flush arriving within the window AFTER adoption, with a
//     mismatched counter, is refused 409 STALE_SPAWN at the fence,
//     while a matching one is accepted.
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
//  1. a flush naming a not-yet-adopted derivation counter is parked in
//     the adoption window (no answer reaches its sender — in
//     particular no STALE_SPAWN refusal);
//  2. when adoption lands under a SUPERSEDING counter, the parked call
//     is released and, still mismatched, answered with the STALE_SPAWN
//     refusal shape;
//  3. the refusal sealed nothing: no pending question was minted from
//     the refused call.
func TestFlushRecoveryWindowQueuesMismatchedThenRefusesAtAdoption(t *testing.T) {
	h1 := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h1, nil)

	// (1) pre-adoption: the flush names a derivation counter the monitor
	// has not adopted (the hub's latest known announcement). It must be
	// queued.
	queued := FlushCall{
		Req: protocol.FlushRequest{
			WorkItem: "wi_test", SpawnN: 2,
			Reason: protocol.FlushNeedsInput, QuestionID: "q_win", Question: "which?", Step: "code",
		},
		Resp: make(chan protocol.FlushAck, 1),
	}
	m.flushCh <- queued
	// Sentinel on the SAME channel: flushCh is FIFO, so once the
	// sentinel (carrying the CURRENT counter) settles, the queued call
	// has already been processed — and parked, since its counter is
	// ahead.
	sentinel := FlushCall{
		Req: protocol.FlushRequest{
			WorkItem: "wi_test", SpawnN: 1,
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
	// Queued, not refused: no answer — in particular no STALE_SPAWN
	// error — was surfaced to the parked call's sender.
	select {
	case ack := <-queued.Resp:
		t.Fatalf("pre-adoption mismatched flush must be queued, not answered: %+v", ack)
	case <-time.After(150 * time.Millisecond):
	}

	// (2) adoption lands under a SUPERSEDING counter (a second, newer
	// announcement won the race): the parked flush's counter can never
	// become current, so the release answers the stale refusal.
	h3 := newFakeHandle("att_3")
	recoverMonitor(m, h3, 3)
	var ack protocol.FlushAck
	select {
	case ack = <-queued.Resp:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred flush was never released at adoption")
	}
	if ack.OK {
		t.Fatalf("a still-mismatched deferred flush must be refused, not accepted: %+v", ack)
	}
	if !bytes.Contains([]byte(ack.Error), []byte("STALE_SPAWN")) {
		t.Fatalf("the refusal must carry the STALE_SPAWN shape: %q", ack.Error)
	}
	if !bytes.Contains([]byte(ack.Error), []byte("spawn_n 2")) {
		t.Fatalf("the refusal must name the flush's own counter: %q", ack.Error)
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
//  1. inside the pre-adoption window, a flush carrying the announced
//     counter passes the fence and is QUEUED — the sender sees no
//     instant STALE_SPAWN 409;
//  2. adoption under a superseding counter releases the queued flush
//     and the sender receives the stale refusal in its ack (the flush
//     contract: the ack — even an OK=false one — is the response, never
//     an HTTP error);
//  3. after adoption, a second flush within the window with a
//     mismatched counter is refused DAEMON-409 STALE_SPAWN at the
//     fence, while the matching counter is accepted.
func TestHTTPFlushRecoveryWindowMismatchedRefused(t *testing.T) {
	h := newFakeHandle("att_cur")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// The successor derivation is ANNOUNCED (emission-time registration,
	// as the daemon bridge does at RTRecovered emission) but not yet
	// adopted by the monitor.
	hs.o.NoteSpawn("wi_http", 2)

	// (1) the window flush: it names the announced counter, so the fence
	// must not refuse it — the monitor queues it. Its HTTP caller stays
	// open until the call settles.
	type result struct {
		code int
		body []byte
	}
	windowRes := make(chan result, 1)
	go func() {
		code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
			WorkItem: "wi_http", SpawnN: 2,
			Reason: protocol.FlushNeedsInput, QuestionID: "q_win", Question: "which?",
		}, nil)
		windowRes <- result{code, body}
	}()
	time.Sleep(100 * time.Millisecond) // let the window flush reach the monitor
	select {
	case r := <-windowRes:
		t.Fatalf("pre-adoption window flush answered too early — it must be queued, not refused: %d %s", r.code, r.body)
	default:
	}
	// Fence cross-check (the s10 hole, now pinned here too): once a
	// successor is ANNOUNCED, the monitor's still-lagging current
	// counter is ALREADY stale — a flush carrying it is refused at the
	// fence, never reaching the monitor. (The runtime superseded that
	// agent before the announcement was emitted; only a zombie speaks
	// in its name.)
	code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", SpawnN: 1,
		Reason: protocol.FlushNeedsInput, QuestionID: "q_fence", Question: "fence?",
	}, nil)
	if code != http.StatusConflict || !bytes.Contains(body, []byte("STALE_SPAWN")) {
		t.Fatalf("zombie (lagging-current) flush must be refused at the fence: %d %s", code, body)
	}

	// (2) adoption lands under a SUPERSEDING counter: the parked call is
	// released still-mismatched and its sender receives the stale
	// refusal (in the ack, per the flush contract).
	h2 := newFakeHandle("att_next2")
	hs.rt <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_http", SpawnN: 3},
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
		if !bytes.Contains(r.body, []byte("STALE_SPAWN")) {
			t.Fatalf("the refusal must carry the STALE_SPAWN shape: %s", r.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred flush was never released at adoption")
	}

	// Adoption observable: the view now carries the adopted instance id.
	soon(t, 2*time.Second, "adoption never landed", func() bool {
		_, body := hs.get(t, "/jobs/wi_http/observe")
		var view protocol.JobView
		return json.Unmarshal(body, &view) == nil && view.SpawnN == 3
	})

	// (3) a SECOND flush within the window AFTER adoption: every
	// mismatched counter — the superseded announcement and the
	// pre-recovery current counter included — is refused DAEMON-409
	// STALE_SPAWN and never reaches the monitor.
	for _, stale := range []int{1, 2} {
		code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
			WorkItem: "wi_http", SpawnN: stale,
			Reason: protocol.FlushNeedsInput, QuestionID: "q_z", Question: "which?",
		}, nil)
		if code != http.StatusConflict || !bytes.Contains(body, []byte("STALE_SPAWN")) {
			t.Fatalf("post-adoption mismatched counter %d: %d %s, want 409 STALE_SPAWN", stale, code, body)
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
		WorkItem: "wi_http", SpawnN: 3,
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
