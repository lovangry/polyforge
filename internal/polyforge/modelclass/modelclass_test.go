package modelclass

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/harness"
)

// testModelsJSON is the fixture view: two providers with models, one
// provider with an empty model list (no pool entry — no split anchor),
// one gateway provider whose model ID is itself provider-shaped.
const testModelsJSON = `{
  "providers": {
    "w19p-good": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "w19p-model", "name": "W19 P Model"}]
    },
    "example-claude": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "claude-opus-5", "name": "claude-opus-5"}]
    },
    "w19p-empty": {"baseUrl": "https://w19p.invalid", "models": []},
    "gw": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "vendor/model-x", "name": "vendor model x"}]
    }
  }
}`

func fixtureDir(t *testing.T, modelsJSON string) string {
	t.Helper()
	dir := t.TempDir()
	if modelsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(modelsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestPreflightTable pins the skip rule against one fixture models.json:
// only a provider-qualified candidate whose provider is in NO pool — not
// the file (with at least one model) and not pi's built-in catalog — is
// skipped, with pi's own startup diagnostic as the reason. Every other
// shape fails open.
func TestPreflightTable(t *testing.T) {
	dir := fixtureDir(t, testModelsJSON)
	cases := []struct {
		name    string
		cand    string
		skip    bool
		reasons []string
	}{
		{"provider absent from every pool", "pi:w19p-bad/w19p-x", true,
			[]string{"w19p-bad/w19p-x", "not found", "--list-models"}},
		{"shipped placeholder provider absent", "pi:example-gateway/model-a", true,
			[]string{"example-gateway/model-a", "not found"}},
		{"provider declared with no models is no anchor", "pi:w19p-empty/w19p-model", true,
			[]string{"w19p-empty/w19p-model"}},
		{"known provider, unknown model id passes through", "pi:w19p-good/w19p-other", false, nil},
		{"known provider, known model", "pi:w19p-good/w19p-model", false, nil},
		{"provider match is case-insensitive", "pi:W19P-Good/w19p-model", false, nil},
		{"pi built-in provider not in the file", "pi:anthropic/claude-sonnet-4-5", false, nil},
		{"pi built-in provider with unknown model", "pi:openai/whatever-model", false, nil},
		{"gateway literal model id matches the raw pool", "pi:vendor/model-x", false, nil},
		{"bare model id matching a configured id", "pi:w19p-model", false, nil},
		{"bare model id contained in a configured name", "pi:opus", false, nil},
		{"bare unknown id fails open (built-in catalogs unseen)", "pi:not-anywhere", false, nil},
		{"claude candidates are never file-inspected", "claude:w19p-bad/w19p-x", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cand, err := harness.ParseCandidate(tc.cand)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.cand, err)
			}
			skip, reason := Preflight(dir, cand)
			if skip != tc.skip {
				t.Fatalf("Preflight(%q) = skip %v (reason %q), want %v", tc.cand, skip, reason, tc.skip)
			}
			for _, want := range tc.reasons {
				if !strings.Contains(reason, want) {
					t.Fatalf("reason %q must name %q", reason, want)
				}
			}
		})
	}
}

// TestPreflightComposedDiagnosticFiresTheMovedPredicates pins that the
// skip gate IS the classification: the diagnostic the preflight composes
// must match the very predicates verdictCause archives exit_model_class
// with — the same two the plan's mechanism decision names.
func TestPreflightComposedDiagnosticFiresTheMovedPredicates(t *testing.T) {
	dir := fixtureDir(t, testModelsJSON)
	cand, _ := harness.ParseCandidate("pi:w19p-bad/w19p-x")
	skip, reason := Preflight(dir, cand)
	if !skip {
		t.Fatal("the absent-provider candidate must be skipped")
	}
	if !UnresolvedModel(reason) {
		t.Fatalf("the composed diagnostic %q must fire UnresolvedModel — the preflight gate is the classification itself", reason)
	}
	if !AllowanceText.MatchString(reason) && reason == "" {
		t.Fatal("unreachable shape guard")
	}
}

// TestPreflightFailsOpen pins the fail-open rules: an agent dir with no
// models.json, an unreadable one and an unparseable one all skip nothing
// (nothing is proven unresolvable; the dispatch lane is unchanged).
func TestPreflightFailsOpen(t *testing.T) {
	cand, _ := harness.ParseCandidate("pi:w19p-bad/w19p-x")
	for name, dir := range map[string]string{
		"no models.json":   fixtureDir(t, ""),
		"unparseable":     fixtureDir(t, "{not json"),
		"empty agent dir": "",
	} {
		t.Run(name, func(t *testing.T) {
			if skip, reason := Preflight(dir, cand); skip {
				t.Fatalf("Preflight with %s must fail open, got a skip (reason %q)", name, reason)
			}
		})
	}
}

// TestPredicateMatchesPreflight pins the once-per-dispatch form against
// the single-shot one: the same view answers the same decisions.
func TestPredicateMatchesPreflight(t *testing.T) {
	dir := fixtureDir(t, testModelsJSON)
	pred := Predicate(dir)
	for _, id := range []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model", "pi:w19p-good/other"} {
		cand, _ := harness.ParseCandidate(id)
		sSkip, sReason := Preflight(dir, cand)
		pSkip, pReason := pred(cand)
		if sSkip != pSkip || sReason != pReason {
			t.Fatalf("Predicate and Preflight disagree on %q: (%v, %q) vs (%v, %q)", id, sSkip, sReason, pSkip, pReason)
		}
	}
}

// TestUnresolvedModelStillClassifies pins the moved predicates'
// behavior in their new home (the regression guard for the relocation
// itself: the driver's verdictCause and the preflight share them).
func TestUnresolvedModelStillClassifies(t *testing.T) {
	yes := []string{
		`unknown model "w19p-x"`,
		"model_not_found",
		`Model "w19p-bad/w19p-x" not found. Use --list-models to see available models.`,
		"The model `x` does not exist",
	}
	no := []string{
		"The model encountered an unknown error",
		"model server returned unknown status",
		"model endpoint not found, retrying",
		"model is temporarily unavailable",
	}
	for _, s := range yes {
		if !UnresolvedModel(s) {
			t.Fatalf("UnresolvedModel(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if UnresolvedModel(s) {
			t.Fatalf("UnresolvedModel(%q) = true, want false", s)
		}
	}
	if !AllowanceText.MatchString("429 exceeded your current quota") {
		t.Fatal("AllowanceText must still match the quota row")
	}
	if AllowanceText.MatchString("upstream connection reset while streaming") {
		t.Fatal("AllowanceText must not match a bare transport signature")
	}
}
