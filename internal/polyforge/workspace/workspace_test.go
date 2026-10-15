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
	cfg, err := Load(t.TempDir(), stepsDir(t), nil)
	if err != nil || cfg != nil {
		t.Fatalf("absent yaml: %+v, %v; want nil, nil", cfg, err)
	}
	for name, content := range map[string]string{
		"empty":          "",
		"whitespace":     "\n\n   \n",
		"comments only":  "# a comment\n# another comment\n",
		"empty document": "---\n",
		"null steps":     "steps:\n",
	} {
		cfg, err := Load(writeYaml(t, content), stepsDir(t), nil)
		if err != nil || cfg != nil {
			t.Fatalf("%s: %+v, %v; want nil, nil (absent-equivalent)", name, cfg, err)
		}
	}
}

// TestLoadValidGrammars pins the valid shapes and the decoded content.
func TestLoadValidGrammars(t *testing.T) {
	cfg, err := Load(writeYaml(t, "steps:\n  e2e:\n    default: { models: [a, b] }\n    code: { models: [c] }\n    review: { models: [] }\n  other:\n    spec: { models: [d] }\n"), stepsDir(t), nil)
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
		{"torn yaml", "steps:\n  e2e:\n    code: [\n", nil},
		{"old shape: projects section", "projects:\n  e2e:\n    repos: []\n", []string{"projects"}},
		{"old shape: workflow_defaults", "workflow_defaults:\n  model_candidates: [a]\n", []string{"workflow_defaults"}},
		{"old shape: step_kinds", "workflow_defaults:\n  step_kinds:\n    code: [a]\n", []string{"workflow_defaults"}},
		{"retired release section", "release:\n  cadence: weekly\n", []string{"release"}},
		{"unknown top-level key beside steps", "steps:\n  e2e:\n    code: { models: [a] }\nmodel: x\n", []string{"model"}},
		{"unknown step id", "steps:\n  e2e:\n    build: { models: [a] }\n", []string{"steps.e2e.build"}},
		{"every unknown step id named", "steps:\n  e2e:\n    zz: { models: [a] }\n    fix: { models: [a] }\n  b:\n    gate: { models: [a] }\n", []string{"steps.b.gate, steps.e2e.zz"}},
		{"misspelled default", "steps:\n  e2e:\n    defaults: { models: [a] }\n", []string{"defaults"}},
		{"empty model id in default", "steps:\n  e2e:\n    default: { models: [\"\"] }\n", []string{"e2e", "default"}},
		{"blank model id in step list", "steps:\n  e2e:\n    code: { models: [a, \"  \"] }\n", []string{"e2e", "code"}},
		{"null project entry", "steps:\n  e2e:\n", []string{"e2e"}},
		{"null list", "steps:\n  e2e:\n    code:\n", []string{"e2e", "code"}},
		{"steps not a map", "steps: 42\n", []string{"steps: must be a mapping of project → step entries"}},
		{"steps a list", "steps: [a]\n", []string{"steps: must be a mapping"}},
		{"project not a map", "steps:\n  e2e: [a]\n", []string{"steps.e2e: must be a mapping of step entries"}},
		{"entry a scalar", "steps:\n  e2e:\n    code: a\n", []string{"steps.e2e.code: must be `{ models: [...] }`"}},
		{"list of non-strings", "steps:\n  e2e:\n    code: { models: [[a]] }\n", []string{"steps.e2e.code.models[0]: must be a plain model-id string"}},
		{"null model item", "steps:\n  e2e:\n    code: { models: [a, ~] }\n", []string{"steps.e2e.code.models[1]"}},
		{"numeric model item", "steps:\n  e2e:\n    code: { models: [a, 42] }\n", []string{"steps.e2e.code.models[1]: must be a plain model-id string"}},
		{"duplicate model id after trim", "steps:\n  e2e:\n    code: { models: [a, \" a \"] }\n", []string{"steps.e2e.code.models[1]: repeats \"a\""}},
		{"models a scalar", "steps:\n  e2e:\n    code: { models: a }\n", []string{"steps.e2e.code.models: must be a list of model ids"}},
		{"legacy models wins over other unknown keys", "workflow_defaults: {}\nmodels:\n  e2e:\n    code: [a]\n", []string{"`models:` was replaced by `steps:`"}},
		{"duplicate project key", "steps:\n  e2e:\n    code: { models: [a] }\n  e2e:\n    code: { models: [b] }\n", []string{"e2e"}},
		{"duplicate step key", "steps:\n  e2e:\n    code: { models: [a] }\n    code: { models: [b] }\n", []string{"code"}},
		{"duplicate steps key", "steps:\n  e2e:\n    code: { models: [a] }\nsteps:\n  x:\n    code: { models: [b] }\n", []string{"steps"}},
		{"legacy top-level models", "models:\n  e2e:\n    default: [a]\n", []string{"`models:` was replaced by `steps:`", "steps.<project>.<key>.models", "docs/upgrade.md"}},
		{"legacy models beside steps", "steps:\n  e2e:\n    default: { models: [a] }\nmodels:\n  e2e:\n    code: [b]\n", []string{"`models:` was replaced by `steps:`"}},
		{"unknown sub-key", "steps:\n  e2e:\n    fix: { models: [a], modles: [b] }\n", []string{"steps.e2e.fix.modles", "only key is `models`"}},
		{"harness not accepted yet", "steps:\n  e2e:\n    fix: { models: [a], harness: pi }\n", []string{"steps.e2e.fix.harness", "only key is `models`"}},
		{"entry without models", "steps:\n  e2e:\n    fix: {}\n", []string{"steps.e2e.fix.models"}},
		{"merge key at top level", "base: &b {}\n<<: *b\nsteps:\n  e2e:\n    fix: { models: [a] }\n", []string{"top level: YAML merge keys (`<<`) are not supported", "{ models: *x }"}},
		{"legacy models wins over a top-level merge key", "<<: {}\nmodels:\n  e2e:\n    code: [a]\n", []string{"`models:` was replaced by `steps:`"}},
		{"merge key at project level", "steps:\n  base: &p\n    fix: { models: [a] }\n  e2e:\n    <<: *p\n", []string{"steps.e2e: YAML merge keys (`<<`) are not supported"}},
		{"merge key at entry level", "steps:\n  e2e:\n    fix: &f { models: [a] }\n    <<: { code: *f }\n", []string{"steps.e2e: YAML merge keys"}},
		{"merge key inside an entry", "steps:\n  e2e:\n    fix: { <<: { models: [a] } }\n", []string{"steps.e2e.fix: YAML merge keys"}},
		{"merge key list form", "steps:\n  base: &p\n    fix: { models: [a] }\n  other: &q\n    code: { models: [b] }\n  e2e:\n    <<: [*p, *q]\n", []string{"steps.e2e: YAML merge keys (`<<`) are not supported"}},
		{"second yaml document", "steps:\n  e2e:\n    fix: { models: [a] }\n---\nsteps: {}\n", []string{"exactly one YAML document"}},
		{"null project key", "steps:\n  ~:\n    fix: { models: [a] }\n", []string{"steps: a null key"}},
		{"null step key", "steps:\n  e2e:\n    null: { models: [a] }\n", []string{"steps.e2e: a null key"}},
		{"alias item in a list", "steps:\n  e2e:\n    fix: { models: [&m a, *m] }\n", []string{"steps.e2e.fix.models[1]: must be a plain model-id string"}},
		{"tagged item in a list", "steps:\n  e2e:\n    fix: { models: [!!binary aGk=] }\n", []string{"steps.e2e.fix.models[0]"}},
		{"bare list instead of entry", "steps:\n  e2e:\n    fix: [a]\n", []string{"steps.e2e.fix: must be `{ models: [...] }` (the pre-#169 list form?)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := writeYaml(t, c.yaml)
			cfg, err := Load(ws, stepsDir(t), nil)
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
	if _, err := Load(ws, stepsDir(t), nil); err == nil {
		t.Fatal("a directory at the yaml path must refuse")
	}
}

// TestLoadStepKeysNeedTheInstalledSet: no workflows dir skips only the
// step-id check, an unreadable one refuses; yaml anchors let one list
// serve several step ids.
func TestLoadStepKeysNeedTheInstalledSet(t *testing.T) {
	ws := writeYaml(t, "steps:\n  e2e:\n    code: { models: &c [a, b] }\n    fix: { models: *c }\n    bogus: { models: *c }\n")
	if _, err := Load(ws, stepsDir(t), nil); err == nil || !strings.Contains(err.Error(), "steps.e2e.bogus") {
		t.Fatalf("an unknown step id must refuse, got %v", err)
	}
	cfg, err := Load(ws, "", nil)
	if err != nil || len(cfg.Models["e2e"]["fix"]) != 2 {
		t.Fatalf("no workflows dir: %+v, %v; want the anchored lists loaded", cfg, err)
	}
	if _, err := Load(ws, filepath.Join(t.TempDir(), "missing"), nil); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("an unreadable workflows dir must refuse, got %v", err)
	}
}

// TestLoadEmptyWorkflowsDirSaysSo: an existing workflows dir with no
// readable bundle refuses a per-step key with that fact, not by calling
// every key unknown; a default-only yaml still loads.
func TestLoadEmptyWorkflowsDirSaysSo(t *testing.T) {
	empty := t.TempDir()
	_, err := Load(writeYaml(t, "steps:\n  e2e:\n    fix: { models: [a] }\n    code: { models: [b] }\n"), empty, nil)
	if err == nil || !strings.Contains(err.Error(), "no workflows found in "+empty) || strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("empty workflows dir: %v", err)
	}
	if cfg, err := Load(writeYaml(t, "steps:\n  e2e:\n    default: { models: [a] }\n"), empty, nil); err != nil || cfg == nil {
		t.Fatalf("default-only yaml over an empty workflows dir: %+v, %v", cfg, err)
	}
}

// TestLoadProjectsMustBeMapped: a project key under steps that cli.toml
// does not map to this workspace refuses, naming it and the mapped set;
// nil skips the check. Model ids are trimmed on load.
func TestLoadProjectsMustBeMapped(t *testing.T) {
	ws := writeYaml(t, "steps:\n  e2e:\n    fix: { models: [\" a \"] }\n  ghost:\n    default: { models: [b] }\n")
	_, err := Load(ws, stepsDir(t), []string{"e2e", "other"})
	if err == nil || !strings.Contains(err.Error(), "project(s) ghost are not mapped to this workspace in cli.toml (it maps: e2e, other)") {
		t.Fatalf("unmapped project: %v", err)
	}
	cfg, err := Load(ws, stepsDir(t), nil)
	if err != nil || cfg.Models["e2e"]["fix"][0] != "a" {
		t.Fatalf("nil project set: %+v, %v; want loaded with trimmed ids", cfg, err)
	}
}

// TestLoadRefusesFirstDefectInFileOrder: with several defects the
// refusal is the first one in file order — deterministic, never Go map
// iteration order.
func TestLoadRefusesFirstDefectInFileOrder(t *testing.T) {
	yaml := "steps:\n  a:\n    fix: { models: [x] }\n  b:\n    fix: [x]\n  c:\n    fix: { bogus: [x] }\n  d:\n    fix: { models: [~] }\n"
	for i := 0; i < 20; i++ {
		_, err := Load(writeYaml(t, yaml), stepsDir(t), nil)
		if err == nil || !strings.Contains(err.Error(), "steps.b.fix: must be `{ models: [...] }`") {
			t.Fatalf("run %d: %v, want the first defect in file order (steps.b.fix)", i, err)
		}
	}
	yaml = "steps:\n  e2e:\n    zz: { models: [x] }\n    fix: { models: [~] }\n    aa: { models: [y, y] }\n"
	if _, err := Load(writeYaml(t, yaml), stepsDir(t), nil); err == nil || !strings.Contains(err.Error(), "steps.e2e.fix.models[0]") {
		t.Fatalf("within one project: %v, want steps.e2e.fix.models[0] first", err)
	}
}

// TestLoadZeroProjectWorkspaceRefusesProjectKeys: a mapped workspace with
// no projects answers a non-nil empty set, so every project key refuses.
func TestLoadZeroProjectWorkspaceRefusesProjectKeys(t *testing.T) {
	_, err := Load(writeYaml(t, "steps:\n  e2e:\n    fix: { models: [a] }\n"), stepsDir(t), []string{})
	if err == nil || !strings.Contains(err.Error(), "project(s) e2e are not mapped") {
		t.Fatalf("zero-project workspace: %v", err)
	}
}
