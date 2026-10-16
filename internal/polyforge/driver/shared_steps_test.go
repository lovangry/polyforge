package driver

// shared_steps_test.go: the guard of decision 2026-10-16 G1 — step
// definitions shared across bundles live once under workflows/_shared/
// steps/, every declared step of every shipped bundle resolves to a
// definition (its own or the shared one), and no byte-identical copy
// creeps back into a bundle.

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
		if _, err := readStepDefinition(bs[0], bs[1]); err != nil {
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
