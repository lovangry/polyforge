package mainagent

// spawnjournal_test.go: R3N-F6 regressions on the spawner side — the
// supervisor reports the derived process identity (pid + creation
// started-at) to the daemon's OnProcessStarted callback IMMEDIATELY after
// the process exists and BEFORE Spawn returns, so the daemon can journal
// it into the pending_jobs[] row ahead of the formal jobs[] record. A
// callback failure must fail the spawn AND tear the just-started process
// down (never leak a half-initialized agent).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// TestSupervisorJournalsProcessIdentityBeforeReturning: the callback fires
// once, before Spawn returns, carrying the real pid and a parseable
// started-at that matches the handle's own identity.
func TestSupervisorJournalsProcessIdentityBeforeReturning(t *testing.T) {
	bin := writeFakeHarness(t, false)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_journal")

	var calls int32
	var gotPID int
	var gotStartedAt string
	p.OnProcessStarted = func(pid int, startedAt string) error {
		atomic.AddInt32(&calls, 1)
		gotPID = pid
		gotStartedAt = startedAt
		return nil
	}
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("OnProcessStarted must fire exactly once, got %d", n)
	}
	if gotPID != h.PID() {
		t.Fatalf("the callback must carry the REAL pid: got %d, handle says %d", gotPID, h.PID())
	}
	if gotStartedAt == "" || gotStartedAt != h.StartedAt() {
		t.Fatalf("the callback must carry the handle's started-at: got %q, handle says %q", gotStartedAt, h.StartedAt())
	}
	if !strings.Contains(gotStartedAt, "T") {
		t.Fatalf("the started-at must be an RFC3339 stamp, got %q", gotStartedAt)
	}
}

// TestSupervisorJournalFailureTearsDownProcess: a callback error (the
// journal write failed) must fail the Spawn with that error and leave NO
// process behind — the agent would otherwise run unrecorded, which is
// exactly the undiscoverable orphan R3N-F6 exists to shrink.
func TestSupervisorJournalFailureTearsDownProcess(t *testing.T) {
	bin := writeFakeHarness(t, false)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_journal_fail")

	var gotPID int32
	p.OnProcessStarted = func(pid int, startedAt string) error {
		atomic.StoreInt32(&gotPID, int32(pid))
		return errors.New("simulated journal write failure")
	}
	_, err := s.Spawn(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "simulated journal write failure") {
		t.Fatalf("the callback failure must fail the spawn with its error, got %v", err)
	}
	pid := int(atomic.LoadInt32(&gotPID))
	if pid == 0 {
		t.Fatal("the callback must have fired")
	}
	// The process was torn down (allow a moment for the SIGKILL to land).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			break // ESRCH: gone (or a reaped zombie)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the half-initialized agent (pid %d) must be torn down after a journal failure", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// No orphaned group member either.
	if empty, gerr := daemon.ProcessGroupEmpty(pid); gerr != nil || !empty {
		t.Fatalf("the torn-down agent's group must be empty: (%v, %v)", empty, gerr)
	}
}
