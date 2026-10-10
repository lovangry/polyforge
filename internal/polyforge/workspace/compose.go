package workspace

// compose.go: the four-level model-candidate precedence chain
// (polyforge#79) as a PURE function over the bundle frontmatter and the
// parsed workspace yaml — no I/O, no state, fully table-testable. The
// runtime computes it once per claim; the recovery path re-derives it
// through the same function (rehydrating or re-reading), never through
// a second composition rule.
//
// The chain, outermost first (spec Requirement 3):
//
//	yaml step_kinds[kind] > bundle step-level model_candidates >
//	yaml workflow_defaults.model_candidates > bundle defaults.model_candidates
//
// step_kinds is keyed by the step's V8 KIND, not its id — a `code` pin
// governs EVERY kind:code step of the bundle. Resolution is non-merge:
// the first PRESENT level wholly replaces the others; a more-specific
// entry is never concatenated with a less-specific one.
//
// Presence is "declared" (non-nil), matching the bundle parse's
// semantics: a declared-but-empty list is a PRESENT level that wholly
// governs — it yields no switchable chain for the steps it governs
// (the harness adapter's default model stays in effect) and NEVER a
// silent fall-through to the next level. Absent keys fall through per
// the chain.

import (
	"github.com/silgrid/polyforge/internal/common/workflow"
)

// ComposeCandidates answers the per-step effective candidate lists of
// one claim, keyed by step id. An entry exists for every step whose
// composed list is non-nil (including present-empty lists — they
// govern, see above); a step absent from the map (or a nil map) means
// no list applies anywhere — the harness adapter's default model stays
// in effect. fm may be nil (no bundle frontmatter — the yaml alone has
// no steps to key on); cfg may be nil (no yaml — the bundle alone).
func ComposeCandidates(fm *workflow.Frontmatter, cfg *Config) map[string][]string {
	if fm == nil || len(fm.Steps) == 0 {
		return nil
	}
	out := make(map[string][]string, len(fm.Steps))
	for _, s := range fm.Steps {
		if list := composeStep(fm, cfg, s); list != nil {
			out[s.ID] = list
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// composeStep resolves one step's effective list through the chain.
func composeStep(fm *workflow.Frontmatter, cfg *Config, s workflow.FrontmatterStep) []string {
	// 1. yaml step_kinds[kind] — the outermost level, keyed by the
	// step's V8 kind. A present entry (even empty) wholly replaces.
	if wd := yamlDefaults(cfg); wd != nil && wd.StepKinds != nil {
		if list, ok := wd.StepKinds[s.Kind]; ok && list != nil {
			return list
		}
	}
	// 2. the bundle step-level list, when the step declares one
	// (present = non-nil; a declared-empty list governs).
	if s.ModelCandidates != nil {
		return s.ModelCandidates
	}
	// 3. the yaml flow default, when the yaml declares one.
	if wd := yamlDefaults(cfg); wd != nil && wd.ModelCandidates != nil {
		return wd.ModelCandidates
	}
	// 4. the bundle's defaults.model_candidates (nil when the bundle is
	// silent — no chain anywhere).
	if fm.Defaults != nil {
		return fm.Defaults.ModelCandidates
	}
	return nil
}

// yamlDefaults answers the yaml's workflow_defaults section (nil when
// the workspace yaml is absent or carries none).
func yamlDefaults(cfg *Config) *WorkflowDefaults {
	if cfg == nil {
		return nil
	}
	return cfg.WorkflowDefaults
}
