package watch

// summarize.go: the delta-gated summarize lane. The summarize model
// comes ONLY from the explicit per-harness config
// ([watch.summarize_model.<harness>] in cli.toml): no default, no env
// fallback, never a silent no-summary. A missing entry is a typed
// failure naming the key, printing the config sample and pointing at
// `pf doctor`. Dispatch happens ONLY on transcript growth (the
// delta-trigger: zero model calls on a silent transcript), and a failed
// dispatch never fails the probe: the snapshot stays intact with summary
// null and summary_error carrying the raw error text — the error is
// never faked as "no new output".

import (
	"fmt"
	"strings"
)

// ModelConfig is the per-harness summarize-model configuration read from
// cli.toml's [watch.summarize_model.<harness>] sections: harness → model.
type ModelConfig map[string]string

// NormalizeHarness maps the capsule's recorded harness onto the watch
// lane's vocabulary (the config key and the dispatch binary): pi stays
// pi; claude (and the legacy capsule spelling "cc") map to claude.
func NormalizeHarness(harness string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "pi":
		return "pi", nil
	case "claude", "cc":
		return "claude", nil
	case "":
		return "", fmt.Errorf("watch: the capsule's session.json records no harness — cannot pick the summarize-model key")
	default:
		return "", fmt.Errorf("watch: unknown harness %q in the capsule's session.json — the watch lane dispatches pi or claude", harness)
	}
}

// SummarizeModel answers the configured summarize model for harness.
// Missing or empty is the typed failure (spec scenario "Missing key
// fails typed"): the key named, the config sample printed, `pf doctor`
// pointed at.
func SummarizeModel(models ModelConfig, harness string) (string, error) {
	m := strings.TrimSpace(models[harness])
	if m == "" {
		return "", fmt.Errorf("watch: no summarize model configured for harness %q — the key watch.summarize_model.%s is missing or empty in cli.toml. The summarize model is explicit config: no default, no env fallback. Add to cli.toml:\n\n"+
			"  [watch.summarize_model.%s]\n"+
			"  model = \"<provider>/<model>\"\n\n"+
			"then run pf doctor to re-check this machine's watch configuration.", harness, harness, harness)
	}
	return m, nil
}

// BuildSummarizePrompt renders the one-line summarize ask over the
// snapshot's transcript tail (bounded by TranscriptTailRecords).
func BuildSummarizePrompt(snap *Snapshot) string {
	var b strings.Builder
	b.WriteString("Summarize in ONE short line what this coding-agent work item is doing right now: concrete current activity, plain text, no markdown. Transcript tail (oldest first):\n")
	for _, line := range snap.TranscriptTail {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// AttachSummary dispatches ONE summarize call and fills the snapshot's
// summary fields. On success Summary carries the model's line (collapsed
// to one line; labeled non-authoritative by the callers). On failure
// Summary stays null and SummaryError carries the RAW error text.
func AttachSummary(snap *Snapshot, dispatch Dispatcher, harness, model string) {
	out, err := dispatch.Dispatch(harness, model, BuildSummarizePrompt(snap))
	if err != nil {
		msg := err.Error()
		snap.SummaryError = &msg
		return
	}
	line := strings.Join(strings.Fields(out), " ")
	snap.Summary = &line
}

// Probe is one one-shot probe run: assemble the snapshot, dispatch the
// delta-gated summarize (ZERO model calls when the transcript has not
// grown), persist the delta state either way (the transcript size is a
// fact; the summary is narration).
type Probe struct {
	Deps       Deps
	Models     ModelConfig
	Harness    string // normalized harness (pi | claude)
	Dispatcher Dispatcher
}

// Run executes the probe. The missing-model check is eager and typed:
// it fires before any state or dispatch work, exactly as the spec pins
// ("WHEN pf watch-probe runs, THEN it fails typed").
func (p Probe) Run() (*Snapshot, error) {
	model, err := SummarizeModel(p.Models, p.Harness)
	if err != nil {
		return nil, err
	}
	if p.Dispatcher == nil {
		return nil, fmt.Errorf("watch: no dispatch adapter wired")
	}
	snap, delta, err := Assemble(p.Deps)
	if err != nil {
		return nil, err
	}
	if delta.Grew {
		AttachSummary(snap, p.Dispatcher, p.Harness, model)
	}
	if err := SaveState(delta.StatePath, delta.Next); err != nil {
		return nil, err
	}
	return snap, nil
}
