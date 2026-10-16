package memory

// search_any_test.go: the ranked any-term mode (match=any) — a long task
// description finds the memories that share its terms (where the
// default AND mode finds none), bm25 ranks the stronger overlap first,
// the type/tag/allowed filters and the limit still hold, FTS5 operator
// characters stay inert, and a query with no usable term answers no
// hits without an error.

import (
	"fmt"
	"strings"
	"testing"
)

// claimQuery is a realistic claim-time query: a work item's goal plus
// the opening of its markdown content.
const claimQuery = `Fix the flaky provider retry ladder in the daemon

## Context
The step agent hits a 429 from the provider and the retry ladder gives up
after two attempts; see https://example.com/issues/171 for the trace.
- [ ] make the backoff exponential
- [ ] log every attempt`

func TestSearchAnyLongQueryFindsRelevantMemory(t *testing.T) {
	e := newEnv(t)
	strong := e.create(t, "Lesson: the provider retry ladder must back off exponentially; a flat ladder gives up on 429.")
	weak := e.create(t, "Unrelated note that merely mentions a daemon restart.")
	e.create(t, "Totally different subject: CSS grid layout quirks.")

	// The default AND mode answers nothing for a whole task description.
	if hits, err := e.store.Search(claimQuery, "", nil, 10, nil); err != nil || len(hits) != 0 {
		t.Fatalf("AND mode on the long query: %v, %d hits (want 0 — the bug's shape)", err, len(hits))
	}
	hits, err := e.store.SearchAny(claimQuery, "", nil, 10, nil)
	if err != nil {
		t.Fatalf("SearchAny: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchAny hits = %d, want 2 (the retry lesson and the daemon note): %+v", len(hits), hits)
	}
	if hits[0].ID != strong.ID || hits[1].ID != weak.ID {
		t.Fatalf("rank order = [%s %s], want the stronger overlap (%s) first", hits[0].ID, hits[1].ID, strong.ID)
	}
}

func TestSearchAnyFiltersAndLimit(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, e.create(t, fmt.Sprintf("retry ladder lesson number %d", i)).ID)
	}
	other, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "fact.note", Visibility: "project",
		Content: "retry ladder fact", Tags: []string{"ops"},
	}, e.user)
	if err != nil {
		t.Fatalf("create fact: %v", err)
	}

	// Limit honoured.
	if hits, err := e.store.SearchAny("retry ladder", "", nil, 2, nil); err != nil || len(hits) != 2 {
		t.Fatalf("limit 2: %v, %d hits", err, len(hits))
	}
	// Type filter.
	hits, err := e.store.SearchAny("retry ladder", "fact.*", nil, 10, nil)
	if err != nil || len(hits) != 1 || hits[0].ID != other.ID {
		t.Fatalf("type filter: %v %+v", err, hits)
	}
	// Tag filter.
	hits, err = e.store.SearchAny("retry ladder", "", []string{"ops"}, 10, nil)
	if err != nil || len(hits) != 1 || hits[0].ID != other.ID {
		t.Fatalf("tag filter: %v %+v", err, hits)
	}
	// The allowed predicate (membership / visibility / project — the
	// server layer's filters) is still applied to every candidate, and
	// paging fills the limit past refused candidates.
	allowed := func(en *Entry) bool { return en.ID == ids[4] }
	hits, err = e.store.SearchAny("retry ladder", "", nil, 1, allowed)
	if err != nil || len(hits) != 1 || hits[0].ID != ids[4] {
		t.Fatalf("allowed filter: %v %+v", err, hits)
	}
	if hits, err = e.store.SearchAny("retry ladder", "", nil, 10, func(*Entry) bool { return false }); err != nil || len(hits) != 0 {
		t.Fatalf("refusing allowed must answer none: %v %+v", err, hits)
	}
}

func TestSearchAnyOperatorCharactersInert(t *testing.T) {
	e := newEnv(t)
	target := e.create(t, "The trigram index finds xyzzyplugh.")
	e.create(t, "Another memory about something else entirely.")
	e.create(t, "A third one, unrelated as well.")

	for _, q := range []string{
		`"xyzzyplugh" OR (NOT) -- body:* ^near NEAR(a b)`,
		`xyzzyplugh*`,
		`{xyzzyplugh} AND`,
	} {
		hits, err := e.store.SearchAny(q, "", nil, 10, nil)
		if err != nil {
			t.Fatalf("SearchAny(%q): %v", q, err)
		}
		if len(hits) != 1 || hits[0].ID != target.ID {
			t.Fatalf("SearchAny(%q) = %d hits, want only the xyzzyplugh memory", q, len(hits))
		}
	}
	// Pure operator noise yields no term at all: no hits, no error, and
	// certainly not every memory.
	for _, q := range []string{`*** "(" ) : ^ -`, `"(*)"`} {
		hits, err := e.store.SearchAny(q, "", nil, 10, nil)
		if err != nil || len(hits) != 0 {
			t.Fatalf("SearchAny(%q): %v, %d hits (want none)", q, err, len(hits))
		}
	}
}

func TestSearchAnyNoUsableTerm(t *testing.T) {
	e := newEnv(t)
	e.create(t, "the and of to a with from this that")
	hits, err := e.store.SearchAny("the and of to a with from this that", "", nil, 10, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("stopword-only query: %v, %d hits (want none, no error)", err, len(hits))
	}
	if _, err := e.store.SearchAny("   ", "", nil, 10, nil); err == nil || err.(*APIError).Code != "MEM-400" {
		t.Fatalf("blank query must be MEM-400 like Search: %v", err)
	}
}

func TestAnyTerms(t *testing.T) {
	got := anyTerms("Fix the Retry-Ladder: retry ladder, https://x.io/a_b__c 42 ok 中文 trigram_index")
	want := []string{"fix", "retry", "ladder", "a_b__c", "trigram_index"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("anyTerms = %v, want %v", got, want)
	}
	// The cap keeps the longest maxAnyTerms terms.
	var words []string
	for i := 0; i < 50; i++ {
		words = append(words, fmt.Sprintf("w%03d", i)) // 4 runes
	}
	words = append(words, "longestterm")
	got = anyTerms(strings.Join(words, " "))
	if len(got) != maxAnyTerms || got[0] != "longestterm" {
		t.Fatalf("cap: %d terms, first %q", len(got), got[0])
	}
}
