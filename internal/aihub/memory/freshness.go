package memory

// freshness.go: the freshness term of search ordering. Text relevance
// alone ranks a year-old note level with yesterday's; recall should
// prefer the fresher of two comparable matches without letting age bury
// a clearly better one.
//
// Per entry, with age_days = now - memories.updated_at (the authority
// document's last revision: the create time unless the file was revised
// since — memories have no update endpoint, so for API-written entries
// it equals created_at):
//
//	w     = exp(-age_days / stability_days)       ∈ (0, 1]; 1 for a non-decaying type
//	score = rank × (1 + freshnessBoost × w)       (rank: FTS5 bm25, negative = better)
//
// ordered ascending (most negative first). The factor lies in
// [1, 1+freshnessBoost], so freshness can scale a match's relevance by
// at most 1.5×: a fresh entry wins a near-tie, and an old match more than
// 1.5× as relevant as a fresh one always stays ahead of it. The
// multiplicative form is independent of bm25's absolute scale, which
// varies with corpus size and query length. Exact ties fall back to
// updated_at DESC, then id. The substring-scan path (short tokens only,
// no rank) orders by w alone.

import (
	"strconv"
	"strings"
)

// freshnessBoost is the largest relative lift freshness gives a match.
const freshnessBoost = 0.5

// typeStability maps a type prefix to its stability in days (0 = never
// decays). experience.* is tied to circumstances that change; fact.* and
// methodology.* hold longer; rule.* and doc.* are standing references
// and do not decay. Any other type uses defaultStabilityDays.
var typeStability = []struct {
	prefix string
	days   float64
}{
	{"experience.", 90},
	{"fact.", 180},
	{"methodology.", 180},
	{"rule.", 0},
	{"doc.", 0},
}

const defaultStabilityDays = 90

// freshnessSQL is the SQL expression of w for the memories row aliased m;
// nowParam is the placeholder (e.g. "?5") bound to the query time as an
// RFC3339 UTC string. An unparseable updated_at counts as fully stale
// (w = 0).
func freshnessSQL(nowParam string) string {
	var sb strings.Builder
	age := "max(0, julianday(" + nowParam + ") - julianday(m.updated_at))"
	sb.WriteString("COALESCE(CASE")
	for _, ts := range typeStability {
		sb.WriteString(" WHEN m.type GLOB '" + ts.prefix + "*' THEN ")
		if ts.days == 0 {
			sb.WriteString("1.0")
		} else {
			sb.WriteString("exp(-" + age + " / " + ftoa(ts.days) + ")")
		}
	}
	sb.WriteString(" ELSE exp(-" + age + " / " + ftoa(defaultStabilityDays) + ") END, 0.0)")
	return sb.String()
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
