package driver

// workflow_ext_test.go: the extended frontmatter read surface — the
// context_policy / handoff parse, the inherit default, the
// gate-must-be-fresh rule, and the deliberate IGNORANCE of the retired
// keys (polyforge#147: inputs / outputs / schema /
// requires_command_evidence / silence_minutes / tool_minutes /
// include_verdict / on_fail action are no longer read at all).

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
  - id: fix
    kind: code
    context_policy: inherit
    handoff: true
  - id: verification
    kind: gate
    gate: true
    context_policy: fresh
    on_fail: { target: fix }
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
	fix := wf.step("fix")
	if fix.ContextPolicy != ContextInherit || !fix.Handoff {
		t.Fatalf("fix policy/handoff: %q/%v", fix.ContextPolicy, fix.Handoff)
	}
	ver := wf.step("verification")
	if !ver.Gate || ver.ContextPolicy != ContextFresh {
		t.Fatalf("verification gate/policy: %v/%q", ver.Gate, ver.ContextPolicy)
	}
	if ver.OnFail == nil || ver.OnFail.Target != "fix" {
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

// TestRetiredFrontmatterKeysAreIgnored: the parser no longer reads the
// retired keys (polyforge#147 C5) — a bundle that still carries them
// parses and validates with the keys having NO effect at all (the loose
// read ignores unknown keys; there is no field left to fill, no jail to
// enforce, no schema vocabulary to police).
func TestRetiredFrontmatterKeysAreIgnored(t *testing.T) {
	front := "steps:\n" +
		"  - id: s\n" +
		"    inputs:\n" +
		"      - artifact: ../escape.md\n" +
		"      - /abs/also-escapes.md\n" +
		"    outputs:\n" +
		"      - { path: ../out.md, schema: dictionary }\n" +
		"    requires_command_evidence: true\n" +
		"    silence_minutes: 11\n" +
		"    tool_minutes: 22\n" +
		"  - id: g\n" +
		"    gate: true\n" +
		"    context_policy: fresh\n" +
		"    on_fail: { action: terminate, target: s, include_verdict: false }\n"
	wf := mustParseExt(t, front)
	if got := len(wf.Steps); got != 2 {
		t.Fatalf("steps: %d", got)
	}
	// Nothing of the retired keys survives into the step spec: the only
	// on_fail face left is the rewind target (action/include_verdict are
	// gone, and a declared "terminate" no longer buys a terminate edge).
	g := wf.step("g")
	if g.OnFail == nil || g.OnFail.Target != "s" {
		t.Fatalf("on_fail target is the only surviving face: %+v", g.OnFail)
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
