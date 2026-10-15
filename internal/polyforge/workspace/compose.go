package workspace

// compose.go: the model-candidate precedence chain as a PURE function
// over the bundle frontmatter, the parsed workspace yaml and the claimed
// project — no I/O, no state, fully table-testable. The runtime computes
// it once per claim and snapshots the result into capsule.json; recovery
// rehydrates that snapshot, never a second composition.
//
// The chain, outermost first (five levels):
//
//	manual `pf submit --models` > yaml steps.<project>.<step-id>.models >
//	bundle step-level model_candidates > yaml steps.<project>.default.models >
//	bundle defaults.model_candidates
//
// A step's own list outranks the project yaml `default` (polyforge#164,
// H10): the yaml default is the project's fallback for steps that make
// no choice of their own, so a bundle step that pins its model (e.g. an
// Opus-first root-cause, a flash-first verification) keeps it. A yaml
// step-id list still overrides the step — it is the project's explicit
// per-step decision.
//
// The manual list is the per-work-item level: when present it is the
// candidate chain of EVERY step of that work item.
//
// The key is the step id: a `fix` list governs the `fix` step of every
// bundle that has one. Resolution is non-merge: the first
// PRESENT level wholly replaces the others. Presence is "declared": a
// declared-but-empty list is a PRESENT level that governs — it yields no
// switchable chain (the harness adapter's default model stays in
// effect) and NEVER a silent fall-through. A project the yaml does not
// list leaves the bundle alone.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/silgrid/polyforge/internal/common/harness"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

// ComposeCandidates answers the per-step effective candidate lists of
// one claim of project, keyed by step id. An entry exists for every step
// whose composed list is non-nil (including present-empty lists); a step
// absent from the map (or a nil map) means no list applies anywhere. fm
// may be nil (no bundle frontmatter — no steps to key on); cfg may be
// nil (no yaml — the bundle alone).
// manual is the work item's `pf submit --models` list (nil/empty = none).
func ComposeCandidates(fm *workflow.Frontmatter, cfg *Config, project string, manual []string) map[string][]string {
	if fm == nil || len(fm.Steps) == 0 {
		return nil
	}
	var pm map[string][]string
	if cfg != nil {
		pm = cfg.Models[project]
	}
	out := make(map[string][]string, len(fm.Steps))
	for _, s := range fm.Steps {
		if len(manual) > 0 {
			out[s.ID] = append([]string(nil), manual...)
		} else if list := composeStep(fm, pm, s); list != nil {
			out[s.ID] = list
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// composeStep resolves one step's effective list through the chain; pm
// is the claimed project's yaml entry (nil when it has none).
func composeStep(fm *workflow.Frontmatter, pm map[string][]string, s workflow.FrontmatterStep) []string {
	list, _ := composeStepFrom(fm, pm, s, "", "")
	return list
}

// composeStepFrom is composeStep that also names the level the list came
// from ("" when none applies); project/bundle only label the source.
func composeStepFrom(fm *workflow.Frontmatter, pm map[string][]string, s workflow.FrontmatterStep, project, bundle string) ([]string, string) {
	if list, ok := pm[s.ID]; ok {
		return list, fmt.Sprintf("steps.%s.%s.models", project, s.ID)
	}
	if s.ModelCandidates != nil {
		return s.ModelCandidates, fmt.Sprintf("bundle %s step %s model_candidates", bundle, s.ID)
	}
	if list, ok := pm[DefaultKey]; ok {
		return list, fmt.Sprintf("steps.%s.%s.models", project, DefaultKey)
	}
	if fm.Defaults != nil && fm.Defaults.ModelCandidates != nil {
		return fm.Defaults.ModelCandidates, fmt.Sprintf("bundle %s defaults.model_candidates", bundle)
	}
	return nil, ""
}

// ---------------------------------------------------------------------------
// Harness (polyforge#159): the second per-step property, resolved
// INDEPENDENTLY down the same levels (non-merge, first present level
// wins), with the built-in pi at the bottom and no per-work-item level:
//
//	yaml steps.<project>.<step-id>.harness > bundle step harness >
//	yaml steps.<project>.default.harness > bundle defaults.harness > pi

// ComposeHarness answers the per-step effective harness of one claim of
// project, keyed by step id — an entry for every step of the bundle (nil
// when fm declares no steps). Like the candidates it is computed once
// per claim and snapshotted daemon-side; an edit governs the next claim.
func ComposeHarness(fm *workflow.Frontmatter, cfg *Config, project string) map[string]string {
	if fm == nil || len(fm.Steps) == 0 {
		return nil
	}
	var ph map[string]string
	if cfg != nil {
		ph = cfg.Harness[project]
	}
	out := make(map[string]string, len(fm.Steps))
	for _, s := range fm.Steps {
		h, _ := composeHarnessFrom(fm, ph, s, project, "")
		out[s.ID] = h
	}
	return out
}

// composeHarnessFrom resolves one step's harness and names its level.
func composeHarnessFrom(fm *workflow.Frontmatter, ph map[string]string, s workflow.FrontmatterStep, project, bundle string) (string, string) {
	if h, ok := ph[s.ID]; ok {
		return h, fmt.Sprintf("steps.%s.%s.harness", project, s.ID)
	}
	if s.Harness != "" {
		return s.Harness, fmt.Sprintf("bundle %s step %s harness", bundle, s.ID)
	}
	if h, ok := ph[DefaultKey]; ok {
		return h, fmt.Sprintf("steps.%s.%s.harness", project, DefaultKey)
	}
	if fm.Defaults != nil && fm.Defaults.Harness != "" {
		return fm.Defaults.Harness, fmt.Sprintf("bundle %s defaults.harness", bundle)
	}
	return harness.Pi, "built-in default"
}

// StepResolution is one step's resolved pair (the doctor's resolution
// table and the compatibility check share it): the effective harness and
// candidate list, each with the level it came from, and the candidates
// that harness can actually run.
type StepResolution struct {
	Project     string   `json:"project"`
	Bundle      string   `json:"bundle"`
	Step        string   `json:"step"`
	Harness     string   `json:"harness"`
	HarnessFrom string   `json:"harness_from"`
	Models      []string `json:"models,omitempty"`
	ModelsFrom  string   `json:"models_from,omitempty"`
	// Served is Models filtered to the ids Harness serves (the list the
	// driver walks); nil when no list is in effect.
	Served []string `json:"served,omitempty"`
}

// Mismatch reports a step whose harness serves none of a non-empty
// candidate list (an empty or absent list is never a mismatch: the
// harness's own default model runs).
func (r StepResolution) Mismatch() bool {
	return len(r.Models) > 0 && len(r.Served) == 0
}

// ResolveSteps answers the resolution of every step of one bundle for
// one project (manual = the work item's `pf submit --models` list).
func ResolveSteps(bundle string, fm *workflow.Frontmatter, cfg *Config, project string, manual []string) []StepResolution {
	if fm == nil {
		return nil
	}
	var pm map[string][]string
	var ph map[string]string
	if cfg != nil {
		pm, ph = cfg.Models[project], cfg.Harness[project]
	}
	out := make([]StepResolution, 0, len(fm.Steps))
	for _, s := range fm.Steps {
		r := StepResolution{Project: project, Bundle: bundle, Step: s.ID}
		r.Harness, r.HarnessFrom = composeHarnessFrom(fm, ph, s, project, bundle)
		if len(manual) > 0 {
			r.Models, r.ModelsFrom = append([]string(nil), manual...), "pf submit --models"
		} else {
			r.Models, r.ModelsFrom = composeStepFrom(fm, pm, s, project, bundle)
		}
		r.Served = harness.Filter(r.Harness, r.Models)
		out = append(out, r)
	}
	return out
}

// Bundles answers the installed bundles under workflowsDir (name →
// frontmatter, nil frontmatter skipped) — or only the named one when
// bundle is non-empty. A torn frontmatter is the bundle's own defect
// (its claim surfaces it) and is skipped here.
func Bundles(workflowsDir, bundle string) (map[string]*workflow.Frontmatter, error) {
	out := map[string]*workflow.Frontmatter{}
	if workflowsDir == "" {
		return out, nil
	}
	names := []string{bundle}
	if bundle == "" {
		ents, err := os.ReadDir(workflowsDir)
		if err != nil {
			return nil, err
		}
		names = names[:0]
		for _, e := range ents {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	for _, n := range names {
		fm, err := workflow.ReadFrontmatter(filepath.Join(workflowsDir, n, "workflow.md"))
		if err != nil || fm == nil {
			continue
		}
		out[n] = fm
	}
	return out, nil
}

// CheckHarness is the polyforge#159 load-time compatibility check — the
// ONE function the pre-claim read (runtime.LoadWorkspace) and pf doctor
// share. For every project × installed bundle (only bundle when named) ×
// step it resolves the effective harness and candidate list and refuses
// (a typed *Error naming the workspace yaml path) when the harness is
// outside the closed set or serves none of a non-empty list. Every
// problem is named, sorted; nil when all steps are runnable. cfg may be
// nil (bundle levels only). manual is the work item's --models list.
func CheckHarness(workspaceRoot string, cfg *Config, workflowsDir string, projects []string, bundle string, manual []string) error {
	bundles, err := Bundles(workflowsDir, bundle)
	if err != nil {
		return &Error{Path: PathFor(workspaceRoot), Reason: fmt.Sprintf("the installed workflows are unreadable, so the model/harness pairs cannot be checked: %v", err)}
	}
	var problems []string
	for _, project := range projects {
		for name, fm := range bundles {
			for _, r := range ResolveSteps(name, fm, cfg, project, manual) {
				where := project + "/" + name + "/" + r.Step
				switch {
				case !harness.Valid(r.Harness):
					problems = append(problems, fmt.Sprintf("%s: unknown harness %q (%s; want %s)", where, r.Harness, r.HarnessFrom, strings.Join(harness.Names, " or ")))
				case r.Mismatch():
					problems = append(problems, fmt.Sprintf("%s: harness %s (%s) cannot serve any of [%s] (%s) — %s", where, r.Harness, r.HarnessFrom, strings.Join(r.Models, ", "), r.ModelsFrom, compatRule(r.Harness)))
				}
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return &Error{Path: PathFor(workspaceRoot), Reason: "model/harness mismatch: " + strings.Join(problems, "; ")}
}

// compatRule states the id-shape rule a mismatched harness needs.
func compatRule(h string) string {
	if h == harness.Claude {
		return "claude runs only official `claude-…` model ids (no `/`); declare steps.<project>.<step>.models with such ids"
	}
	return "pi runs `provider/model` ids, never a bare `claude-…` id (that is Claude Code's)"
}

// NormalizeModels validates a manual `--models` list: non-empty, every
// id trimmed and non-empty, no duplicates. It returns the trimmed list.
// There is no model registry to check ids against.
func NormalizeModels(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("models list is empty")
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, fmt.Errorf("models list has an empty model id")
		}
		if seen[m] {
			return nil, fmt.Errorf("models list repeats %q", m)
		}
		seen[m] = true
		out = append(out, m)
	}
	return out, nil
}
