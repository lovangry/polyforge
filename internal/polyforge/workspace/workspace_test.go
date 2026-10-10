package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadAbsentAndEmptyAreNil pins the absent-equivalence rule: a
// missing file and an empty/comments-only file both answer (nil, nil).
func TestLoadAbsentAndEmptyAreNil(t *testing.T) {
	ws := t.TempDir()

	cfg, err := Load(ws)
	if err != nil {
		t.Fatalf("absent yaml: %v", err)
	}
	if cfg != nil {
		t.Fatalf("absent yaml: %+v, want nil", cfg)
	}

	for name, content := range map[string]string{
		"empty":          "",
		"whitespace":     "\n\n   \n",
		"comments only":  "# a comment\n# another comment\n",
		"empty document": "---\n",
	} {
		if err := os.WriteFile(filepath.Join(ws, FileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(ws)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg != nil {
			t.Fatalf("%s: %+v, want nil (absent-equivalent)", name, cfg)
		}
	}
}

// TestLoadValidGrammars pins the exhaustive valid shapes.
func TestLoadValidGrammars(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"projects with repos", "projects:\n  e2e:\n    repos:\n      - name: src\n        path: .repo/src\n"},
		{"empty repos list governs", "projects:\n  e2e:\n    repos: []\n"},
		{"flow default", "workflow_defaults:\n  model_candidates: [a, b]\n"},
		{"empty flow default governs", "workflow_defaults:\n  model_candidates: []\n"},
		{"step kinds", "workflow_defaults:\n  step_kinds:\n    code: [a, b]\n    spec: [c]\n"},
		{"empty step-kind list governs", "workflow_defaults:\n  step_kinds:\n    code: []\n"},
		{"exhaustive", "projects:\n  e2e:\n    repos:\n      - name: src\n        path: .repo/src\n      - name: docs\n        path: sub/dir/docs\nworkflow_defaults:\n  model_candidates: [a]\n  step_kinds:\n    review: [b]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, FileName), []byte(c.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(ws)
			if err != nil {
				t.Fatalf("valid yaml refused: %v", err)
			}
			if cfg == nil {
				t.Fatal("valid yaml parsed to nil")
			}
		})
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
		{"torn yaml", "projects:\n  e2e:\n    repos: [\n      - name: a\n", []string{FileName}},
		{"unknown top-level key", "project: e2e\nprojects:\n  e2e:\n    repos: []\n", []string{FileName, "project"}},
		{"unknown top-level key 2", "workflow_default:\n  model_candidates: [a]\n", []string{FileName, "workflow_default"}},
		{"unknown nested key on repo entry", "projects:\n  e2e:\n    repos:\n      - name: a\n        paths: .repo/a\n", []string{FileName, "paths"}},
		{"unknown nested key under workflow_defaults", "workflow_defaults:\n  model_candidate: [a]\n", []string{FileName, "model_candidate"}},
		{"step_kinds key outside the V8 set", "workflow_defaults:\n  step_kinds:\n    build: [a]\n", []string{FileName, "build"}},
		{"project entry without repos", "projects:\n  e2e: {}\n", []string{FileName}},
		{"repo entry without name", "projects:\n  e2e:\n    repos:\n      - path: .repo/a\n", []string{FileName}},
		{"repo entry without path", "projects:\n  e2e:\n    repos:\n      - name: a\n", []string{FileName}},
		{"unsafe repo name", "projects:\n  e2e:\n    repos:\n      - name: ../escape\n        path: .repo/a\n", []string{FileName}},
		{"repo name with separator", "projects:\n  e2e:\n    repos:\n      - name: a/b\n        path: .repo/a\n", []string{FileName}},
		{"absolute repo path", "projects:\n  e2e:\n    repos:\n      - name: a\n        path: /tmp/a\n", []string{FileName}},
		{"parent-escaping repo path", "projects:\n  e2e:\n    repos:\n      - name: a\n        path: ../a\n", []string{FileName}},
		{"empty repo path", "projects:\n  e2e:\n    repos:\n      - name: a\n        path: \"\"\n", []string{FileName}},
		{"empty model id in flow default", "workflow_defaults:\n  model_candidates: [\"\"]\n", []string{FileName}},
		{"empty model id in step kinds", "workflow_defaults:\n  step_kinds:\n    code: [a, \"\"]\n", []string{FileName}},
		{"type mismatch on projects", "projects: 42\n", []string{FileName}},
		{"type mismatch on repos", "projects:\n  e2e:\n    repos: 7\n", []string{FileName}},
		{"duplicate project key", "projects:\n  e2e:\n    repos: []\n  dup:\n    repos: []\n  dup:\n    repos: []\n", []string{FileName, "dup"}},
		{"duplicate nested key", "projects:\n  e2e:\n    repos: []\n    repos: []\n", []string{FileName, "repos"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, FileName), []byte(c.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(ws)
			if err == nil {
				t.Fatalf("invalid yaml accepted: %+v", cfg)
			}
			if cfg != nil {
				t.Fatalf("refused but returned a config: %+v", cfg)
			}
			for _, n := range c.needles {
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

// TestResolveRepoSource pins the local-checkout resolution.
func TestResolveRepoSource(t *testing.T) {
	got := ResolveRepoSource("/ws", RepoEntry{Name: "src", Path: ".repo/src"})
	if got != filepath.Join("/ws", ".repo", "src") {
		t.Fatalf("resolved %q", got)
	}
}

// TestKindVocabulary pins the closed V8 set the loader validates
// step_kinds keys against.
func TestKindVocabulary(t *testing.T) {
	want := "qa spec author code gate review"
	if got := strings.Join(KindVocabulary, " "); got != want {
		t.Fatalf("kind vocabulary %q, want %q", got, want)
	}
}
