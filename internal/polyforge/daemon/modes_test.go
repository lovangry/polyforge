package daemon

// modes_test.go: the auto switch + admission (v2-converge §5), the
// daemon-state migration, the legacy -mode alias and the auto pull.

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestDaemonStateMigratesLegacyMode: a pre-v2 daemon-state.json carries
// only "mode"; drain -> auto on, interactive/batch -> off; a rewrite drops
// the legacy field.
func TestDaemonStateMigratesLegacyMode(t *testing.T) {
	cases := []struct {
		legacy string
		want   bool
	}{{"drain", true}, {"interactive", false}, {"batch", false}, {"", false}}
	for _, c := range cases {
		t.Run(c.legacy, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(data, 0o755); err != nil {
				t.Fatal(err)
			}
			body := `{"version":1,"machine_id":"m-test","mode":"` + c.legacy + `","max_concurrent_jobs":3}`
			if err := os.WriteFile(filepath.Join(data, daemonStateFile), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			d, err := bootCfg(t, data, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d.modes.Auto() != c.want {
				t.Fatalf("legacy %q -> auto=%v, want %v", c.legacy, d.modes.Auto(), c.want)
			}
			if d.q.MaxConcurrent() != 3 {
				t.Fatalf("max = %d, want 3 (carried over)", d.q.MaxConcurrent())
			}
			d.persistModeQuietly()
			raw, _ := os.ReadFile(filepath.Join(data, daemonStateFile))
			if strings.Contains(string(raw), `"mode"`) || !strings.Contains(string(raw), `"auto"`) {
				t.Fatalf("rewritten state not in the new shape: %s", raw)
			}
			// A second load reads the new shape (no re-migration).
			d2, _ := bootCfg(t, data, nil)
			if d2.modes.Auto() != c.want {
				t.Fatalf("reload after rewrite: auto=%v, want %v", d2.modes.Auto(), c.want)
			}
		})
	}
}

// TestResolveAutoLegacyModeAlias: the deprecated -mode flag maps to -auto
// and warns; an explicit -auto wins; unknown values are refused.
func TestResolveAutoLegacyModeAlias(t *testing.T) {
	parse := func(args ...string) (*bool, string, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		auto := fs.Bool("auto", false, "")
		mode := fs.String("mode", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		got, err := resolveAuto(fs, *auto, *mode, &sb)
		return got, sb.String(), err
	}
	cases := []struct {
		args []string
		want string // "", "on", "off"
		warn bool
	}{
		{nil, "", false},
		{[]string{"-auto"}, "on", false},
		{[]string{"-auto=false"}, "off", false},
		{[]string{"-mode", "drain"}, "on", true},
		{[]string{"-mode", "interactive"}, "off", true},
		{[]string{"-mode", "batch"}, "off", true},
		{[]string{"-mode", "drain", "-auto=false"}, "off", true},
	}
	for _, c := range cases {
		got, warn, err := parse(c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		have := ""
		if got != nil {
			have = map[bool]string{true: "on", false: "off"}[*got]
		}
		if have != c.want || (warn != "") != c.warn {
			t.Fatalf("%v -> %q warn=%q, want %q warn=%v", c.args, have, warn, c.want, c.warn)
		}
		if c.warn && !strings.Contains(warn, "deprecated") {
			t.Fatalf("%v: warning %q lacks 'deprecated'", c.args, warn)
		}
	}
	if _, _, err := parse("-mode", "turbo"); err == nil {
		t.Fatal("unknown legacy mode must be refused")
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
