package workspace

// compose_test.go: the precedence chain (project kind > project default
// > bundle step > bundle default), table-driven. Fixture model ids are
// placeholders.

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

func models(project string, entry map[string][]string) *Config {
	return &Config{Models: map[string]map[string][]string{project: entry}}
}

var testBundle = func() *workflow.Frontmatter {
	return bundleWith([]string{"prov/weak-a"}, []workflow.FrontmatterStep{
		{ID: "grill", Kind: "spec"},
		{ID: "fix", Kind: "code", ModelCandidates: []string{"prov/flash"}},
		{ID: "release", Kind: "code"},
		{ID: "verification", Kind: "gate"},
	})
}

// A project kind list wholly replaces every step of that kind (a bundle
// step-level list included); other kinds keep bundle resolution.
func TestComposeProjectKindReplacesEveryKindStep(t *testing.T) {
	got := ComposeCandidates(testBundle(), models("p", map[string][]string{"code": {"y/c1", "y/c2"}}), "p", nil)
	want := map[string][]string{
		"grill":        {"prov/weak-a"},
		"fix":          {"y/c1", "y/c2"},
		"release":      {"y/c1", "y/c2"},
		"verification": {"prov/weak-a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
}

// The project default governs every step lacking a kind list, beating
// the bundle's step-level and default lists; a kind list beats it.
func TestComposeProjectDefaultAndKindPrecedence(t *testing.T) {
	got := ComposeCandidates(testBundle(), models("p", map[string][]string{"default": {"y/d"}, "gate": {"y/g"}}), "p", nil)
	want := map[string][]string{
		"grill":        {"y/d"},
		"fix":          {"y/d"},
		"release":      {"y/d"},
		"verification": {"y/g"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose = %v, want %v", got, want)
	}
}

// A declared-empty list is present and governs: no fall-through.
func TestComposePresentEmptyGoverns(t *testing.T) {
	got := ComposeCandidates(testBundle(), models("p", map[string][]string{"default": {"y/d"}, "code": {}}), "p", nil)
	if l, ok := got["fix"]; !ok || l == nil || len(l) != 0 {
		t.Fatalf("fix = %#v, want present-empty", got["fix"])
	}
	if !reflect.DeepEqual(got["grill"], []string{"y/d"}) {
		t.Fatalf("grill = %v", got["grill"])
	}
	got = ComposeCandidates(testBundle(), models("p", map[string][]string{"default": {}}), "p", nil)
	for id, l := range got {
		if len(l) != 0 {
			t.Fatalf("%s = %v, want empty (default [] governs)", id, l)
		}
	}
	if len(got) != 4 {
		t.Fatalf("present-empty entries must exist for every step: %v", got)
	}
}

// A project the yaml does not list, a nil config, and a nil bundle.
func TestComposeUnlistedNilAndBundleOnly(t *testing.T) {
	bundleOnly := map[string][]string{
		"grill":        {"prov/weak-a"},
		"fix":          {"prov/flash"},
		"release":      {"prov/weak-a"},
		"verification": {"prov/weak-a"},
	}
	cfg := models("p", map[string][]string{"default": {"y/d"}})
	if got := ComposeCandidates(testBundle(), cfg, "other", nil); !reflect.DeepEqual(got, bundleOnly) {
		t.Fatalf("unlisted project = %v, want bundle-only %v", got, bundleOnly)
	}
	if got := ComposeCandidates(testBundle(), nil, "p", nil); !reflect.DeepEqual(got, bundleOnly) {
		t.Fatalf("nil config = %v, want bundle-only", got)
	}
	if got := ComposeCandidates(nil, cfg, "p", nil); got != nil {
		t.Fatalf("nil bundle = %v, want nil", got)
	}
	if got := ComposeCandidates(bundleWith(nil, []workflow.FrontmatterStep{{ID: "a", Kind: "code"}}), nil, "p", nil); got != nil {
		t.Fatalf("no list anywhere = %v, want nil", got)
	}
}

// Two projects in one yaml compose independently.
func TestComposeOtherProjectUnaffected(t *testing.T) {
	cfg := &Config{Models: map[string]map[string][]string{
		"a": {"code": {"y/a"}},
		"b": {"code": {"y/b"}},
	}}
	if got := ComposeCandidates(testBundle(), cfg, "a", nil); !reflect.DeepEqual(got["fix"], []string{"y/a"}) {
		t.Fatalf("a fix = %v", got["fix"])
	}
	if got := ComposeCandidates(testBundle(), cfg, "b", nil); !reflect.DeepEqual(got["fix"], []string{"y/b"}) {
		t.Fatalf("b fix = %v", got["fix"])
	}
}

func TestComposeManualBeatsEveryOtherLevel(t *testing.T) {
	manual := []string{"man/a", "man/b"}
	cfg := models("p", map[string][]string{"default": {"y/d"}, "code": {"y/c"}})
	for _, c := range []*Config{cfg, nil} {
		got := ComposeCandidates(testBundle(), c, "p", manual)
		for _, step := range []string{"grill", "fix", "release"} {
			if !reflect.DeepEqual(got[step], manual) {
				t.Fatalf("step %s = %v, want the manual list over yaml kind/default and bundle", step, got[step])
			}
		}
	}
	// a present-empty yaml level does not beat the manual list either
	if got := ComposeCandidates(testBundle(), models("p", map[string][]string{"default": {}}), "p", manual); !reflect.DeepEqual(got["grill"], manual) {
		t.Fatalf("manual must beat a present-empty yaml level, got %v", got["grill"])
	}
	// no manual list: unchanged
	if got := ComposeCandidates(testBundle(), cfg, "p", nil); !reflect.DeepEqual(got["fix"], []string{"y/c"}) {
		t.Fatalf("without a manual list the yaml chain governs, got %v", got["fix"])
	}
}

func TestNormalizeModels(t *testing.T) {
	if got, err := NormalizeModels([]string{" a/x ", "b"}); err != nil || !reflect.DeepEqual(got, []string{"a/x", "b"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for name, in := range map[string][]string{
		"empty list": nil, "empty id": {"a", ""}, "blank id": {"a", "  "}, "duplicate": {"a", "b", " a"},
	} {
		if _, err := NormalizeModels(in); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}
