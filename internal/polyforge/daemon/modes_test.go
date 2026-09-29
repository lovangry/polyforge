package daemon

// modes_test.go: mode transitions + admission refusals (04 §2.2/§6.7).

import (
	"errors"
	"testing"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestModeCtlTransitions(t *testing.T) {
	m := NewModeCtl()
	if m.Current() != protocol.ModeInteractive {
		t.Fatalf("start mode = %q", m.Current())
	}
	for _, next := range []string{protocol.ModeBatch, protocol.ModeDrain, protocol.ModeInteractive, protocol.ModeBatch} {
		if err := m.Set(next); err != nil {
			t.Fatalf("Set(%s): %v", next, err)
		}
	}
	// Entering drain closes every transition except abort-drain.
	if err := m.Set(protocol.ModeDrain); err != nil {
		t.Fatalf("Set(drain): %v", err)
	}
	if err := m.Set(protocol.ModeBatch); !errors.Is(err, errModeConflict) {
		t.Fatalf("drain→batch = %v, want mode conflict", err)
	}
	if err := m.Set(protocol.ModeDrain); !errors.Is(err, errModeConflict) {
		t.Fatalf("drain→drain = %v, want mode conflict (abort is the only exit)", err)
	}
	if err := m.Set(protocol.ModeInteractive); err != nil {
		t.Fatalf("drain→interactive (abort): %v", err)
	}
	// AbortDrain outside drain refuses.
	if err := m.AbortDrain(); !errors.Is(err, errModeConflict) {
		t.Fatalf("abort-drain outside drain = %v, want conflict", err)
	}
	// Unknown modes refuse.
	if err := m.Set("turbo"); !errors.Is(err, errBadMode) {
		t.Fatalf("Set(turbo) = %v, want bad mode", err)
	}
}

func TestModeCtlAdmission(t *testing.T) {
	m := NewModeCtl()
	if err := m.AdmitSingle(); err != nil {
		t.Fatalf("interactive single: %v", err)
	}
	if err := m.AdmitBatch(); err != nil {
		t.Fatalf("interactive batch: %v", err)
	}
	if m.AdmitDrainWave() {
		t.Fatal("drain wave admitted outside drain")
	}
	_ = m.Set(protocol.ModeDrain)
	if err := m.AdmitSingle(); !errors.Is(err, errModeConflict) {
		t.Fatalf("drain single = %v, want conflict", err)
	}
	if err := m.AdmitBatch(); !errors.Is(err, errModeConflict) {
		t.Fatalf("drain batch = %v, want conflict", err)
	}
	if !m.AdmitDrainWave() {
		t.Fatal("drain wave refused inside drain")
	}
	// The shutting_down latch is a separate dimension: it gates every
	// source, and abort-drain too.
	m.SetShuttingDown()
	if err := m.AdmitSingle(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("single during shutdown = %v, want 410", err)
	}
	if err := m.AdmitBatch(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("batch during shutdown = %v, want 410", err)
	}
	if m.AdmitDrainWave() {
		t.Fatal("drain wave admitted during shutdown")
	}
	if err := m.AbortDrain(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("abort-drain during shutdown = %v, want 410", err)
	}
}
