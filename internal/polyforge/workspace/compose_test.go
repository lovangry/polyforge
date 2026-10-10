package workspace

// compose_test.go: the four-level precedence chain, table-driven over
// the spec's Scenario blocks (Requirement 3). Fixture model ids are
// provider-qualified placeholders; no real model names appear.

import (
	"reflect"
	"testing"

	"github.com/silgrid/polyforge/internal/common/workflow"
)

func bundleWith(defaults []string, steps []workflow.FrontmatterStep) *workflow.Frontmatter {
	fm := &workflow.Frontmatter{Steps: steps}
	if defaults != nil {
		fm.Defaults = &workflow.Defaults{ModelCandidates: defaults}
	}
	return fm
}

// yamlWith builds a workspace yaml Config the way the loader would.
func yamlWith(modelCandidates []string, stepKinds map[string][]string) *Config {
	wd := &WorkflowDefaults{}
	if modelCandidates != nil {
		wd.ModelCandidates = modelCandidates
	}
	if stepKinds != nil {
		wd.StepKinds = stepKinds
	}
	return &Config{WorkflowDefaults: wd}
}

// yaml step_kinds[kind] wholly replaces the bundle step-level list for
// EVERY kind:code step; unlisted kinds keep the bundle's own resolution.
func TestComposeStepKindsReplacesEveryKindStep(t *testing.T) {
	fm := bundleWith([]string{"prov/weak-a", "prov/weak-b"}, []workflow.FrontmatterStep{
		{ID: "grill", Kind: "spec"},
		{ID: "fix", Kind: "code", ModelCandidates: []string{"prov/flash", "prov/glm"}},
		{ID: "release", Kind: "code", ModelCandidates: []string{"prov/glm", "prov/opus"}},
		{ID: "verification", Kind: "gate"},
	})
	got := ComposeCandidates(fm, yamlWith(nil, map[string][]string{"code": {"w79/code-a", "w79/code-b"}}))
	want := map[string][]string{
		"grill":        {"prov/weak-a", "prov/weak-b"}, // unlisted kind: bundle weak default
		"fix":          {"w79/code-a", "w79/code-b"},   // the per-kind pin replaces the step-level list
		"release":      {"w79/code-a", "w79/code-b"},   // …for every kind:code step
		"verification": {"prov/weak-a", "prov/weak-b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
}

// step_kinds is keyed by the step's KIND: a spec pin hits the
// kind-spec step while the kind-code fix keeps its own bundle list.
func TestComposeStepKindsKindScoping(t *testing.T) {
	fm := bundleWith([]string{"prov/weak-a"}, []workflow.FrontmatterStep{
		{ID: "grill", Kind: "spec"},
		{ID: "fix", Kind: "code", ModelCandidates: []string{"prov/flash"}},
		{ID: "release", Kind: "code"},
	})
	got := ComposeCandidates(fm, yamlWith(nil, map[string][]string{"spec": {"w79/spec-x"}}))
	want := map[string][]string{
		"grill":   {"w79/spec-x"},
		"fix":     {"prov/flash"},
		"release": {"prov/weak-a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
}

// Bundle step-level list beats the yaml flow default; the flow default
// replaces the bundle's WEAK default for steps without a step-level list.
func TestComposeFlowDefaultVsStepLevel(t *testing.T) {
	fm := bundleWith([]string{"prov/weak-a", "prov/weak-b"}, []workflow.FrontmatterStep{
		{ID: "grill", Kind: "spec"},
		{ID: "fix", Kind: "code", ModelCandidates: []string{"prov/flash"}},
		{ID: "release", Kind: "code", ModelCandidates: []string{"prov/glm"}},
	})
	got := ComposeCandidates(fm, yamlWith([]string{"w79/flow-a", "w79/flow-b"}, nil))
	want := map[string][]string{
		"grill":   {"w79/flow-a", "w79/flow-b"}, // the flow default replaces the weak default
		"fix":     {"prov/flash"},               // …never a step-specific list
		"release": {"prov/glm"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
}

// A yaml-silent step keeps the bundle's resolution exactly; with no
// list anywhere the map has no entry (no switchable chain — the
// harness adapter's default model stays in effect).
func TestComposeYamlSilentAndNoChainAnywhere(t *testing.T) {
	fm := bundleWith([]string{"prov/weak-a"}, []workflow.FrontmatterStep{
		{ID: "grill", Kind: "spec"},
		{ID: "naked", Kind: "code"},
	})
	// A workflow-only yaml (no step_kinds) leaves the bundle untouched.
	got := ComposeCandidates(fm, yamlWith([]string{"w79/flow-a"}, nil))
	want := map[string][]string{
		"grill": {"w79/flow-a"},
		"naked": {"w79/flow-a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}

	// A nil yaml: bundle-only behavior.
	got = ComposeCandidates(fm, nil)
	want = map[string][]string{
		"grill": {"prov/weak-a"},
		"naked": {"prov/weak-a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose(nil yaml) = %v, want %v", got, want)
	}

	// No chain anywhere: no entries at all.
	fm2 := bundleWith(nil, []workflow.FrontmatterStep{
		{ID: "grill", Kind: "spec"},
		{ID: "fix", Kind: "code"},
	})
	if got := ComposeCandidates(fm2, nil); got != nil {
		t.Fatalf("no chain anywhere must answer nil, got %v", got)
	}
	if got := ComposeCandidates(fm2, &Config{}); got != nil {
		t.Fatalf("an empty yaml over a silent bundle must answer nil, got %v", got)
	}
}

// Declared-empty lists are PRESENT levels: they govern (no switchable
// chain) and never fall through.
func TestComposeEmptyListsGovern(t *testing.T) {
	fm := bundleWith([]string{"prov/weak-a"}, []workflow.FrontmatterStep{
		{ID: "fix", Kind: "code", ModelCandidates: []string{"prov/flash"}},
		{ID: "release", Kind: "code"},
		{ID: "grill", Kind: "spec"},
	})
	// step_kinds[code]: [] wholly governs both kind:code steps…
	got := ComposeCandidates(fm, yamlWith(nil, map[string][]string{"code": {}}))
	want := map[string][]string{
		"fix":     {},
		"release": {},
		"grill":   {"prov/weak-a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
	// …and the yaml flow default [] governs the unlisted kind too
	// (replacing the bundle's weak default with no chain at all).
	got = ComposeCandidates(fm, yamlWith([]string{}, nil))
	want = map[string][]string{
		"fix":     {"prov/flash"},
		"release": {},
		"grill":   {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose(flow []) = %v, want %v", got, want)
	}
}

// The bundle's declared-empty step-level list governs over the yaml
// flow default (level 2 beats level 3), mirroring the driver parse's
// nil-collapse semantics.
func TestComposeBundleEmptyStepLevelBeatsYamlFlowDefault(t *testing.T) {
	fm := bundleWith([]string{"prov/weak-a"}, []workflow.FrontmatterStep{
		{ID: "release", Kind: "code", ModelCandidates: []string{}},
	})
	got := ComposeCandidates(fm, yamlWith([]string{"w79/flow-a"}, nil))
	want := map[string][]string{"release": {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
}

// A nil/absent bundle answers nil — the yaml alone has no steps to key
// on (the runtime still stashes the yaml for the audit).
func TestComposeNilBundle(t *testing.T) {
	if got := ComposeCandidates(nil, yamlWith([]string{"a"}, nil)); got != nil {
		t.Fatalf("nil frontmatter must answer nil, got %v", got)
	}
	if got := ComposeCandidates(&workflow.Frontmatter{}, yamlWith([]string{"a"}, nil)); got != nil {
		t.Fatalf("frontmatter without steps must answer nil, got %v", got)
	}
}
