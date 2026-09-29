package daemon

// batch_test.go: the Batch wave's daemon-side halves (04 §6.1b), which had
// ZERO tests anywhere until now — GetRecommendation (the read-only
// scheduler half), BatchSubmit (the human-confirmed wave admission), and
// the hMode(get_recommendation)/hBatchSubmit faces. The pilot's Batch
// mode must not first run in production. The end-to-end operator path
// (CLI batch-confirm → real daemon process → real socket) lives in
// test/usage_s11_batch_wave_test.go; this file pins the daemon internals:
// the admission criteria (mode + shutdown), the whole-wave no-truncation
// guarantee, per-WI priority handling, and the recommendation's
// read-only-ness.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fileExecWI files one more EXECUTION ticket through the real server face
// (same shape newDaemonEnv uses for its first WI) and returns the id.
func fileExecWI(t *testing.T, e *denv, goal string) string {
	t.Helper()
	body := `{"project":"p1","goal":"` + goal + `","bundle_name":"demo","repo_inventory":[],"repo_inventory_base":[]}`
	req, err := http.NewRequest("POST", e.ts.url+"/api/v1/work-items", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer pfk_daemon_it")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("file exec wi: %d %s", resp.StatusCode, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		t.Fatalf("file exec wi decode: %s (%v)", raw, err)
	}
	return created.ID
}

// wiStatusOf reads any WI's status from the real Core DB.
func (e *denv) wiStatusOf(t *testing.T, wi string) string {
	t.Helper()
	var status string
	if err := e.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wi).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// killSpawnedAgents reaps the fake agents' child processes on cleanup so
// a wave of "sleep 60" children cannot outlive the test.
func (e *denv) killSpawnedAgents(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, a := range e.spawner.agents {
			if a != nil && a.child != nil && a.child.Process != nil {
				a.child.Process.Kill()
				a.child.Wait()
			}
		}
	})
}

// TestBatchGetRecommendationReadOnly pins the read-only half (04 §6.1b
// step 1): through BOTH the daemon method and the hMode face, the
// recommendation must surface the queued execution WIs and create
// NOTHING — no job, no claim, no state change.
func TestBatchGetRecommendationReadOnly(t *testing.T) {
	e := newDaemonEnv(t)
	e.killSpawnedAgents(t)
	ctx := context.Background()
	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	// Method half.
	rq, err := e.d.GetRecommendation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rq.Project != "p1" {
		t.Fatalf("recommendation project: %q", rq.Project)
	}
	found := false
	for _, entry := range rq.Executable {
		if entry.ID == e.wi {
			found = true
		}
	}
	if !found {
		t.Fatalf("the queued execution WI must appear in the recommendation: %+v", rq.Executable)
	}

	// Face half: POST /mode action=get_recommendation answers the same
	// body through the socket.
	var out map[string]any
	if code, err := c.do("POST", "/mode", map[string]any{"action": "get_recommendation"}, &out); err != nil || code != 200 {
		t.Fatalf("get_recommendation over the face: %d %v %v", code, err, out)
	}
	if p, _ := out["project"].(string); p != "p1" {
		t.Fatalf("face recommendation project: %v", out)
	}
	exe, _ := out["executable"].([]any)
	if len(exe) != 1 {
		t.Fatalf("face recommendation must list the one executable WI, got %v", out)
	}

	// Read-only-ness: nothing was created or claimed.
	if len(e.spawner.spawns) != 0 {
		t.Fatal("a recommendation must never spawn")
	}
	e.d.mu.Lock()
	jobs := len(e.d.jobs)
	e.d.mu.Unlock()
	if jobs != 0 {
		t.Fatal("a recommendation must never create a job")
	}
	if s := e.wiStatusOf(t, e.wi); s != "queued" {
		t.Fatalf("a recommendation must never claim; WI status: %s", s)
	}
}

// TestBatchSubmitWaveAdmissionCriteria pins the wave admission and the
// wave semantics (04 §6.1b step 2): the WHOLE wave enters the pending
// queue (never truncated, priority ordering applies), Batch mode is the
// wave's home but Interactive admits it too, Drain refuses it with the
// mode-exclusion error, and the shutdown latch refuses every source.
func TestBatchSubmitWaveAdmissionCriteria(t *testing.T) {
	e := newDaemonEnv(t)
	e.killSpawnedAgents(t)
	ctx := context.Background()
	wiB := fileExecWI(t, e, "wave member B")
	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	// Interactive (default): the wave is admitted. The whole wave enters
	// the queue; per-WI priorities override the normal default.
	if err := e.d.BatchSubmit([]string{e.wi, wiB}, map[string]string{wiB: "high"}); err != nil {
		t.Fatalf("interactive wave admission: %v", err)
	}
	// Priority ordering: the high member spawns first, FIFO within
	// priority keeps the wave's own order after that.
	ent, ok, err := e.d.SpawnNextEntry(ctx)
	if err != nil || !ok {
		t.Fatalf("first spawn: %v %v", ok, err)
	}
	if ent.WorkItemID != wiB {
		t.Fatalf("the high-priority wave member must spawn first, got %s", ent.WorkItemID)
	}
	ent, ok, err = e.d.SpawnNextEntry(ctx)
	if err != nil || !ok {
		t.Fatalf("second spawn: %v %v", ok, err)
	}
	if ent.WorkItemID != e.wi {
		t.Fatalf("the whole wave must enter the queue (never truncated), second entry %s", ent.WorkItemID)
	}
	// The wave is drained: no third entry exists.
	if _, ok, _ := e.d.SpawnNextEntry(ctx); ok {
		t.Fatal("no entries beyond the confirmed wave may exist")
	}
	for _, wi := range []string{e.wi, wiB} {
		if s := e.wiStatusOf(t, wi); s != "running" {
			t.Fatalf("wave member %s must be running after the spawn, got %s", wi, s)
		}
	}

	// The hBatchSubmit face: accepted count echoes the wave size, and the
	// bad-body refusal is DAEMON-400.
	wiC := fileExecWI(t, e, "wave member C")
	var out map[string]any
	if code, err := c.do("POST", "/batch_submit", map[string]any{
		"wi_ids": []string{wiC}, "priorities": map[string]any{wiC: "low"},
	}, &out); err != nil || code != 200 {
		t.Fatalf("batch_submit over the face: %d %v %v", code, err, out)
	}
	if n, _ := out["accepted"].(float64); n != 1 {
		t.Fatalf("accepted count must echo the wave size, got %v", out)
	}
	// A JSON scalar is not a batch_submit body → DAEMON-400.
	if code, err := c.do("POST", "/batch_submit", "not an object", &out); err != nil || code != 400 {
		t.Fatalf("malformed batch_submit: %d %v %v", code, err, out)
	}
	if code := errCodeOf(t, out); code != "DAEMON-400" {
		t.Fatalf("malformed batch_submit must be DAEMON-400, got %s", code)
	}
	if _, ok, _ := e.d.SpawnNextEntry(ctx); !ok {
		t.Fatal("the accepted face wave member must be queued")
	}

	// Batch is the wave's home mode: still admitted there (and the empty
	// wave is a no-op, not an error — the daemon never widens it).
	if err := e.d.setMode(ModeBatch); err != nil {
		t.Fatal(err)
	}
	if err := e.d.BatchSubmit(nil, nil); err != nil {
		t.Fatalf("batch-mode admission: %v", err)
	}
	if _, ok, _ := e.d.SpawnNextEntry(ctx); ok {
		t.Fatal("the empty wave must enqueue nothing")
	}

	// Drain refuses the wave with the mode-exclusion error (04 §5.2).
	if err := e.d.setMode(ModeDrain); err != nil {
		t.Fatal(err)
	}
	err = e.d.BatchSubmit([]string{e.wi}, nil)
	if !errors.Is(err, ErrModeConflict) {
		t.Fatalf("drain must refuse a wave with ErrModeConflict, got %v", err)
	}
	if code, err := c.do("POST", "/batch_submit", map[string]any{"wi_ids": []string{e.wi}}, &out); err != nil || code != 409 {
		t.Fatalf("face drain refusal: %d %v %v", code, err, out)
	}
	if code := errCodeOf(t, out); code != ErrCodeConflict {
		t.Fatalf("face drain refusal must be DAEMON-409, got %s", code)
	}

	// Abort-drain restores admission.
	if err := e.d.abortDrain(); err != nil {
		t.Fatal(err)
	}

	// The shutdown latch refuses every external source with DAEMON-410.
	e.d.modes.SetShuttingDown()
	err = e.d.BatchSubmit([]string{e.wi}, nil)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("post-shutdown wave must be refused with ErrShuttingDown, got %v", err)
	}
	if code, err := c.do("POST", "/batch_submit", map[string]any{"wi_ids": []string{e.wi}}, &out); err != nil || code != 410 {
		t.Fatalf("face post-shutdown refusal: %d %v %v", code, err, out)
	}
	if code := errCodeOf(t, out); code != ErrCodeShuttingDown {
		t.Fatalf("face post-shutdown refusal must be DAEMON-410, got %s", code)
	}
}
