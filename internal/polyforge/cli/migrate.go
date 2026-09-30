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
type ResolveError struct{ msg string }

func (e *ResolveError) Error() string { return e.msg }

func resolveErrf(format string, args ...any) error {
	return &ResolveError{msg: fmt.Sprintf(format, args...)}
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

// migSmokeSubstring picks the retrieval probe: the leading 3 runes of the
// longest alphanumeric word of the body (or the whole word when shorter).
// Alphanumeric runs survive the structural strip verbatim, so the probe is
// guaranteed to appear in the FTS-indexed body.
func migSmokeSubstring(content string) (string, error) {
	best, cur := "", ""
	flush := func() {
		if len(cur) > len(best) {
			best = cur
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
	if best == "" {
		return "", fmt.Errorf("no alphanumeric run in body to probe retrieval with")
	}
	runes := []rune(best)
	if len(runes) > 3 {
		runes = runes[:3]
	}
	return string(runes), nil
}
