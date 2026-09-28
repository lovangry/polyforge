package daemon

// rollback_test.go: the 04 §7.3 rollback path. An integration-level test
// drives the daemon's full claim→capsule→WI-Agent sequence against a
// STUBBED aihub whose token-checked write face answers
// CORE-403/SYNC-403 reason=secret_rolled_back, then asserts the isolation
// sequence: WI Agent process group terminated, capsule quarantined (marked,
// not deleted), jobs[] record dropped, and NO auto re-claim. Plus unit
// coverage of the trigger classification (both faces' codes, and the
// near-miss reasons that must NOT fire the path).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/sync/aihub"
)

// rollbackStub is a scripted AIHub: claim/preflight succeed, the publish
// face (block PUT, manifest POST) answers 403 secret_rolled_back after
// `rollBackAt` publish-write calls have been SEEN (so the daemon can first
// do a healthy publish, then the rotation lands).
type rollbackStub struct {
	rolledBack atomic.Bool
	publishHit atomic.Int64
	wi         string
}

func (s *rollbackStub) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/work-items/wi_probe_invalid/head"):
		writeStubErr(w, 404, "404", "", "not found")
	case strings.HasSuffix(r.URL.Path, "/claim") && r.Method == "POST":
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
		s.publishHit.Add(1)
		s.writeFence(w)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/manifest"):
		s.publishHit.Add(1)
		s.writeFence(w)
	default:
		writeStubErr(w, 404, "404", "", "unscripted "+r.Method+" "+r.URL.Path)
	}
}

func (s *rollbackStub) writeFence(w http.ResponseWriter) {
	if !s.rolledBack.Load() {
		writeStubJSON(w, 200, map[string]any{"head_seq": 0})
		return
	}
	writeStubErr(w, 403, "CORE-403", ReasonSecretRolledBack, "cached credential predates rollback boundary")
}

func writeStubJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeStubErr(w http.ResponseWriter, status int, code, reason, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "reason": reason, "detail": detail}})
}

func TestSecretRolledBackClassification(t *testing.T) {
	cases := []struct {
		code, reason string
		want         bool
	}{
		{"CORE-403", "secret_rolled_back", true},      // execution face (end-generation)
		{"SYNC-403", "secret_rolled_back", true},      // publish face (block/manifest fencing)
		{"CORE-403", "stale_generation", false},       // takeover race: local seal path, NOT this one
		{"CORE-403", "secret_rotated_expired", false}, // grace rotation: re-authorize path
		{"CORE-403", "forbidden", false},
		{"HTTP-500", "", false},
	}
	for _, c := range cases {
		err := &aihub.APIError{Code: c.code, Reason: c.reason, Detail: "x"}
		if got := isSecretRolledBack(err); got != c.want {
			t.Errorf("isSecretRolledBack(%s %s) = %v, want %v", c.code, c.reason, got, c.want)
		}
	}
	if isSecretRolledBack(nil) {
		t.Error("nil error must not classify")
	}
	if isSecretRolledBack(errors.New("plain transport error")) {
		t.Error("transport error must not classify")
	}
	// Wrapped API errors classify too (publish wraps its remotes' errors).
	if !isSecretRolledBack(fmt.Errorf("publish: %w", &aihub.APIError{Code: "SYNC-403", Reason: ReasonSecretRolledBack})) {
		t.Error("wrapped APIError must classify via errors.As")
	}
}

// TestRollbackQuarantineIntegration: full 04 §6.1 spawn against the stub,
// one healthy publish, then the server-side rollback lands and the next
// publish write is answered secret_rolled_back → the daemon must isolate the
// scene and refuse any auto re-claim.
func TestRollbackQuarantineIntegration(t *testing.T) {
	wi := "wi_rb01"
	stub := &rollbackStub{wi: wi}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, "skills", "pf-execute"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\nrun\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "workflows"), 0o755); err != nil {
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
		Adapter:       "pi", Version: "rb-test",
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
	// The agent "works": one healthy publish lands (HEAD bootstrap).
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	exec := &capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: wi, Generation: 1,
		CurrentStep: "code", WorkflowStatus: capsule.StatusRunning,
		Steps: map[string]*capsule.Step{"code": {Status: capsule.StepInProgress}}, Version: 1,
	}
	if err := capc.SaveExecution(exec); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PublishRequest(ctx, wi, "step_completed"); err != nil {
		t.Fatalf("pre-rollback publish must succeed: %v", err)
	}

	// The server-side rollback lands: every subsequent token-bearing write
	// is answered CORE-403 reason=secret_rolled_back.
	stub.rolledBack.Store(true)
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	exec.Version = 2
	if err := capc.SaveExecution(exec); err != nil {
		t.Fatal(err)
	}
	_, perr := d.PublishRequest(ctx, wi, "step_completed")
	if perr == nil {
		t.Fatal("the rolled-back publish must fail")
	}
	if !errors.Is(perr, ErrSecretRolledBack) {
		t.Fatalf("the failure must surface ErrSecretRolledBack, got: %v", perr)
	}

	// 04 §7.3 step 2: the WI Agent process group was terminated.
	if agent.Alive() {
		t.Fatal("the WI Agent must be terminated on rollback")
	}
	// Step 3: the capsule is QUARANTINED (moved + marked, not deleted): the
	// live path is gone, a _quarantine copy exists, and its marker names
	// the cause.
	if _, err := os.Stat(capc.Dir); !os.IsNotExist(err) {
		t.Fatalf("capsule dir must be moved away, stat err = %v", err)
	}
	qglob := filepath.Join(cfg.WorkspaceRoot, "capsules", "_quarantine", wi, "*")
	matches, _ := filepath.Glob(qglob)
	if len(matches) != 1 {
		t.Fatalf("exactly one quarantined copy expected, got %v", matches)
	}
	raw, err := os.ReadFile(filepath.Join(matches[0], "logs", "quarantine.json"))
	if err != nil {
		t.Fatalf("quarantine marker missing: %v", err)
	}
	var marker struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Reason != ReasonSecretRolledBack {
		t.Fatalf("marker must name the cause, got: %s", raw)
	}
	// Step 3 (state side): the jobs[] record is gone.
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.FindJob(wi) != nil {
		t.Fatal("jobs[] entry must be removed from daemon/state.json")
	}
	if d.jobq.IsRunning(wi) || d.jobq.LenPending() != 0 {
		t.Fatal("no queue slot or pending entry may survive the rollback")
	}
	// Step 4: NO auto re-claim — the daemon does not re-derive, re-queue or
	// retry anything for this WI; the next publish_request finds no local
	// job at all (the only recovery is an admin force-takeover elsewhere).
	if len(spawner.spawns) != 1 {
		t.Fatalf("no re-derivation may happen, spawns = %d", len(spawner.spawns))
	}
	if _, err := d.PublishRequest(ctx, wi, "step_completed"); err == nil || !strings.Contains(err.Error(), "DAEMON-404") {
		t.Fatalf("post-rollback publish must report no local job, got: %v", err)
	}
}

// TestBeatTrackerStaleListing: the stale-heartbeat status signal (04 §7.2/§9).
func TestBeatTrackerStaleListing(t *testing.T) {
	tr := NewBeatTracker()
	now := time.Now()
	// Fresh beats: nothing stale.
	tr.Record("mac_fresh", now)
	// A machine silent past the threshold: listed.
	tr.Record("mac_stale", now.Add(-10*time.Minute))
	// An unobserved machine: unknown is not stale (not listed).
	got := tr.Stale(now, 3*time.Minute)
	if len(got) != 1 || got[0].MachineID != "mac_stale" {
		t.Fatalf("stale listing: %+v", got)
	}
	if got[0].AgeSeconds < 590 || got[0].LastHeartbeatAt == "" {
		t.Fatalf("stale row must carry age and last beat: %+v", got[0])
	}
	// Threshold boundary: exactly-at-threshold is NOT stale (exceeds only).
	tr2 := NewBeatTracker()
	tr2.Record("m", now.Add(-3*time.Minute))
	if l := tr2.Stale(now, 3*time.Minute); len(l) != 0 {
		t.Fatalf("at-threshold must not be listed: %+v", l)
	}
	// Empty machine id is refused (no anonymous rows).
	tr2.Record("", now)
	if l := tr2.Stale(now.Add(time.Hour), time.Minute); len(l) != 1 {
		t.Fatalf("only real machines listed: %+v", l)
	}
}
