package observe

// wallbackstop_test.go: the polyforge#114 wall backstop — the §6.2
// absolute bound on a dispatch's PROGRESS silence that no probe ack can
// restart. The failure it closes: a step process wedges (open turn, no
// envelope, no exit) while the liveness probe is answered from the
// HARNESS side (driver-alive), so every watchdog window restarts and no
// recovery ever fires. These tests pin, at unit scale:
//
//   - the acked-probe loop still runs inside the bound (hang_cleared is
//     written), but the backstop confirms the hang once the measured
//     progress silence crosses it;
//   - the turn-closed shape lands on the idle reason (the F5 lane's own
//     acked-probe loop has the identical hole);
//   - the #83 tool-dispatch exemption and the needs_input
//     pending-question exemption both hold against the backstop;
//   - a disabled backstop (negative) changes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// TestWallBackstopConfirmsWedgePastAckedProbes is the #114 unit shape:
// the probe acks (the fake handle's default — exactly what the driver
// bridge does), the hang window keeps restarting, and yet the recovery
// fires once the non-resettable progress witness crosses the bound.
func TestWallBackstopConfirmsWedgePastAckedProbes(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = 40 * time.Millisecond
		c.WallBackstop = 200 * time.Millisecond
	})

	// The wedge: a turn opens and the stream never speaks again. The
	// default sendFn ACKS every probe — the driver-alive ack.
	feed(h, fxTurnStart)

	msg := waitOB(t, ob, "WINeedsRecovery(hang) past the wall bound")
	if msg.Kind != protocol.OBNeedsRecovery {
		t.Fatalf("kind: got %s", msg.Kind)
	}
	if msg.Reason != protocol.RecoverHang {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverHang)
	}
	// The acked rounds really ran: at least one hang_cleared must exist
	// before the backstop overrode them (the ack lane was alive and was
	// still not allowed to mask the wedge past the bound).
	soon(t, 500*time.Millisecond, "hang_cleared written before the backstop fired", func() bool {
		for _, e := range eventsOf(m, EvInfraIncident) {
			var b struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(e.Body, &b); err == nil && b.Kind == HangCleared {
				return true
			}
		}
		return false
	})
	expectNoOB(t, ob, 250*time.Millisecond, "the backstop fires exactly once (storm guard)")
}

// TestWallBackstopIdleLane: the turn-closed wedge — the same acked-probe
// loop on the idle lane. The idle window is left huge (its watchdog never
// arms), so ONLY the wall bound can fire, and it must fire with the idle
// reason.
func TestWallBackstopIdleLane(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = time.Hour
		c.WallBackstop = 200 * time.Millisecond
	})

	// A clean, closed turn: progress anchored, turn closed, then silence.
	feed(h, fxTurnStart)
	feed(h, fxTurnEndStop)

	msg := waitOB(t, ob, "WINeedsRecovery(idle) past the wall bound")
	if msg.Reason != protocol.RecoverIdle {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverIdle)
	}
	_ = m
}

// TestWallBackstopExemptDuringToolDispatch: the #83 exemption holds — a
// dispatched tool call within its window is live work the wall bound
// must not judge.
func TestWallBackstopExemptDuringToolDispatch(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = 40 * time.Millisecond
		c.WallBackstop = 120 * time.Millisecond
	})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		t.Errorf("no probe may be sent while a tool dispatch is live, got %s", msg.Method)
		return protocol.ControlAck{}, errors.New("unexpected probe")
	})

	// message_end with the turn open = a tool dispatched and executing.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)

	time.Sleep(300 * time.Millisecond)
	if h.sentCount() != 0 {
		t.Fatalf("probes sent during the #83 tool window: %d", h.sentCount())
	}
	expectNoOB(t, ob, 100*time.Millisecond, "the wall backstop must honor the #83 tool exemption")
	_ = m
}

// TestWallBackstopExemptWhilePendingQuestion: needs_input is designed
// stillness — the §6.2a wall clock does not judge waiting-for-human
// either.
func TestWallBackstopExemptWhilePendingQuestion(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = 40 * time.Millisecond
		c.WallBackstop = 120 * time.Millisecond
	})

	// The stale-open shape (#99): the turn opens and never closes.
	feed(h, fxTurnStart)

	// Arm the pending question.
	writePendingQuestion(t, m, "q_wbs", "parked", "code")

	time.Sleep(300 * time.Millisecond)
	expectNoOB(t, ob, 100*time.Millisecond, "the wall backstop must honor the pending-question exemption")
	_ = m
}

// TestWallBackstopDisabledChangesNothing: a negative bound disables the
// backstop entirely — the acked-probe loop runs forever, exactly the
// pre-#114 world (kept as an explicit escape hatch, asserted here).
func TestWallBackstopDisabledChangesNothing(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = 40 * time.Millisecond
		c.WallBackstop = -1
	})

	feed(h, fxTurnStart) // wedges; probes ack; windows restart forever
	time.Sleep(300 * time.Millisecond)
	if h.sentCount() < 2 {
		t.Fatalf("the acked-probe loop should have run several rounds, got %d probes", h.sentCount())
	}
	expectNoOB(t, ob, 100*time.Millisecond, "a disabled backstop never fires")
	_ = m
}
