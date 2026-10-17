package runtime

// recall.go: the F6-A lane — the claim-time memory snapshot. The goal's
// v0-era "recall the lessons ledger first" instruction was in-contract
// unexecutable in v1 (the agent holds no aihub credential and no read
// path existed at all); the F6 ruling answers it in two halves, and this
// file is the STATIC half:
//
//   - at claim time the daemon recalls the project's memories — a short
//     keyword query built from the goal and labels (recallKeywords), the
//     aihub memory search face's ranked any-term mode (match=any) as the
//     index — and writes the
//     top-K hits as a static snapshot into the capsule:
//     runtime-state/recalled-memories.json, which the driver renders
//     into every step prompt as non-binding history
//     (driver/spawn.go recalledMemoriesSection, decision 2026-10-16 G3);
//   - the live half is the daemon's step memory face (daemon/stepmem.go:
//     `pf recall` / `pf remember` inside a step, with the daemon's key).
//
// The snapshot is an ENRICHMENT, never a gate: an unreachable aihub, a
// refused search or a zero-hit query all degrade to an empty list plus a
// notice field — nothing here may fail a spawn.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/silgrid/polyforge/internal/common/atomicfile"
	"github.com/silgrid/polyforge/internal/common/capsule"
)

const (
	// relRecalledMemories is the snapshot's capsule-relative path.
	relRecalledMemories = capsule.RecalledMemoriesJSON
	// defaultRecallTopK is the snapshot's default hit count (F6 ruling:
	// K=10, configurable through runtime.Config.RecallTopK).
	defaultRecallTopK = 10
	// maxRecallTopK bounds a configured K.
	maxRecallTopK = 50
)

// RecalledMemory is one snapshot hit: the memory's identity plus a
// bounded content excerpt (the full content stays behind the aihub
// read face; the UDS recall channel is the live fetch path).
type RecalledMemory struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// recalledSnapshot is the recalled-memories.json shape. Items is never
// null (an empty recall is an empty list + Notice, a shape the consumer
// can distinguish from a torn file).
type recalledSnapshot struct {
	WorkItemID string           `json:"work_item_id"`
	Query      string           `json:"query"`
	TopK       int              `json:"top_k"`
	Items      []RecalledMemory `json:"items"`
	// Notice carries the degradation reason on an empty recall: a
	// search failure, a zero-hit query, a missing task definition.
	// Empty on a populated recall.
	Notice    string `json:"notice,omitempty"`
	CreatedAt string `json:"created_at"`
}

// recallMaxTerms caps the claim query's keywords.
const recallMaxTerms = 12

// recallStopwords are English words too common in a task goal to name
// its subject (function words and task verbs); words under 3 runes are
// dropped before this list is consulted.
var recallStopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		the and for are but not all any can has have its may new now via
		with from into onto over under then them they their there here
		what when where which while who why how will would should could
		must been being were does done each every some such only also
		just very more most much many other same both about above below
		after before again between during through until upon within
		without because since this that these those than make made need
		needs want able get got use used using let etc
		fix fixes fixed add adds added implement implements update updates
		remove removes support supports change changes ensure handle
		allow allows keep instead still
	`) {
		m[w] = true
	}
	return m
}()

// recallParticles are Chinese function characters: a 3-rune shingle
// holding two of them carries at most one content character.
const recallParticles = "的了是在和与及或为把被从到也都就而这那之其个"

// recallKeywords builds the claim query without a model: the labels'
// and the goal's keywords — identifiers (a digit, an underscore or an
// inner capital: SessionClaim, claim_journal, s24b) first, then the
// other words of 3+ runes that are not stopwords, and for a CJK clause
// its 3-rune shingles at a stride of 2 (the trigram unit aihub's search
// matches) — deduplicated, capped at recallMaxTerms and joined by
// spaces for aihub's any-term search (match=any).
func recallKeywords(goal string, labels []string) string {
	var ids, rest []string
	for _, src := range append(append([]string(nil), labels...), goal) {
		i, r := keywordTerms(src)
		ids, rest = append(ids, i...), append(rest, r...)
	}
	seen := map[string]bool{}
	var terms []string
	for _, t := range append(ids, rest...) {
		if len(terms) == recallMaxTerms {
			break
		}
		if k := strings.ToLower(t); !seen[k] {
			seen[k] = true
			terms = append(terms, t)
		}
	}
	return strings.Join(terms, " ")
}

// keywordTerms cuts s into runs of word runes (letters, digits, '_'),
// splitting where the script switches between CJK and the rest, and
// answers the identifiers and the remaining keywords (see recallKeywords).
func keywordTerms(s string) (ids, rest []string) {
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	cjk := func(r rune) bool {
		return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
	}
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if !word(rs[i]) {
			i++
			continue
		}
		j, c := i, cjk(rs[i])
		for j < len(rs) && word(rs[j]) && cjk(rs[j]) == c {
			j++
		}
		run := rs[i:j]
		i = j
		if c {
			rest = append(rest, cjkShingles(run)...)
			continue
		}
		t := strings.Trim(string(run), "_")
		n := utf8.RuneCountInString(t)
		switch {
		case n < 3 || n > 40:
		case isIdentifier(t):
			ids = append(ids, t)
		case !recallStopwords[strings.ToLower(t)]:
			rest = append(rest, t)
		}
	}
	return ids, rest
}

// isIdentifier reports a code-like word: a digit, an underscore or an
// inner capital, with at least one letter and 3+ runes.
func isIdentifier(t string) bool {
	if utf8.RuneCountInString(t) < 3 {
		return false
	}
	letter, mark := false, false
	for i, r := range t {
		letter = letter || unicode.IsLetter(r)
		mark = mark || unicode.IsDigit(r) || r == '_' || (i > 0 && unicode.IsUpper(r))
	}
	return letter && mark
}

// cjkShingles answers a CJK run's 3-rune shingles at a stride of 2 (the
// last one ends the run), skipping a shingle with two particles; a run
// under 3 runes yields nothing.
func cjkShingles(run []rune) []string {
	var starts []int
	for i := 0; i+3 <= len(run); i += 2 {
		starts = append(starts, i)
	}
	if k := len(starts); k > 0 && starts[k-1]+3 < len(run) {
		starts = append(starts, len(run)-3)
	}
	var out []string
	for _, i := range starts {
		p := 0
		for _, r := range run[i : i+3] {
			if strings.ContainsRune(recallParticles, r) {
				p++
			}
		}
		if p < 2 {
			out = append(out, string(run[i:i+3]))
		}
	}
	return out
}

// recallTopK answers the effective K for the snapshot (configured,
// defaulted, bounded).
func (r *Runtime) recallTopK() int {
	k := r.cfg.RecallTopK
	if k <= 0 {
		k = defaultRecallTopK
	}
	if k > maxRecallTopK {
		k = maxRecallTopK
	}
	return k
}

// writeRecalledMemories materializes the F6-A snapshot. Best-effort on
// every path: a missing seam (nil — slim installs, most unit tests)
// writes nothing at all; every other failure mode lands as an empty
// list plus a notice, never a failed start.
func (r *Runtime) writeRecalledMemories(j *job) {
	if r.cfg.MemoryRecall == nil {
		return
	}
	snap := recalledSnapshot{
		WorkItemID: j.wi,
		TopK:       r.recallTopK(),
		Items:      []RecalledMemory{},
		CreatedAt:  nowStamp(),
	}
	// The query is the keywords of the claim's own task definition (goal
	// and labels; recallKeywords). j.item is the claim answer's
	// snapshot; a missing one (never in the start path) falls back to an
	// aihub lookup, and even THAT failing degrades to a noticed empty
	// recall.
	item := j.item
	if item == nil {
		if fetched, err := r.hub.GetWorkItem(j.wi); err == nil {
			item = fetched
		}
	}
	if item != nil {
		snap.Query = recallKeywords(item.Goal, item.Labels)
	}
	switch {
	case strings.TrimSpace(snap.Query) == "":
		snap.Notice = "no task definition to recall against"
	default:
		// The search is scoped to the work item's own project (audit B5): the
		// snapshot must not draw on every project the daemon credential
		// can read. A populated query implies item != nil (the query is
		// built from it); the nil check keeps the seam honest anyway.
		project := ""
		if item != nil {
			project = item.ProjectID
		}
		hits, err := r.cfg.MemoryRecall(snap.Query, project, snap.TopK)
		if err != nil {
			// aihub unreachable or the search refused: an EMPTY list plus
			// the notice — never a failed spawn.
			snap.Notice = "memory search failed: " + err.Error()
		} else {
			for _, h := range hits {
				snap.Items = append(snap.Items, RecalledMemory{
					ID:      h.ID,
					Type:    h.Type,
					Title:   h.Title,
					Excerpt: h.Excerpt,
				})
			}
			if len(snap.Items) == 0 {
				snap.Notice = "no memories matched the claim query"
			}
		}
	}
	blob, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: recalled-memories snapshot marshal failed: %v\n", j.wi, err)
		return
	}
	if err := atomicfile.Write(filepath.Join(j.capsuleDir, filepath.FromSlash(relRecalledMemories)), blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: recalled-memories snapshot write failed: %v\n", j.wi, err)
	}
}
