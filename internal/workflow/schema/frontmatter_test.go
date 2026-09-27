package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func mustRuleErr(t *testing.T, err error, rule string) {
	t.Helper()
	re, ok := err.(*RuleError)
	if !ok {
		t.Fatalf("expected *RuleError with rule %s, got %v", rule, err)
	}
	if re.RuleID != rule {
		t.Fatalf("expected rule %s, got %s (%s)", rule, re.RuleID, re.Msg)
	}
}

const validFM = `---
name: demo
description: reference flow
rewind:
  max_total: 4
  max_per_step: 2
defaults:
  model_candidates: ["pi/claude-opus", "pi/gpt-5"]
budget:
  requests: 100
steps:
  - id: grill
    kind: qa
    interactive: true
  - id: spec
    kind: spec
  - id: code
    kind: code
  - id: verification
    kind: gate
    gate: true
    on_fail: {action: rewind, target: code}
  - id: review
    kind: review
    gate: true
    on_fail: {action: rewind, target: code}
---
# body is never parsed
`

func validBundleFiles() map[string]string {
	return map[string]string{
		"workflow.md":            validFM,
		"agents/grill.md":        "---\nname: grill\ndescription: q\ntools: [read]\noutput: {type: object}\n---\n",
		"agents/spec.md":         "---\nname: spec\ndescription: s\ntools: [read, write]\noutput: {type: object}\n---\n",
		"agents/code.md":         "---\nname: code\ndescription: c\ntools: [bash]\noutput: {type: object}\n---\n",
		"agents/verification.md": "---\nname: verification\ndescription: v\ntools: [bash]\noutput: {type: object}\n---\n",
		"agents/review.md":       "---\nname: review\ndescription: r\ntools: [read]\noutput: {type: object}\n---\n",
	}
}

func TestValidFrontmatter(t *testing.T) {
	dir := writeBundle(t, validBundleFiles())
	fm, err := ValidateWorkflowFrontmatter(dir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(fm.Steps) != 5 || fm.Budget == nil || fm.Budget.Requests == nil || *fm.Budget.Requests != 100 {
		t.Fatalf("unexpected parse: %+v", fm)
	}
	if err := CheckAgentsExist(dir, fm); err != nil {
		t.Fatal(err)
	}
}

func TestRuleViolations(t *testing.T) {
	base := func() map[string]string { return validBundleFiles() }

	cases := []struct {
		name   string
		mutate func(files map[string]string) map[string]string
		rule   string
	}{
		{"unknown top key", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "description: reference flow", "isolation: container\ndescription: reference flow", 1)
			return f
		}, "V1"},
		{"binary tag", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "description: reference flow", "description: !!binary aGVsbG8=\n", 1)
			return f
		}, "V1"},
		{"name equals bundle dir", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "name: demo", "name: other", 1)
			return f
		}, "V2"},
		{"duplicate step id", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "- id: spec\n    kind: spec", "- id: code\n    kind: spec", 1)
			return f
		}, "V2"},
		{"unknown step key", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "- id: spec\n    kind: spec", "- id: spec\n    kind: spec\n    position: 2", 1)
			return f
		}, "V1"},
		{"on_fail on non-gate", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "- id: spec\n    kind: spec", "- id: spec\n    kind: spec\n    on_fail: {action: rewind, target: grill}", 1)
			return f
		}, "V5"},
		{"on_fail target later", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM,
				"- id: code\n    kind: code",
				"- id: code\n    kind: code\n    gate: true\n    on_fail: {action: rewind, target: review}", 1)
			return f
		}, "V5"},
		{"budget zero", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "requests: 100", "requests: 0", 1)
			return f
		}, "V6"},
		{"bad kind", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, "kind: spec", "kind: deploy", 1)
			return f
		}, "V8"},
		{"empty model candidates", func(f map[string]string) map[string]string {
			f["workflow.md"] = strings.Replace(validFM, `model_candidates: ["pi/claude-opus", "pi/gpt-5"]`, "model_candidates: []", 1)
			return f
		}, "V9"},
		{"missing agents file", func(f map[string]string) map[string]string {
			delete(f, "agents/review.md")
			return f
		}, "V7"},
		{"too many steps", func(f map[string]string) map[string]string {
			var steps []string
			for i := 0; i < 33; i++ {
				steps = append(steps, "  - id: s"+strings.Repeat("x", 1)+"\n    kind: code")
			}
			f["workflow.md"] = "---\nname: demo\nsteps:\n" + strings.Join(steps, "\n") + "\n---\n"
			return f
		}, "V2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBundle(t, tc.mutate(base()))
			fm, err := ValidateWorkflowFrontmatter(dir, "demo")
			if err == nil {
				if tc.rule == "V7" {
					if err := CheckAgentsExist(dir, fm); err == nil {
						t.Fatal("expected V7 failure")
					} else {
						mustRuleErr(t, err, "V7")
						return
					}
				}
				t.Fatal("expected validation failure")
			}
			mustRuleErr(t, err, tc.rule)
		})
	}
}

// TestV10NonGateQa: a non-gate qa step with no spec step anywhere is
// rejected; with a spec step after it, it passes.
func TestV10NonGateQa(t *testing.T) {
	noSpec := strings.Replace(validFM, "  - id: spec\n    kind: spec\n", "", 1)
	dir := writeBundle(t, map[string]string{"workflow.md": noSpec})
	_, err := ValidateWorkflowFrontmatter(dir, "demo")
	mustRuleErr(t, err, "V10")

	qaAfterSpec := strings.Replace(validFM, "  - id: verification", "  - id: ask2\n    kind: qa\n  - id: verification", 1)
	dir = writeBundle(t, map[string]string{"workflow.md": qaAfterSpec})
	_, err = ValidateWorkflowFrontmatter(dir, "demo")
	mustRuleErr(t, err, "V10")

	// gate:true qa step (grill-review style) is exempt from V10 at any position.
	gateQA := strings.Replace(validFM, "  - id: verification",
		"  - id: grill-review\n    kind: qa\n    gate: true\n    on_fail: {action: rewind, target: spec}\n  - id: verification", 1)
	gateQA = strings.Replace(gateQA, "  - id: grill\n    kind: qa\n    interactive: true\n", "", 1)
	dir = writeBundle(t, map[string]string{"workflow.md": gateQA})
	if _, err := ValidateWorkflowFrontmatter(dir, "demo"); err != nil {
		t.Fatal(err)
	}
}

// TestStepSkillValidator: required fields, name==file, forbidden model key.
func TestStepSkillValidator(t *testing.T) {
	valid := "---\nname: code\ndescription: c\ntools: [bash]\noutput: {type: object}\n---\nbody"
	if err := ValidateStepSkill("code", valid); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStepSkill("code", strings.Replace(valid, "name: code", "name: other", 1)); err == nil {
		t.Fatal("expected name==file failure")
	}
	if err := ValidateStepSkill("code", strings.Replace(valid, "output: {type: object}", "", 1)); err == nil {
		t.Fatal("expected missing output failure")
	}
	model := strings.Replace(valid, "output: {type: object}", "output: {type: object}\nmodel: gpt-5", 1)
	if err := ValidateStepSkill("code", model); err == nil {
		t.Fatal("expected forbidden model key failure (INV-1g)")
	}
}

// TestFeatureFullBundle validates the real reference bundle shipped in the
// repo (workflows/feature-full), ensuring the shipped bundle satisfies the
// full V rule set.
func TestFeatureFullBundle(t *testing.T) {
	root := filepath.Join("..", "..", "..", "workflows", "feature-full")
	if _, err := os.Stat(filepath.Join(root, "workflow.md")); err != nil {
		t.Skip("reference bundle not present in this checkout")
	}
	fm, err := ValidateWorkflowFrontmatter(root, "feature-full")
	if err != nil {
		t.Fatalf("reference bundle invalid: %v", err)
	}
	if err := CheckAgentsExist(root, fm); err != nil {
		t.Fatalf("reference bundle agents: %v", err)
	}
	for _, st := range fm.Steps {
		raw, err := os.ReadFile(filepath.Join(root, "agents", st.ID+".md"))
		if err != nil {
			t.Fatalf("agents/%s.md: %v", st.ID, err)
		}
		if err := ValidateStepSkill(st.ID, string(raw)); err != nil {
			t.Errorf("agents/%s.md: %v", st.ID, err)
		}
	}
}
