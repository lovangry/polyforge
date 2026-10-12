package daemon

// modes_test.go: the auto switch + admission (v2-converge §5) and the
// auto pull.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

func TestModeCtlSwitchAndAdmission(t *testing.T) {
	m := NewModeCtl()
	if m.Auto() || m.HeartbeatMode() != "manual" {
		t.Fatalf("start: auto=%v hb=%q, want off/manual", m.Auto(), m.HeartbeatMode())
	}
	if m.AdmitAutoPull() {
		t.Fatal("auto pull admitted with auto off")
	}
	if err := m.AdmitSubmit(); err != nil {
		t.Fatalf("submit with auto off: %v", err)
	}
	if err := m.SetAuto(true); err != nil {
		t.Fatal(err)
	}
	if !m.AdmitAutoPull() || m.HeartbeatMode() != "auto" {
		t.Fatalf("auto on: pull=%v hb=%q", m.AdmitAutoPull(), m.HeartbeatMode())
	}
	// Manual submit stays legal while auto is on (no DAEMON-409).
	if err := m.AdmitSubmit(); err != nil {
		t.Fatalf("submit with auto on: %v", err)
	}
	// Auto is freely reversible.
	if err := m.SetAuto(false); err != nil || m.Auto() {
		t.Fatalf("auto off again: %v auto=%v", err, m.Auto())
	}
	// The shutting_down latch gates every source.
	_ = m.SetAuto(true)
	m.SetShuttingDown()
	if err := m.AdmitSubmit(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("submit during shutdown = %v, want 410", err)
	}
	if m.AdmitAutoPull() {
		t.Fatal("auto pull admitted during shutdown")
	}
	if err := m.SetAuto(false); !errors.Is(err, errShuttingDown) {
		t.Fatalf("SetAuto during shutdown = %v, want 410", err)
	}
}

func bootCfg(t *testing.T, data string, auto *bool) (*Daemon, error) {
	t.Helper()
	dir := filepath.Dir(data)
	ws := filepath.Join(dir, "ws")
	for _, d := range []string{ws, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return New(Config{
		MachineID:     "m-test",
		Mapping:       mappingFor(ws),
		DataDir:       data,
		GitRemoteBase: filepath.Join(dir, "hub"),
		Auto:          auto,
		Hub:           newFakeHub(),
	})
}

// TestNewKeepsPersistedAutoAcrossRestart (#18): an unset -auto keeps the
// persisted switch; an explicit flag overrides it AND is persisted.
func TestNewKeepsPersistedAutoAcrossRestart(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	on, off := true, false

	d1, err := bootCfg(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d1.modes.Auto() {
		t.Fatal("first boot: auto must default off")
	}
	if err := d1.modes.SetAuto(true); err != nil {
		t.Fatal(err)
	}
	d1.persistModeQuietly()

	d2, _ := bootCfg(t, data, nil)
	if !d2.modes.Auto() {
		t.Fatal("restart with flag unset: persisted auto-on must win")
	}
	d3, _ := bootCfg(t, data, &on)
	if !d3.modes.Auto() {
		t.Fatal("explicit auto on over auto on")
	}
	d4, _ := bootCfg(t, data, &off)
	if d4.modes.Auto() {
		t.Fatal("explicit auto off must override the persisted on")
	}
	d4b, _ := bootCfg(t, data, nil)
	if d4b.modes.Auto() {
		t.Fatal("the flag override must land in daemon-state.json (fx3 gap)")
	}
}

func seedReady(td *testDaemon, t *testing.T, ids ...string) {
	t.Helper()
	var items []api.WorkItem
	for _, id := range ids {
		mkBare(t, td.cfg.GitRemoteBase, id)
		items = append(items, api.WorkItem{ID: id, Slug: id, ProjectName: "proj", Status: api.StatusQueued,
			Priority: api.PriorityNormal, BundleName: strp("demo-bundle")})
	}
	td.hub.mu.Lock()
	td.hub.queues = items
	td.hub.mu.Unlock()
}

func liveCount(td *testDaemon) int {
	n := 0
	for _, ji := range td.rt.List() {
		_ = ji
		n++
	}
	return n
}

// TestAutoOffPullsNothing: with auto off the daemon never touches the
// ready queue.
func TestAutoOffPullsNothing(t *testing.T) {
	td := newTestDaemon(t, nil)
	seedReady(td, t, "wi_a1", "wi_a2")
	time.Sleep(300 * time.Millisecond) // ~30 fetch ticks at 10ms
	if n := liveCount(td); n != 0 || td.q.LenPending() != 0 {
		t.Fatalf("auto off pulled work: live=%d pending=%d", n, td.q.LenPending())
	}
}

// TestAutoOnPullsUpToMax: auto on pulls the ready queue itself, running
// at most max jobs; a manual submit is still accepted meanwhile.
func TestAutoOnPullsUpToMax(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) { c.MaxConcurrent = 2 })
	seedReady(td, t, "wi_a1", "wi_a2", "wi_a3")
	if err := td.modes.SetAuto(true); err != nil {
		t.Fatal(err)
	}
	td.waitLive(t, "wi_a1")
	td.waitLive(t, "wi_a2")
	time.Sleep(200 * time.Millisecond)
	if n := liveCount(td); n != 2 {
		t.Fatalf("live = %d, want exactly max=2", n)
	}
	for _, ji := range td.rt.List() {
		if ji.Origin == nil || ji.Origin.Kind != "auto" {
			t.Fatalf("auto-pulled job %s origin = %+v, want kind auto", ji.WorkItem, ji.Origin)
		}
	}
	mkBare(t, td.cfg.GitRemoteBase, "wi_manual")
	if _, err := td.client.Claim("wi_manual", "normal"); err != nil {
		t.Fatalf("manual submit while auto on: %v", err)
	}
}

// TestMaxConcurrentCountsJobsStartedOutsideTheQueue: jobs the runtime holds
// without the dispatcher having started them (restart recovery,
// resume-blocked, repair) occupy slots too — `max N` must hold after a
// restart, so further submits stay queued.
func TestMaxConcurrentCountsJobsStartedOutsideTheQueue(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) { c.MaxConcurrent = 2 })
	for _, wi := range []string{"wi_rec1", "wi_rec2"} {
		mkBare(t, td.cfg.GitRemoteBase, wi)
		if err := td.rt.Start(td.runCtx, wi); err != nil {
			t.Fatalf("start %s directly: %v", wi, err)
		}
	}
	for _, wi := range []string{"wi_q1", "wi_q2"} {
		mkBare(t, td.cfg.GitRemoteBase, wi)
		if _, err := td.client.Claim(wi, "normal"); err != nil {
			t.Fatalf("claim %s: %v", wi, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := liveCount(td); n != 2 {
		t.Fatalf("live = %d, want max=2 (recovered jobs must hold their slots)", n)
	}
	if td.q.LenPending() != 2 {
		t.Fatalf("pending = %d, want 2 queued submits", td.q.LenPending())
	}
}

// TestInvalidWorkspaceYamlRefusedBeforeClaim: an invalid .polyforge.yaml
// makes the manual submit answer DAEMON-414 synchronously and makes
// auto-pull skip the project — nothing is claimed at the hub either way
// (the WI stays queued for other machines); fixing the file lets the
// next wave pull it.
func TestInvalidWorkspaceYamlRefusedBeforeClaim(t *testing.T) {
	td := newTestDaemon(t, nil)
	ws, err := td.cfg.Mapping.WorkspaceFor("proj")
	if err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(ws, ".polyforge.yaml")
	if err := os.WriteFile(yamlPath, []byte("workflow_defaults:\n  model_candidates: [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkBare(t, td.cfg.GitRemoteBase, "wi_manual")
	_, cerr := td.client.Claim("wi_manual", "normal")
	var ae *api.Error
	if !errors.As(cerr, &ae) || ae.Code != api.ErrDaemonWorkspaceYaml {
		t.Fatalf("submit over an invalid yaml = %v, want DAEMON-414", cerr)
	}
	seedReady(td, t, "wi_a1")
	if err := td.modes.SetAuto(true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // ~30 drain ticks
	td.hub.mu.Lock()
	claims := len(td.hub.claims)
	td.hub.mu.Unlock()
	if claims != 0 || liveCount(td) != 0 || td.q.LenPending() != 0 {
		t.Fatalf("invalid yaml must claim and enqueue nothing: claims=%d live=%d pending=%d", claims, liveCount(td), td.q.LenPending())
	}
	if err := os.WriteFile(yamlPath, []byte("models:\n  proj:\n    code: [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	td.waitLive(t, "wi_a1")
}
