package daemon

// modes_test.go: mode transitions + admission refusals (04 §2.2/§6.7).

import (
	"errors"
	"os"
	"path/filepath"
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

// TestNewKeepsPersistedModeAcrossRestart (#18): an unset --mode flag keeps
// the persisted mode (a drain daemon restarting stays drain); an explicit
// flag overrides it; no persisted value defaults to interactive.
func TestNewKeepsPersistedModeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	data := filepath.Join(dir, "data")
	for _, d := range []string{ws, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	boot := func(mode string) (*Daemon, error) {
		return New(Config{
			MachineID:     "m-test",
			WorkspaceRoot: ws,
			DataDir:       data,
			Project:       "proj",
			GitRemoteBase: filepath.Join(dir, "hub"),
			Mode:          mode,
			Hub:           newFakeHub(),
		})
	}

	// First boot, no flag: defaults to interactive.
	d1, err := boot("")
	if err != nil {
		t.Fatal(err)
	}
	if got := d1.modes.Current(); got != protocol.ModeInteractive {
		t.Fatalf("first boot mode = %q, want interactive", got)
	}
	// Enter drain and persist, exactly as the mode route does.
	if err := d1.modes.Set(protocol.ModeDrain); err != nil {
		t.Fatal(err)
	}
	d1.persistModeQuietly()

	// Restart with the flag UNSET: drain survives the restart.
	d2, err := boot("")
	if err != nil {
		t.Fatal(err)
	}
	if got := d2.modes.Current(); got != protocol.ModeDrain {
		t.Fatalf("restart mode = %q, want drain (persisted value must win)", got)
	}
	// An explicit flag equal to the persisted mode is a harmless no-op.
	d3, err := boot(protocol.ModeDrain)
	if err != nil {
		t.Fatal(err)
	}
	if got := d3.modes.Current(); got != protocol.ModeDrain {
		t.Fatalf("explicit drain on drain = %q, want drain", got)
	}
	// An explicit interactive flag switches back.
	d4, err := boot(protocol.ModeInteractive)
	if err != nil {
		t.Fatal(err)
	}
	if got := d4.modes.Current(); got != protocol.ModeInteractive {
		t.Fatalf("explicit interactive = %q, want interactive", got)
	}
	// An explicit bad mode is still a construction error.
	if _, err := boot("turbo"); err == nil {
		t.Fatal("unknown initial mode must fail New")
	}

	// A fresh data dir with the flag unset: interactive again.
	d5, err := New(Config{
		MachineID:     "m-test",
		WorkspaceRoot: ws,
		DataDir:       filepath.Join(dir, "data2"),
		Project:       "proj",
		GitRemoteBase: filepath.Join(dir, "hub"),
		Hub:           newFakeHub(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d5.modes.Current(); got != protocol.ModeInteractive {
		t.Fatalf("fresh boot mode = %q, want interactive", got)
	}
}
