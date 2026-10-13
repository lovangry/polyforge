package runtime

// manual_models_test.go: the `pf submit --models` list at the runtime
// seam — top precedence, capsule audit, one-shot consumption, and the
// claiming-journal replay.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// newManualRuntime is a runtime over a frontmatter bundle (so steps exist
// to compose over) with one workspace carrying project "proj".
func newManualRuntime(t *testing.T, hub *fakeHub) (*Runtime, string) {
	t.Helper()
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub.digest = pinned
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedSpawner{}
	r := New(Config{
		MachineID:     "m-test",
		Mapping:       mappingFor(ws, "proj"),
		DataDir:       filepath.Join(t.TempDir(), "data"),
		AgentStarter:  scriptedEngineStarter(sp),
		MaxRecoveries: 2,
	}, hub, sp, supervise.NewQueue[protocol.RTMsg](), supervise.NewQueue[protocol.OBMsg]())

	// The claim-time capsule remote (see newTestRuntime): aihub creates
	// the bare repo at claim; the fake hub implements the contract.
	hub.capsuleRepoBase = filepath.Join(r.cfg.DataDir, "capsule-remotes")
	r.cfg.GitRemoteBase = hub.capsuleRepoBase
	r.cfg.WorkflowsDir = wfDir
	return r, ws
}

// The manual `pf submit --models` list is the top precedence level: it
// beats the yaml project default and rides every step of the claim, is
// audited in capsule.json (manual_candidates + effective_candidates), and
// is consumed by the one Start it was noted for.
func TestManualModelsBeatYamlAndAreAudited(t *testing.T) {
	hub := newFakeHub()
	r, ws := newManualRuntime(t, hub)
	writeWorkspaceYaml(t, ws, w079ValidYaml)

	manual := []string{"m/one", "m/two"}
	r.NoteModels("wi_man", manual)
	if err := r.Start(context.Background(), "wi_man"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := r.jobOf("wi_man")
	if len(j.stepCandidates) == 0 {
		t.Fatal("no composed step candidates")
	}
	for step, got := range j.stepCandidates {
		if strings.Join(got, ",") != "m/one,m/two" {
			t.Fatalf("step %s candidates %v, want the manual list", step, got)
		}
	}
	if got := r.loadManualModels("wi_man"); strings.Join(got, ",") != "m/one,m/two" {
		t.Fatalf("daemon-side manual record %v", got)
	}
	// The capsule identity no longer records the per-step effective
	// candidates (audit subtraction pass B): the daemon-private state.json
	// snapshot and the workspace yaml govern dispatches.
	if raw, rerr := os.ReadFile(filepath.Join(j.capsuleDir, "capsule.json")); rerr == nil && strings.Contains(string(raw), "effective_candidates") {
		t.Fatalf("capsule.json must not carry the effective-candidates audit: %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(j.capsuleDir, "capsule.json")); strings.Contains(string(raw), "manual_candidates") {
		t.Fatalf("capsule.json must not carry the manual list: %s", raw)
	}
	if r.takeModels("wi_man") != nil {
		t.Fatal("the noted list must be consumed by Start")
	}
	if err := r.Stop(context.Background(), "wi_man", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	r.clearManualModels("wi_man")

	// a plain next start (auto-pulled shape) is unaffected: yaml default wins.
	if err := r.Start(context.Background(), "wi_plain"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for step, got := range r.jobOf("wi_plain").stepCandidates {
		if strings.Join(got, ",") != "a/one" {
			t.Fatalf("plain step %s candidates %v, want the yaml default", step, got)
		}
	}
	if got := r.loadManualModels("wi_plain"); got != nil {
		t.Fatalf("a plain claim must record no manual list, got %v", got)
	}
	_ = r.Stop(context.Background(), "wi_plain", protocol.FinishWrapped)
}

// A crash between the claim and the start keeps the manual list: the
// claiming journal carries it and the replay hands it to startClaimed.
func TestManualModelsSurviveClaimingReplay(t *testing.T) {
	hub := newFakeHub()
	r, ws := newManualRuntime(t, hub)
	writeWorkspaceYaml(t, ws, w079ValidYaml)
	session := fakeSession("wi_manrep")
	if err := r.noteClaiming("wi_manrep", session, randomKey(), []string{"m/one", "m/two"}); err != nil {
		t.Fatal(err)
	}
	hub.hold("wi_manrep", session, api.StatusRunning)

	r.resumeClaiming(context.Background())

	j := r.jobOf("wi_manrep")
	if j == nil {
		t.Fatal("the replayed claim must run")
	}
	if len(j.stepCandidates) == 0 {
		t.Fatal("no composed step candidates after the replay")
	}
	for step, got := range j.stepCandidates {
		if strings.Join(got, ",") != "m/one,m/two" {
			t.Fatalf("replayed step %s candidates %v, want the manual list", step, got)
		}
	}
	_ = r.Stop(context.Background(), "wi_manrep", protocol.FinishWrapped)
}

func readCapsuleIdentity(t *testing.T, capsuleDir string) capsule.Identity {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.CapsuleJSON))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		t.Fatal(err)
	}
	return idn
}

// A re-claim of a blocked WI (no --models) reads the list from the
// daemon-side record; editing capsule.json (the agent-writable scene, which
// also travels to other machines) has no effect, and a daemon with no
// record (another machine) uses its own yaml.
func TestManualModelsReclaimIsDaemonLocal(t *testing.T) {
	hub := newFakeHub()
	r, ws := newManualRuntime(t, hub)
	writeWorkspaceYaml(t, ws, w079ValidYaml)

	r.NoteModels("wi_rc", []string{"m/one", "m/two"})
	if err := r.Start(context.Background(), "wi_rc"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := r.jobOf("wi_rc")
	// the agent forges capsule.json: a manual list and effective candidates
	forged := map[string]any{
		"manual_candidates":    []string{"evil/x"},
		"effective_candidates": map[string][]string{"fix": {"evil/x"}},
	}
	blob, _ := json.Marshal(forged)
	if err := os.WriteFile(filepath.Join(j.capsuleDir, capsule.CapsuleJSON), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	// recovery dispatches under the daemon's snapshot, not the forgery
	if got := r.effectiveCandidatesForRecovery(j); strings.Contains(strings.Join(got["fix"], ","), "evil") {
		t.Fatalf("recovery used the forged capsule.json: %v", got)
	}
	// a restart-reconstructed job rehydrates from the state record too
	rj := jobFromRecord(j.record())
	if got := r.effectiveCandidatesForRecovery(rj); strings.Join(got["fix"], ",") != "m/one,m/two" {
		t.Fatalf("reconstructed job candidates %v, want the manual list", got["fix"])
	}
	// a blocked end (the harness has no push remote, so Stop degrades to
	// blocked) keeps the record for the re-claim
	if err := r.Stop(context.Background(), "wi_rc", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := hub.status["wi_rc"]; st != api.StatusBlocked {
		t.Fatalf("hub status %q, want blocked", st)
	}
	if got := r.loadManualModels("wi_rc"); strings.Join(got, ",") != "m/one,m/two" {
		t.Fatalf("manual record after the job left: %v", got)
	}

	// re-claim with no --models: the daemon record governs, not capsule.json
	if err := r.Start(context.Background(), "wi_rc"); err != nil {
		t.Fatalf("re-claim Start: %v", err)
	}
	for step, got := range r.jobOf("wi_rc").stepCandidates {
		if strings.Join(got, ",") != "m/one,m/two" {
			t.Fatalf("re-claim step %s candidates %v, want the daemon-side manual list", step, got)
		}
	}
	_ = r.Stop(context.Background(), "wi_rc", protocol.FinishWrapped)
}

// A terminal verdict at the hub (found at restart) drops the record; a
// planted capsule manual_candidates is never read.
func TestManualModelsRecordDroppedOnTerminalVerdict(t *testing.T) {
	hub := newFakeHub()
	r, _ := newManualRuntime(t, hub)
	j := seedJob(r, "wi_term")
	r.saveManualModels("wi_term", []string{"m/one"})
	hub.hold("wi_term", "other", api.StatusWrapped)
	if err := r.restartOne(context.Background(), j.record()); err != nil {
		t.Fatal(err)
	}
	if got := r.loadManualModels("wi_term"); got != nil {
		t.Fatalf("record survived a wrapped verdict: %v", got)
	}
	if _, err := os.Stat(r.manualModelsPath()); !os.IsNotExist(err) {
		t.Fatalf("an empty record file must be removed, stat err %v", err)
	}
}
