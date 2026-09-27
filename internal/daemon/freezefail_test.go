package daemon

// freezefail_test.go: R2N-2 regressions — the 05 §2.7 dead-writers shortcut
// must NEVER be granted from an unknown agent state: a nil agent with a
// LIVE recorded process is not "dead", a nil agent with no pid at all is
// unverifiable, and only a procfs-verified empty group (or a handle whose
// group is verified empty) may shortcut the freeze.

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// nilAgentEnv wires a daemon whose jobRuntime has NO agent handle but whose
// jobs[] bookkeeping records the given pid — the genuinely-unknown state.
func nilAgentEnv(t *testing.T, wi string, pid int, startedAt string) (*Daemon, *jobRuntime) {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	capc, _, err := capsule.Create(ws, wi, 1, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MarkReady(); err != nil {
		t.Fatal(err)
	}
	store := NewStateStore(filepath.Join(dir, "daemon"))
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if pid > 0 {
		if err := store.UpsertJob(st, Job{
			WorkItemID: wi, CapsuleID: capc.ID(), Generation: 1,
			HarnessAdapter: "pi", HarnessSessionID: "sess_" + wi,
			MainAgentPID: pid, MainAgentStartedAt: startedAt, Status: JobRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := New(Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "freezefail-test",
	}, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	jr := &jobRuntime{cap: capc, ring: &Ring{}}
	d.mu.Lock()
	d.jobs[wi] = jr
	d.mu.Unlock()
	return d, jr
}

// TestNilAgentWithLiveProcessDeniesShortcut: the old code answered
// ProcessGroupDead=true for EVERY nil-agent job — a live recorded writer
// included. The shortcut must be denied: the recorded pid's group is
// verifiably non-empty.
func TestNilAgentWithLiveProcessDeniesShortcut(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group verification is Linux-only")
	}
	wi := "wi_nz01"
	proc := exec.Command("sleep", "300")
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	pid := proc.Process.Pid
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL); proc.Wait() }()

	d, jr := nilAgentEnv(t, wi, pid, time.Now().UTC().Format(time.RFC3339Nano))
	cond := d.freezeConditions(jr, false)
	if cond.ProcessGroupDead == nil {
		t.Fatal("the dead-path conditions must carry a ProcessGroupDead probe")
	}
	if cond.ProcessGroupDead() {
		t.Fatal("a nil agent with a LIVE recorded process must NOT be granted the dead-writers shortcut (fail closed)")
	}
	// The live writer must have survived the probe (no stray collection of
	// an alive, pid-guard-matching process).
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("the live recorded process must not have been killed by the probe")
	}
}

// TestNilAgentWithoutPidDeniesShortcut: no handle, no recorded pid —
// unverifiable, never a shortcut.
func TestNilAgentWithoutPidDeniesShortcut(t *testing.T) {
	d, jr := nilAgentEnv(t, "wi_nz02", 0, "")
	cond := d.freezeConditions(jr, false)
	if cond.ProcessGroupDead() {
		t.Fatal("an unverifiable (no pid) agent state must NOT grant the dead-writers shortcut")
	}
}

// TestNilAgentWithGoneProcessGrantsShortcut: the shortcut is still granted
// where it is TRUE — a recorded pid whose whole group is verifiably gone.
func TestNilAgentWithGoneProcessGrantsShortcut(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group verification is Linux-only")
	}
	// A group leader that exits leaving NO group members.
	proc := exec.Command("sh", "-c", "exit 0")
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	pid := proc.Process.Pid
	if err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	d, jr := nilAgentEnv(t, "wi_nz03", pid, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	cond := d.freezeConditions(jr, false)
	if !cond.ProcessGroupDead() {
		t.Fatal("a recorded pid whose group is verifiably empty MUST be granted the shortcut")
	}
}
