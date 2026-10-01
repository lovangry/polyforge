package observe

// session_test.go: the v1.5 M4 revalidation semantics — the periodic
// session-verify the monitor tick piggybacks.
//
//   - matches == false → OBSuperseded reaches the runtime channel, an
//     event records the new holder's fingerprint, and the monitor
//     stops supervising (detection suppressed, flushes refused with
//     the STALE_SPAWN shape) while making NO hub transition;
//   - matches == true → nothing happens (still the holder);
//   - transport error → NO action (never act on a network error), one
//     event, and the retry is bounded.
//   - a config without a verifier (the default until the merge wires
//     the shared client) simply skips the whole mechanism.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// fakeVerifier is the SessionVerifier test double.
type fakeVerifier struct {
	calls int
	ans   func(wi, sid string) (*api.SessionVerifyResponse, error)
}

func (f *fakeVerifier) SessionVerify(wi, sid string) (*api.SessionVerifyResponse, error) {
	f.calls++
	return f.ans(wi, sid)
}

// newSessionTestMonitor builds a monitor whose attachment carried a
// session id and whose cfg binds the given verifier + shrunk cadence.
func newSessionTestMonitor(t *testing.T, sid string, verifier SessionVerifier, cadence time.Duration) (*Monitor, chan protocol.OBMsg) {
	t.Helper()
	h := newFakeHandle("att_s")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.revalidate = cadence
		c.verify = verifier
		c.HangWindow = time.Hour // keep the hang probe out of the way
	})
	// The session id rides the RTStarted/RTRecovered message in
	// production; re-attach through rtCh so the monitor goroutine
	// adopts it under the same lock discipline as a real recovery.
	m.rtCh <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_test", SpawnN: 1, SessionID: sid},
		Handle: h,
		WI:     m.wi,
	}
	soon(t, 2*time.Second, "session attach never adopted", func() bool {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		return m.sessionID == sid
	})
	return m, ob
}

// TestRevalidateMismatchSupersedes: matches=false → OBSuperseded is
// emitted (carrying the new holder's fingerprint as the m4 audit),
// one session_superseded event lands, and the monitor refuses further
// flushes with the stale shape — superseded, not terminal: no
// OBCompleted and no hub transition (the fake verifier's call log is
// the only server contact).
func TestRevalidateMismatchSupersedes(t *testing.T) {
	verifier := &fakeVerifier{ans: func(wi, sid string) (*api.SessionVerifyResponse, error) {
		return &api.SessionVerifyResponse{WorkItemID: wi, Matches: false, Status: "running", CurrentFingerprint: "ab12cd34ef56aa00"}, nil
	}}
	m, ob := newSessionTestMonitor(t, "sess_secret_1", verifier, 20*time.Millisecond)

	msg := waitOB(t, ob, "OBSuperseded")
	if msg.Kind != protocol.OBSuperseded {
		t.Fatalf("OB message: %+v", msg)
	}
	if msg.WorkItem != "wi_test" || msg.SpawnN != 1 {
		t.Fatalf("OBSuperseded identity: %+v", msg)
	}
	if msg.Detail == "" || !contains(msg.Detail, "ab12cd34ef56aa00") {
		t.Fatalf("OBSuperseded detail must carry the current holder fingerprint: %+v", msg)
	}

	// The event face records the supersession with the PUBLIC
	// fingerprint — never the secret.
	evs := eventsOf(m, EvSessionSuperseded)
	if len(evs) != 1 {
		t.Fatalf("session_superseded events: %d, want 1", len(evs))
	}
	if !contains(string(evs[0].Body), "ab12cd34ef56aa00") {
		t.Fatalf("event must carry the current fingerprint: %s", evs[0].Body)
	}
	if contains(string(evs[0].Body), "sess_secret_1") {
		t.Fatalf("event must NEVER carry the session secret: %s", evs[0].Body)
	}

	// Superseded, not terminal: no OBCompleted, and a later flush is
	// answered with the STALE_SPAWN refusal instead of sealing.
	expectNoOB(t, ob, 150*time.Millisecond, "OBCompleted after supersession")
	ack := doFlush(m, FlushCall{
		Req:  protocol.FlushRequest{WorkItem: "wi_test", SpawnN: 1, Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped},
		Resp: make(chan protocol.FlushAck, 1),
	})
	if ack.OK || !contains(ack.Error, "STALE_SPAWN") {
		t.Fatalf("a superseded session's flush must be refused with the stale shape: %+v", ack)
	}
	// Exactly one verify call per revalidation slot — the superseded
	// monitor stops revalidating.
	time.Sleep(120 * time.Millisecond)
	if verifier.calls != 1 {
		t.Fatalf("verify calls after supersession: %d, want 1", verifier.calls)
	}
}

// TestRevalidateMatchIsQuiet: matches=true → no OB message, no event,
// and the cadence keeps ticking (verify calls accumulate).
func TestRevalidateMatchIsQuiet(t *testing.T) {
	verifier := &fakeVerifier{ans: func(wi, sid string) (*api.SessionVerifyResponse, error) {
		return &api.SessionVerifyResponse{WorkItemID: wi, Matches: true, Status: "running"}, nil
	}}
	m, ob := newSessionTestMonitor(t, "sess_secret_2", verifier, 20*time.Millisecond)

	expectNoOB(t, ob, 150*time.Millisecond, "any OB message on a matching verify")
	if n := len(eventsOf(m, EvSessionSuperseded)); n != 0 {
		t.Fatalf("no session_superseded event may land on a match: %d", n)
	}
	if verifier.calls == 0 {
		t.Fatal("the revalidation never ran")
	}
	// The live derivation's flushes keep working.
	ack := doFlush(m, FlushCall{
		Req:  protocol.FlushRequest{WorkItem: "wi_test", SpawnN: 1, Reason: protocol.FlushNeedsInput, QuestionID: "q_m", Question: "ok?"},
		Resp: make(chan protocol.FlushAck, 1),
	})
	if !ack.OK {
		t.Fatalf("live flush after a matching verify: %+v", ack)
	}
}

// TestRevalidateTransportErrorNeverActs: a verify that fails on the
// network is LOG-ONLY — no OB message, no supersession — and the
// monitor retries on a later tick (bounded by the error backoff, never
// the raw poll cadence).
func TestRevalidateTransportErrorNeverActs(t *testing.T) {
	verifier := &fakeVerifier{ans: func(wi, sid string) (*api.SessionVerifyResponse, error) {
		return nil, errors.New("connection refused")
	}}
	m, ob := newSessionTestMonitor(t, "sess_secret_3", verifier, 30*time.Millisecond)

	expectNoOB(t, ob, 150*time.Millisecond, "any OB message on a transport error")
	if n := len(eventsOf(m, EvSessionSuperseded)); n != 0 {
		t.Fatalf("a transport error must never supersede: %d events", n)
	}
	// The failure itself is visible (one event per attempt).
	if n := len(eventsOf(m, EvSessionVerifyFailed)); n == 0 {
		t.Fatal("the transport failure must be logged as session_verify_failed")
	}
	// The retry fires (calls grow past 1 within the backoff bound —
	// the shrunk cadence caps the error backoff at the cadence).
	soon(t, 2*time.Second, "the verify was never retried", func() bool {
		return verifier.calls >= 2
	})
	// The monitor still behaves normally: the live derivation's flush
	// keeps its ordinary path.
	ack := doFlush(m, FlushCall{
		Req:  protocol.FlushRequest{WorkItem: "wi_test", SpawnN: 1, Reason: protocol.FlushNeedsInput, QuestionID: "q_t", Question: "ok?"},
		Resp: make(chan protocol.FlushAck, 1),
	})
	if !ack.OK {
		t.Fatalf("live flush after transport errors: %+v", ack)
	}
}

// TestRevalidateDisabledWithoutVerifier: no verifier bound (the
// default until the merge wires the shared aihub client) → the tick
// simply skips the whole mechanism, session id or not.
func TestRevalidateDisabledWithoutVerifier(t *testing.T) {
	m, ob := newSessionTestMonitor(t, "sess_secret_4", nil, 20*time.Millisecond)
	_ = m
	expectNoOB(t, ob, 150*time.Millisecond, "any OB message without a verifier")
}

// TestRevalidateSkipsWithoutSessionID: a legacy attachment that carried
// no session id has nothing to verify — the verifier is never called.
func TestRevalidateSkipsWithoutSessionID(t *testing.T) {
	verifier := &fakeVerifier{ans: func(wi, sid string) (*api.SessionVerifyResponse, error) {
		return &api.SessionVerifyResponse{WorkItemID: wi, Matches: false}, nil
	}}
	// A monitor whose attachment carried no SessionID (legacy message).
	h := newFakeHandle("att_ns")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.revalidate = 20 * time.Millisecond
		c.verify = verifier
		c.HangWindow = time.Hour
	})
	_ = m
	expectNoOB(t, ob, 150*time.Millisecond, "OBSuperseded for a session-less attachment")
	if verifier.calls != 0 {
		t.Fatalf("a session-less attachment must not be verified: %d calls", verifier.calls)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
