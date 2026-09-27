package daemon

// adopt_test.go: R2N-1 regressions — the crash-restart alive branch must
// re-attach a REAL handle to the live recorded pid (04 §7.1 step 3), not
// leave jobRuntime.agent nil: proxy operations must target the adopted pid,
// Shutdown/seal must be able to terminate the whole group, and the lost
// control channel must fail TYPED (ErrControlChannelLost), never pretend a
// quiesce/stop/resume was delivered.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// TestRecoverAliveAgentReattachesHandle: a state.json job whose recorded
// main_agent_pid is ALIVE (creation time matches) must come back from
// RecoverFromState with a non-nil agent handle wrapping that exact pid.
func TestRecoverAliveAgentReattachesHandle(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	wi := "wi_ad01"

	capc, _, err := capsule.Create(ws, wi, 1, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MarkReady(); err != nil {
		t.Fatal(err)
	}

	// A live "agent": a real process group leader, recorded with a
	// creation timestamp the pid-reuse guard accepts (same tolerance the
	// daemon uses, ±2s).
	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	agent := exec.Command("sleep", "300")
	agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	pid := agent.Process.Pid
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL); agent.Wait() }()

	store := NewStateStore(filepath.Join(dir, "daemon"))
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertJob(st, Job{
		WorkItemID: wi, CapsuleID: capc.ID(), Generation: 1,
		HarnessAdapter: "pi", HarnessSessionID: "sess_ad01_1",
		MainAgentPID: pid, MainAgentStartedAt: startedAt, Status: JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	os.MkdirAll(filepath.Join(dir, "skills", "pf-execute"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\n"), 0o644)

	d, err := New(Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "adopt-test",
	}, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}

	d.mu.Lock()
	jr := d.jobs[wi]
	d.mu.Unlock()
	if jr == nil {
		t.Fatal("the alive job must be recovered into the runtime")
	}
	if jr.agent == nil {
		t.Fatal("R2N-1: the alive branch must re-attach an AgentHandle, not leave agent nil")
	}
	if jr.agent.PID() != pid {
		t.Fatalf("adopted handle must wrap the recorded pid %d, got %d", pid, jr.agent.PID())
	}
	if !jr.agent.Alive() {
		t.Fatal("the adopted handle must observe the live process as alive")
	}
	if jr.agent.GroupEmpty() {
		t.Fatal("the adopted live agent's group is NOT empty (the leader is alive)")
	}
}

// TestAdoptedHandleControlChannelFailsTyped: the stdio control channel died
// with the previous daemon — every control method on the adopted handle
// must fail with ErrControlChannelLost so no caller can believe a
// quiesce/stop/resume was delivered.
func TestAdoptedHandleControlChannelFailsTyped(t *testing.T) {
	a := Adopt(12345, NowStamp())
	ctx := context.Background()
	if err := a.NotifyStop(ctx); !errors.Is(err, ErrControlChannelLost) {
		t.Fatalf("NotifyStop must fail typed with ErrControlChannelLost, got %v", err)
	}
	if err := a.Quiesce(ctx); !errors.Is(err, ErrControlChannelLost) {
		t.Fatalf("Quiesce must fail typed with ErrControlChannelLost, got %v", err)
	}
	if err := a.Resume(ctx); !errors.Is(err, ErrControlChannelLost) {
		t.Fatalf("Resume must fail typed with ErrControlChannelLost, got %v", err)
	}
	if err := a.DeliverAnswer(AnswerRelay{QuestionID: "q", AnswerText: "a"}); !errors.Is(err, ErrControlChannelLost) {
		t.Fatalf("DeliverAnswer must fail typed with ErrControlChannelLost, got %v", err)
	}
}

// TestAdoptedHandleTerminatesGroup: the adopted handle's Terminate must be
// able to kill the whole recorded process group (Shutdown/seal path) even
// though the leader was never this daemon's own child.
func TestAdoptedHandleTerminatesGroup(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group-emptiness verification is Linux-only")
	}
	leader := exec.Command("sh", "-c", "sleep 300 & wait")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	a := Adopt(pgid, time.Now().UTC().Format(time.RFC3339Nano))
	if !a.Alive() {
		t.Fatal("precondition: the adopted leader must be alive")
	}
	if err := a.Terminate(10 * time.Second); err != nil {
		t.Fatalf("adopted Terminate must collect the group: %v", err)
	}
	// The leader was OUR child: reap the zombie it leaves behind (zombies
	// are excluded from group-emptiness by design) before asserting the
	// group id itself is gone.
	_ = leader.Wait()
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("the whole adopted process group must be dead after Terminate")
	}
	if a.Alive() {
		t.Fatal("the adopted handle must observe the terminated leader as dead")
	}
}
