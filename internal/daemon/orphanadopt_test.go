package daemon

// orphanadopt_test.go: R3N-F6 regressions — the crash window between a
// successful Spawn and the durable jobs[] row.
//
// The spawn's OnProcessStarted callback journals (pid, started-at) into the
// session_persisted pending row BEFORE Spawn returns, so the next boot can
// DISCOVER the orphan agent:
//   - alive  → ADOPT it (no re-checkout, no remote recovery, no second
//     agent; 04 §6.1 orphan-recovery + 05 §7.4 same-generation no-replace);
//   - dead   → collect the residual group first, then requeue for a fresh
//     spawn pass (04 §7.1 step 4);
//   - reused → leave the row, never requeue (would risk a second writer
//     generation).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// crashAfterStartSpawner simulates the R3N-F6 crash point END-TO-END: it
// starts a REAL long-lived process (the WI Agent that survives the
// daemon's death), invokes the daemon's OnProcessStarted journaling
// callback exactly like Supervisor.Spawn does, then "crashes" — Spawn
// returns an error, so spawnJob aborts before the step-10 jobs[] record
// while the process keeps running and the journal row carries its pid.
type crashAfterStartSpawner struct {
	spawns  int
	lastPID int
}

func (s *crashAfterStartSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	s.spawns++
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.lastPID = cmd.Process.Pid
	if p.OnProcessStarted != nil {
		if jerr := p.OnProcessStarted(cmd.Process.Pid, NowStamp()); jerr != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return nil, jerr
		}
	}
	// The daemon "dies" right after the successful derivation: the agent
	// stays alive, unowned.
	return nil, errors.New("simulated crash after successful spawn")
}

// TestCrashAfterSpawnAdoptsSurvivingAgent (R3N-F6, the full regression):
// crash between the successful Spawn and the jobs[] record → restart →
// exactly ONE agent alive (the original, ADOPTED), no re-checkout, and
// the unpublished local work intact.
func TestCrashAfterSpawnAdoptsSurvivingAgent(t *testing.T) {
	wi := "wi_or01"
	stub := &claimRecordingStub{wi: wi}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	os.MkdirAll(filepath.Join(home, "skills", "pf-execute"), 0o755)
	os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\nrun\n"), 0o644)
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_stub", MachineID: "mac_or",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "orphan-adopt-test",
	}

	// --- first life: claim lands, the agent is derived and journaled,
	// then the daemon "crashes" before the jobs[] record ---
	crash := &crashAfterStartSpawner{}
	d1, err := New(cfg, crash)
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.Submit(SubmitIntent{WorkItemID: wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d1.SpawnNext(context.Background()); err == nil {
		t.Fatal("the simulated crash must abort the first life's spawn")
	}
	orphanPID := crash.lastPID
	defer func() { _ = syscall.Kill(-orphanPID, syscall.SIGKILL) }()
	if orphanPID <= 0 {
		t.Fatal("the crash spawner must have started a real agent")
	}
	// The crash residue: a session_persisted journal row carrying the pid,
	// no jobs[] record, a live agent.
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	row := st.FindPending(wi, "")
	if row == nil || row.Status != PendingSessionPersisted || row.MainAgentPID != orphanPID || row.MainAgentStartedAt == "" {
		t.Fatalf("R3N-F6: the crash must leave a DISCOVERABLE journal row (pid+started-at), got %+v", st.PendingJobs)
	}
	if st.FindJob(wi) != nil {
		t.Fatal("precondition: no jobs[] record yet")
	}
	// The agent's unpublished local work (the crash struck before any
	// publish): must survive the restart untouched.
	unpublished := filepath.Join(cfg.WorkspaceRoot, "capsules", wi, "artifacts", "draft.md")
	os.MkdirAll(filepath.Dir(unpublished), 0o755)
	if err := os.WriteFile(unpublished, []byte("unpublished local work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- restart: the next boot must ADOPT the survivor ---
	spawner2 := newIntegSpawner()
	d2, err := New(cfg, spawner2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Exactly ONE agent: the original survives, no second was spawned.
	if len(spawner2.spawns) != 0 {
		t.Fatalf("R3N-F6: the live orphan must be ADOPTED, not re-spawned (spawns=%d)", len(spawner2.spawns))
	}
	if n := d2.jobq.LenPending(); n != 0 {
		t.Fatalf("the adopted orphan must not be requeued, pending=%d", n)
	}
	if err := syscall.Kill(orphanPID, 0); err != nil {
		t.Fatal("the surviving agent must still be alive after adoption")
	}
	d2.mu.Lock()
	jr := d2.jobs[wi]
	d2.mu.Unlock()
	if jr == nil || jr.agent == nil {
		t.Fatal("the adopted orphan must be live in the runtime with a handle")
	}
	if jr.agent.PID() != orphanPID {
		t.Fatalf("the adopted handle must wrap the surviving pid %d, got %d", orphanPID, jr.agent.PID())
	}
	if !jr.agent.Alive() {
		t.Fatal("the adopted survivor must read as alive")
	}
	if jr.token != row.GenerationToken {
		t.Fatalf("the adopted job must keep the journaled generation token, got %q", jr.token)
	}
	// The jobs[] record the crash interrupted is completed from the journal.
	st2, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st2.FindJob(wi)
	if j == nil || j.MainAgentPID != orphanPID || j.Generation != row.Generation {
		t.Fatalf("the jobs[] record must be completed from the journal: %+v", j)
	}
	if len(st2.PendingJobs) != 0 {
		t.Fatalf("the journal row must be cleared after adoption: %+v", st2.PendingJobs)
	}
	// Unpublished local work intact, no re-checkout residue (no
	// quarantine, no rebuild): the capsule is exactly as the agent left it.
	if raw, rerr := os.ReadFile(unpublished); rerr != nil || string(raw) != "unpublished local work\n" {
		t.Fatalf("the unpublished local work must survive the restart: %q %v", raw, rerr)
	}
	q, _ := filepath.Glob(filepath.Join(cfg.WorkspaceRoot, "capsules", "_quarantine", wi))
	if len(q) != 0 {
		t.Fatalf("adoption must not re-checkout or quarantine the capsule: %v", q)
	}
}

// TestOrphanDeadRecordedAgentRequeuedAfterCollection (R3N-F6): a
// session_persisted orphan whose journaled process is DEAD (the agent died
// with the daemon, or later) gets its residual group collected and is then
// requeued for the fresh spawn pass — the pre-F6 behavior, now gated on a
// PROVEN collection.
func TestOrphanDeadRecordedAgentRequeuedAfterCollection(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	// A dead, fully reaped pid.
	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := dead.Process.Pid
	_ = dead.Wait()

	store := NewStateStore(filepath.Join(dir, "daemon"))
	st, err := store.LoadOrAdopt(StateHeader{
		MachineID: "m-01", Mode: ModeInteractive,
		WorkspaceRoot: ws, StartedAt: NowStamp(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePending(st, PendingJob{
		WorkItemID: "wi_or02", IdempotencyKey: "pf-or02-1", Intent: "claim",
		CreatedAt: NowStamp(), Status: PendingSessionPersisted,
		Generation: 2, GenerationToken: "tok_g2", HarnessAdapter: "pi",
		HarnessSessionID: "sess_or02_2",
		MainAgentPID:     deadPID, MainAgentStartedAt: NowStamp(),
	}); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "orphan-dead-test",
	}, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := d.jobq.LenPending(); n != 1 {
		t.Fatalf("the dead orphan must be requeued exactly once after collection, pending=%d", n)
	}
}

// TestOrphanReusedPidNotRequeued (R3N-F6): a journaled orphan pid that now
// belongs to an unrelated LIVE process with a mismatched creation time is
// NOT adoptable and NOT safely collectable — requeueing it would spawn a
// second writer generation beside whatever still lives in the recorded
// group. The row must stay in the journal and the innocent must survive.
func TestOrphanReusedPidNotRequeued(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the pid-reuse guard is Linux-only")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	innocent := exec.Command("sh", "-c", "sleep 300 & wait")
	innocent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := innocent.Start(); err != nil {
		t.Fatal(err)
	}
	pid := innocent.Process.Pid
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL); innocent.Wait() }()

	store := NewStateStore(filepath.Join(dir, "daemon"))
	st, err := store.LoadOrAdopt(StateHeader{
		MachineID: "m-01", Mode: ModeInteractive,
		WorkspaceRoot: ws, StartedAt: NowStamp(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePending(st, PendingJob{
		WorkItemID: "wi_or03", IdempotencyKey: "pf-or03-1", Intent: "claim",
		CreatedAt: NowStamp(), Status: PendingSessionPersisted,
		Generation: 4, GenerationToken: "tok_g4", HarnessAdapter: "pi",
		HarnessSessionID:   "sess_or03_4",
		MainAgentPID:       pid,
		MainAgentStartedAt: time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "orphan-reuse-test",
	}, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := d.jobq.LenPending(); n != 0 {
		t.Fatalf("a pid-reused orphan must NOT be requeued (second-writer risk), pending=%d", n)
	}
	if d.jobs["wi_or03"] != nil {
		t.Fatal("a pid-reused orphan must not be adopted into the runtime")
	}
	// The journal row survives for human attention; the innocent is intact.
	st2, err := NewStateStore(filepath.Join(dir, "daemon")).Load()
	if err != nil {
		t.Fatal(err)
	}
	if row := st2.FindPending("wi_or03", ""); row == nil {
		t.Fatal("the journal row must be KEPT when neither adoption nor collection can proceed")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the innocent process must never be touched: %v", err)
	}
}
