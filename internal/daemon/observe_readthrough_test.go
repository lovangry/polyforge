package daemon

// observe_readthrough_test.go: the 04 §5.1a two-layer observe protocol.
// An integration test against the REAL Core: daemon A claims, spawns and
// publishes a revision whose execution.json holds an open pending_question;
// daemon B — same Core, different machine (no local capsule) — observes the
// WI and must get the layer-2 read-through: the published execution.json
// rendered verbatim, the freshness note, and the open question presented in
// the 00 §3.4a observation shape. Daemon A's layer-1 snapshot carries the
// same question projection from the live capsule.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

func TestObserveTwoLayersPendingQuestionShape(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	// The WI Agent enters needs_input: pending_question persisted in the
	// SAME atomic write that parks the step (03 §4.2), then published.
	capc, _, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	question := &capsule.PendingQuestion{QuestionID: "q_ob01a", AskedAt: NowStamp(), Content: "Which design?"}
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: e.wi, Generation: 1,
		CurrentStep: "grill", WorkflowStatus: capsule.StatusRunning,
		Steps:   map[string]*capsule.Step{"grill": {Status: capsule.StepInProgress, PendingQuestion: question}},
		Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.PublishRequest(ctx, e.wi, "step_interrupted"); err != nil {
		t.Fatal(err)
	}

	// --- Layer 1 (machine A, local capsule): live ring + snapshot. ---
	apiA, err := NewAPIServer(e.d, filepath.Join(e.dataDir, "sockA.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { apiA.Close() })
	go apiA.Serve()
	ca := &udsClient{path: apiA.path}
	var l1 map[string]any
	if code, err := ca.do("GET", "/jobs/"+e.wi+"/observe?after=0", nil, &l1); err != nil || code != 200 {
		t.Fatalf("layer-1 observe: %d %v", code, err)
	}
	snap1, _ := l1["state_snapshot"].(map[string]any)
	if snap1 == nil || snap1["current_step"] != "grill" {
		t.Fatalf("layer-1 snapshot: %v", l1)
	}
	pq1, _ := snap1["pending_question"].(map[string]any)
	if pq1 == nil || pq1["question_id"] != "q_ob01a" || pq1["step_id"] != "grill" ||
		pq1["status"] != "pending" || pq1["content"] != "Which design?" || pq1["asked_at"] == "" {
		t.Fatalf("layer-1 pending question must follow the 00 §3.4a shape: %v", pq1)
	}

	// --- Layer 2 (machine B, no local capsule): Core read-through. ---
	home2 := filepath.Join(e.dataDir, "home2")
	if err := os.MkdirAll(filepath.Join(home2, "skills", "pf-execute"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home2, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home2, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg2 := Config{
		AIHubURL: e.ts.url, APIKey: "pfk_daemon_it", MachineID: "mac_b",
		DataDir:       filepath.Join(home2, "daemon"),
		WorkspaceRoot: filepath.Join(home2, "ws"),
		WorkflowsDir:  filepath.Join(home2, "workflows"),
		SkillsDir:     filepath.Join(home2, "skills"),
		Adapter:       "pi", Version: "it-2",
	}
	d2, err := New(cfg2, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	apiB, err := NewAPIServer(d2, filepath.Join(e.dataDir, "sockB.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { apiB.Close() })
	go apiB.Serve()
	cb := &udsClient{path: apiB.path}

	var l2 map[string]any
	if code, err := cb.do("GET", "/jobs/"+e.wi+"/observe?after=0", nil, &l2); err != nil || code != 200 {
		t.Fatalf("layer-2 observe: %d %v", code, err)
	}
	if l2["read_through"] != true {
		t.Fatalf("cross-machine observe must be read-through: %v", l2)
	}
	if fn, _ := l2["freshness_note"].(string); fn == "" {
		t.Fatal("read-through must carry the freshness note (data as of the last publish)")
	}
	if hs, ok := l2["head_seq"].(float64); !ok || hs != 0 {
		t.Fatalf("read-through must report the published HEAD seq: %v", l2["head_seq"])
	}
	snap2, _ := l2["state_snapshot"].(map[string]any)
	if snap2 == nil || snap2["current_step"] != "grill" || snap2["workflow_status"] != capsule.StatusRunning {
		t.Fatalf("read-through must render the published execution.json verbatim: %v", snap2)
	}
	pq2, _ := snap2["pending_question"].(map[string]any)
	if pq2 == nil || pq2["question_id"] != "q_ob01a" || pq2["step_id"] != "grill" ||
		pq2["status"] != "pending" || pq2["content"] != "Which design?" || pq2["asked_at"] == "" {
		t.Fatalf("read-through pending question must follow the 00 §3.4a shape: %v", pq2)
	}
	if _, has := pq2["timeout_hint"]; has {
		t.Fatalf("no timeout hint is persisted (03 §4.2), none may be invented: %v", pq2)
	}
}

func TestStatusListsStaleMachines(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	// The daemon's own machine beat long ago (a Core outage, say): the
	// status output must list it — monitoring signal only.
	e.d.beats.Record(e.d.cfg.MachineID, fakeClockAgo(t, DefaultStaleHeartbeatThreshold+time.Minute))
	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}
	var st map[string]any
	if code, err := c.do("GET", "/status", nil, &st); err != nil || code != http.StatusOK {
		t.Fatalf("status: %d %v", code, err)
	}
	stale, _ := st["stale_machines"].([]any)
	if len(stale) != 1 {
		t.Fatalf("the silent machine must be listed: %v", stale)
	}
	row, _ := stale[0].(map[string]any)
	if row["machine_id"] != e.d.cfg.MachineID || row["last_heartbeat_at"] == "" {
		t.Fatalf("stale row: %v", row)
	}
	// A fresh beat clears the signal.
	e.d.beats.Record(e.d.cfg.MachineID, fakeClockAgo(t, 0))
	if code, err := c.do("GET", "/status", nil, &st); err != nil || code != http.StatusOK {
		t.Fatal(err)
	}
	if stale, _ := st["stale_machines"].([]any); len(stale) != 0 {
		t.Fatalf("fresh machine must not be listed: %v", stale)
	}
}

func fakeClockAgo(t *testing.T, d time.Duration) (ago time.Time) {
	t.Helper()
	return time.Now().Add(-d)
}
