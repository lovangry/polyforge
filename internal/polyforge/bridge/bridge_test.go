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
	for _, method := range []string{protocol.ControlStopNotice, protocol.ControlAnswer} {
		if err := h.SendControl(protocol.ControlMsg{Method: method}); err == nil {
			t.Fatalf("non-probe control %q must stay refused (no stdio channel)", method)
		}
	}
}
