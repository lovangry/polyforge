package runtime

// workspace_integration_test.go: the polyforge#79 WI acceptance test —
// ONE daemon (one Runtime) serves TWO workspaces with TWO different
// valid yamls over the SAME bundle fixture: each workspace's WI gets its
// own model assignments (the precedence runs against its own yaml and
// its own project), and its own capsule.json audit record — no
// cross-workspace leak. Also pins the per-claim freshness observable end to end: an edit
// between claims changes the NEXT claim's audit while the earlier
// claim's audit stands.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/mapping"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// twoWorkspaceRuntime builds one Runtime whose mapping routes proj-a →
// wsA and proj-b → wsB, over the SAME frontmatter bundle fixture for
// both claims.
func twoWorkspaceRuntime(t *testing.T) (*Runtime, *fakeHub, string, string) {
	t.Helper()
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub := newFakeHub()
	hub.digest = pinned
	hub.items = map[string]*api.WorkItem{
		"wi_w079intA":  {ID: "wi_w079intA", ProjectName: "proj-a"},
		"wi_w079intB":  {ID: "wi_w079intB", ProjectName: "proj-b"},
		"wi_w079intA2": {ID: "wi_w079intA2", ProjectName: "proj-a"},
	}
	wsA := filepath.Join(t.TempDir(), "ws-a")
	wsB := filepath.Join(t.TempDir(), "ws-b")
	for _, ws := range []string{wsA, wsB} {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := mapping.Build([]mapping.Entry{
		{Workspace: wsA, Projects: []string{"proj-a"}},
		{Workspace: wsB, Projects: []string{"proj-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := &scriptedSpawner{}
	cfg := Config{
		MachineID:               "m-test",
		Mapping:                 plan,
		DataDir:                 filepath.Join(t.TempDir(), "data"),
		AgentStarter:            scriptedEngineStarter(sp),
		MaxRecoveries:           2,
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := supervise.NewQueue[protocol.RTMsg]()
	fromOB := supervise.NewQueue[protocol.OBMsg]()
	r := New(cfg, hub, sp, toOB, fromOB)
	r.cfg.WorkflowsDir = wfDir
	return r, hub, wsA, wsB
}

// TestTwoWorkspacesTwoYamlsOneDaemon: the same bundle, two workspaces,
// two yamls — different model assignments and audits, no leak.
func TestTwoWorkspacesTwoYamlsOneDaemon(t *testing.T) {
	r, _, wsA, wsB := twoWorkspaceRuntime(t)

	// wsA: a project default. wsB: a per-kind pin.
	writeWorkspaceYaml(t, wsA, "models:\n  proj-a:\n    default: [w079a-flow, w079a-flow2]\n")
	writeWorkspaceYaml(t, wsB, "models:\n  proj-b:\n    code: [w079b-code]\n")

	if err := r.Start(context.Background(), "wi_w079intA"); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	if err := r.Start(context.Background(), "wi_w079intB"); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	capA := filepath.Join(wsA, "capsules", "wi_w079intA")
	capB := filepath.Join(wsB, "capsules", "wi_w079intB")
	idnA, rawA := readIdentity(t, capA)
	idnB, rawB := readIdentity(t, capB)

	// proj-a's default governs EVERY step (it beats the bundle step-level
	// list too).
	for _, step := range []string{"grill", "fix", "release"} {
		if got := idnA.EffectiveCandidates[step]; len(got) != 2 || got[0] != "w079a-flow" {
			t.Fatalf("proj-a %s candidates %v, want [w079a-flow w079a-flow2]", step, got)
		}
	}
	// proj-b's kind pin governs every kind:code step; kind spec keeps the
	// bundle default.
	if got := idnB.EffectiveCandidates["fix"]; len(got) != 1 || got[0] != "w079b-code" {
		t.Fatalf("proj-b fix candidates %v, want [w079b-code]", got)
	}
	if got := idnB.EffectiveCandidates["grill"]; len(got) != 2 || got[0] != "prov/weak-a" {
		t.Fatalf("proj-b grill candidates %v, want the bundle default", got)
	}
	if strings.Contains(rawA, "w079b") || strings.Contains(rawB, "w079a") {
		t.Fatalf("an audit leaked the other workspace's yaml:\nA %s\nB %s", rawA, rawB)
	}

	// Per-claim freshness: an edit between claims governs the NEXT claim
	// while the earlier claim's audit stands.
	writeWorkspaceYaml(t, wsA, "models:\n  proj-a:\n    default: [w079-edit-v2]\n")
	if err := r.Start(context.Background(), "wi_w079intA2"); err != nil {
		t.Fatalf("Start A2: %v", err)
	}
	idnA2, _ := readIdentity(t, filepath.Join(wsA, "capsules", "wi_w079intA2"))
	if got := idnA2.EffectiveCandidates["grill"]; len(got) != 1 || got[0] != "w079-edit-v2" {
		t.Fatalf("second-claim grill candidates %v, want [w079-edit-v2]", got)
	}
	again, _ := readIdentity(t, capA)
	if got := again.EffectiveCandidates["grill"]; len(got) != 2 || got[0] != "w079a-flow" {
		t.Fatalf("first-claim audit changed under the edit: %v", got)
	}
	for _, wi := range []string{"wi_w079intA", "wi_w079intB", "wi_w079intA2"} {
		if err := r.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
			t.Fatalf("Stop %s: %v", wi, err)
		}
	}
}

// Another project in the SAME workspace is unaffected by a sibling's
// models entry, and a broken yaml refuses both before any claim.
func TestSiblingProjectUnaffectedAndSharedYamlRefusesBoth(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub := newFakeHub()
	hub.digest = pinned
	hub.items = map[string]*api.WorkItem{
		"wi_w079s1": {ID: "wi_w079s1", ProjectName: "p1"},
		"wi_w079s2": {ID: "wi_w079s2", ProjectName: "p2"},
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedSpawner{}
	r := New(Config{
		MachineID: "m-test", Mapping: mappingFor(ws, "p1", "p2"),
		DataDir:      filepath.Join(t.TempDir(), "data"),
		AgentStarter: scriptedEngineStarter(sp), MaxRecoveries: 2,
		RecoveryStabilityWindow: time.Nanosecond,
	}, hub, sp, supervise.NewQueue[protocol.RTMsg](), supervise.NewQueue[protocol.OBMsg]())
	r.cfg.WorkflowsDir = wfDir

	writeWorkspaceYaml(t, ws, "models:\n  p1:\n    default: [w79/p1]\n")
	if err := r.Start(context.Background(), "wi_w079s1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), "wi_w079s2"); err != nil {
		t.Fatal(err)
	}
	i1, _ := readIdentity(t, filepath.Join(ws, "capsules", "wi_w079s1"))
	i2, _ := readIdentity(t, filepath.Join(ws, "capsules", "wi_w079s2"))
	if got := i1.EffectiveCandidates["fix"]; len(got) != 1 || got[0] != "w79/p1" {
		t.Fatalf("p1 fix %v, want [w79/p1]", got)
	}
	if i2.EffectiveCandidates != nil {
		t.Fatalf("p2 has no models entry, must record nothing: %v", i2.EffectiveCandidates)
	}
	j2 := r.jobOf("wi_w079s2")
	j2.mu.Lock()
	if got := j2.stepCandidates["fix"]; len(got) == 0 || got[0] != "prov/flash" {
		t.Fatalf("p2 fix %v, want the bundle step-level list", got)
	}
	j2.mu.Unlock()
	for _, wi := range []string{"wi_w079s1", "wi_w079s2"} {
		if err := r.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
			t.Fatal(err)
		}
	}
}
