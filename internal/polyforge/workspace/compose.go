package workspace

// compose.go: the model-candidate precedence chain as a PURE function
// over the bundle frontmatter, the parsed workspace yaml and the claimed
// project — no I/O, no state, fully table-testable. The runtime computes
// it once per claim and snapshots the result into capsule.json; recovery
// rehydrates that snapshot, never a second composition.
//
// The chain, outermost first:
//
//	yaml models.<project>.<kind> > yaml models.<project>.default >
//	bundle step-level model_candidates > bundle defaults.model_candidates
//
// The kind is the step's V8 KIND, not its id — a `code` list governs
// EVERY kind:code step of the bundle. Resolution is non-merge: the first
// PRESENT level wholly replaces the others. Presence is "declared": a
// declared-but-empty list is a PRESENT level that governs — it yields no
// switchable chain (the harness adapter's default model stays in
// effect) and NEVER a silent fall-through. A project the yaml does not
// list leaves the bundle alone.

import (
	"github.com/silgrid/polyforge/internal/common/workflow"
)

// ComposeCandidates answers the per-step effective candidate lists of
// one claim of project, keyed by step id. An entry exists for every step
// whose composed list is non-nil (including present-empty lists); a step
// absent from the map (or a nil map) means no list applies anywhere. fm
// may be nil (no bundle frontmatter — no steps to key on); cfg may be
// nil (no yaml — the bundle alone).
func ComposeCandidates(fm *workflow.Frontmatter, cfg *Config, project string) map[string][]string {
	if fm == nil || len(fm.Steps) == 0 {
		return nil
	}
	var pm map[string][]string
	if cfg != nil {
		pm = cfg.Models[project]
	}
	out := make(map[string][]string, len(fm.Steps))
	for _, s := range fm.Steps {
		if list := composeStep(fm, pm, s); list != nil {
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
	if list, ok := pm[s.Kind]; ok {
		return list
	}
	if list, ok := pm[DefaultKey]; ok {
		return list
	}
	if s.ModelCandidates != nil {
		return s.ModelCandidates
	}
	if fm.Defaults != nil {
		return fm.Defaults.ModelCandidates
	}
	return nil
}
