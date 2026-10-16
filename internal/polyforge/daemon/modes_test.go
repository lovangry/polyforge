package daemon

// modes_test.go: the auto switch + admission (v2-converge §5) and the
// auto pull.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/observe"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
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
	if _, err := td.client.ClaimWithModels("wi_manual", "normal", nil); err != nil {
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
		if _, err := td.client.ClaimWithModels(wi, "normal", nil); err != nil {
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
	_, cerr := td.client.ClaimWithModels("wi_manual", "normal", nil)
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
	if err := os.WriteFile(yamlPath, []byte("steps:\n  proj:\n    fix: { models: [\"pi:x\"] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	td.waitLive(t, "wi_a1")
}

// TestTransientProbeFailureKeepsSubmitQueued: an explicit submit whose
// pre-claim hub probe hits a transient blip at dispatch time is not
// dropped — it stays queued, no submit_rejected is recorded, and the next
// dispatch tick claims it once the hub answers again.
func TestTransientProbeFailureKeepsSubmitQueued(t *testing.T) {
	td := newTestDaemon(t, nil)
	mkBare(t, td.cfg.GitRemoteBase, "wi_blip")
	// call 1 is the submit route's own (advisory) probe, call 2 the
	// dispatcher's Start probe: both fail; the third succeeds.
	td.hub.mu.Lock()
	td.hub.probeFails = 2
	td.hub.mu.Unlock()
	if _, err := td.client.ClaimWithModels("wi_blip", "normal", nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	td.waitLive(t, "wi_blip")
	for _, e := range td.obs.Events().SinceAfter("wi_blip", 0) {
		if e.Kind == observe.EvSubmitRejected {
			t.Fatalf("a transient probe failure must not reject the submit: %+v", e)
		}
	}
}

// DAEMON-503 backoff: 1s, 2s, 4s ... capped at 30s.
func TestDeferralBackoffGrowsAndCaps(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if got := deferralBackoff(i + 1); got != w*time.Second {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if got := deferralBackoff(1000); got != deferralMax {
		t.Fatalf("backoff(1000) = %v, want the cap", got)
	}
}

// A deferred entry is skipped by Take until its NotBefore, so another
// pending entry (even a lower-priority one) proceeds; the deferred entry is
// never dropped, keeps its rank, and its backoff persists with it.
func TestRequeueBackoffDoesNotBlockOthers(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	q := NewJobQueue(4, func() map[string]bool { return map[string]bool{} })
	q.now = func() time.Time { return now }
	var persisted []qEntry
	q.persist = func(es []qEntry) error { persisted = append([]qEntry(nil), es...); return nil }

	if err := q.Enqueue(qEntry{WorkItemID: "wi_bad", Priority: "urgent"}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(qEntry{WorkItemID: "wi_ok", Priority: "low"}); err != nil {
		t.Fatal(err)
	}
	e := q.Take()
	if e == nil || e.WorkItemID != "wi_bad" || e.Deferrals != 0 {
		t.Fatalf("first take = %+v, want wi_bad (urgent)", e)
	}
	if err := q.Requeue("wi_bad"); err != nil {
		t.Fatal(err)
	}
	// the persisted entry carries the backoff (survives a restart)
	var saved *qEntry
	for i := range persisted {
		if persisted[i].WorkItemID == "wi_bad" {
			saved = &persisted[i]
		}
	}
	if saved == nil || saved.Deferrals != 1 || !saved.NotBefore.Equal(now.Add(time.Second)) {
		t.Fatalf("persisted wi_bad = %+v, want deferrals 1 and not_before now+1s", saved)
	}
	// not due: the lower-priority entry proceeds, then nothing is takeable
	if e := q.Take(); e == nil || e.WorkItemID != "wi_ok" {
		t.Fatalf("second take = %+v, want wi_ok", e)
	}
	if e := q.Take(); e != nil {
		t.Fatalf("a not-due entry was taken: %+v", e)
	}
	if !q.IsPending("wi_bad") {
		t.Fatal("the deferred entry must stay pending")
	}
	// due again: back first by rank; a second deferral doubles the wait
	now = now.Add(time.Second)
	e = q.Take()
	if e == nil || e.WorkItemID != "wi_bad" || e.Deferrals != 1 {
		t.Fatalf("take after the backoff = %+v", e)
	}
	_ = q.Requeue("wi_bad")
	if e := q.Take(); e != nil {
		t.Fatalf("second backoff not honored: %+v", e)
	}
	now = now.Add(2*time.Second - time.Millisecond)
	if e := q.Take(); e != nil {
		t.Fatalf("taken 1ms early: %+v", e)
	}
	now = now.Add(time.Millisecond)
	if e := q.Take(); e == nil {
		t.Fatal("not taken once due")
	}
}

// TestEnqueueIfAtomicWithAutoOffPurge pins the B3 race closure
// (wi_seWTgHeKtaa8): the auto wave's switch re-check and its append are
// one step under the queue lock, so an auto-off flip that lands right
// after the re-check passed still purges the entry, and a flip that
// landed before refuses it. Deterministic: the flip is started from
// inside admit (i.e. while the queue lock is held).
func TestEnqueueIfAtomicWithAutoOffPurge(t *testing.T) {
	m := NewModeCtl()
	if err := m.SetAuto(true); err != nil {
		t.Fatal(err)
	}
	q := NewJobQueue(4, func() map[string]bool { return map[string]bool{} })
	auto := qEntry{WorkItemID: "wi_a", Origin: runtime.Origin{Kind: runtime.OriginAuto}}

	// flip lands between the re-check and the append
	done := make(chan []qEntry)
	ok, err := q.EnqueueIf(auto, func() bool {
		admitted := m.AdmitAutoPull()
		go func() {
			_ = m.SetAuto(false)
			dropped, _ := q.PurgeOrigin(runtime.OriginAuto)
			done <- dropped
		}()
		return admitted
	})
	if err != nil || !ok {
		t.Fatalf("EnqueueIf with auto on = %v, %v; want admitted", ok, err)
	}
	dropped := <-done
	if len(dropped) != 1 || q.IsPending("wi_a") {
		t.Fatalf("auto-off purge after an admitted enqueue dropped %d entries, pending=%v; want the entry purged", len(dropped), q.IsPending("wi_a"))
	}

	// flip landed before: the conditional enqueue refuses
	ok, err = q.EnqueueIf(auto, m.AdmitAutoPull)
	if err != nil || ok || q.IsPending("wi_a") {
		t.Fatalf("EnqueueIf with auto off = %v, %v (pending=%v); want refused", ok, err, q.IsPending("wi_a"))
	}
}

// polyforge#175: an installed bundle whose candidates are not
// `<harness>:<model>` refuses the manual submit synchronously (DAEMON-414
// naming file and step) and is skipped by auto-pull — never enqueued,
// never claimed, no submit_rejected churn every wave; fixing the bundle
// lets the next wave pull it.
func TestBundleCandidateFormatRefusedBeforeEnqueue(t *testing.T) {
	wf := t.TempDir()
	bundleMD := filepath.Join(wf, "demo-bundle", "workflow.md")
	if err := os.MkdirAll(filepath.Dir(bundleMD), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleMD, []byte("---\nsteps:\n  - { id: fix, model_candidates: [sub2api-glm/glm-5.3] }\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	td := newTestDaemon(t, func(c *Config) { c.WorkflowsDir = wf })
	td.hub.mu.Lock()
	td.hub.bundles = map[string]string{"wi_manual": "demo-bundle"}
	td.hub.mu.Unlock()
	mkBare(t, td.cfg.GitRemoteBase, "wi_manual")
	_, cerr := td.client.ClaimWithModels("wi_manual", "normal", nil)
	var ae *api.Error
	if !errors.As(cerr, &ae) || ae.Code != api.ErrDaemonWorkspaceYaml || !strings.Contains(ae.Detail, `step fix: model_candidates[0]: "sub2api-glm/glm-5.3" needs a harness prefix`) {
		t.Fatalf("submit on a bad bundle = %v, want DAEMON-414 naming the step", cerr)
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
		t.Fatalf("a bad bundle must claim and enqueue nothing: claims=%d live=%d pending=%d", claims, liveCount(td), td.q.LenPending())
	}
	for _, e := range td.obs.Events().SinceAfter("wi_a1", 0) {
		if e.Kind == observe.EvSubmitRejected {
			t.Fatalf("auto-pull enqueued and rejected a work item on a bad bundle: %+v", e)
		}
	}
	if err := os.WriteFile(bundleMD, []byte("---\nsteps:\n  - { id: fix, model_candidates: [pi:sub2api-glm/glm-5.3] }\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	td.waitLive(t, "wi_a1")
}

// TestMaxCapPersistedOnlyWhenExplicit (decision 2026-10-16 G2): only an
// explicit `pf ops max N` is persisted; a daemon that never had one
// writes no cap (so the next boot takes the configured one); --reset
// returns to the configured cap and clears the persisted one; a cap an
// older daemon persisted is read as explicit.
func TestMaxCapPersistedOnlyWhenExplicit(t *testing.T) {
	dir := t.TempDir()
	boot := func(configured int) *Daemon {
		d := &Daemon{cfg: Config{DataDir: dir, MaxConcurrent: configured}, modes: NewModeCtl(),
			q: NewJobQueue(configured, func() map[string]bool { return nil })}
		if err := d.loadDaemonState(); err != nil {
			t.Fatal(err)
		}
		return d
	}
	readState := func() string {
		raw, _ := os.ReadFile(filepath.Join(dir, daemonStateFile))
		return string(raw)
	}
	d := boot(5)
	if err := d.persistDaemonState(); err != nil { // a graceful shutdown
		t.Fatal(err)
	}
	if strings.Contains(readState(), "max_concurrent_jobs") {
		t.Fatalf("a configured cap was persisted: %s", readState())
	}
	if d = boot(6); d.q.MaxConcurrent() != 6 || d.q.MaxSource() != "config" {
		t.Fatalf("the next boot must take the configured cap, got %d (%s)", d.q.MaxConcurrent(), d.q.MaxSource())
	}
	if err := d.q.SetExplicitMax(2); err != nil {
		t.Fatal(err)
	}
	_ = d.persistDaemonState()
	if d = boot(6); d.q.MaxConcurrent() != 2 || d.q.MaxSource() != "ops" {
		t.Fatalf("an explicit cap must survive restarts, got %d (%s)", d.q.MaxConcurrent(), d.q.MaxSource())
	}
	d.q.ResetMax(d.cfg.MaxConcurrent)
	_ = d.persistDaemonState()
	if d.q.MaxConcurrent() != 6 || strings.Contains(readState(), "max_concurrent_jobs") {
		t.Fatalf("reset: cap %d state %s", d.q.MaxConcurrent(), readState())
	}
	// an older daemon's every-shutdown cap: explicit
	if err := os.WriteFile(filepath.Join(dir, daemonStateFile), []byte(`{"version":1,"auto":false,"max_concurrent_jobs":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d = boot(6); d.q.MaxConcurrent() != 3 || d.q.MaxSource() != "ops" {
		t.Fatalf("a legacy persisted cap must read as explicit, got %d (%s)", d.q.MaxConcurrent(), d.q.MaxSource())
	}
}

// TestOpsMaxResetRoute: POST /ops/config max (explicit, source "ops"),
// max_reset (back to the configured cap, source "config"), and the two
// together refused.
func TestOpsMaxResetRoute(t *testing.T) {
	td := newTestDaemon(t, nil)
	seven := 7
	raw, err := td.client.OpsConfig(cli.OpsConfigRequest{Max: &seven})
	if err != nil || !strings.Contains(string(raw), `"max":7`) || !strings.Contains(string(raw), `"max_source":"ops"`) {
		t.Fatalf("explicit max: %s %v", raw, err)
	}
	raw, err = td.client.OpsConfig(cli.OpsConfigRequest{MaxReset: true})
	if err != nil || !strings.Contains(string(raw), `"max":4`) || !strings.Contains(string(raw), `"max_source":"config"`) {
		t.Fatalf("reset: %s %v", raw, err)
	}
	if _, err := td.client.OpsConfig(cli.OpsConfigRequest{Max: &seven, MaxReset: true}); err == nil {
		t.Fatal("max together with max_reset must be refused")
	}
}
