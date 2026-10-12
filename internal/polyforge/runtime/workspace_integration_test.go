package runtime

// workspace_integration_test.go: the polyforge#79 WI acceptance test —
// ONE daemon (one Runtime) serves TWO workspaces with TWO different
// valid yamls over the SAME bundle fixture: each workspace's WI gets its
// own model assignments (the four-level precedence runs against its own
// yaml), its own cloned repo set cloned from its own resolved local
// checkouts, and its own capsule.json audit record — no cross-workspace
// leak. Also pins the per-claim freshness observable end to end: an edit
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

// TestTwoWorkspacesTwoYamlsOneDaemon is the WI's integration criterion:
// the same bundle, two workspaces, two yamls — different model
// assignments, different repo sets, different audits, no leak.
func TestTwoWorkspacesTwoYamlsOneDaemon(t *testing.T) {
	r, _, wsA, wsB := twoWorkspaceRuntime(t)

	// wsA: a flow-default yaml listing repo w79a from its own checkout.
	w079GitRepo(t, filepath.Join(wsA, ".repo", "w79a"), "w79a.txt", "ws-a source\n")
	writeWorkspaceYaml(t, wsA, "projects:\n  proj-a:\n    repos:\n      - name: w79a\n        path: .repo/w79a\nworkflow_defaults:\n  model_candidates: [w079a-flow, w079a-flow2]\n")
	// wsB: a per-kind yaml listing repo w79b from its own checkout.
	w079GitRepo(t, filepath.Join(wsB, ".repo", "w79b"), "w79b.txt", "ws-b source\n")
	writeWorkspaceYaml(t, wsB, "projects:\n  proj-b:\n    repos:\n      - name: w79b\n        path: .repo/w79b\nworkflow_defaults:\n  step_kinds:\n    code: [w079b-code]\n")

	if err := r.Start(context.Background(), "wi_w079intA"); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	if err := r.Start(context.Background(), "wi_w079intB"); err != nil {
		t.Fatalf("Start B: %v", err)
	}

	wsACapsule, err := r.cfg.Mapping.WorkspaceFor("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	wsBCapsule, err := r.cfg.Mapping.WorkspaceFor("proj-b")
	if err != nil {
		t.Fatal(err)
	}
	capA := filepath.Join(wsACapsule, "capsules", "wi_w079intA")
	capB := filepath.Join(wsBCapsule, "capsules", "wi_w079intB")
	idnA, rawA := readIdentity(t, capA)
	idnB, rawB := readIdentity(t, capB)

	// Different model assignments from the different yamls over the same
	// bundle: wsA's flow default replaces the bundle's WEAK default for
	// the kinds the bundle leaves step-level-silent (grill), while a
	// bundle step-level list still beats the yaml flow default (the S07
	// chain: fix keeps [prov/flash prov/glm]); wsB's per-kind pin governs
	// every kind:code step, its unlisted kinds keep the bundle's lists.
	if got := idnA.EffectiveCandidates["fix"]; len(got) != 2 || got[0] != "prov/flash" {
		t.Fatalf("ws-a fix candidates %v, want the bundle step-level list [prov/flash prov/glm] — it beats the yaml flow default", got)
	}
	if got := idnA.EffectiveCandidates["grill"]; len(got) != 2 || got[0] != "w079a-flow" {
		t.Fatalf("ws-a grill candidates %v, want the ws-a flow default [w079a-flow w079a-flow2] (replaces the bundle weak default)", got)
	}
	if got := idnB.EffectiveCandidates["fix"]; len(got) != 1 || got[0] != "w079b-code" {
		t.Fatalf("ws-b fix candidates %v, want the ws-b per-kind pin [w079b-code]", got)
	}
	if got := idnB.EffectiveCandidates["grill"]; len(got) != 2 || got[0] != "prov/weak-a" {
		t.Fatalf("ws-b grill candidates %v, want the bundle weak default (kind spec unlisted, no ws-b flow default)", got)
	}
	// The two workspaces genuinely disagree on the fix step's model.
	if idnA.EffectiveCandidates["fix"][0] == idnB.EffectiveCandidates["fix"][0] {
		t.Fatal("the two workspaces must carry DIFFERENT fix model assignments over the same bundle")
	}
	// ...and on the grill step's model (the yaml layer visibly at work).
	if idnA.EffectiveCandidates["grill"][0] == idnB.EffectiveCandidates["grill"][0] {
		t.Fatal("the two workspaces must carry DIFFERENT grill model assignments over the same bundle")
	}

	// Different cloned repo sets, each from its own resolved local
	// checkout — and no cross-workspace leak.
	if m, rerr := os.ReadFile(filepath.Join(capA, "repos", "w79a", "w79a.txt")); rerr != nil || string(m) != "ws-a source\n" {
		t.Fatalf("ws-a capsule did not clone its own local checkout: %q, %v", m, rerr)
	}
	if _, rerr := os.Stat(filepath.Join(capA, "repos", "w79b")); rerr == nil {
		t.Fatal("ws-a capsule leaked ws-b's repo")
	}
	if m, rerr := os.ReadFile(filepath.Join(capB, "repos", "w79b", "w79b.txt")); rerr != nil || string(m) != "ws-b source\n" {
		t.Fatalf("ws-b capsule did not clone its own local checkout: %q, %v", m, rerr)
	}
	if _, rerr := os.Stat(filepath.Join(capB, "repos", "w79a")); rerr == nil {
		t.Fatal("ws-b capsule leaked ws-a's repo")
	}
	if got := idnA.RepoSources["w79a"]; got != filepath.Join(wsA, ".repo", "w79a") {
		t.Fatalf("ws-a repo source %q, want its resolved local checkout", got)
	}
	if got := idnB.RepoSources["w79b"]; got != filepath.Join(wsB, ".repo", "w79b") {
		t.Fatalf("ws-b repo source %q, want its resolved local checkout", got)
	}

	// Each audit carries its own parsed yaml verbatim, and never the
	// other workspace's.
	if !strings.Contains(rawA, "w79a") || !strings.Contains(rawA, "w079a-flow") {
		t.Fatalf("ws-a audit does not carry its own yaml verbatim: %s", rawA)
	}
	if strings.Contains(rawA, "w79b") || strings.Contains(rawA, "w079b-code") {
		t.Fatalf("ws-a audit leaked ws-b's yaml: %s", rawA)
	}
	if !strings.Contains(rawB, "w79b") || !strings.Contains(rawB, "w079b-code") {
		t.Fatalf("ws-b audit does not carry its own yaml verbatim: %s", rawB)
	}
	if strings.Contains(rawB, "w79a") || strings.Contains(rawB, "w079a-flow") {
		t.Fatalf("ws-b audit leaked ws-a's yaml: %s", rawB)
	}
	if idnA.WorkspaceYaml == nil || idnB.WorkspaceYaml == nil {
		t.Fatal("both audits must carry their parsed yaml")
	}

	// Per-claim freshness, end to end: edit ws-a's yaml BETWEEN claims —
	// the NEXT claim in ws-a composes and audits the EDITED list (on the
	// flow-default-governed grill step) while wi_w079intA's claim-time
	// audit stands.
	writeWorkspaceYaml(t, wsA, "projects:\n  proj-a:\n    repos:\n      - name: w79a\n        path: .repo/w79a\nworkflow_defaults:\n  model_candidates: [w079-edit-v2]\n")
	if err := r.Start(context.Background(), "wi_w079intA2"); err != nil {
		t.Fatalf("Start A2: %v", err)
	}
	capA2 := filepath.Join(wsACapsule, "capsules", "wi_w079intA2")
	idnA2, _ := readIdentity(t, capA2)
	if got := idnA2.EffectiveCandidates["grill"]; len(got) != 1 || got[0] != "w079-edit-v2" {
		t.Fatalf("ws-a second-claim grill candidates %v, want the EDITED yaml's [w079-edit-v2] — an edit governs the next claim", got)
	}
	if got := idnA.EffectiveCandidates["grill"]; len(got) != 2 || got[0] != "w079a-flow" {
		t.Fatalf("ws-a first-claim audit changed under the edit: %v — the claim-time snapshot must stand", got)
	}
	for _, wi := range []string{"wi_w079intA", "wi_w079intB", "wi_w079intA2"} {
		if err := r.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
			t.Fatalf("Stop %s: %v", wi, err)
		}
	}
}
