package daemon

// submit503_test.go: R3-F2 regression — the promised 503 contract of a
// step-0 materialization failure, asserted at the UDS face where the
// contract lives. A fresh claim whose pinned bundle is MISSING from the
// daemon's own workflows checkout fails ensureMaterialized (CAP-404);
// before the fix that failure surfaced as HTTP 500 / DAEMON-500 because
// writeSubmitErr could not recognize the error. The submit call must
// answer HTTP 503 with the DAEMON-503 code, and nothing may be derived.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSubmitMissingBundleAnswers503(t *testing.T) {
	wi := "wi_m503"
	stub := &claimRecordingStub{wi: wi}
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
	// Deliberately NO MakeDemoBundle here: the Core stub pins bundle
	// "demo", the daemon's workflows checkout cannot serve it, and the
	// step-0 materialization gate fails (CAP-404) BEFORE any derivation.
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_stub", MachineID: "mac_503",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "submit-503-test",
	}
	spawner := newIntegSpawner()
	d, err := New(cfg, spawner)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "submit503.sock")
	api, err := NewAPIServer(d, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()

	// A fresh claim over the real UDS face: the opportunistic spawn
	// inside hSubmit hits the materialization gate and the submit call
	// itself must carry the typed verdict.
	c := &udsClient{path: socket}
	var out struct {
		Error struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	code, err := c.do("POST", "/jobs/"+wi+"/claim", nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("R3-F2: a fresh claim with a missing bundle must answer HTTP 503, got %d (%s: %s)", code, out.Error.Code, out.Error.Detail)
	}
	if out.Error.Code != "DAEMON-503" {
		t.Fatalf("R3-F2: the envelope must carry DAEMON-503, got %q (%s)", out.Error.Code, out.Error.Detail)
	}
	// Fail-closed residue: nothing was derived over the missing snapshot.
	if len(spawner.spawns) != 0 {
		t.Fatalf("no WI Agent may be derived when materialization fails, spawns=%d", len(spawner.spawns))
	}
	d.mu.Lock()
	_, inRuntime := d.jobs[wi]
	d.mu.Unlock()
	if inRuntime {
		t.Fatal("no job may enter the runtime when materialization fails")
	}
	// The failed claim is not silently dropped either: the journal keeps
	// the pending intent for the next boot (04 §6.1 step 4).
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if row := st.FindPending(wi, ""); row == nil {
		t.Fatal("the failed claim's journal row must survive for the next retry")
	}
}
