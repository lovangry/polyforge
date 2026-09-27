package daemon

// pidguard_test.go: fix-round-3 regressions over the pid-identity and
// group-emptiness machinery.
//
//   - R3N-F1: an adopted Terminate verifies the recorded creation identity
//     (pid + started-at) BEFORE any destructive signal. An exited adopted
//     process whose pid was reused by a live innocent process must never be
//     signaled: Terminate aborts typed (ErrAgentIdentityMismatch) and the
//     innocent survives, on BOTH the SIGTERM path and the SIGKILL
//     escalation path.
//   - R3N-F2: a readable proc root with an unreadable/malformed member stat
//     makes ProcessGroupEmpty's whole answer unprovable (typed
//     ErrGroupUnverifiable, never a fail-open "empty"); waitForAdoptedDead
//     never converts an unprovable group into success.
//   - R3N-F3: an unreaped zombie leader (ours) is NOT mistaken for pid
//     reuse: its surviving children are collected, the corpse is reaped
//     when possible, and only the PROVEN outcome earns the re-derivation.
//     Genuine pid reuse skips signaling typed (ErrPIDReused) and the
//     innocent group is left untouched.
//   - R4N-1 (fix round 4): a member stat whose PGRP field is not a number
//     is a per-entry failure — the whole emptiness answer is typed
//     ErrGroupUnverifiable, never a fail-open "not this group".
//   - R4N-2: the "recorded pid is gone" collection branch PROVES group
//     ownership before any group signal — a pgid hosting an innocent
//     FRESH group (pid reused as a new group leader in the race window)
//     is refused typed (ErrResidualOwnershipUnprovable), nothing signaled.
//   - R4N-5: a kill(pid,0) probe answering EPERM PROVES a process exists:
//     the identity guard treats it as occupied/unverifiable and fails
//     closed (typed abort, no signal), never as "pid gone".

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// aliveProc starts a real group-leader process and returns its pid and a
// cleanup that tears the whole group down.
func aliveProc(t *testing.T, argv ...string) (int, func()) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	return pid, func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
}

// waitForZombie polls until pid sits in state Z (the caller must NOT have
// reaped it); returns false when the state cannot be observed in time.
func waitForZombie(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if procZombie(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestAdoptedTerminateNeverSignalsReusedPid (R3N-F1): the recorded adopted
// process has exited and its pid is now occupied by a live INNOCENT process
// (creation time mismatched). Terminate must abort typed and never signal:
// the innocent and its whole group must survive.
func TestAdoptedTerminateNeverSignalsReusedPid(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the pid-reuse guard is Linux-only")
	}
	innocent, cleanup := aliveProc(t, "sleep", "300")
	defer cleanup()

	// The recorded identity describes a process that exited 2 minutes
	// before the innocent was created: pid-reuse from the guard's point
	// of view.
	stale := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	a := Adopt(innocent, stale)
	if a.Alive() {
		t.Fatal("precondition: a mismatched creation time must NOT read as alive")
	}
	err := a.Terminate(2 * time.Second)
	if !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("Terminate must abort typed with ErrAgentIdentityMismatch, got %v", err)
	}
	// The innocent was never signaled.
	if err := syscall.Kill(innocent, 0); err != nil {
		t.Fatalf("the innocent process must survive the aborted termination: %v", err)
	}
	// And neither was its group.
	if empty, err := ProcessGroupEmpty(innocent); err != nil || empty {
		t.Fatalf("the innocent group must be untouched: (%v, %v)", empty, err)
	}
}

// TestAdoptedTerminateEscalationGuarded (R3N-F1): the SIGKILL escalation
// re-verifies the identity. An adopted agent that exits inside the SIGTERM
// wait window and whose pid is immediately reused must not get the
// replacement SIGKILLed. The mid-window reuse is simulated by pointing the
// adopted handle at a live innocent while its started-at still matches the
// ORIGINAL (now dead) agent: processAlive then answers false for the
// mismatch and the escalation aborts before signaling.
func TestAdoptedTerminateEscalationGuarded(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the pid-reuse guard is Linux-only")
	}
	innocent, cleanup := aliveProc(t, "sleep", "300")
	defer cleanup()

	stale := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	a := &adoptedAgent{pid: innocent, startedAt: stale}
	// Drive the escalation path directly: the first guard aborts before
	// any signal, exactly as it would at the escalation point.
	if err := a.guardIdentityBeforeSignal(); !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("the identity guard must abort typed, got %v", err)
	}
	// No signal reached the innocent.
	if err := syscall.Kill(innocent, 0); err != nil {
		t.Fatalf("the innocent must survive the guarded escalation: %v", err)
	}
}

// fakeProcRoot builds a synthetic procfs tree: every dir <pid>/stat holds a
// stat line with the given pgrp (fields after the comm terminator are
// state, ppid, pgrp). Entries named in broken are corrupted per their mode.
func fakeProcRoot(t *testing.T, entries map[string]string, broken map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, stat := range entries {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, mode := range broken {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "missing":
			// stat file absent
		case "malformed":
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("no terminator here"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "truncated":
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("1 (x) S"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func withProcRoot(t *testing.T, root string) {
	t.Helper()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

// TestProcessGroupEmptyPerEntryFailureIsUnprovable (R3N-F2): a readable
// proc root whose member stat is unreadable, malformed or truncated makes
// the WHOLE emptiness answer unprovable — never a fail-open (true, nil).
func TestProcessGroupEmptyPerEntryFailureIsUnprovable(t *testing.T) {
	goodStat := func(pgrp string) string {
		return "1 (proc) S 0 1 " + pgrp + " 1 1 1 0 0 0 1 0 0 0 0 0 0 0 0"
	}
	for name, broken := range map[string]string{
		"unreadable member stat": "missing",
		"malformed member stat":  "malformed",
		"truncated member stat":  "truncated",
	} {
		t.Run(name, func(t *testing.T) {
			root := fakeProcRoot(t, map[string]string{"100": goodStat("999")}, map[string]string{"200": broken})
			withProcRoot(t, root)
			empty, err := ProcessGroupEmpty(4242)
			if empty {
				t.Fatal("an unprovable scan must NEVER report the group empty")
			}
			if !errors.Is(err, ErrGroupUnverifiable) {
				t.Fatalf("the answer must be typed ErrGroupUnverifiable, got %v", err)
			}
		})
	}
	// Sanity: the same fake root WITHOUT the broken entry answers cleanly.
	root := fakeProcRoot(t, map[string]string{"100": goodStat("999")}, nil)
	withProcRoot(t, root)
	if empty, err := ProcessGroupEmpty(4242); err != nil || !empty {
		t.Fatalf("a clean synthetic proc root must answer (true, nil), got (%v, %v)", empty, err)
	}
}

// TestWaitForAdoptedDeadNotSuccessWhenUnverifiable (R3N-F2): a DEAD leader
// plus an UNVERIFIABLE group is NOT success — the old code returned true
// ("the leader's death is the provable part"), converting unprovable into
// the success that gates the dead-writers shortcut.
func TestWaitForAdoptedDeadNotSuccessWhenUnverifiable(t *testing.T) {
	// A real dead pid: the child is fully gone (reaped), so processAlive
	// is false through ESRCH.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	withProcRoot(t, filepath.Join(t.TempDir(), "no-such-proc"))
	if waitForAdoptedDead(pid, NowStamp(), 300*time.Millisecond) {
		t.Fatal("a dead leader with an unverifiable group must NOT read as success")
	}
}

// TestCollectZombieLeaderWithLiveChild (R3N-F3): an unreaped zombie leader
// (OUR corpse — creation time matches) whose group still holds a live
// child. The old code mistook the corpse for pid reuse (kill(pid,0)
// succeeds) and skipped the child. Collection must kill the child, prove
// the group dead, and reap the corpse when the caller is its parent.
func TestCollectZombieLeaderWithLiveChild(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the zombie state check is Linux-only")
	}
	leader := exec.Command("sh", "-c", "sleep 300 & exit 0")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	startedAt := NowStamp()
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = leader.Wait()
	}()

	if !waitForZombie(pgid) {
		t.Skip("could not observe the zombie state on this platform/kernel")
	}
	if empty, err := ProcessGroupEmpty(pgid); err != nil || empty {
		t.Fatalf("precondition: the zombie's live child must occupy the group: (%v, %v)", empty, err)
	}

	if err := collectResidualProcessGroup(pgid, startedAt); err != nil {
		t.Fatalf("collecting OUR zombie leader's residue must succeed: %v", err)
	}
	// The surviving child was collected.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if empty, err := ProcessGroupEmpty(pgid); err == nil && empty {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the zombie leader's surviving child must be collected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The corpse was reaped by the collection (we are its parent).
	if err := syscall.Kill(pgid, 0); err == nil {
		t.Fatal("the zombie leader must be reaped after collection")
	}
}

// TestCollectGenuinePidReuseSkipsInnocent (R3N-F3): a live innocent process
// whose creation time does not match the record is genuine pid reuse. When
// its group still holds members, collection must skip typed
// (ErrPIDReused) and never signal; when procfs proves the group id empty,
// there is provably no residual writer (nil) and the innocent is untouched
// in both cases.
func TestCollectGenuinePidReuseSkipsInnocent(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the group-emptiness check is Linux-only")
	}
	stale := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)

	t.Run("non-empty innocent group: typed skip, nothing signaled", func(t *testing.T) {
		innocent, cleanup := aliveProc(t, "sh", "-c", "sleep 300 & wait")
		defer cleanup()
		err := collectResidualProcessGroup(innocent, stale)
		if !errors.Is(err, ErrPIDReused) {
			t.Fatalf("collection over a reused pid with live members must skip typed ErrPIDReused, got %v", err)
		}
		if serr := syscall.Kill(innocent, 0); serr != nil {
			t.Fatalf("the innocent leader must never be signaled: %v", serr)
		}
		if empty, perr := ProcessGroupEmpty(innocent); perr != nil || empty {
			t.Fatalf("the innocent group must be untouched: (%v, %v)", empty, perr)
		}
	})

	t.Run("provably empty group id: nil without a signal", func(t *testing.T) {
		// The innocent never becomes a group leader, so the group id
		// `innocent` provably holds no members.
		cmd := exec.Command("sleep", "300")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		innocent := cmd.Process.Pid
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		if err := collectResidualProcessGroup(innocent, stale); err != nil {
			t.Fatalf("a provably empty group id means no residual writer: %v", err)
		}
		if serr := syscall.Kill(innocent, 0); serr != nil {
			t.Fatalf("the innocent must never be signaled: %v", serr)
		}
	})
}

// TestRecoverZombieLeaderRederivesAfterCollection (R3N-F3): the full
// 04 §7.1 step-4 regression — a recorded WI Agent that sits UNREAPED in
// state Z while its process group still holds a live child. The old
// collectResidualProcessGroup mistook the corpse for pid reuse
// (kill(pid,0) succeeds on a zombie) and skipped the child, so
// re-derivation spawned a SECOND writer generation beside the survivor.
// Recovery must kill the child, reap the corpse when it can, and only
// then re-derive.
func TestRecoverZombieLeaderRederivesAfterCollection(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group-emptiness verification is Linux-only")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	wi := "wi_rc02"

	capc, _, err := capsule.Create(ws, wi, 1, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MarkReady(); err != nil {
		t.Fatal(err)
	}

	// The dead "agent": a group leader that spawned a long-lived child and
	// exited WITHOUT being reaped — a state-Z corpse whose child keeps the
	// group id alive.
	leader := exec.Command("sh", "-c", "sleep 300 & exit 0")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	startedAt := NowStamp()
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL); _ = leader.Wait() }()
	if !waitForZombie(pgid) {
		t.Skip("could not observe the zombie state on this platform/kernel")
	}

	store := NewStateStore(filepath.Join(dir, "daemon"))
	st, err := store.LoadOrAdopt(StateHeader{
		MachineID: "m-01", Mode: ModeInteractive,
		WorkspaceRoot: ws, StartedAt: NowStamp(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertJob(st, Job{
		WorkItemID: wi, CapsuleID: capc.ID(), Generation: 1,
		HarnessAdapter: "pi", HarnessSessionID: "sess_rc02_1",
		MainAgentPID: pgid, MainAgentStartedAt: startedAt, Status: JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	spawner := newIntegSpawner()
	cfg := Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "zombie-recover-test",
	}
	os.MkdirAll(filepath.Join(cfg.SkillsDir, "pf-execute"), 0o755)
	os.WriteFile(filepath.Join(cfg.SkillsDir, "pf-execute", "SKILL.md"), []byte("# pf-execute\n"), 0o644)
	d, err := New(cfg, spawner)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.spawns) != 1 {
		t.Fatalf("R3N-F3: the zombie leader must be treated as DEAD-OF-OURS and re-derived after collection, got %d spawns", len(spawner.spawns))
	}
	// The survivor was collected: the group id holds no live member.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if empty, perr := ProcessGroupEmpty(pgid); perr == nil && empty {
			break
		} else if perr != nil {
			t.Fatal(perr)
		}
		if time.Now().After(deadline) {
			t.Fatal("the zombie leader's surviving child must be collected before the re-derivation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The corpse was reaped on the way (we are its parent); either way the
	// re-derivation happened exactly once and no writer generation was
	// left beside the new instance.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = leader.Wait()
}

// ---------------------------------------------------------------------------
// fix-round-4 regressions (R4N-1 / R4N-2 / R4N-5).

// fakeMemberStat builds a synthetic /proc/<pid>/stat whose pgrp (field 5) and
// starttime (field 22, ticks since boot) are controlled. Twenty fields after
// the comm terminator, so the starttime sits at index 19 exactly where the
// ownership proof reads it.
func fakeMemberStat(pgrp, startUnix int64) string {
	ticks := (startUnix - bootTime()) * hertz
	return fmt.Sprintf("1 (member) S 0 %d 1 1 1 0 0 0 0 0 1 0 0 0 0 0 0 0 %d", pgrp, ticks)
}

// memberProcRoot installs a synthetic proc root holding exactly one group
// member stat (the named pid directory with the given stat content).
func memberProcRoot(t *testing.T, pidDir, stat string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, pidDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	withProcRoot(t, root)
}

// TestProcessGroupEmptyMalformedPgrpFailsClosed (R4N-1): a member stat whose
// pgrp field is not a number used to be silently treated as "not this
// group" and skipped, so one corrupted member stat could hide a live writer
// and let the scan answer (true, nil) it never proved. A malformed pgrp is
// a per-entry failure: the whole answer must be ErrGroupUnverifiable.
func TestProcessGroupEmptyMalformedPgrpFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "300")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("1 (weird) S 0 not-a-pgrp 1 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	withProcRoot(t, root)

	empty, err := ProcessGroupEmpty(4242)
	if empty {
		t.Fatal("a scan with a malformed pgrp field must NEVER report the group empty")
	}
	if !errors.Is(err, ErrGroupUnverifiable) {
		t.Fatalf("a malformed pgrp is a per-entry failure: the answer must be typed ErrGroupUnverifiable, got %v", err)
	}
	// Sanity: the same member with a WELL-FORMED foreign pgrp answers empty,
	// and with the MATCHING pgrp answers non-empty — the fail-closed turn
	// did not break the healthy paths.
	memberProcRoot(t, "300", "1 (plain) S 0 9999 1 1 1 0 0 0 0 0 1 0 0 0 0 0 0 0 1")
	if empty, err := ProcessGroupEmpty(4242); err != nil || !empty {
		t.Fatalf("a well-formed foreign pgrp member must leave the group empty: (%v, %v)", empty, err)
	}
	memberProcRoot(t, "300", "1 (plain) S 0 4242 1 1 1 0 0 0 0 0 1 0 0 0 0 0 0 0 1")
	if empty, err := ProcessGroupEmpty(4242); err != nil || empty {
		t.Fatalf("a well-formed matching pgrp member must keep the group non-empty: (%v, %v)", empty, err)
	}
}

// TestCollectGonePidRefusesInnocentFreshGroup (R4N-2): the recorded pid is
// gone (ESRCH) but the scanned pgid now hosts an INNOCENT FRESH group — the
// pid was reused as a new group leader in the window between the check and
// the signal. The old code went straight to kill(-pgid) on the assumption
// the group could only hold the dead agent's orphans; collection must PROVE
// ownership first and refuse typed (ErrResidualOwnershipUnprovable)
// without any signal.
func TestCollectGonePidRefusesInnocentFreshGroup(t *testing.T) {
	if bootTime() == 0 {
		t.Skip("procfs not available: creation-time proofs are Linux-only")
	}
	// A genuinely gone recorded pid: a fully reaped child.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := cmd.Process.Pid
	_ = cmd.Wait()
	if err := syscall.Kill(gone, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skipf("the reaped pid is not observably gone on this platform: %v", err)
	}

	// The innocent fresh group: one member of group `gone` whose creation
	// time (starttime) lies AFTER the gone-observation — exactly what a
	// pid reused as a new group leader in the race window looks like.
	memberProcRoot(t, "501", fakeMemberStat(int64(gone), time.Now().Add(30*time.Second).Unix()))

	startedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	err := collectResidualProcessGroup(gone, startedAt)
	if !errors.Is(err, ErrResidualOwnershipUnprovable) {
		t.Fatalf("an innocent fresh group on the gone pid's pgid must be refused typed ErrResidualOwnershipUnprovable, got %v", err)
	}
}

// TestProveResidualGroupOursWindow (R4N-2): the ownership proof accepts
// only members born inside the dead agent's provable lifetime — at/after
// the recorded agent start, strictly before the gone-observation. Members
// born outside it, or members whose proof inputs are broken, are a typed
// refusal; an empty group and a properly-old member pass.
func TestProveResidualGroupOursWindow(t *testing.T) {
	if bootTime() == 0 {
		t.Skip("procfs not available: creation-time proofs are Linux-only")
	}
	const pgid = 4242
	now := time.Now()
	startedAt := now.UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)

	t.Run("member born during the agent's lifetime: proven ours", func(t *testing.T) {
		memberProcRoot(t, "501", fakeMemberStat(pgid, now.Add(-time.Minute).Unix()))
		if err := proveResidualGroupOurs(pgid, startedAt, now); err != nil {
			t.Fatalf("a member born inside the agent's lifetime must be provably ours: %v", err)
		}
	})
	t.Run("empty group: vacuously ours", func(t *testing.T) {
		memberProcRoot(t, "501", fakeMemberStat(9999, now.Add(-time.Minute).Unix()))
		if err := proveResidualGroupOurs(pgid, startedAt, now); err != nil {
			t.Fatalf("an empty group must be vacuously provable: %v", err)
		}
	})
	t.Run("member born at/after the gone-observation: innocent reuse", func(t *testing.T) {
		memberProcRoot(t, "501", fakeMemberStat(pgid, now.Add(30*time.Second).Unix()))
		if err := proveResidualGroupOurs(pgid, startedAt, now); !errors.Is(err, ErrResidualOwnershipUnprovable) {
			t.Fatalf("a member born after the leader was observed gone must be refused typed, got %v", err)
		}
	})
	t.Run("member born before the recorded agent start: provably not ours", func(t *testing.T) {
		memberProcRoot(t, "501", fakeMemberStat(pgid, now.Add(-5*time.Minute).Unix()))
		if err := proveResidualGroupOurs(pgid, startedAt, now); !errors.Is(err, ErrResidualOwnershipUnprovable) {
			t.Fatalf("a member predating the agent must be refused typed, got %v", err)
		}
	})
	t.Run("malformed member stat: unprovable", func(t *testing.T) {
		memberProcRoot(t, "501", "1 (x) S 0 4242 1") // truncated before starttime
		if err := proveResidualGroupOurs(pgid, startedAt, now); !errors.Is(err, ErrResidualOwnershipUnprovable) {
			t.Fatalf("a member whose starttime cannot be read must make the proof unprovable, got %v", err)
		}
	})
	t.Run("unreadable proc root: unprovable", func(t *testing.T) {
		withProcRoot(t, filepath.Join(t.TempDir(), "no-such-proc"))
		if err := proveResidualGroupOurs(pgid, startedAt, now); !errors.Is(err, ErrResidualOwnershipUnprovable) {
			t.Fatalf("an unreadable procfs must make the proof unprovable, got %v", err)
		}
	})
}

// TestGuardIdentityEPERMMeansOccupied (R4N-5): kill(pid,0) answering EPERM
// PROVES a process exists — the old guard treated any non-nil probe error
// as "pid gone" and let the residual group collection signal proceed. Only
// ESRCH means gone; EPERM (or anything else) is occupied/unverifiable:
// the guard must fail closed with a typed identity abort and never signal.
func TestGuardIdentityEPERMMeansOccupied(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the pid-identity guard is Linux-only")
	}
	innocent, cleanup := aliveProc(t, "sleep", "300")
	defer cleanup()

	old := killProbe
	killProbe = func(pid int, sig syscall.Signal) error {
		if pid == innocent && sig == 0 {
			return syscall.EPERM // a process exists; this daemon may not probe it
		}
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { killProbe = old })

	// A started-at the pid-reuse guard REJECTS, so guardIdentityBeforeSignal's
	// own probe is the only thing standing between the innocent group and
	// the group-wide SIGTERM Terminate would send.
	stale := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	a := &adoptedAgent{pid: innocent, startedAt: stale}
	if err := a.guardIdentityBeforeSignal(); !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("an EPERM probe must abort typed (occupied/unverifiable), got %v", err)
	}
	if serr := syscall.Kill(innocent, 0); serr != nil {
		t.Fatalf("the innocent must survive the guarded abort un-signaled: %v", serr)
	}
	// Through the public face: Terminate must abort the SAME way, before
	// any signal — the innocent group survives intact.
	if err := a.Terminate(2 * time.Second); !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("Terminate over an EPERM-occupied pid must abort typed, got %v", err)
	}
	if serr := syscall.Kill(innocent, 0); serr != nil {
		t.Fatalf("the innocent must survive Terminate un-signaled: %v", serr)
	}
}
