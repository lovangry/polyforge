package daemon

// recover_test.go: crash-recovery regressions.
//
//   - FD-1a: an orphaned pending intent requeued by RecoverFromState keeps
//     its PERSISTED idempotency key (04 §6.1 step 4: "generate if absent,
//     REUSE the same key on retry"), so the re-run claim hits the Core's
//     idempotent cache (02 §6.1/§6.6) instead of opening a duplicate
//     generation — asserted via a stub Core that records every claim's key.
//   - FD-1b: a dead recorded WI Agent's residual process group is collected
//     BEFORE the re-derivation (04 §7.1 step 4: collect first, then
//     re-derive), so orphaned writers cannot keep writing the capsule next
//     to the new instance.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// claimRecordingStub is a scripted AIHub that records the idempotency key
// of every claim it sees and answers with a generation the same way each
// time (the Core's idempotent-cache behavior is what the daemon RELIES on;
// the stub stands in for the cache by just answering claims).
type claimRecordingStub struct {
	mu   sync.Mutex
	keys []string
	wi   string
}

func (s *claimRecordingStub) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/work-items/wi_probe_invalid/head"):
		writeStubErr(w, 404, "404", "", "not found")
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/claim"):
		var body struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.keys = append(s.keys, body.IdempotencyKey)
		s.mu.Unlock()
		boot := map[string]any{
			"work_item_id": s.wi, "bundle_name": "demo", "bundle_digest": "sha256:demo",
			"repo_inventory": []any{},
		}
		writeStubJSON(w, 200, map[string]any{
			"generation": 1, "generation_token": "tok_g1", "secret_epoch": 1,
			"owner": "usr_worker", "capsule_bootstrap": boot,
		})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/head"):
		writeStubErr(w, 404, "404", "", "no head")
	case r.Method == "PUT" && strings.Contains(r.URL.Path, "/blocks/"):
		writeStubJSON(w, 200, map[string]any{"head_seq": 0})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/manifest"):
		writeStubJSON(w, 200, map[string]any{"head_seq": 1})
	default:
		writeStubErr(w, 404, "404", "", "unscripted "+r.Method+" "+r.URL.Path)
	}
}

func (s *claimRecordingStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

// failingSpawner simulates the daemon dying between the confirmed claim and
// the derivation (04 §6.1 step 9): Spawn always fails, so the pending
// journal record survives at session_persisted — the crash-recovery orphan.
type failingSpawner struct{}

func (f *failingSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	return nil, errors.New("simulated crash: derivation failed")
}

// TestCrashedClaimRequeueReusesIdempotencyKey: first run claims (key seen
// once by the Core) and dies before derivation; the RESTARTED daemon's
// RecoverFromState requeues the orphan carrying the persisted key, and the
// re-run claim presents the SAME key to the Core (its idempotent cache then
// returns the same attempt — no duplicate generation).
func TestCrashedClaimRequeueReusesIdempotencyKey(t *testing.T) {
	wi := "wi_cr01"
	stub := &claimRecordingStub{wi: wi}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	for _, d := range []string{
		filepath.Join(home, "skills", "pf-execute"),
		filepath.Join(home, "workflows"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\nrun\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_stub", MachineID: "mac_a",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "recover-test",
	}

	// --- first "life": the claim lands, the derivation dies (crash) ---
	d1, err := New(cfg, &failingSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.Submit(SubmitIntent{WorkItemID: wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d1.SpawnNext(context.Background()); err == nil {
		t.Fatal("the simulated-crash spawn must fail")
	}
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	var orphan *PendingJob
	for i := range st.PendingJobs {
		if st.PendingJobs[i].WorkItemID == wi && st.PendingJobs[i].Status == PendingSessionPersisted {
			orphan = &st.PendingJobs[i]
		}
	}
	if orphan == nil || orphan.IdempotencyKey == "" {
		t.Fatalf("a session_persisted orphan with its key must survive the crash: %+v", st.PendingJobs)
	}
	if keys := stub.seen(); len(keys) != 1 {
		t.Fatalf("the first life must have claimed exactly once, got %v", keys)
	}

	// --- restart: a second daemon over the SAME data dir; crash recovery
	// requeues the orphan and the re-run claim presents the SAME key ---
	d2, err := New(cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := d2.jobq.LenPending(); n != 1 {
		t.Fatalf("the orphan must be requeued exactly once, pending=%d", n)
	}
	if _, err := d2.SpawnNext(context.Background()); err != nil {
		t.Fatalf("the re-run spawn must complete: %v", err)
	}
	keys := stub.seen()
	if len(keys) != 2 {
		t.Fatalf("the re-run must re-claim through the Core's idempotent face, keys=%v", keys)
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("the re-run claim must REUSE the persisted idempotency key, got %q then %q", keys[0], keys[1])
	}
	// The journal is clean after the completed re-run.
	st2, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.PendingJobs) != 0 || st2.FindJob(wi) == nil {
		t.Fatalf("the completed re-run must clear the journal and record the job: %+v", st2)
	}
}

// TestRecoverCollectsResidualProcessGroup: a recorded-dead WI Agent whose
// process group still holds an orphaned child (the leader exited, the child
// lives on in the group) must have that residue COLLECTED before the
// re-derivation — 04 §7.1 step 4 — so the orphaned writer cannot keep
// writing next to the new instance.
func TestRecoverCollectsResidualProcessGroup(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group-emptiness verification is Linux-only")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	wi := "wi_rc01"

	// A real capsule for the recovery to open.
	capc, _, err := capsule.Create(ws, wi, 1, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MarkReady(); err != nil {
		t.Fatal(err)
	}

	// A dead "agent": a process-group leader that spawned a long-lived
	// child and exited. The child KEEPS the group id — the residue a naive
	// recovery would leave running next to the new instance.
	leader := exec.Command("sh", "-c", "sleep 300 & exit 0")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	if err := leader.Wait(); err != nil { // the leader is dead and reaped
		t.Fatal(err)
	}
	if empty, err := ProcessGroupEmpty(pgid); err != nil || empty {
		if err != nil {
			t.Fatalf("group emptiness must be verifiable on this platform: %v", err)
		}
		t.Fatal("precondition: the orphaned child must still occupy the group")
	}

	// state.json records the dead leader as the job's agent.
	store := NewStateStore(filepath.Join(dir, "daemon"))
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := store.UpsertJob(st, Job{
		WorkItemID: wi, CapsuleID: capc.ID(), Generation: 1,
		HarnessAdapter: "pi", HarnessSessionID: "sess_rc01_1",
		MainAgentPID: pgid, MainAgentStartedAt: startedAt, Status: JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	// The recovery: re-derives the dead agent (fake spawner) and MUST have
	// collected the residual group first.
	spawner := newIntegSpawner()
	cfg := Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "residue-test",
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
		t.Fatalf("the dead agent must be re-derived exactly once: %d", len(spawner.spawns))
	}
	// The residue is gone: the group the dead leader left behind is empty.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if empty, err := ProcessGroupEmpty(pgid); err == nil && empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the residual process group must be collected before the re-derivation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("the residual process group must not survive recovery")
	}
}
