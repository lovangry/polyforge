package daemon

// proc_test.go: R2N-6/R2N-7 regressions — group-emptiness verification
// fails CLOSED (never grants the dead-writers shortcut from a check it
// could not perform), and a zombie is DEAD for adoption purposes (recovery
// must collect + re-derive, not adopt a reapable corpse).

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestProcessGroupEmptyFailsClosedWhenProcfsUnreadable: when the procfs
// scan cannot even be performed, ProcessGroupEmpty must answer
// (false, ErrGroupUnverifiable) — the 05 §2.7 shortcut condition ①
// requires every member CONFIRMED gone, and an unverifiable group is the
// opposite of a confirmation.
func TestProcessGroupEmptyFailsClosedWhenProcfsUnreadable(t *testing.T) {
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "no-such-proc")
	t.Cleanup(func() { procRoot = old })

	empty, err := ProcessGroupEmpty(4242)
	if empty {
		t.Fatal("an unreadable procfs must NEVER report the group empty (fail-open)")
	}
	if !errors.Is(err, ErrGroupUnverifiable) {
		t.Fatalf("the failure must be typed ErrGroupUnverifiable, got %v", err)
	}
}

// TestProcessGroupEmptyPositiveAndNegative: sanity around the new
// signature — a live member keeps the group non-empty; a group whose
// members are all gone is empty; pgid <= 0 is trivially empty.
func TestProcessGroupEmptyPositiveAndNegative(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group-emptiness verification is Linux-only")
	}
	if empty, err := ProcessGroupEmpty(0); err != nil || !empty {
		t.Fatalf("pgid <= 0 must be trivially empty, got (%v, %v)", empty, err)
	}
	leader := exec.Command("sh", "-c", "sleep 300 & wait")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL); leader.Wait() }()

	if empty, err := ProcessGroupEmpty(pgid); err != nil || empty {
		t.Fatalf("a live leader must keep the group non-empty, got (%v, %v)", empty, err)
	}
	if err := leader.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Reap the leader: the group's remaining member (sleep) still holds it.
	_ = leader.Wait()
	if empty, err := ProcessGroupEmpty(pgid); err != nil || empty {
		t.Fatalf("the orphaned child must keep the group non-empty, got (%v, %v)", empty, err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if empty, err := ProcessGroupEmpty(pgid); err == nil && empty {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the killed group must become empty")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestZombieIsDeadForAdoption: a recorded pid that still exists but sits in
// state Z (its parent has not reaped it yet) must NOT be reported alive —
// a zombie cannot write, cannot be adopted as a supervision target, and
// crash recovery must take the collect + re-derive path (04 §7.1 step 4).
func TestZombieIsDeadForAdoption(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the zombie state check is Linux-only")
	}
	// Spawn a child and let it exit WITHOUT reaping: until this test
	// process calls Wait, the child is a zombie with our recorded pid.
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }() // reap at teardown regardless

	// Give the kernel a moment to move the child into state Z.
	deadline := time.Now().Add(5 * time.Second)
	zombieSeen := false
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "stat")); err == nil {
			if state, ok := procStateOf(string(raw)); ok && state == "Z" {
				zombieSeen = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !zombieSeen {
		t.Skip("could not observe the zombie state on this platform/kernel")
	}

	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if processAlive(pid, startedAt) {
		t.Fatal("a zombie must be DEAD for adoption purposes (state Z: reapable, cannot write)")
	}
	// The zombie still occupies its pid, so the plain kill(0) probe says
	// "exists" — the fix must be in processAlive's state check, which the
	// assertion above exercises.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("precondition: the unreaped zombie must still occupy its pid: %v", err)
	}
}
