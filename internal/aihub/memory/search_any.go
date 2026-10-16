package memory

// search_any.go: the ranked any-term search mode (match=any). The
// default search (Search) ANDs every query token, which is the right
// shape for a short human query but answers nothing for a long one: the
// daemon's claim-time recall sends a work item's goal + content, and no
// memory carries every word of a task description. Any-term mode ORs
// the query's distinct informative terms and lets FTS5's bm25 rank put
// the memories sharing the most (and rarest) terms first.

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MatchAll is the default search mode: every token must match.
	MatchAll = "all"
	// MatchAny is the ranked any-term mode (SearchAny).
	MatchAny = "any"

	// maxAnyTerms caps the OR'd terms of one any-term query.
	maxAnyTerms = 32
	// minAnyTermRunes drops tokens the trigram index cannot answer.
	minAnyTermRunes = 3
	// maxAnyTermRunes drops non-CJK words longer than this (hashes,
	// base64, URLs without separators): they would win the longest-first
	// cap while almost never recurring verbatim in a memory. CJK runs are
	// shingled instead (anyTerms), so the bound never applies to them.
	maxAnyTermRunes = 40
	// cjkShingleRunes is the CJK shingle width: exactly the trigram
	// index's own unit, the shortest piece FTS5 trigram can MATCH.
	cjkShingleRunes = 3
	// anyCandidateFloor / anyCandidatesPerHit bound the FTS candidates
	// one any-term search examines: max(floor, perHit×limit), in bm25
	// order, with no further paging. An OR of up to 32 terms (3-rune
	// terms also match inside longer words) can match a large share of
	// the table, and every candidate costs an entry load, an authority
	// file check and the caller's membership check — the best-ranked
	// window is what a ranked recall wants anyway. (The AND mode keeps
	// paging to the end: its candidate set is the exact match set.)
	anyCandidateFloor   = 100
	anyCandidatesPerHit = 10
)

// anyStopwords are terms too common to carry signal in a task
// description: high-frequency English function words plus markdown /
// URL noise. Tokens shorter than minAnyTermRunes are already dropped, so
// only ≥3-rune words need listing.
var anyStopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		the and for are but not you all any can had her was one our out has
		have him his how its may new now old see two way who did get got let
		say she too use via yes yet off own per than that this these those
		with from into onto over under then them they their there here what
		when where which while whom whose why will would shall should could
		must might been being were does doing done each every some such only
		also just very more most much many other another same both either
		neither about above below after before again against between during
		through until upon within without because since though although
		unless whether like make made need needs needed want wants able
		etc
		http https www com org net html htm png jpg svg json yaml yml
		todo fixme nbsp quot amp
	`) {
		m[w] = true
	}
	return m
}()

// isCJK reports whether r belongs to a script written without spaces
// between words (Han, Hiragana, Katakana, Hangul): such a run is a whole
// clause, not a word, so anyTerms shingles it instead of keeping it as
// one term.
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// isWordRune: letters, digits, '_' and combining marks (Devanagari and
// other Indic vowel signs / viramas are marks — splitting on them would
// cut a word into sub-3-rune pieces).
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '_'
}

// anyTerms answers the any-term query's terms. The lowercased query is
// cut into runs of word runes (isWordRune), and a run is further split
// where it switches between CJK and non-CJK script (a mark stays with
// the run it follows), so "修复retry阶梯" is three runs.
//
//   - A non-CJK run is one word: kept when it is 3..maxAnyTermRunes runes
//     (after trimming edge '_') and not a stopword.
//   - A CJK run is a clause with no word boundaries: it becomes its
//     overlapping 3-rune shingles ("重试阶梯" → 重试阶, 试阶梯) — the exact
//     pieces the trigram index can match, so any memory sharing a 3-rune
//     stretch of the clause is a candidate and bm25 favours the ones
//     sharing more of them. A CJK run under 3 runes yields nothing (the
//     trigram index cannot answer it).
//
// Terms are deduplicated, then capped at maxAnyTerms by interleaving two
// pools so neither script can starve the other: words (longest first —
// the more specific ones; ties keep first appearance) and shingles
// (round-robin across the CJK runs, so one long clause cannot crowd out
// a later one). Every term is made of word runes only, so no FTS5
// operator character can survive into a term.
func anyTerms(q string) []string {
	type run struct {
		rs  []rune
		cjk bool
	}
	var runs []run
	var cur []rune
	curCJK := false
	flush := func() {
		if len(cur) > 0 {
			runs = append(runs, run{cur, curCJK})
		}
		cur = nil
	}
	for _, r := range strings.ToLower(q) {
		if !isWordRune(r) {
			flush()
			continue
		}
		if unicode.IsMark(r) && len(cur) > 0 {
			cur = append(cur, r)
			continue
		}
		c := isCJK(r)
		if len(cur) > 0 && c != curCJK {
			flush()
		}
		curCJK = c
		cur = append(cur, r)
	}
	flush()

	seen := map[string]bool{}
	var words []string
	var shingleRuns [][]string
	for _, rn := range runs {
		if rn.cjk {
			var sh []string
			for i := 0; i+cjkShingleRunes <= len(rn.rs); i++ {
				t := string(rn.rs[i : i+cjkShingleRunes])
				if !seen[t] {
					seen[t] = true
					sh = append(sh, t)
				}
			}
			if len(sh) > 0 {
				shingleRuns = append(shingleRuns, sh)
			}
			continue
		}
		t := strings.Trim(string(rn.rs), "_")
		n := utf8.RuneCountInString(t)
		if n < minAnyTermRunes || n > maxAnyTermRunes || anyStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		words = append(words, t)
	}
	sort.SliceStable(words, func(i, j int) bool {
		return utf8.RuneCountInString(words[i]) > utf8.RuneCountInString(words[j])
	})
	var shingles []string
	for i := 0; ; i++ {
		added := false
		for _, sh := range shingleRuns {
			if i < len(sh) {
				shingles = append(shingles, sh[i])
				added = true
			}
		}
		if !added {
			break
		}
	}
	// Interleave the pools up to the cap; when one runs dry the other
	// fills the remaining slots.
	var terms []string
	for wi, si := 0, 0; len(terms) < maxAnyTerms && (wi < len(words) || si < len(shingles)); {
		if wi < len(words) {
			terms = append(terms, words[wi])
			wi++
		}
		if len(terms) < maxAnyTerms && si < len(shingles) {
			terms = append(terms, shingles[si])
			si++
		}
	}
	return terms
}

// SearchAny is the ranked any-term search: a memory matches when it
// contains at least one of q's terms (anyTerms), and hits come back in
// FTS5 rank order. The rank is bm25 over the trigram index's 3-rune
// pieces, not over words: a short term also matches inside longer words
// ("fix" in "prefix"), which adds some noise the ranking only partly
// absorbs. At most max(anyCandidateFloor, anyCandidatesPerHit×limit)
// best-ranked candidates are examined; the type/tag/allowed filters and
// the limit are Search's. A blank q is the same MEM-400 Search answers;
// a q with no usable term after filtering (only stopwords / short
// tokens) answers no hits, not an error.
func (s *Store) SearchAny(q string, memType string, tags []string, limit int, allowed func(*Entry) bool) ([]Entry, error) {
	return s.SearchMode(MatchAny, "", q, memType, tags, limit, allowed)
}

// searchAny runs the any-term mode for SearchMode (q and the limit are
// already validated).
func (s *Store) searchAny(q string, f searchFilter) ([]Entry, error) {
	terms := anyTerms(q)
	if len(terms) == 0 {
		return []Entry{}, nil
	}
	maxCands := anyCandidatesPerHit * f.limit
	if maxCands < anyCandidateFloor {
		maxCands = anyCandidateFloor
	}
	return s.collect(ftsJoin(terms, " OR "), nil, maxCands, f)
}
