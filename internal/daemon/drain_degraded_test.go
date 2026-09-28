package daemon

// drain_degraded_test.go: the drain ready-queue error triage (04 §6.1c).
// A drain machine whose identity is NOT a project member gets the typed
// CORE-404 (the server folds the canRead denial into CORE-404) — the
// daemon must surface that authorization-class denial as the
// drain_fetch_degraded signal (warn once, keep polling, auto-clear on
// the first subsequent successful fetch), while connection-class
// failures (unreachable Core / 5xx) and other 4xx verdicts keep the
// silent idle-poll behavior and never raise the flag.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr swaps os.Stderr for a pipe around fn and returns what
// fn wrote (the daemon's warning idiom is fmt.Fprintf(os.Stderr, ...)).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = pw
	defer func() { os.Stderr = saved }()
	fn()
	pw.Close()
	raw, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// drainStatusFlag reads drain_fetch_degraded off the ops face answer.
func drainStatusFlag(t *testing.T, e *denv) bool {
	t.Helper()
	var st map[string]any
	if code, err := (&udsClient{path: e.socket}).do("GET", "/status", nil, &st); err != nil || code != http.StatusOK {
		t.Fatalf("status: %d %v", code, err)
	}
	got, _ := st["drain_fetch_degraded"].(bool)
	return got
}

func TestDrainFetchDegradedOnNonMemberDenialAndAutoRepair(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	if err := e.d.setMode(ModeDrain); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()

	// Break the daemon identity's project membership server-side: the
	// ready-queue read now answers the folded non-member denial.
	if _, err := e.db.Exec(`DELETE FROM project_members WHERE project_id='pj1' AND user_id='usr_worker'`); err != nil {
		t.Fatal(err)
	}

	// (a) The denial is NOT silent: one warning naming machine, drain
	// project, and the typed Core verdict, plus the degraded flag.
	out := captureStderr(t, func() { e.d.fetchDrainWave(ctx) })
	for _, want := range []string{"drain fetch degraded", e.d.cfg.MachineID, "p1", "CORE-404"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the degradation warning must name %q:\n%s", want, out)
		}
	}
	if !drainStatusFlag(t, e) {
		t.Fatal("ops status must show drain_fetch_degraded: true after the auth-class denial")
	}

	// Warn exactly once per episode: a second failing fetch stays silent
	// and keeps the flag raised (the loop keeps polling every tick).
	if out := captureStderr(t, func() { e.d.fetchDrainWave(ctx) }); out != "" {
		t.Fatalf("a still-degraded fetch must not warn again:\n%s", out)
	}
	if !drainStatusFlag(t, e) {
		t.Fatal("the degraded flag must stay raised while fetches keep failing")
	}

	// (b) Repair the identity's membership (the server-side fix): the
	// next fetch succeeds, the flag auto-clears, and the machine fetches
	// the executable ticket normally.
	if _, err := e.db.Exec(`INSERT INTO project_members(project_id, user_id, role, created_at) VALUES ('pj1','usr_worker','writer','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	e.d.fetchDrainWave(ctx)
	if drainStatusFlag(t, e) {
		t.Fatal("the first successful fetch after the repair must clear the degraded flag")
	}
	if got := e.d.jobq.LenPending(); got != 1 {
		t.Fatalf("the repaired fetch must enqueue the executable ticket, pending=%d", got)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.d.jobq.Running(); got != 1 {
		t.Fatalf("the repaired fetch must lead to a spawned job, running=%d", got)
	}
	t.Cleanup(func() {
		for _, a := range e.spawner.agents {
			if a != nil && a.child != nil && a.child.Process != nil {
				a.child.Process.Kill()
				a.child.Wait()
			}
		}
	})
}

func TestDrainFetchDegradedNotRaisedByConnectionClass(t *testing.T) {
	ctx := context.Background()

	newDrainDaemon := func(t *testing.T, url string) *Daemon {
		t.Helper()
		dir := t.TempDir()
		d, err := New(Config{
			AIHubURL: url, APIKey: "pfk_x", MachineID: "mac_x",
			DataDir:       filepath.Join(dir, "daemon"),
			WorkspaceRoot: filepath.Join(dir, "ws"),
			DrainProject:  "p1", Adapter: "pi", Version: "it",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.setMode(ModeDrain); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Unreachable Core (connection refused): transport-class, silent.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	d := newDrainDaemon(t, dead.URL)
	if out := captureStderr(t, func() { d.fetchDrainWave(ctx) }); out != "" {
		t.Fatalf("an unreachable Core must stay silent (Drain's idle state):\n%s", out)
	}
	if d.drainFetchDegraded {
		t.Fatal("a connection-class failure must not raise the degraded flag")
	}

	// 5xx fault injection: an answered server, but the scheduler is
	// failing — still connection-class for drain idle purposes.
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("core on fire"))
	}))
	t.Cleanup(fault.Close)
	d = newDrainDaemon(t, fault.URL)
	if out := captureStderr(t, func() { d.fetchDrainWave(ctx) }); out != "" {
		t.Fatalf("a 5xx must stay silent (Drain's idle state):\n%s", out)
	}
	if d.drainFetchDegraded {
		t.Fatal("a 5xx must not raise the degraded flag")
	}

	// Other 4xx verdicts (e.g. CORE-400) are not reclassified: silent.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"CORE-400","detail":"bad request"}}`))
	}))
	t.Cleanup(bad.Close)
	d = newDrainDaemon(t, bad.URL)
	if out := captureStderr(t, func() { d.fetchDrainWave(ctx) }); out != "" {
		t.Fatalf("a CORE-400 must keep the existing silent behavior:\n%s", out)
	}
	if d.drainFetchDegraded {
		t.Fatal("a CORE-400 must not raise the degraded flag")
	}
}
