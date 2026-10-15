package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stepsDir writes an installed-workflows dir whose bundles declare the
// step ids code, fix, review and spec, and answers it.
func stepsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	fm := "---\nsteps:\n  - { id: code }\n  - { id: fix }\n  - { id: review }\n  - { id: spec }\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "b", "workflow.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeYaml(t *testing.T, content string) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestLoadAbsentAndEmptyAreNil pins the absent-equivalence rule: a
// missing file and an empty/comments-only file both answer (nil, nil).
func TestLoadAbsentAndEmptyAreNil(t *testing.T) {
	cfg, err := Load(t.TempDir(), stepsDir(t))
	if err != nil || cfg != nil {
		t.Fatalf("absent yaml: %+v, %v; want nil, nil", cfg, err)
	}
	for name, content := range map[string]string{
		"empty":          "",
		"whitespace":     "\n\n   \n",
		"comments only":  "# a comment\n# another comment\n",
		"empty document": "---\n",
		"null models":    "models:\n",
	} {
		cfg, err := Load(writeYaml(t, content), stepsDir(t))
		if err != nil || cfg != nil {
			t.Fatalf("%s: %+v, %v; want nil, nil (absent-equivalent)", name, cfg, err)
		}
	}
}

// TestLoadValidGrammars pins the valid shapes and the decoded content.
func TestLoadValidGrammars(t *testing.T) {
	cfg, err := Load(writeYaml(t, "models:\n  e2e:\n    default: [a, b]\n    code: [c]\n    review: []\n  other:\n    spec: [d]\n"), stepsDir(t))
	if err != nil || cfg == nil {
		t.Fatalf("valid yaml refused: %v", err)
	}
	e2e := cfg.Models["e2e"]
	if len(e2e["default"]) != 2 || e2e["code"][0] != "c" {
		t.Fatalf("e2e decoded wrong: %+v", e2e)
	}
	if got, ok := e2e["review"]; !ok || got == nil || len(got) != 0 {
		t.Fatalf("declared-empty list must be present and non-nil: %#v", got)
	}
	if cfg.Models["other"]["spec"][0] != "d" {
		t.Fatalf("other decoded wrong: %+v", cfg.Models["other"])
	}
}

// TestLoadRefusals mirrors every strict-schema refusal family: the
// typed error names the file (and, where pinned, the offending key).
func TestLoadRefusals(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		needles []string
	}{
		{"torn yaml", "models:\n  e2e:\n    code: [\n", nil},
		{"old shape: projects section", "projects:\n  e2e:\n    repos: []\n", []string{"projects"}},
		{"old shape: workflow_defaults", "workflow_defaults:\n  model_candidates: [a]\n", []string{"workflow_defaults"}},
		{"old shape: step_kinds", "workflow_defaults:\n  step_kinds:\n    code: [a]\n", []string{"workflow_defaults"}},
		{"retired release section", "release:\n  cadence: weekly\n", []string{"release"}},
		{"unknown top-level key beside models", "models:\n  e2e:\n    code: [a]\nmodel: x\n", []string{"model"}},
		{"unknown step id", "models:\n  e2e:\n    build: [a]\n", []string{"models.e2e.build"}},
		{"every unknown step id named", "models:\n  e2e:\n    zz: [a]\n    fix: [a]\n  b:\n    gate: [a]\n", []string{"models.b.gate, models.e2e.zz"}},
		{"misspelled default", "models:\n  e2e:\n    defaults: [a]\n", []string{"defaults"}},
		{"empty model id in default", "models:\n  e2e:\n    default: [\"\"]\n", []string{"e2e", "default"}},
		{"blank model id in step list", "models:\n  e2e:\n    code: [a, \"  \"]\n", []string{"e2e", "code"}},
		{"null project entry", "models:\n  e2e:\n", []string{"e2e"}},
		{"null list", "models:\n  e2e:\n    code:\n", []string{"e2e", "code"}},
		{"models not a map", "models: 42\n", nil},
		{"project not a map", "models:\n  e2e: [a]\n", nil},
		{"list not a list", "models:\n  e2e:\n    code: a\n", nil},
		{"list of non-strings", "models:\n  e2e:\n    code: [[a]]\n", nil},
		{"duplicate project key", "models:\n  e2e:\n    code: [a]\n  e2e:\n    code: [b]\n", []string{"e2e"}},
		{"duplicate step key", "models:\n  e2e:\n    code: [a]\n    code: [b]\n", []string{"code"}},
		{"duplicate models key", "models:\n  e2e:\n    code: [a]\nmodels:\n  x:\n    code: [b]\n", []string{"models"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := writeYaml(t, c.yaml)
			cfg, err := Load(ws, stepsDir(t))
			if err == nil {
				t.Fatalf("invalid yaml accepted: %+v", cfg)
			}
			if cfg != nil {
				t.Fatalf("refused but returned a config: %+v", cfg)
			}
			for _, n := range append([]string{FileName}, c.needles...) {
				if !strings.Contains(err.Error(), n) {
					t.Fatalf("refusal %q does not name %q", err.Error(), n)
				}
			}
			wsErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("refusal is not a *workspace.Error: %T", err)
			}
			if wsErr.Path != filepath.Join(ws, FileName) {
				t.Fatalf("refusal path %q, want the workspace yaml path", wsErr.Path)
			}
		})
	}
}

// TestUnreadableYamlIsTyped pins the unreadable-file branch (a
// directory at the yaml path is the sharpest unreadable shape).
func TestUnreadableYamlIsTyped(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ws, stepsDir(t)); err == nil {
		t.Fatal("a directory at the yaml path must refuse")
	}
}

// TestLoadStepKeysNeedTheInstalledSet: no workflows dir skips only the
// step-id check, an unreadable one refuses; yaml anchors let one list
// serve several step ids.
func TestLoadStepKeysNeedTheInstalledSet(t *testing.T) {
	ws := writeYaml(t, "models:\n  e2e:\n    code: &c [a, b]\n    fix: *c\n    bogus: *c\n")
	if _, err := Load(ws, stepsDir(t)); err == nil || !strings.Contains(err.Error(), "models.e2e.bogus") {
		t.Fatalf("an unknown step id must refuse, got %v", err)
	}
	cfg, err := Load(ws, "")
	if err != nil || len(cfg.Models["e2e"]["fix"]) != 2 {
		t.Fatalf("no workflows dir: %+v, %v; want the anchored lists loaded", cfg, err)
	}
	if _, err := Load(ws, filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("an unreadable workflows dir must refuse, got %v", err)
	}
}
