package daemon

// finish_test.go: the 00 §5.1 pause/wrap routes (04 §6.2 A-path close over
// the operations face). Against the REAL Core: pause closes the generation
// with reason pause (WI paused server-side, sealed locally, observable
// through the layer-2 read-through afterwards); wrap closes with reason
// wrap|fail and a second wrap is the typed DAEMON-404; pause→resume→wrap
// walks the full round trip. Against a STUBBED Core: a secret_rolled_back
// on the wrap's end-generation isolates through the 04 §7.3 path
// (quarantine, no re-claim).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
)

// spawnWorkingJob drives the 04 §6.1 sequence for e.wi and makes the WI
// Agent "work" one step: a written execution.json plus one published
// revision, so the later final flush has a real HEAD to compare against.
func spawnWorkingJob(t *testing.T, e *denv) (*capsule.Capsule, *integAgent) {
	t.Helper()
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	agent := e.spawner.agents[0]
	capc, _, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: e.wi, Generation: 1,
		CurrentStep: "code", WorkflowStatus: capsule.StatusRunning,
		Steps: map[string]*capsule.Step{"code": {Status: capsule.StepInProgress}}, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.PublishRequest(ctx, e.wi, "step_completed"); err != nil {
		t.Fatalf("working publish: %v", err)
	}
	return capc, agent
}

// wiStatus reads the WI's status straight from the real Core DB.
func (e *denv) wiStatus(t *testing.T) string {
	t.Helper()
	var status string
	if err := e.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, e.wi).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// daemonStateJobs loads daemon/state.json's jobs[] from the daemon's data
// dir.
func (e *denv) daemonStateJobs(t *testing.T) []Job {
	t.Helper()
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	return st.Jobs
}

// assertSealedCapsule checks the close's local residue: capsule still on
// disk (never deleted — 04 §6.2 A step 6) but sealed read-only.
func assertSealedCapsule(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("sealed capsule must remain on disk: %v", err)
	}
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatal("capsule must be sealed read-only after the close")
	}
}

// TestPauseRouteClosesGeneration: POST /jobs/{id}/pause is the 00 §5.1
// pause verb — the 04 §6.2 A-path close with reason "pause". Server-side
// the WI must be PAUSED (not still claimed), locally sealed, and a second
// pause is the typed DAEMON-404. Afterwards the WI is observable through
// the layer-2 read-through (no local capsule anymore).
func TestPauseRouteClosesGeneration(t *testing.T) {
	e := newDaemonEnv(t)
	capc, _ := spawnWorkingJob(t, e)

	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	// Unknown WI: typed DAEMON-404 before anything else happens.
	var errBody map[string]any
	if code, err := c.do("POST", "/jobs/wi_nope01/pause", map[string]any{}, &errBody); err != nil || code != 404 {
		t.Fatalf("pause of unknown wi: %d %v", code, err)
	} else if errBody["error"].(map[string]any)["code"] != ErrCodeNotFound {
		t.Fatalf("pause of unknown wi must be DAEMON-404: %v", errBody)
	}

	// The pause itself: 200 with the close-out facts, flush_evidence
	// included (the evidence the daemon generated and submitted).
	var out FinishOutcome
	if code, err := c.do("POST", "/jobs/"+e.wi+"/pause", map[string]any{}, &out); err != nil || code != 200 {
		t.Fatalf("pause: %d %v", code, err)
	}
	if out.Reason != "pause" || out.FlushEvidence == nil || out.WorkItemID != e.wi {
		t.Fatalf("pause outcome: %+v", out)
	}
	if out.FlushEvidence.PublishedGeneration != 1 {
		t.Fatalf("flush evidence must name generation 1: %+v", out.FlushEvidence)
	}

	// Server-side: the generation ended with reason pause — the WI is
	// paused (resume is a fresh claim), NOT still claimed/running.
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after pause route: %s", s)
	}
	// Locally: jobs[] dropped from daemon state, queue slot released,
	// capsule sealed read-only.
	if jobs := e.daemonStateJobs(t); len(jobs) != 0 {
		t.Fatalf("jobs[] must be empty after pause: %+v", jobs)
	}
	if e.d.jobq.IsRunning(e.wi) || e.d.jobq.LenPending() != 0 {
		t.Fatal("no queue slot or pending entry may survive the pause")
	}
	assertSealedCapsule(t, capc.Dir)

	// Second pause: the job is sealed and gone — typed DAEMON-404.
	if code, err := c.do("POST", "/jobs/"+e.wi+"/pause", map[string]any{}, &errBody); err != nil || code != 404 {
		t.Fatalf("second pause: %d %v", code, err)
	}
	if errBody["error"].(map[string]any)["code"] != ErrCodeNotFound {
		t.Fatalf("second pause must be DAEMON-404: %v", errBody)
	}

	// Post-pause observability: no local capsule → observe answers through
	// the layer-2 Core read-through of the published HEAD (04 §5.1a).
	var obs map[string]any
	if code, err := c.do("GET", "/jobs/"+e.wi+"/observe?after=0", nil, &obs); err != nil || code != 200 {
		t.Fatalf("observe after pause: %d %v", code, err)
	}
	if obs["read_through"] != true {
		t.Fatalf("observe after pause must be layer-2 read-through: %v", obs)
	}
}

// TestWrapRouteEndsGeneration: POST /jobs/{id}/wrap with reason wrap (the
// default) ends the generation terminally.
func TestWrapRouteEndsGeneration(t *testing.T) {
	e := newDaemonEnv(t)
	capc, _ := spawnWorkingJob(t, e)

	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	var out FinishOutcome
	if code, err := c.do("POST", "/jobs/"+e.wi+"/wrap", map[string]any{"reason": "wrap"}, &out); err != nil || code != 200 {
		t.Fatalf("wrap: %d %v", code, err)
	}
	if out.Reason != "wrap" || out.FlushEvidence == nil {
		t.Fatalf("wrap outcome: %+v", out)
	}
	if s := e.wiStatus(t); s != "wrapped" {
		t.Fatalf("wi status after wrap route: %s", s)
	}
	if jobs := e.daemonStateJobs(t); len(jobs) != 0 {
		t.Fatalf("jobs[] must be empty after wrap: %+v", jobs)
	}
	assertSealedCapsule(t, capc.Dir)

	// Second wrap: typed DAEMON-404 (the job is sealed and gone).
	var errBody map[string]any
	if code, err := c.do("POST", "/jobs/"+e.wi+"/wrap", map[string]any{"reason": "wrap"}, &errBody); err != nil || code != 404 {
		t.Fatalf("second wrap: %d %v", code, err)
	}
	if errBody["error"].(map[string]any)["code"] != ErrCodeNotFound {
		t.Fatalf("second wrap must be DAEMON-404: %v", errBody)
	}

	// A bad reason is DAEMON-400 and closes nothing.
	e2 := newDaemonEnv(t)
	spawnWorkingJob(t, e2)
	api2, err := NewAPIServer(e2.d, e2.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api2.Close() })
	go api2.Serve()
	c2 := &udsClient{path: e2.socket}
	if code, err := c2.do("POST", "/jobs/"+e2.wi+"/wrap", map[string]any{"reason": "bogus"}, &errBody); err != nil || code != 400 {
		t.Fatalf("bad reason: %d %v", code, err)
	}
	if s := e2.wiStatus(t); s != "running" {
		t.Fatalf("a refused wrap must close nothing, status: %s", s)
	}
}

// TestWrapRouteReasonFail: reason "fail" is the same close with the
// terminal-failure reason.
func TestWrapRouteReasonFail(t *testing.T) {
	e := newDaemonEnv(t)
	_, _ = spawnWorkingJob(t, e)

	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	var out FinishOutcome
	if code, err := c.do("POST", "/jobs/"+e.wi+"/wrap", map[string]any{"reason": "fail"}, &out); err != nil || code != 200 {
		t.Fatalf("wrap fail: %d %v", code, err)
	}
	if out.Reason != "fail" {
		t.Fatalf("wrap outcome reason: %+v", out)
	}
	if s := e.wiStatus(t); s != "failed" {
		t.Fatalf("wi status after fail route: %s", s)
	}
}

// TestPauseThenResumeRoundTrip: pause ends the generation; resume is a
// fresh claim (generation+1) through the ordinary submit face — the
// round trip the pause semantics exist to enable.
func TestPauseThenResumeRoundTrip(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	spawnWorkingJob(t, e)

	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	var out FinishOutcome
	if code, err := c.do("POST", "/jobs/"+e.wi+"/pause", map[string]any{}, &out); err != nil || code != 200 {
		t.Fatalf("pause: %d %v", code, err)
	}
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after pause: %s", s)
	}

	// Resume: a fresh claim of the paused WI opens generation 2 (04 §5.1
	// resume row); the old sealed capsule is quarantined by capsule_create.
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "resume"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatalf("resume spawn: %v", err)
	}
	if len(e.spawner.spawns) != 2 {
		t.Fatalf("resume must derive a second persistent WI Agent: %d", len(e.spawner.spawns))
	}
	if e.spawner.spawns[1].Generation != 2 {
		t.Fatalf("resume must open generation 2: %+v", e.spawner.spawns[1])
	}
	if s := e.wiStatus(t); s != "running" {
		t.Fatalf("wi status after resume: %s", s)
	}

	// And the resumed generation wraps terminally.
	if code, err := c.do("POST", "/jobs/"+e.wi+"/wrap", map[string]any{"reason": "wrap"}, &out); err != nil || code != 200 {
		t.Fatalf("wrap after resume: %d %v", code, err)
	}
	if out.FlushEvidence == nil || out.FlushEvidence.PublishedGeneration != 2 {
		t.Fatalf("generation-2 wrap must carry generation-2 evidence: %+v", out.FlushEvidence)
	}
	if s := e.wiStatus(t); s != "wrapped" {
		t.Fatalf("wi status after wrap: %s", s)
	}
}

// TestCloseInProgressGuard: while one close is mid-flight, a second close
// of the same WI is the typed DAEMON-409 (never a racing double close).
func TestCloseInProgressGuard(t *testing.T) {
	e := newDaemonEnv(t)
	_, agent := spawnWorkingJob(t, e)

	// Park the first close inside its boundary wait so it stays
	// mid-flight until the test releases it.
	entered := make(chan struct{})
	release := make(chan struct{})
	agent.onNotifyStop = func() { close(entered); <-release }

	done := make(chan error, 1)
	go func() {
		_, err := e.d.PauseJob(context.Background(), e.wi)
		done <- err
	}()
	<-entered // the close is now parked: the guard flag is held
	if _, err := e.d.WrapJob(context.Background(), e.wi, "wrap"); err == nil || !strings.Contains(err.Error(), "DAEMON-409") {
		t.Fatalf("second close must be the typed DAEMON-409, got: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first close must finish after release: %v", err)
	}
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after the guarded pause: %s", s)
	}
}

// TestMarkJobStatusInterrupting: the close marks the jobs[] row
// interrupting (04 §4.1) — the state mark a crash mid-close would leave
// behind.
func TestMarkJobStatusInterrupting(t *testing.T) {
	e := newDaemonEnv(t)
	spawnWorkingJob(t, e)
	if j := e.daemonStateJobs(t); len(j) != 1 || j[0].Status != JobRunning {
		t.Fatalf("fresh job must be running in daemon state: %+v", j)
	}
	e.d.markJobStatus(e.wi, JobInterrupting)
	j := e.daemonStateJobs(t)
	if len(j) != 1 || j[0].Status != JobInterrupting {
		t.Fatalf("markJobStatus must persist interrupting: %+v", j)
	}
}

// ---------------------------------------------------------------------------
// secret_rolled_back on the wrap close (stubbed Core)

// finishStub is a scripted AIHub for the wrap-rollback test: claim and the
// publish face succeed, and the end-generation call answers CORE-403
// reason=secret_rolled_back once the rollback has landed — the case where
// the final flush succeeds but the close's Core write is refused.
type finishStub struct {
	rolledBack atomic.Bool
	wi         string
}

func (s *finishStub) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/work-items/wi_probe_invalid/head"):
		writeStubErr(w, 404, "404", "", "not found")
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/claim"):
		boot := map[string]any{
			"work_item_id": s.wi, "bundle_name": "demo", "bundle_digest": "sha256:demo",
			"repo_inventory": []any{},
		}
		writeStubJSON(w, 200, map[string]any{
			"generation": 1, "generation_token": "tok_g1", "secret_epoch": 1,
			"owner": "usr_worker", "capsule_bootstrap": boot,
		})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/end-generation"):
		if s.rolledBack.Load() {
			writeStubErr(w, 403, "CORE-403", ReasonSecretRolledBack, "cached credential predates rollback boundary")
			return
		}
		writeStubJSON(w, 200, map[string]any{"ok": true})
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

// TestWrapRolledBackIsolates: the wrap's end-generation answered
// secret_rolled_back → the 04 §7.3 isolation path, not a retry: DAEMON-403
// envelope, capsule quarantined (moved + marked, never deleted), jobs[]
// record dropped, no re-claim.
func TestWrapRolledBackIsolates(t *testing.T) {
	wi := "wi_wr01"
	stub := &finishStub{wi: wi}
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
	spawner := newIntegSpawner()
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_stub", MachineID: "mac_a",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "wrap-rb-test",
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

	// The server-side rollback lands BEFORE the wrap: the final flush still
	// succeeds, the end-generation is refused with secret_rolled_back.
	stub.rolledBack.Store(true)

	socket := filepath.Join(dir, "finish.sock")
	api, err := NewAPIServer(d, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: socket}

	var errBody map[string]any
	if code, err := c.do("POST", "/jobs/"+wi+"/wrap", map[string]any{"reason": "wrap"}, &errBody); err != nil || code != 403 {
		t.Fatalf("rolled-back wrap: %d %v", code, err)
	}
	apiErr := errBody["error"].(map[string]any)
	if apiErr["code"] != ErrCodeForbidden || !strings.Contains(apiErr["detail"].(string), ReasonSecretRolledBack) {
		t.Fatalf("rolled-back wrap envelope: %v", errBody)
	}

	// 04 §7.3 step 2: the WI Agent process group was terminated.
	if agent.Alive() {
		t.Fatal("the WI Agent must be terminated on rollback")
	}
	// Step 3: the capsule is QUARANTINED (moved + marked, not deleted).
	if _, err := os.Stat(capc.Dir); !os.IsNotExist(err) {
		t.Fatalf("capsule dir must be moved away, stat err = %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.WorkspaceRoot, "capsules", "_quarantine", wi, "*"))
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
	// The jobs[] record is gone.
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.FindJob(wi) != nil || d.jobq.IsRunning(wi) || d.jobq.LenPending() != 0 {
		t.Fatal("no jobs[] record, queue slot or pending entry may survive the rollback")
	}
	// Step 4: NO auto re-claim — no re-derivation, no re-queue; recovery
	// is an admin force-takeover elsewhere.
	if len(spawner.spawns) != 1 {
		t.Fatalf("no re-derivation may happen, spawns = %d", len(spawner.spawns))
	}
}
