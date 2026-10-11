package observe

// trickle_wedge_repro_test.go: repro for wi_b2cudgqhTAeB (polyforge#124)
// — the #117 trickle-wedge. Incident facts (2026-10-10, wi_LMSxPTQTtz5x
// code step): a turn stayed open ~80 minutes while progress-class
// records trickled every ~5-11 minutes; an 11.6-minute stretch with no
// progress records at all (15:58:54 → 16:10:30) ran inside a daemon
// started with --wall-window 8m and the wall backstop did NOT fire; the
// same shape recurred on the next spawn (gap ~15m). The idle-shape
// positive control fired exactly at 8m0s the same night, so the bound
// itself and the flag pass-through were live — the defect is specific
// to the InFlight (hang) shape with trickling records.
//
// Two pinned failures, one per reported defect, at unit scale
// (1 incident minute := 50ms; the 8m wall := 400ms):
//
//   - TestW124ProbeAdjacentRecordLaundersWallWitness is the WI's
//     defect-1 spec verbatim: stream InFlight, last progress at T, ONE
//     record injected at T+6m (probe-adjacent class; the pinned
//     `response` echo shape stands in for the production class, whose
//     identification is root-cause's deliverable), then nothing. The
//     wall must still fire within the original bound measured from T —
//     the injected record must not buy the zombie another full window.
//
//   - TestW124TrickleZombieTurnUnbounded pins the second reported
//     defect: a turn that never closes and trickles one progress-class
//     record every ~5m (under the wall window) keeps every witness
//     fresh, and today NO lane bounds it. The WI's acceptance: SOME
//     lane must fire needs_recovery within a bounded time.
//
// Both fail for the REPORTED reason on the pre-fix code. Which
// production record class laundered the witness is deliberately NOT
// asserted here.

import (
	"fmt"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// w124Minute is the incident-to-unit scale: one incident minute.
const w124Minute = 50 * time.Millisecond

// w124Wall is the incident's --wall-window 8m at unit scale (also the
// hang window: the incident daemon's probe rounds cycled on the same
// order of silence).
const w124Wall = 8 * w124Minute

// w124Snapshot is the repro's evidence snapshot: everything the
// supervision surface shows at the moment the bound expired.
func w124Snapshot(m *Monitor, h *fakeHandle, since time.Time) string {
	hb := m.stream.heartbeat()
	progressAge := "never"
	if !hb.LastProgressAt.IsZero() {
		progressAge = time.Since(hb.LastProgressAt).Round(time.Millisecond).String()
	}
	return fmt.Sprintf(
		"elapsed since last real progress: %s\n"+
			"heartbeat: in_flight=%v records=%d noise=%d last_progress_ago=%s tool_dispatch_set=%v\n"+
			"wall bound: %s (configured --wall-window 8m at unit scale)\n"+
			"liveness probes sent: %d\n"+
			"infra incidents: %v\n"+
			"needs_recovery events: %d",
		time.Since(since).Round(time.Millisecond),
		hb.InFlight, hb.Records, hb.Noise, progressAge, !hb.ToolDispatchAt.IsZero(),
		w124Wall,
		h.sentCount(),
		incidentKinds(m),
		len(eventsOf(m, EvNeedsRecovery)),
	)
}

// TestW124ProbeAdjacentRecordLaundersWallWitness — the 11.6-minute
// non-fire, unit-scaled. The turn is in flight; progress is anchored at
// T by the turn's own burst; at T+6m ONE record lands (probe-adjacent
// class — the incident measured a fresh record ~2m after each
// no_progress probe event), then the stream never speaks again. The
// wall bound is 8m from T: the injected record must NOT restart the
// wall's witness, and the recovery must fire within T+8m plus
// scheduling slack. Today the injected record refreshes
// StreamHeartbeat.LastProgressAt, the wall silence restarts, and no
// needs_recovery lands inside the bound.
func TestW124ProbeAdjacentRecordLaundersWallWitness(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = w124Wall
		c.WallBackstop = w124Wall
	})

	// The wedge: turn opens (InFlight) and its burst anchors progress at
	// T≈0; the model call behind it never returns, nothing closes the turn.
	feed(h, fxTurnStart)
	feed(h, fxMsgStart)
	feed(h, fxTextDelta)
	start := time.Now()

	// THE PINNED BOUND, anchored at T before the injection: recovery must
	// arrive within the ORIGINAL bound — T + 8m plus 2m of scheduling
	// slack for the ~5ms poll tick — no matter what lands in between.
	bound := time.NewTimer(10 * w124Minute)
	defer bound.Stop()

	// T+6m: the probe-adjacent record lands (the pinned response echo —
	// the harness echoes accepted commands as `response`, which the F15
	// whitelist counts as progress), then NOTHING, ever.
	time.Sleep(6 * w124Minute)
	feed(h, fxResponse)

	select {
	case msg := <-ob:
		if msg.Kind != protocol.OBNeedsRecovery {
			t.Fatalf("expected WINeedsRecovery inside the original bound, got %s", msg.Kind)
		}
		t.Logf("bounded after %s (reason=%s): the injected record did not extend the wall",
			time.Since(start).Round(time.Millisecond), msg.Reason)
		return
	case <-bound.C:
		t.Fatalf("the wall bound did not hold against the injected record: no needs_recovery within %s "+
			"of the last real progress (T+8m bound, +2m slack) — one record at T+6m restarted the wall's silence.\n"+
			"observed state at bound expiry:\n%s",
			10*w124Minute, w124Snapshot(m, h, start))
	}
}

// TestW124TrickleZombieTurnUnbounded — the trickle immunity hole: a
// turn that never closes and emits one progress-class record every ~5m
// (inside the incident's reported 5-11m cadence, under the 8m wall)
// keeps the progress witness fresh forever. The hang lane sees fresh
// progress, the idle lane sees a turn in flight, the wall sees a witness
// younger than its bound: today NOTHING ever fires, for as long as the
// turn trickles (the incident's turn ran ~80 minutes, ≈10 wall windows,
// until the operator killed it). Asserts the WI's acceptance at unit
// scale: SOME lane must fire needs_recovery within a bounded time —
// bounded here at 16 wall windows.
func TestW124TrickleZombieTurnUnbounded(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = w124Wall
		c.WallBackstop = w124Wall
		// IdleWindow stays at the inert default: the incident shape has a
		// turn in flight, so the idle lane never owned this silence.
	})

	// The zombie: turn opens and never closes; progress-class records
	// (message_update — whitelist, no tool side-state) trickle every 5m
	// for 24 records (~12 wall windows; the incident's ~80-minute open
	// turn was ~10 windows before the operator intervened).
	feed(h, fxTurnStart)
	feed(h, fxMsgStart)
	go func() {
		for i := 0; i < 24; i++ {
			time.Sleep(5 * w124Minute)
			feed(h, fxTextDelta)
		}
	}()
	start := time.Now()

	select {
	case msg := <-ob:
		if msg.Kind != protocol.OBNeedsRecovery {
			t.Fatalf("expected WINeedsRecovery to bound the trickling turn, got %s", msg.Kind)
		}
		t.Logf("the trickling zombie was bounded after %s (reason=%s)",
			time.Since(start).Round(time.Millisecond), msg.Reason)
		return
	case <-time.After(16 * w124Wall):
		t.Fatalf("no supervision lane bounded the trickling zombie turn: %s elapsed with the turn in flight "+
			"and a progress record arriving every %s — the wall window is %s and never measured that much silence.\n"+
			"observed state at bound expiry:\n%s",
			time.Since(start).Round(time.Millisecond), 5*w124Minute, w124Wall,
			w124Snapshot(m, h, start))
	}
}
