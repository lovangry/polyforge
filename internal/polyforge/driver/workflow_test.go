package driver

// workflow_ext_test.go: the extended frontmatter read surface — the
// context_policy / inputs / outputs / handoff parse, the inherit
// default, the gate-must-be-fresh rule, the artifact-path jail and the
// schema-name closed set.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// extExample is the task's frontmatter extension example, rounded out
// into a full bundle (a handoff producer, an inherit consumer with
// declared inputs, a fresh gate with a scalar-form input).
const extExample = `
name: quick-fix-x
description: extended-frontmatter probe bundle
rewind:
  max_total: 3
  max_per_step: 2
defaults:
  model_candidates: [prov/a, prov/b]
steps:
  - id: grill
    kind: spec
    context_policy: fresh
    handoff: true
    outputs:
      - path: artifacts/fix-memo.md
        schema: object
  - id: fix
    kind: code
    context_policy: inherit
    handoff: true
    inputs:
      - artifact: artifacts/fix-memo.md
      - artifact: artifacts/grill-qa.md
    outputs:
      - path: artifacts/fix-summary.md
        schema: object
  - id: verification
    kind: gate
    gate: true
    context_policy: fresh
    inputs:
      - artifacts/fix-summary.md
    on_fail: { action: rewind, target: fix }
`

func mustParseExt(t *testing.T, front string) *Workflow {
	t.Helper()
	wf, err := parseWorkflow([]byte(front))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}
	if err := validateWorkflow(wf); err != nil {
		t.Fatalf("validateWorkflow: %v", err)
	}
	return wf
}

func TestParseExtendedFrontmatter(t *testing.T) {
	wf := mustParseExt(t, extExample)
	if wf.MaxTotal != 3 || wf.MaxPerStep != 2 {
		t.Fatalf("rewind bounds: %d/%d", wf.MaxTotal, wf.MaxPerStep)
	}
	if len(wf.Steps) != 3 {
		t.Fatalf("steps: %d", len(wf.Steps))
	}
	grill := wf.step("grill")
	if grill == nil || grill.Gate {
		t.Fatalf("grill (a non-gate spec step): %+v", grill)
	}
	if !grill.Handoff || grill.ContextPolicy != ContextFresh {
		t.Fatalf("grill handoff/policy: %v/%q", grill.Handoff, grill.ContextPolicy)
	}
	if len(grill.Outputs) != 1 || grill.Outputs[0].Path != "artifacts/fix-memo.md" || grill.Outputs[0].Schema != "object" {
		t.Fatalf("grill outputs: %+v", grill.Outputs)
	}
	fix := wf.step("fix")
	if fix.ContextPolicy != ContextInherit || !fix.Handoff {
		t.Fatalf("fix policy/handoff: %q/%v", fix.ContextPolicy, fix.Handoff)
	}
	if len(fix.Inputs) != 2 || fix.Inputs[0].Artifact != "artifacts/fix-memo.md" || fix.Inputs[1].Artifact != "artifacts/grill-qa.md" {
		t.Fatalf("fix inputs: %+v", fix.Inputs)
	}
	if len(fix.Outputs) != 1 || fix.Outputs[0].Schema != "object" {
		t.Fatalf("fix outputs: %+v", fix.Outputs)
	}
	ver := wf.step("verification")
	if !ver.Gate || ver.ContextPolicy != ContextFresh {
		t.Fatalf("verification gate/policy: %v/%q", ver.Gate, ver.ContextPolicy)
	}
	// the scalar input form parses to the same InputRef
	if len(ver.Inputs) != 1 || ver.Inputs[0].Artifact != "artifacts/fix-summary.md" {
		t.Fatalf("verification inputs: %+v", ver.Inputs)
	}
	if ver.OnFail == nil || ver.OnFail.Action != "rewind" || ver.OnFail.Target != "fix" || !ver.OnFail.IncludeVerdict {
		t.Fatalf("verification on_fail: %+v", ver.OnFail)
	}
	// the defaults chain applies where no per-step chain is declared
	if got := len(grill.ModelCandidates); got != 2 {
		t.Fatalf("grill default candidates: %d", got)
	}
}

func TestParseContextPolicyDefaultsToInherit(t *testing.T) {
	wf := mustParseExt(t, "steps:\n  - { id: solo }\n")
	s := wf.step("solo")
	if s.ContextPolicy != ContextInherit {
		t.Fatalf("default policy: %q", s.ContextPolicy)
	}
}

func TestValidateGateMustBeFresh(t *testing.T) {
	// a gate without a declaration inherits — and a gate must be fresh,
	// so the missing declaration is a validation error, not a default.
	wf, err := parseWorkflow([]byte("steps:\n  - { id: g, gate: true }\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = validateWorkflow(wf)
	if err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("gate defaulting to inherit must be refused, got %v", err)
	}
	// an explicit inherit on a gate is refused the same way
	wf, err = parseWorkflow([]byte("steps:\n  - { id: g, gate: true, context_policy: inherit }\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err = validateWorkflow(wf); err == nil || !strings.Contains(err.Error(), ContextFresh) {
		t.Fatalf("inherit gate must be refused, got %v", err)
	}
	// declared fresh passes
	mustParseExt(t, "steps:\n  - { id: g, gate: true, context_policy: fresh }\n")
}

func TestValidateUnknownContextPolicy(t *testing.T) {
	wf, err := parseWorkflow([]byte("steps:\n  - { id: s, context_policy: sometimes }\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = validateWorkflow(wf)
	if err == nil || !strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("unknown policy must be refused, got %v", err)
	}
}

func TestValidateArtifactPathJail(t *testing.T) {
	cases := []string{
		"steps:\n  - id: s\n    inputs:\n      - artifact: ../escape.md\n",                        // parent escape
		"steps:\n  - id: s\n    inputs:\n      - artifact: /abs/escape.md\n",                      // absolute
		"steps:\n  - id: s\n    inputs:\n      - artifact: \"\"\n",                                // empty
		"steps:\n  - id: s\n    outputs:\n      - { path: ../out.md }\n",                          // output parent escape
		"steps:\n  - id: s\n    outputs:\n      - { path: /abs/out.md }\n",                        // output absolute
		"steps:\n  - id: s\n    outputs:\n      - { path: \"\" }\n",                               // output empty
		"steps:\n  - id: s\n    outputs:\n      - { path: artifacts/x.md, schema: dictionary }\n", // unknown schema
	}
	for _, front := range cases {
		wf, err := parseWorkflow([]byte(front))
		if err != nil {
			t.Fatalf("parse %q: %v", front, err)
		}
		if err := validateWorkflow(wf); err == nil {
			t.Fatalf("frontmatter %q must be refused", front)
		}
	}
}

func TestValidateStepIdentity(t *testing.T) {
	// duplicate ids
	wf, err := parseWorkflow([]byte("steps:\n  - { id: a }\n  - { id: a }\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateWorkflow(wf); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate ids must be refused, got %v", err)
	}
	// empty workflow
	if err := validateWorkflow(&Workflow{}); err == nil {
		t.Fatal("empty workflow must be refused")
	}
	if err := validateWorkflow(nil); err == nil {
		t.Fatal("nil workflow must be refused")
	}
	// missing id
	wf, err = parseWorkflow([]byte("steps:\n  - { kind: code }\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateWorkflow(wf); err == nil {
		t.Fatal("missing id must be refused")
	}
}

func TestLoadWorkflowFromBundleDir(t *testing.T) {
	dir := t.TempDir()
	raw := "---\n" + strings.TrimPrefix(extExample, "\n") + "---\n"
	if err := os.WriteFile(filepath.Join(dir, "workflow.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := LoadWorkflow(dir)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if len(wf.Steps) != 3 || wf.step("fix") == nil {
		t.Fatalf("loaded steps: %+v", wf.Steps)
	}

	// missing bundle
	if _, err := LoadWorkflow(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("missing workflow.md must be an error")
	}
	// no frontmatter block
	nofm := filepath.Join(dir, "nofm")
	if err := os.MkdirAll(nofm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nofm, "workflow.md"), []byte("# just a body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkflow(nofm); err == nil {
		t.Fatal("workflow.md without a frontmatter block must be an error")
	}
}

// A bundle whose workflow.md the driver cannot honor refuses New (the
// parse error is surfaced, never swallowed into a degraded driver).
func TestNewSurfacesWorkflowParseError(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "workflows", "b")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nsteps:\n  - { id: g, gate: true, context_policy: inherit }\n---\n"
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{
		WI:      WIInfo{ID: "wi", CapsuleDir: dir, BundleName: "b"},
		Spawner: &fakeSpawner{},
	})
	if err == nil || !strings.Contains(err.Error(), ContextFresh) {
		t.Fatalf("New must surface the workflow validation error, got %v", err)
	}
}
