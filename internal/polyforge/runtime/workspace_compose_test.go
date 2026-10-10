package runtime

// workspace_compose_test.go: the claim-time compose wiring — the four
// level precedence runs ONCE per claim over the pinned bundle's
// frontmatter (read from the daemon checkout) and the claim's workspace
// yaml, and the composed map rides the job.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

// frontmatterBundle writes a demo bundle whose workflow.md carries real
// frontmatter (kinds + candidate layers) and answers the digest an
// honest pin of that content would carry.
func frontmatterBundle(t *testing.T, frontmatter string) (workflowsDir, pinned string) {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "demo-bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte(frontmatter), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "agents", "step.md"), []byte("do the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fmSum := sha256.Sum256([]byte(frontmatter))
	stepSum := sha256.Sum256([]byte("do the thing\n"))
	pinned = workflow.Digest([]workflow.FileEntry{
		{Path: "workflow.md", Sha256: hex.EncodeToString(fmSum[:]), Bytes: uint64(len(frontmatter))},
		{Path: "agents/step.md", Sha256: hex.EncodeToString(stepSum[:]), Bytes: 13},
	})
	return dir, pinned
}

const w079Frontmatter = `---
name: demo-bundle
defaults:
  model_candidates: [prov/weak-a, prov/weak-b]
steps:
  - { id: grill, kind: spec }
  - { id: fix, kind: code, model_candidates: [prov/flash, prov/glm] }
  - { id: release, kind: code, model_candidates: [prov/glm] }
---

# demo
`

// A claim composes the effective lists from the bundle frontmatter and
// the workspace yaml, keyed by step id.
func TestClaimComposeRidesTheJob(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub := newFakeHub()
	hub.digest = pinned
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

	writeWorkspaceYaml(t, ws, "workflow_defaults:\n  step_kinds:\n    code: [w79/code-a, w79/code-b]\n")
	if err := r.Start(context.Background(), "wi_w079comp"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := r.jobOf("wi_w079comp")
	if j == nil {
		t.Fatal("the claim must run to a live agent")
	}
	j.mu.Lock()
	got := j.stepCandidates
	j.mu.Unlock()
	if got["fix"] == nil || got["fix"][0] != "w79/code-a" || got["fix"][1] != "w79/code-b" {
		t.Fatalf("fix effective list %v, want [w79/code-a w79/code-b] (the per-kind pin replaces the bundle step-level list)", got["fix"])
	}
	if got["release"] == nil || got["release"][0] != "w79/code-a" {
		t.Fatalf("release effective list %v, want [w79/code-a] (every kind:code step)", got["release"])
	}
	if got["grill"] == nil || got["grill"][0] != "prov/weak-a" {
		t.Fatalf("grill effective list %v, want the bundle weak default (kind spec unlisted)", got["grill"])
	}
	if err := r.Stop(context.Background(), "wi_w079comp", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A no-yaml claim composes the bundle's own resolution — identical to
// the driver's parsed step lists, so substitution is a no-op.
func TestClaimComposeWithoutYamlIsBundleOnly(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub := newFakeHub()
	hub.digest = pinned
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

	if err := r.Start(context.Background(), "wi_w079comp2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := r.jobOf("wi_w079comp2")
	j.mu.Lock()
	got := j.stepCandidates
	j.mu.Unlock()
	if got["fix"] == nil || got["fix"][0] != "prov/flash" {
		t.Fatalf("fix effective list %v, want the bundle step-level head prov/flash", got["fix"])
	}
	if got["grill"] == nil || got["grill"][0] != "prov/weak-a" {
		t.Fatalf("grill effective list %v, want the bundle weak default", got["grill"])
	}
	if err := r.Stop(context.Background(), "wi_w079comp2", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
