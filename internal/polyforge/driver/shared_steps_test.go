package driver

// shared_steps_test.go: the guard of decision 2026-10-16 G1 — step
// definitions shared across bundles live once under workflows/_shared/
// steps/, every declared step of every shipped bundle resolves to a
// definition (its own or the shared one), and no byte-identical copy
// creeps back into a bundle.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

var shippedWorkflowsDir = filepath.Join("..", "..", "..", "workflows")

// shippedBundleDirs answers every shipped bundle (a workflows/ child with
// a workflow.md; _shared is not one).
func shippedBundleDirs(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(shippedWorkflowsDir, "*", "workflow.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no shipped bundles found: %v", err)
	}
	dirs := make([]string, 0, len(paths))
	for _, p := range paths {
		dirs = append(dirs, filepath.Dir(p))
	}
	return dirs
}

// shippedBundleSteps answers every (bundleDir, stepID) a shipped bundle
// declares.
func shippedBundleSteps(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	for _, dir := range shippedBundleDirs(t) {
		wf, err := LoadWorkflow(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, s := range wf.Steps {
			out = append(out, [2]string{dir, s.ID})
		}
	}
	return out
}

func TestShippedStepDefinitionsResolve(t *testing.T) {
	used := map[string]bool{}
	for _, bs := range shippedBundleSteps(t) {
		p := workflow.StepDefinitionPath(bs[0], bs[1])
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s/%s: no step definition (own or shared): %v", filepath.Base(bs[0]), bs[1], err)
		}
		used[filepath.Clean(p)] = true
		if _, err := readStepDefinition(bs[0], bs[1], shippedVars(t, bs[0], bs[1])); err != nil {
			t.Errorf("%s/%s: %v", filepath.Base(bs[0]), bs[1], err)
		}
	}
	shared, _ := filepath.Glob(filepath.Join(shippedWorkflowsDir, workflow.SharedDir, "steps", "*.md"))
	for _, p := range shared {
		if !used[filepath.Clean(p)] {
			t.Errorf("%s is used by no bundle (every bundle carries its own copy or omits the step)", p)
		}
	}
}

// TestShippedStepDefinitionsNotDuplicated: a step file byte-identical in
// two bundles, or a bundle copy identical to the shared one, must be
// moved to (or left in) workflows/_shared/steps/ instead — duplicated
// copies drift.
func TestShippedStepDefinitionsNotDuplicated(t *testing.T) {
	defs, err := filepath.Glob(filepath.Join(shippedWorkflowsDir, "*", "steps", "*.md"))
	if err != nil || len(defs) == 0 {
		t.Fatalf("no shipped step definitions found: %v", err)
	}
	byContent := map[string][]string{}
	for _, p := range defs {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		key := string(bytes.TrimSpace(raw))
		rel, _ := filepath.Rel(shippedWorkflowsDir, p)
		byContent[key] = append(byContent[key], filepath.ToSlash(rel))
	}
	for _, files := range byContent {
		if len(files) > 1 {
			sort.Strings(files)
			t.Errorf("identical step definitions %s: keep one copy under workflows/%s/steps/", strings.Join(files, ", "), workflow.SharedDir)
		}
	}
}

// Every shipped verification gate declares requires_command_evidence: the
// id no longer implies it (decision 2026-10-16 G10).
func TestShippedVerificationStepsDeclareCommandEvidence(t *testing.T) {
	n := 0
	for _, dir := range shippedBundleDirs(t) {
		wf, err := LoadWorkflow(dir)
		if err != nil {
			t.Fatal(err)
		}
		if s := wf.step(verificationStepID); s != nil {
			n++
			if !s.RequiresCommandEvidence {
				t.Errorf("%s: verification must declare requires_command_evidence: true", filepath.Base(dir))
			}
		}
	}
	if n == 0 {
		t.Fatal("no shipped verification step found")
	}
}

// capsuleWithShippedBundle copies the shipped bundle into a fresh capsule
// as workflows/<bundle>/ and answers the capsule dir.
func capsuleWithShippedBundle(t *testing.T, bundle string) string {
	t.Helper()
	capDir := t.TempDir()
	src := filepath.Join(shippedWorkflowsDir, bundle)
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		writeCapsuleFile(t, capDir, filepath.ToSlash(filepath.Join("workflows", bundle, rel)), string(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writeCapsuleFile(t, capDir, capsule.SessionJSON, `{"harness":"pi","session_id":"ab12cd34"}`)
	return capDir
}

func spawnDeliverPrompt(t *testing.T, capDir, bundle string) string {
	t.Helper()
	sp := &captureSpawner{}
	ad, err := NewStepSpawnAdapter(StepSpawnAdapter{
		WI:           WIInfo{ID: "wi_sh", CapsuleDir: capDir, BundleName: bundle},
		Spawner:      sp,
		PollInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewStepSpawnAdapter: %v", err)
	}
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "deliver", SpawnN: 1}); err != nil {
		t.Fatalf("SpawnStep deliver: %v", err)
	}
	p, _ := sp.last()
	return p.InitialPrompt
}

// A capsule materialized before G1 carries its own steps/deliver.md and no
// workflows/_shared/: deliver still resolves from the snapshot, schema
// included (same-daemon recovery of an old capsule).
func TestOldLayoutCapsuleOwnDeliverStillRuns(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(shippedWorkflowsDir, workflow.SharedDir, "steps", "deliver.md"))
	if err != nil {
		t.Fatal(err)
	}
	capDir := capsuleWithShippedBundle(t, "autopilot")
	writeCapsuleFile(t, capDir, "workflows/autopilot/steps/deliver.md", string(shared))
	if _, err := os.Stat(filepath.Join(capDir, "workflows", workflow.SharedDir)); !os.IsNotExist(err) {
		t.Fatalf("old layout must have no _shared: %v", err)
	}
	prompt := spawnDeliverPrompt(t, capDir, "autopilot")
	if !strings.Contains(prompt, strings.TrimSpace(stepDefBody(shared))[:80]) {
		t.Fatalf("deliver prompt does not carry the snapshot's deliver body\n%s", prompt)
	}
	s, err := loadStepOutputSchema(filepath.Join(capDir, "workflows", "autopilot"), "deliver", nil)
	if err != nil || s == nil || s.Schema == nil {
		t.Fatalf("old-layout deliver schema: %v", err)
	}
}

// New layout: deliver resolves from workflows/_shared/ in the capsule, and
// a bundle's own steps/deliver.md still wins over it.
func TestCapsuleSharedDeliverAndOwnFileWins(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(shippedWorkflowsDir, workflow.SharedDir, "steps", "deliver.md"))
	if err != nil {
		t.Fatal(err)
	}
	capDir := capsuleWithShippedBundle(t, "autopilot")
	writeCapsuleFile(t, capDir, "workflows/_shared/steps/deliver.md", string(shared))
	if p := spawnDeliverPrompt(t, capDir, "autopilot"); !strings.Contains(p, "gh pr merge") {
		t.Fatalf("shared deliver not used\n%s", p)
	}
	writeCapsuleFile(t, capDir, "workflows/autopilot/steps/deliver.md", "bundle-own deliver override")
	if p := spawnDeliverPrompt(t, capDir, "autopilot"); !strings.Contains(p, "bundle-own deliver override") {
		t.Fatalf("the bundle's own deliver.md must win\n%s", p)
	}
}

// The step id `verification` alone implies no command evidence in the
// dispatch prompt either: only the frontmatter flag does (decision
// 2026-10-16 G10; TestCommandEvidenceFromDeclarationOnly pins the
// result-validation side).
func TestVerificationIDAloneOwesNoEvidenceInPrompt(t *testing.T) {
	ad, sp, capDir := spawnEnv(t) // its verification declares no flag
	grillDone(t, capDir)
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 1}); err != nil {
		t.Fatal(err)
	}
	p, _ := sp.last()
	if strings.Contains(p.InitialPrompt, "command_evidence") {
		t.Fatalf("an undeclared verification step must not be told to carry command_evidence\n%s", p.InitialPrompt)
	}
	for i := range ad.Workflow.Steps {
		if ad.Workflow.Steps[i].ID == "verification" {
			ad.Workflow.Steps[i].RequiresCommandEvidence = true
		}
	}
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 2}); err != nil {
		t.Fatal(err)
	}
	if p, _ = sp.last(); !strings.Contains(p.InitialPrompt, "this step requires `command_evidence`") {
		t.Fatalf("a declared step must be told to carry command_evidence\n%s", p.InitialPrompt)
	}
}
