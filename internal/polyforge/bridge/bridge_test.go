package bridge

// bridge_test.go: the driver-bridge handle's probe semantics (the #99
// verification lesson) — a liveness probe on a live driver is acknowledged
// (the engine IS the supervised entity; step processes stay record-silent
// for a whole HangWindow during long tool calls), and every other control
// method stays refused (no stdio control channel exists).

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestProbeAckedWhileDriverAlive(t *testing.T) {
	h := &Handle{done: make(chan error)}
	msg := protocol.ControlMsg{Method: protocol.ControlProbe, Nonce: "deadbeef", Seq: 7}
	if err := h.SendControl(msg); err != nil {
		t.Fatalf("probe on a live driver must be accepted, got %v", err)
	}
	if err := h.WaitControlAck(context.Background(), msg, time.Second); err != nil {
		t.Fatalf("probe ack on a live driver must succeed, got %v", err)
	}
}

func TestProbeFailsAfterDriverExit(t *testing.T) {
	h := &Handle{done: make(chan error)}
	h.mu.Lock()
	h.doneSet = true
	h.doneErr = nil
	close(h.done)
	h.mu.Unlock()
	msg := protocol.ControlMsg{Method: protocol.ControlProbe}
	if err := h.SendControl(msg); err == nil {
		t.Fatal("probe on a dead driver must fail — recovery owns it")
	}
	if err := h.WaitControlAck(context.Background(), msg, time.Second); err == nil {
		t.Fatal("probe ack on a dead driver must fail")
	}
}

func TestNonProbeControlStillRefused(t *testing.T) {
	h := &Handle{done: make(chan error)}
	// polyforge#106: the answer relay and the stop-notice delivery died
	// with the WI-Agent engine — the driver handle still refuses them BY
	// NAME (literals: the protocol constants are deleted) alongside
	// anything else that is not the probe.
	for _, method := range []string{"pf/stop_notice", "pf/answer"} {
		if err := h.SendControl(protocol.ControlMsg{Method: method}); err == nil {
			t.Fatalf("non-probe control %q must stay refused (no stdio channel)", method)
		}
	}
}

// TestTerminateJoinsDriverExit is the astra-r2 #1 regression: Terminate
// must BLOCK (bounded) until the driver goroutine actually returned —
// a cancel that merely returns lets the old driver's journal appends
// land after the relaunch reads JournalMax, re-colliding ordinals.
func TestTerminateJoinsDriverExit(t *testing.T) {
	h := &Handle{done: make(chan error), cancel: func() {}}
	// the driver goroutine exits 150ms after cancel
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		<-time.After(150 * time.Millisecond)
		h.mu.Lock()
		h.doneSet = true
		h.doneErr = context.Canceled
		close(h.done)
		h.mu.Unlock()
	}()
	start := time.Now()
	if err := h.Terminate(5 * time.Second); err != nil {
		t.Fatalf("terminate must join a clean (context-cancelled) driver exit: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("terminate returned before the driver exited (after %v)", elapsed)
	}
	select {
	case <-closed:
	default:
		t.Fatal("terminate must return only after the driver goroutine closed done")
	}
}

// TestTerminateBoundedWhenDriverNeverExits: a wedged driver answers the
// bound, loudly — the caller surfaces the incomplete teardown instead of
// assuming the writer is gone.
func TestTerminateBoundedWhenDriverNeverExits(t *testing.T) {
	h := &Handle{done: make(chan error), cancel: func() {}}
	if err := h.Terminate(40 * time.Millisecond); err == nil {
		t.Fatal("terminate must fail loudly when the driver does not exit in bound")
	}
}
