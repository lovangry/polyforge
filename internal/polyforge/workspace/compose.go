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
	"strings"

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
	if list, ok := pm[s.ID]; ok {
		return list
	}
	if s.ModelCandidates != nil {
		return s.ModelCandidates
	}
	if list, ok := pm[DefaultKey]; ok {
		return list
	}
	if fm.Defaults != nil {
		return fm.Defaults.ModelCandidates
	}
	return nil
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
