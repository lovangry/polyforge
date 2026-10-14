package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	cfg, err := Load(t.TempDir())
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
		cfg, err := Load(writeYaml(t, content))
		if err != nil || cfg != nil {
			t.Fatalf("%s: %+v, %v; want nil, nil (absent-equivalent)", name, cfg, err)
		}
	}
}

// TestLoadValidGrammars pins the valid shapes and the decoded content.
func TestLoadValidGrammars(t *testing.T) {
	cfg, err := Load(writeYaml(t, "models:\n  e2e:\n    default: [a, b]\n    code: [c]\n    review: []\n  other:\n    spec: [d]\n"))
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
		{"unknown kind", "models:\n  e2e:\n    build: [a]\n", []string{"e2e", "build"}},
		{"misspelled default", "models:\n  e2e:\n    defaults: [a]\n", []string{"defaults"}},
		{"empty model id in default", "models:\n  e2e:\n    default: [\"\"]\n", []string{"e2e", "default"}},
		{"blank model id in kind", "models:\n  e2e:\n    code: [a, \"  \"]\n", []string{"e2e", "code"}},
		{"null project entry", "models:\n  e2e:\n", []string{"e2e"}},
		{"null list", "models:\n  e2e:\n    code:\n", []string{"e2e", "code"}},
		{"models not a map", "models: 42\n", nil},
		{"project not a map", "models:\n  e2e: [a]\n", nil},
		{"list not a list", "models:\n  e2e:\n    code: a\n", nil},
		{"list of non-strings", "models:\n  e2e:\n    code: [[a]]\n", nil},
		{"duplicate project key", "models:\n  e2e:\n    code: [a]\n  e2e:\n    code: [b]\n", []string{"e2e"}},
		{"duplicate kind key", "models:\n  e2e:\n    code: [a]\n    code: [b]\n", []string{"code"}},
		{"duplicate models key", "models:\n  e2e:\n    code: [a]\nmodels:\n  x:\n    code: [b]\n", []string{"models"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := writeYaml(t, c.yaml)
			cfg, err := Load(ws)
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
	if _, err := Load(ws); err == nil {
		t.Fatal("a directory at the yaml path must refuse")
	}
}

