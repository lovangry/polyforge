package runtime

// manual_models_test.go: the `pf submit --models` list at the runtime
// seam — top precedence, capsule audit, one-shot consumption, and the
// claiming-journal replay.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
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
		MachineID:               "m-test",
		Mapping:                 mappingFor(ws, "proj"),
		DataDir:                 filepath.Join(t.TempDir(), "data"),
		AgentStarter:            scriptedEngineStarter(sp),
		MaxRecoveries:           2,
		RecoveryStabilityWindow: time.Nanosecond,
	}, hub, sp, supervise.NewQueue[protocol.RTMsg](), supervise.NewQueue[protocol.OBMsg]())
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
	if got := readCapsuleManualModels(j.capsuleDir); strings.Join(got, ",") != "m/one,m/two" {
		t.Fatalf("capsule manual_candidates %v", got)
	}
	if got := readCapsuleEffectiveCandidates(j.capsuleDir); len(got) != len(j.stepCandidates) {
		t.Fatalf("capsule effective_candidates %v", got)
	}
	if r.takeModels("wi_man") != nil {
		t.Fatal("the noted list must be consumed by Start")
	}
	if err := r.Stop(context.Background(), "wi_man", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// a plain next start (auto-pulled shape) is unaffected: yaml default wins.
	if err := r.Start(context.Background(), "wi_plain"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for step, got := range r.jobOf("wi_plain").stepCandidates {
		if strings.Join(got, ",") != "a/one" {
			t.Fatalf("plain step %s candidates %v, want the yaml default", step, got)
		}
	}
	if got := readCapsuleManualModels(r.jobOf("wi_plain").capsuleDir); got != nil {
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
