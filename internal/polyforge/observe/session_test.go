package observe

// session_test.go: the v1.5 M4 revalidation semantics — the periodic
// session-verify the monitor tick piggybacks.
//
//   - matches == false → OBSuperseded reaches the runtime channel, an
//     event records the new holder's fingerprint, and the monitor
//     stops supervising (detection suppressed) while making NO hub
//     transition. Since
//     Finding 1(b) (v1.5 review) the emission is RETRYABLE: while the
//     supersession latch is set the revalidation keeps running and
//     every unmatched verdict re-emits OBSuperseded (the event face
//     stays once), until the runtime's release (RTFinished, Finding 4)
//     stops the monitor loop;
//   - matches == true → nothing happens (still the holder);
//   - transport error → NO action (never act on a network error), one
//     event, and the retry is bounded.
//   - a config without a verifier (the default until the merge wires
//     the shared client) simply skips the whole mechanism.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// fakeVerifier is the SessionVerifier test double.
type fakeVerifier struct {
	calls atomic.Int64
	ans   func(wi, sid string) (*api.SessionVerifyResponse, error)
}

func (f *fakeVerifier) SessionVerify(wi, sid string) (*api.SessionVerifyResponse, error) {
	f.calls.Add(1)
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
// emitted (carrying the new holder's fingerprint as the m4 audit), the
// session_superseded VERDICT event lands exactly once, and the monitor
// stops supervising (detection suppressed) — superseded, not
// terminal: no OBCompleted and no hub transition (the fake verifier's
// call log is the only server contact).
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

	// Finding 1(b) (v1.5 review): the emission is RETRYABLE. While the
	// latch is set the revalidation KEEPS RUNNING and every unmatched
	// verdict re-emits OBSuperseded — the old contract fired exactly
	// once, so a delivery dropped on a full OB channel (or fenced out
	// at the runtime's spawn counter) was lost forever and the agent of
	// a WI held elsewhere ran unsupervised. The re-emissions keep
	// arriving until the runtime's release (RTFinished, Finding 4)
	// stops this loop — nothing releases this monitor, so they must
	// keep coming.
	for range 3 {
		remsg := waitOB(t, ob, "a re-emitted OBSuperseded")
		if remsg.Kind != protocol.OBSuperseded {
			t.Fatalf("only OBSuperseded re-emissions may arrive, got %+v", remsg)
		}
	}

	// The event face stays ONCE: the supersession VERDICT is latched
	// (the PUBLIC fingerprint — never the secret); only the OB emission
	// repeats.
	if evs := eventsOf(m, EvSessionSuperseded); len(evs) != 1 {
		t.Fatalf("session_superseded events: %d, want exactly 1 (the verdict latches once)", len(evs))
	} else {
		if !contains(string(evs[0].Body), "ab12cd34ef56aa00") {
			t.Fatalf("event must carry the current fingerprint: %s", evs[0].Body)
		}
		if contains(string(evs[0].Body), "sess_secret_1") {
			t.Fatalf("event must NEVER carry the session secret: %s", evs[0].Body)
		}
	}

	// The revalidation really is still running: the verify calls grew
	// past one (the old contract asserted exactly 1 here — the fire-once
	// shape the review's Finding 1 deleted).
	if verifier.calls.Load() < 2 {
		t.Fatalf("verify calls after supersession: %d, want >= 2 (the re-emission engine)", verifier.calls.Load())
	}

	// The superseded monitor's protective surface is the server-side
	// push fencing, not a local refusal.
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
	if verifier.calls.Load() == 0 {
		t.Fatal("the revalidation never ran")
	}
	// The live derivation keeps being observed: a pending question
	// persisted in execution.json is still projected.
	writePendingQuestion(t, m, "q_m", "ok?", "code")
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
		return verifier.calls.Load() >= 2
	})
	// The monitor still behaves normally: the execution.json poll keeps
	// projecting a pending question.
	writePendingQuestion(t, m, "q_t", "ok?", "code")
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
	if verifier.calls.Load() != 0 {
		t.Fatalf("a session-less attachment must not be verified: %d calls", verifier.calls.Load())
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
