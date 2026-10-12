package observe

// w124_fix_test.go: the fix-side pins for polyforge#124 (the #117
// trickle-wedge). Root-cause named three distinct holes and refuted the
// WI's original "probe-adjacent record class" hypothesis; these tests
// pin one observable per cause, at the repro's unit scale (1 incident
// minute := 50ms; the 8m wall := 400ms):
//
//   - Cause B (the wall's witness): the `response` command echo — an F15
//     progress record for the watchdogs — must NOT move the wall
//     backstop's own witness (WallProgressAt): a daemon-side echo buys
//     no wall window. Pinned at the stream level AND by repro test 1
//     (trickle_wedge_repro_test.go) end to end.
//   - Cause C (no lane bounds turn age): a turn that never closes is
//     bounded at 4x the wall window by the turn-duration lane
//     (TurnOpenedAt -> checkTurnDuration), firing with the
//     turn_duration reason, no probe involved; needs_input and a
//     disabled wall both opt out.
//   - Cause A (the #83 exemption's suspension): a dispatch-shaped
//     message_end as the turn's LAST record — the incident's exact
//     record shape (root-cause §2.1: the stdout message_end carrying
//     the long bash toolCall) — holds the lanes off for the exemption
//     window (4x the wall, not the 30-minute constant) and is then
//     bounded; the pinned #83 design itself stays green in
//     wallbackstop_test.go (TestWallBackstopExemptDuringToolDispatch).

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// TestW124WallWitnessIgnoresResponseEcho pins the wall's own witness:
// the `response` echo refreshes the watchdogs' progress clock (the F15
// ruling is unchanged for them) but never the wall's — one echo can no
// longer buy a full extra wall window (root-cause Cause B, experiment
// E1; repro test 1 is the monitor-level twin).
func TestW124WallWitnessIgnoresResponseEcho(t *testing.T) {
	s := newStreamState()
	s.note([]byte(fxTurnStart))
	s.note([]byte(fxMsgStart))
	s.note([]byte(fxTextDelta))
	before := s.heartbeat()

	s.note([]byte(fxResponse))
	hb := s.heartbeat()
	if hb.Records != before.Records+1 {
		t.Fatalf("the response echo stays an F15 progress record, got records %d -> %d", before.Records, hb.Records)
	}
	if !hb.LastProgressAt.After(before.LastProgressAt) {
		t.Fatal("the response echo must still move the watchdogs' progress clock (LastProgressAt, the F15 ruling)")
	}
	if !hb.WallProgressAt.Equal(before.WallProgressAt) {
		t.Fatal("the response echo must NOT move the wall's witness (WallProgressAt)")
	}
}

// TestW124TurnOpenedAtLifecycle pins the turn-duration lane's input:
// the clock arms on the FIRST opener (a nested turn_start must not
// re-arm it), survives the mid-turn closers until the depth returns to
// zero, clears then, and re-arms fresh on the next open.
func TestW124TurnOpenedAtLifecycle(t *testing.T) {
	s := newStreamState()
	s.note([]byte(fxAgentStart)) // first opener: depth 0 -> 1
	opened := s.heartbeat().TurnOpenedAt
	if opened.IsZero() {
		t.Fatal("agent_start must arm the turn clock")
	}
	s.note([]byte(fxTurnStart)) // nested opener: depth 1 -> 2, no re-arm
	if got := s.heartbeat().TurnOpenedAt; !got.Equal(opened) {
		t.Fatal("a nested turn_start must not re-arm the turn clock")
	}
	s.note([]byte(fxMsgEndStop)) // dispatch mid-turn: turn still open
	if got := s.heartbeat().TurnOpenedAt; !got.Equal(opened) {
		t.Fatal("a dispatch must not move the turn clock")
	}
	s.note([]byte(fxTurnEndStop)) // depth 2 -> 1: still open
	if s.heartbeat().TurnOpenedAt.IsZero() {
		t.Fatal("the turn is still open after one of two closers")
	}
	s.note([]byte(fxAgentEnd)) // depth 1 -> 0: closed
	if !s.heartbeat().TurnOpenedAt.IsZero() {
		t.Fatal("the turn clock must clear when no turn is in flight")
	}
	s.note([]byte(fxTurnStart)) // re-open: a fresh clock
	reopened := s.heartbeat().TurnOpenedAt
	if reopened.IsZero() || !reopened.After(opened) {
		t.Fatal("re-opening must arm a fresh turn clock")
	}
}

// TestW124TurnDurationLaneDirect pins the new lane end to end: the turn
// never closes, a progress-class record trickles every 2 incident
// minutes (inside the reported 5-11m cadence, under the 8m wall) — every
// silence witness stays fresh, so NO silence lane can arm and no probe
// may leave. The turn-duration lane alone bounds it, with the
// turn_duration reason, at 4x the wall window.
func TestW124TurnDurationLaneDirect(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = w124Wall
		c.WallBackstop = w124Wall
	})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		t.Errorf("no probe may be sent for a turn-age verdict, got %s", msg.Method)
		return protocol.ControlAck{}, nil
	})

	feed(h, fxTurnStart)
	feed(h, fxMsgStart)
	go func() {
		for i := 0; i < 40; i++ { // 2 incident minutes cadence, 4 windows' worth
			time.Sleep(2 * w124Minute)
			feed(h, fxTextDelta)
		}
	}()
	start := time.Now()

	select {
	case msg := <-ob:
		if msg.Kind != protocol.OBNeedsRecovery {
			t.Fatalf("expected WINeedsRecovery, got %s", msg.Kind)
		}
		if msg.Reason != protocol.RecoverTurnDuration {
			t.Fatalf("the trickling zombie must be bounded BY THE TURN-DURATION LANE, got reason=%s", msg.Reason)
		}
		if el := time.Since(start); el > 8*w124Wall {
			t.Fatalf("the turn lane fired late: %s (bound is 4 wall windows)", el)
		}
		t.Logf("turn-duration lane fired after %s (reason=%s)", time.Since(start).Round(time.Millisecond), msg.Reason)
		if h.sentCount() != 0 {
			t.Fatalf("turn-age is judged on the turn clock, not a probe: %d probes sent", h.sentCount())
		}
	case <-time.After(16 * w124Wall):
		t.Fatalf("the trickling zombie stayed unbounded: no needs_recovery in 16 wall windows.\n%s",
			w124Snapshot(m, h, start))
	}
}

// TestW124TurnDurationPendingExempt: needs_input is designed stillness
// for the turn-duration lane too — a parked question holds an open turn
// (the #99 stale-open shape) past the bound and nothing fires.
func TestW124TurnDurationPendingExempt(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = w124Wall
		c.WallBackstop = w124Wall
	})

	// The stale-open shape (#99): the turn opens and never closes.
	feed(h, fxTurnStart)
	feed(h, fxMsgStart)

	// Arm the pending question.
	writePendingQuestion(t, m, "q_w124_turn", "parked", "code")

	// 6 wall windows — past the 4x turn bound with slack: nothing fires,
	// no probe leaves.
	time.Sleep(6 * w124Wall)
	expectNoOB(t, ob, 100*time.Millisecond, "the turn-duration lane must honor the pending-question exemption")
	if h.sentCount() != 0 {
		t.Fatalf("no probe while a question is pending, got %d", h.sentCount())
	}
}

// TestW124TurnDurationDisabledWithWall: a negative wall is the explicit
// escape hatch from every absolute bound (TestWallBackstopDisabledChanges
// Nothing pins it for the wall itself) — the turn lane rides the same
// hatch and stays inert.
func TestW124TurnDurationDisabledWithWall(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = w124Wall
		c.WallBackstop = -1
	})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		t.Errorf("the disabled-wall shape must not probe, got %s", msg.Method)
		return protocol.ControlAck{}, nil
	})

	feed(h, fxTurnStart)
	feed(h, fxMsgStart)
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(2 * w124Minute)
			feed(h, fxTextDelta)
		}
	}()

	// 8 wall windows: past any 4x bound the lane would carry if it
	// mis-derived one from a disabled wall.
	time.Sleep(8 * w124Wall)
	expectNoOB(t, ob, 100*time.Millisecond, "a disabled wall must disable the turn-duration lane too")
	_ = m
}

// TestW124IncidentDispatchShapeBounded — root-cause experiment E2's
// replay, the incident-accurate record shape: turn_start, message_start,
// a DISPATCH-SHAPED message_end (the record that in production carried
// the long bash toolCall and armed the 30-minute exemption), then
// NOTHING. The #83 exemption must hold for the first windows (the
// incident's own agent was inside a legitimate 11m40s tool call — the
// fix bounds the shape, it must not fire inside it), and the shape must
// then be bounded at the 4x cap, not the 30-minute constant.
func TestW124IncidentDispatchShapeBounded(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = w124Wall
		c.WallBackstop = w124Wall
	})

	feed(h, fxTurnStart)
	feed(h, fxMsgStart)
	feed(h, fxMsgEndStop) // the dispatch: message_end with the turn open
	start := time.Now()

	// The #83 exemption holds for the first two wall windows: no probe,
	// no recovery — a legitimate long tool call survives the wall window
	// itself exactly as before (root-cause E2b/E2c contrast).
	time.Sleep(2 * w124Wall)
	if h.sentCount() != 0 {
		t.Fatalf("the #83 dispatch exemption must hold inside its window, got %d probes", h.sentCount())
	}
	expectNoOB(t, ob, 100*time.Millisecond, "the #83 dispatch exemption must hold inside its window")

	// Past the 4x cap the shape is bounded (the cap, the exemption
	// expiry, or the turn-duration lane — all 4x the wall window).
	select {
	case msg := <-ob:
		if msg.Kind != protocol.OBNeedsRecovery {
			t.Fatalf("expected WINeedsRecovery, got %s", msg.Kind)
		}
		if el := time.Since(start); el > 8*w124Wall {
			t.Fatalf("the dispatch shape was bounded late: %s (the cap is 4 wall windows)", el)
		}
		t.Logf("the incident dispatch shape was bounded after %s (reason=%s)",
			time.Since(start).Round(time.Millisecond), msg.Reason)
	case <-time.After(10 * w124Wall):
		t.Fatalf("the incident dispatch shape stayed unbounded: no needs_recovery in 10 wall windows.\n%s",
			w124Snapshot(m, h, start))
	}
}

// TestW124DispatchExemptionCappedByWall pins Cause A's cap directly: a
// TIGHT wall (100ms here) must shrink the #83 exemption with it — the
// dispatch holds the lanes off through ~3 wall windows (the pinned #83
// design's own scale) and the wall fires by the 4x cap, not after the
// 30-minute constant.
func TestW124DispatchExemptionCappedByWall(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = 10 * time.Minute // inert: the hang lane never arms here
		c.WallBackstop = 100 * time.Millisecond
	})

	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	start := time.Now()

	// ~3 wall windows in: the exemption (4x = 400ms) still holds.
	time.Sleep(280 * time.Millisecond)
	expectNoOB(t, ob, 20*time.Millisecond, "the capped exemption must still hold through ~3 wall windows")

	select {
	case msg := <-ob:
		if msg.Kind != protocol.OBNeedsRecovery {
			t.Fatalf("expected WINeedsRecovery, got %s", msg.Kind)
		}
		if el := time.Since(start); el > 700*time.Millisecond {
			t.Fatalf("the dispatch shape was bounded late: %s (the 4x cap is 400ms)", el)
		}
		t.Logf("the tight-wall dispatch shape was bounded after %s (reason=%s)",
			time.Since(start).Round(time.Millisecond), msg.Reason)
	case <-time.After(900 * time.Millisecond):
		t.Fatalf("a 100ms wall did not cap the #83 exemption: no needs_recovery in 900ms.\n%s",
			w124Snapshot(m, h, start))
	}
}
