package cli

// migrate.go: shared foundation of the two migration verbs
// (migrate-memories, migrate-work-items). The engine is the v1 port of
// the pilot-validated v0 tool (/v0/internal/migrate): JSONL-only old-world
// input, identity resolution with deterministic placeholders, direct
// authority-file placement (CLI直写 — the memory API is never used for
// stock; the server's file-wins reconcile is the only backfill path), the
// explicit rebuild-index trigger and the four-step post-import
// verification. v1 deltas vs the v0 tool:
//
//   - a second input path: --from-dir reads a v0 authority-file tree
//     (the pilot's memory/<pj>/<mem>/memory.md layout) instead of JSONL,
//     validating the frontmatter against the v1 format field-by-field and
//     logging every conversion;
//   - work_item_id is PRESERVED when the id resolves to a work item the
//     target already carries (v1 migrates terminal WI records with their
//     ids, so the reference stays honest); unresolvable references are
//     still blanked and logged;
//   - the target project is a required explicit flag (--project): the
//     lane imports into one prepared target, it never auto-creates
//     projects (the seeding order in tools/seed-v1.sh creates them).

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"
	"unicode"
)

// StepResult is the outcome of one verification step.
type StepResult struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ResolveError is a fatal resolution/config failure (map naming a
// nonexistent id, unreachable server) — distinct from verification
// failures, which only surface after the import landed.
type ResolveError struct {
	msg string
	// err is the wrapped wire cause, when the failure came from the
	// server: the typed error class (4xx/5xx) must survive the wrap so
	// Run's exit code keeps its contract (4xx → 1, 5xx → 2, #40).
	err error
}

func (e *ResolveError) Error() string { return e.msg }

// Unwrap preserves the typed error chain for exitCode classification.
func (e *ResolveError) Unwrap() error { return e.err }

func resolveErrf(format string, args ...any) error {
	return &ResolveError{msg: fmt.Sprintf(format, args...)}
}

// resolveWrapErrf is resolveErrf for a failure whose CAUSE is a wire
// error: the cause stays wrapped (Unwrap) so a typed 5xx still maps to
// exit 2 instead of degrading to the generic client-class 1.
func resolveWrapErrf(cause error, format string, args ...any) error {
	return &ResolveError{msg: fmt.Sprintf(format, args...), err: cause}
}

// verifyError aggregates the failed verification steps.
type verifyError struct{ failures []string }

func (e *verifyError) Error() string {
	return "verification failed: " + joinNonEmpty(e.failures, "; ")
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

// migParseError names the input line a validation failure came from: a
// malformed record is input corruption, never something to silently drop.
type migParseError struct {
	Line int
	Msg  string
}

func (e *migParseError) Error() string { return fmt.Sprintf("input line %d: %s", e.Line, e.Msg) }

// readMapFile reads an optional --user-map style file: a JSON object
// old identifier → new id.
func readMapFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// migSampleRatio fixes the verification sampling ratio at 10% (v0 §10.1
// ② semantics; at least one entry when anything was imported).
const (
	migSampleRatioN = 1
	migSampleRatioD = 10
)

// migSampleRand is the deterministic sampler: re-runs verify the same
// sample, so the idempotent re-run report is reproducible.
func migSampleRand() *rand.Rand { return rand.New(rand.NewSource(1)) }

// migSampleIDs picks the deterministic sample of placed/imported ids.
func migSampleIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	n := (len(ids)*migSampleRatioN + migSampleRatioD - 1) / migSampleRatioD // ceil
	if n < 1 {
		n = 1
	}
	idx := make([]int, len(ids))
	for i := range idx {
		idx[i] = i
	}
	migSampleRand().Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	if n > len(idx) {
		n = len(idx)
	}
	out := make([]string, 0, n)
	for _, i := range idx[:n] {
		out = append(out, ids[i])
	}
	// sorted for a stable report
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// migParseRFC3339 accepts the RFC3339 timestamps Core uses; old-world
// exports may carry fractional seconds of any precision.
func migParseRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not RFC3339: %q (%v)", s, err)
	}
	return t, nil
}

// migProbeMinRunes is the shortest probeable run: FTS5 trigram answers
// only ≥3-rune tokens (the CJK widening covers shorter queries but
// cannot rank them), so a body without a ≥3-rune alphanumeric run is
// unprobeable.
const migProbeMinRunes = 3

// migProbeMaxRunes caps a probe's length. v1 aihub retrieval is LEXICAL
// (FTS5 trigram + substring widening) — there is no embedding provider
// in the loop, so the "semantic recall" the old leading-3-rune probes
// assumed never happens: 'Err'/'Tes' collide across the whole corpus,
// and the rank-tied hit list misses the target (the measured 16 spurious
// failures at 5915 entries). The fix is the exact content-match shape:
// a LONG distinctive substring of the entry's own body (up to
// migProbeMaxRunes of its longest alphanumeric run), preferring runs no
// other entry in the batch shares (see migAssignProbes), so the probe
// pinpoints the entry even in a 6000-entry corpus.
const migProbeMaxRunes = 48

// migProbeCandidates is how many runs per entry are kept as probe
// candidates (longest first) — spares for when the longest run is not
// batch-unique.
const migProbeCandidates = 3

// migSmokeCandidates picks an entry's probe candidates: the longest
// alphanumeric runs of the body, longest first, each capped at
// migProbeMaxRunes and required to be at least migProbeMinRunes long.
// Alphanumeric runs survive the structural strip verbatim, so every
// candidate is guaranteed to appear in the FTS-indexed body.
func migSmokeCandidates(content string) []string {
	var runs []string
	cur := ""
	flush := func() {
		if cur != "" {
			runes := []rune(cur)
			if len(runes) >= migProbeMinRunes {
				if len(runes) > migProbeMaxRunes {
					runes = runes[:migProbeMaxRunes]
				}
				runs = append(runs, string(runes))
			}
		}
		cur = ""
	}
	for _, rn := range content {
		if unicode.IsLetter(rn) || unicode.IsDigit(rn) {
			cur += string(rn)
		} else {
			flush()
		}
	}
	flush()
	// Deduplicate and order longest-first; keep the top few.
	seen := map[string]bool{}
	uniq := runs[:0]
	for _, r := range runs {
		if !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	runs = uniq
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && len(runs[j]) > len(runs[j-1]); j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
	if len(runs) > migProbeCandidates {
		runs = runs[:migProbeCandidates]
	}
	return runs
}

// migAssignProbes picks each entry's retrieval probe from its
// candidates: the longest candidate run no OTHER entry in the batch
// carries (batch-unique — the search answer then pinpoints the entry
// regardless of corpus size); fallback to the longest candidate.
// Entries without any ≥3-rune run stay unprobeable ("") and are counted
// as skipped by the retrieval step.
func migAssignProbes(placed []placedMem) {
	counts := map[string]int{}
	for i := range placed {
		for _, r := range placed[i].runs {
			counts[r]++
		}
	}
	for i := range placed {
		placed[i].probe = ""
		for _, r := range placed[i].runs {
			if counts[r] == 1 {
				placed[i].probe = r
				break
			}
		}
		if placed[i].probe == "" && len(placed[i].runs) > 0 {
			placed[i].probe = placed[i].runs[0]
		}
	}
}
