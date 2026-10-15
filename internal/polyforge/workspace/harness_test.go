package workspace

// harness_test.go: the polyforge#159 harness chain (yaml step id > bundle
// step > yaml default > bundle defaults > built-in pi) and the shared
// model/harness compatibility check.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/workflow"
)

func harnessCfg(project string, models map[string][]string, h map[string]string) *Config {
	return &Config{
		Models:  map[string]map[string][]string{project: models},
		Harness: map[string]map[string]string{project: h},
	}
}

func TestComposeHarnessFiveLevels(t *testing.T) {
	fm := &workflow.Frontmatter{
		Defaults: &workflow.Defaults{Harness: "claude"},
		Steps: []workflow.FrontmatterStep{
			{ID: "a"},                // bundle defaults (no yaml)
			{ID: "b", Harness: "pi"}, // bundle step beats yaml default
			{ID: "c", Harness: "pi"}, // yaml step id beats bundle step
			{ID: "d"},                // yaml default beats bundle defaults
		},
	}
	got := ComposeHarness(fm, harnessCfg("p", nil, map[string]string{"c": "claude", "default": "pi"}), "p")
	want := map[string]string{"a": "pi", "b": "pi", "c": "claude", "d": "pi"}
	// "a": the yaml default (pi) beats the bundle defaults (claude)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
	// no yaml at all: bundle step, then bundle defaults
	got = ComposeHarness(fm, nil, "p")
	want = map[string]string{"a": "claude", "b": "pi", "c": "pi", "d": "claude"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bundle-only compose = %v, want %v", got, want)
	}
	// nothing declared anywhere: the built-in pi
	plain := &workflow.Frontmatter{Steps: []workflow.FrontmatterStep{{ID: "x"}}}
	if got := ComposeHarness(plain, nil, "p"); got["x"] != "pi" {
		t.Fatalf("built-in default = %v", got)
	}
	// another project's yaml entry does not apply
	if got := ComposeHarness(plain, harnessCfg("other", nil, map[string]string{"default": "claude"}), "p"); got["x"] != "pi" {
		t.Fatalf("foreign project applied: %v", got)
	}
	if ComposeHarness(nil, nil, "p") != nil {
		t.Fatal("nil frontmatter: nil map")
	}
}

func TestResolveStepsNamesSourcesAndFilters(t *testing.T) {
	fm := &workflow.Frontmatter{
		Defaults: &workflow.Defaults{ModelCandidates: []string{"g/glm"}},
		Steps:    []workflow.FrontmatterStep{{ID: "fix"}, {ID: "review"}},
	}
	cfg := harnessCfg("p", map[string][]string{"review": {"g/glm", "claude-opus-5-5"}}, map[string]string{"review": "claude"})
	rs := ResolveSteps("bug-fix", fm, cfg, "p", nil)
	if len(rs) != 2 {
		t.Fatalf("resolutions %v", rs)
	}
	fix, rev := rs[0], rs[1]
	if fix.Harness != "pi" || fix.HarnessFrom != "built-in default" || fix.ModelsFrom != "bundle bug-fix defaults.model_candidates" || !reflect.DeepEqual(fix.Served, []string{"g/glm"}) {
		t.Fatalf("fix %+v", fix)
	}
	if rev.Harness != "claude" || rev.HarnessFrom != "steps.p.review.harness" || rev.ModelsFrom != "steps.p.review.models" || !reflect.DeepEqual(rev.Served, []string{"claude-opus-5-5"}) {
		t.Fatalf("review %+v", rev)
	}
	// the manual list governs every step's models (not the harness)
	rs = ResolveSteps("bug-fix", fm, cfg, "p", []string{"m/x", "claude-sonnet-5"})
	if rs[1].ModelsFrom != "pf submit --models" || !reflect.DeepEqual(rs[1].Served, []string{"claude-sonnet-5"}) || !reflect.DeepEqual(rs[0].Served, []string{"m/x"}) {
		t.Fatalf("manual %+v", rs)
	}
}

func harnessBundles(t *testing.T, bundles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, fm := range bundles {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "workflow.md"), []byte(fm), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckHarness(t *testing.T) {
	wf := harnessBundles(t, map[string]string{
		"b1": "---\ndefaults:\n  model_candidates: [g/glm]\nsteps:\n  - { id: fix }\n  - { id: review }\n---\n",
		"b2": "---\nsteps:\n  - { id: review }\n  - { id: spec }\n---\n",
	})
	ws := t.TempDir()

	// no yaml, bundles all pi: fine
	if err := CheckHarness(ws, nil, wf, []string{"p"}, "", nil); err != nil {
		t.Fatalf("bundle-only: %v", err)
	}
	// claude review with only the (pi) bundle default list: refused, both
	// levels named, every bundle with a review step named
	cfg := harnessCfg("p", nil, map[string]string{"review": "claude"})
	err := CheckHarness(ws, cfg, wf, []string{"p"}, "", nil)
	if err == nil {
		t.Fatal("mismatch accepted")
	}
	msg := err.Error()
	for _, n := range []string{FileName, "model/harness mismatch", "p/b1/review: harness claude (steps.p.review.harness) cannot serve any of [g/glm] (bundle b1 defaults.model_candidates)"} {
		if !strings.Contains(msg, n) {
			t.Fatalf("refusal %q does not name %q", msg, n)
		}
	}
	// b2's review has no list at all: never a mismatch (the harness's own
	// default model runs)
	if strings.Contains(msg, "p/b2/review") {
		t.Fatalf("an absent list was judged: %q", msg)
	}
	// a declared-empty list is no mismatch either
	cfg = harnessCfg("p", map[string][]string{"review": {}}, map[string]string{"review": "claude"})
	if err := CheckHarness(ws, cfg, wf, []string{"p"}, "", nil); err != nil {
		t.Fatalf("empty list judged: %v", err)
	}
	// a mixed list serves: fine
	cfg = harnessCfg("p", map[string][]string{"review": {"g/glm", "claude-opus-5-5"}}, map[string]string{"review": "claude"})
	if err := CheckHarness(ws, cfg, wf, []string{"p"}, "", nil); err != nil {
		t.Fatalf("mixed list: %v", err)
	}
	// a pi step given only claude ids: refused
	cfg = harnessCfg("p", map[string][]string{"default": {"claude-opus-5-5"}}, nil)
	if err := CheckHarness(ws, cfg, wf, []string{"p"}, "", nil); err == nil || !strings.Contains(err.Error(), "harness pi (built-in default) cannot serve any of [claude-opus-5-5] (steps.p.default.models)") {
		t.Fatalf("pi given claude ids: %v", err)
	}
	// the manual list is checked against the named bundle only
	cfg = harnessCfg("p", nil, map[string]string{"spec": "claude"})
	if err := CheckHarness(ws, cfg, wf, []string{"p"}, "b2", []string{"g/glm"}); err == nil || !strings.Contains(err.Error(), "p/b2/spec") || !strings.Contains(err.Error(), "(pf submit --models)") {
		t.Fatalf("manual mismatch: %v", err)
	}
	if err := CheckHarness(ws, cfg, wf, []string{"p"}, "b1", []string{"g/glm"}); err != nil {
		t.Fatalf("manual list judged against another bundle: %v", err)
	}
	// a bundle-level unknown harness is named
	wf2 := harnessBundles(t, map[string]string{"b": "---\nsteps:\n  - { id: x, harness: codex }\n---\n"})
	if err := CheckHarness(ws, nil, wf2, []string{"p"}, "", nil); err == nil || !strings.Contains(err.Error(), `unknown harness "codex" (bundle b step x harness`) {
		t.Fatalf("bundle unknown harness: %v", err)
	}
}
