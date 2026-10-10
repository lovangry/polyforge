package runtime

// workspace_audit_test.go: the capsule.json audit record (polyforge#79)
// — the claim-time snapshot that doubles as the recovery rehydration
// source. A yaml-governed claim records the parsed yaml verbatim, the
// per-step post-precedence candidate lists and the resolved repo
// sources; a no-yaml claim records NOTHING (omitempty, never fabricated)
// and its legacy fields are unchanged.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func readIdentity(t *testing.T, capDir string) (*capsule.Identity, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capDir, "capsule.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		t.Fatalf("capsule.json does not decode: %v", err)
	}
	return &idn, string(raw)
}

// newAuditRuntime is a workspace runtime over a bundle WITH frontmatter
// (the compose has steps to key on), optionally serving registry repos.
func newAuditRuntime(t *testing.T, repos []api.ProjectRepo) (*Runtime, string) {
	t.Helper()
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub := newFakeHub()
	hub.digest = pinned
	hub.repos = repos
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedSpawner{}
	cfg := Config{
		MachineID:               "m-test",
		Mapping:                 mappingFor(ws, "proj"),
		DataDir:                 filepath.Join(t.TempDir(), "data"),
		AgentStarter:            scriptedEngineStarter(sp),
		MaxRecoveries:           2,
		SendBound:               2 * time.Second,
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := make(chan protocol.RTMsg, 64)
	fromOB := make(chan protocol.OBMsg, 64)
	r := New(cfg, hub, sp, toOB, fromOB)
	r.cfg.WorkflowsDir = wfDir
	return r, ws
}

// A yaml-governed claim writes the full audit record.
func TestGovernedClaimWritesAuditRecord(t *testing.T) {
	r, ws := newAuditRuntime(t, nil)
	w079GitRepo(t, filepath.Join(ws, ".repo", "w079src"), "local.txt", "local source\n")
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos:\n      - name: w079src\n        path: .repo/w079src\nworkflow_defaults:\n  model_candidates: [w79/flow-a, w79/flow-b]\n")

	if err := r.Start(context.Background(), "wi_w079aud"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idn, raw := readIdentity(t, testCapsuleDir(r, "wi_w079aud"))
	// The parsed yaml verbatim: the projects path as declared and the
	// flow-default model ids and key.
	for _, tok := range []string{"workspace_yaml", ".repo/w079src", "w79/flow-a", "w79/flow-b", "model_candidates"} {
		if !strings.Contains(raw, tok) {
			t.Fatalf("the audit record misses %q (raw %s)", tok, raw)
		}
	}
	// The effective candidates: post-precedence, per step.
	if got := idn.EffectiveCandidates["grill"]; len(got) != 2 || got[0] != "w79/flow-a" {
		t.Fatalf("grill effective candidates %v, want [w79/flow-a w79/flow-b]", got)
	}
	// The resolved repo source: the workspace-root-joined path.
	if got := idn.RepoSources["w079src"]; got != filepath.Join(ws, ".repo", "w079src") {
		t.Fatalf("repo source %q, want the resolved local checkout", got)
	}
	if err := r.Stop(context.Background(), "wi_w079aud", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A workflow-only yaml (no projects section) records the yaml and the
// effective candidates, and the repo sources name the REGISTRY URLs —
// the sources actually cloned from.
func TestWorkflowOnlyYamlAuditRecordsRegistrySources(t *testing.T) {
	registryURL, _ := w079Remote(t, "registry.txt", "registry marker\n")
	r, ws := newAuditRuntime(t, []api.ProjectRepo{{Name: "w079reg", URL: registryURL}})
	writeWorkspaceYaml(t, ws, "workflow_defaults:\n  model_candidates: [w79/flow-a]\n")

	if err := r.Start(context.Background(), "wi_w079wfl"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idn, _ := readIdentity(t, testCapsuleDir(r, "wi_w079wfl"))
	if idn.WorkspaceYaml == nil {
		t.Fatal("the workflow-only yaml must be recorded")
	}
	if got := idn.RepoSources["w079reg"]; got != registryURL {
		t.Fatalf("repo source %q, want the registry URL (the source actually cloned from)", got)
	}
	if idn.EffectiveCandidates["grill"] == nil {
		t.Fatalf("effective candidates missing: %v", idn.EffectiveCandidates)
	}
	if err := r.Stop(context.Background(), "wi_w079wfl", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A no-yaml claim writes NO audit content — the legacy fields (including
// the Repos name→SHA baselines) are unchanged.
func TestNoYamlClaimWritesNoAuditContent(t *testing.T) {
	registryURL, remoteHead := w079Remote(t, "registry.txt", "registry marker\n")
	r, _ := newAuditRuntime(t, []api.ProjectRepo{{Name: "w079reg", URL: registryURL}})

	if err := r.Start(context.Background(), "wi_w079noaud"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idn, raw := readIdentity(t, testCapsuleDir(r, "wi_w079noaud"))
	if strings.Contains(raw, "yaml") {
		t.Fatalf("capsule.json fabricated yaml audit content (raw %s)", raw)
	}
	if idn.WorkspaceYaml != nil || idn.EffectiveCandidates != nil || idn.RepoSources != nil {
		t.Fatalf("a no-yaml claim must record no audit content: %+v", idn)
	}
	if idn.Repos["w079reg"] != remoteHead {
		t.Fatalf("legacy Repos baseline changed: %v", idn.Repos)
	}
	if err := r.Stop(context.Background(), "wi_w079noaud", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// An empty repos list under a yaml: the audit records the yaml, the
// legacy Repos stay empty, and no repo source is fabricated.
func TestEmptyReposListAuditShape(t *testing.T) {
	r, ws := newAuditRuntime(t, nil)
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos: []\n")

	if err := r.Start(context.Background(), "wi_w079aud2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idn, _ := readIdentity(t, testCapsuleDir(r, "wi_w079aud2"))
	if idn.WorkspaceYaml == nil {
		t.Fatal("the yaml must be recorded")
	}
	if len(idn.Repos) != 0 {
		t.Fatalf("legacy Repos %v, want empty", idn.Repos)
	}
	if len(idn.RepoSources) != 0 {
		t.Fatalf("repo sources %v, want none (nothing was cloned)", idn.RepoSources)
	}
	if err := r.Stop(context.Background(), "wi_w079aud2", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
