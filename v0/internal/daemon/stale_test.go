package daemon

// stale_test.go: the takeover-fence local isolation (05 §2.9 / 04 §6.2 B /
// 02 §6.4 B03-second-half). A generation-token-bearing write answered
// CORE-403 reason=stale_generation must NOT be retried or returned raw:
// the daemon isolates the scene locally — watcher stopped, agent process
// group terminated, capsule quarantined (kept for audit, never deleted),
// jobs[] record dropped — and surfaces the typed ErrStaleGeneration.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/sync/aihub"
)

func TestStaleGenerationClassification(t *testing.T) {
	cases := []struct {
		code, reason string
		want         bool
	}{
		{"CORE-403", "stale_generation", true},        // execution face (end-generation)
		{"SYNC-403", "stale_generation", true},        // publish face (block/manifest fencing)
		{"CORE-403", "secret_rolled_back", false},     // 04 §7.3 quarantine: a different path
		{"CORE-403", "secret_rotated_expired", false}, // 02 §6.6 grace re-authorize path
		{"CORE-403", "forbidden", false},
		{"HTTP-500", "", false},
	}
	for _, c := range cases {
		err := &aihub.APIError{Code: c.code, Reason: c.reason, Detail: "x"}
		if got := isStaleGeneration(err); got != c.want {
			t.Errorf("isStaleGeneration(%s %s) = %v, want %v", c.code, c.reason, got, c.want)
		}
	}
	if isStaleGeneration(nil) || isStaleGeneration(errors.New("plain transport error")) {
		t.Error("nil and non-API errors must not classify")
	}
	if !isStaleGeneration(errors.Join(&aihub.APIError{Code: "SYNC-403", Reason: ReasonStaleGeneration})) {
		t.Error("wrapped APIError must classify via errors.As")
	}
}

// TestWrapTakenOverIsolates: the wrap's end-generation answered
// stale_generation → the 05 §2.9 / 04 §6.2 B isolation, not a retry:
// typed DAEMON-403 envelope naming stale_generation, WI Agent terminated,
// capsule quarantined (moved + marked, never deleted), jobs[] record
// dropped, no re-queue.
func TestWrapTakenOverIsolates(t *testing.T) {
	wi := "wi_to01"
	stub := &finishStub{wi: wi}
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
	// The daemon's own workflows checkout must carry the demo bundle: the
	// spawn path materializes the snapshot from it (03 §4.5, fix round 2).
	if err := testsupport.MakeDemoBundle(home); err != nil {
		t.Fatal(err)
	}
	spawner := newIntegSpawner()
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_stub", MachineID: "mac_a",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "stale-test",
	}
	d, err := New(cfg, spawner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Submit(SubmitIntent{WorkItemID: wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	agent := spawner.agents[0]
	capc, _, err := capsule.Open(cfg.WorkspaceRoot, wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: wi, Generation: 1,
		CurrentStep: "code", WorkflowStatus: capsule.StatusRunning,
		Steps: map[string]*capsule.Step{"code": {Status: capsule.StepInProgress}}, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// The takeover lands server-side BEFORE the wrap: the final flush still
	// succeeds (fencing hits the end-generation write), the close is
	// answered CORE-403 reason=stale_generation.
	stub.stale.Store(true)

	socket := filepath.Join(dir, "stale.sock")
	api, err := NewAPIServer(d, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: socket}

	var errBody map[string]any
	if code, err := c.do("POST", "/jobs/"+wi+"/wrap", map[string]any{"reason": "wrap"}, &errBody); err != nil || code != 403 {
		t.Fatalf("taken-over wrap: %d %v", code, err)
	}
	apiErr := errBody["error"].(map[string]any)
	if apiErr["code"] != ErrCodeForbidden || !strings.Contains(apiErr["detail"].(string), ReasonStaleGeneration) {
		t.Fatalf("taken-over wrap envelope: %v", errBody)
	}

	// 04 §6.2 B step 2: the WI Agent process group was terminated.
	if agent.Alive() {
		t.Fatal("the fenced-out WI Agent must be terminated")
	}
	// Step 3: the capsule is QUARANTINED (moved + marked, not deleted).
	if _, err := os.Stat(capc.Dir); !os.IsNotExist(err) {
		t.Fatalf("capsule dir must be moved away, stat err = %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.WorkspaceRoot, "capsules", "_quarantine", wi, "*"))
	if len(matches) != 1 {
		t.Fatalf("exactly one quarantined copy expected, got %v", matches)
	}
	// Step 4: the jobs[] record is gone and nothing was re-queued.
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.FindJob(wi) != nil || d.jobq.IsRunning(wi) || d.jobq.LenPending() != 0 {
		t.Fatal("no jobs[] record, queue slot or pending entry may survive the takeover fence")
	}
	// No retry / re-derivation: recovery is a fresh claim/force-takeover.
	if len(spawner.spawns) != 1 {
		t.Fatalf("no re-derivation may happen, spawns = %d", len(spawner.spawns))
	}
}

// TestShutdownContinuesPastTakenOverWI: the shutdown sequence treats a
// stale_generation like the rolled-back credential — the WI is already
// dead server-side, so the shutdown isolates it and moves on instead of
// failing the whole sequence (and holding every OTHER WI's close hostage).
func TestShutdownContinuesPastTakenOverWI(t *testing.T) {
	wi := "wi_to02"
	stub := &finishStub{wi: wi}
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
	// The daemon's own workflows checkout must carry the demo bundle: the
	// spawn path materializes the snapshot from it (03 §4.5, fix round 2).
	if err := testsupport.MakeDemoBundle(home); err != nil {
		t.Fatal(err)
	}
	spawner := newIntegSpawner()
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_stub", MachineID: "mac_a",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "stale-shutdown-test",
	}
	d, err := New(cfg, spawner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Submit(SubmitIntent{WorkItemID: wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	// The agent "worked": execution state exists so the final flush has a
	// real sync surface to scan.
	capc, _, err := capsule.Open(cfg.WorkspaceRoot, wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: wi, Generation: 1,
		CurrentStep: "code", WorkflowStatus: capsule.StatusRunning,
		Steps: map[string]*capsule.Step{"code": {Status: capsule.StepInProgress}}, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	stub.stale.Store(true)
	spawner.agents[0].dead = true // the agent already exited at its boundary

	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown must isolate the taken-over WI and continue: %v", err)
	}
	if st, _ := NewStateStore(cfg.DataDir).Load(); st.FindJob(wi) != nil {
		t.Fatal("the isolated WI's jobs[] record must be gone")
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.WorkspaceRoot, "capsules", "_quarantine", wi, "*"))
	if len(matches) != 1 {
		t.Fatalf("the isolated scene must be quarantined exactly once, got %v", matches)
	}
}
