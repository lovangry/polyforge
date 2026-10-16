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
	// maxAnyTermRunes drops hashes, URLs-without-separators, base64 and
	// unsegmented CJK runs: they would win the longest-first cap while
	// almost never recurring verbatim in a memory.
	maxAnyTermRunes = 40
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

// anyTerms answers the any-term query's terms: the lowercased,
// deduplicated letter/digit/underscore runs of q, minus too-short,
// too-long and stopword tokens, capped at maxAnyTerms by preferring the
// longest terms (ties keep first appearance) — longer terms are the
// more specific ones, and the cap keeps the FTS expression bounded
// whatever the query length. Every term is made of letters, digits and
// '_' only, so no FTS5 operator character can survive into a term.
func anyTerms(q string) []string {
	toks := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	seen := map[string]bool{}
	var terms []string
	for _, t := range toks {
		t = strings.Trim(t, "_")
		n := utf8.RuneCountInString(t)
		if n < minAnyTermRunes || n > maxAnyTermRunes || anyStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		terms = append(terms, t)
	}
	if len(terms) > maxAnyTerms {
		sort.SliceStable(terms, func(i, j int) bool {
			return utf8.RuneCountInString(terms[i]) > utf8.RuneCountInString(terms[j])
		})
		terms = terms[:maxAnyTerms]
	}
	return terms
}

// SearchAny is the ranked any-term search: a memory matches when it
// contains at least one of q's terms (anyTerms), and hits come back in
// FTS5 bm25 rank order. The type/tag/allowed filters and the limit are
// exactly Search's (the same candidate paging). A blank q is the same
// MEM-400 Search answers; a q with no usable term after filtering (only
// stopwords / short tokens) answers no hits, not an error.
func (s *Store) SearchAny(q string, memType string, tags []string, limit int, allowed func(*Entry) bool) ([]Entry, error) {
	if strings.TrimSpace(q) == "" {
		return nil, errf("MEM-400", "", "q required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	terms := anyTerms(q)
	if len(terms) == 0 {
		return []Entry{}, nil
	}
	return s.collect(ftsJoin(terms, " OR "), nil, memType, tags, limit, allowed)
}
