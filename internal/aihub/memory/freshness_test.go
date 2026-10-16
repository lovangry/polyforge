package memory

// freshness_test.go: freshness-weighted search ordering under a pinned
// clock — the fresher of two equal matches first (both match modes and
// the short-token scan), rule.* never decaying, and a much better old
// match staying ahead of a weak fresh one.

import (
	"strings"
	"testing"
	"time"
)

var freshNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// pinClock fixes the search clock.
func pinClock(e *env) { e.store.now = func() time.Time { return freshNow } }

// age sets an entry's updated_at to days before freshNow.
func age(t *testing.T, e *env, id string, days int) {
	t.Helper()
	ts := freshNow.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := e.db.Exec(`UPDATE memories SET updated_at=?1 WHERE id=?2`, ts, id); err != nil {
		t.Fatal(err)
	}
}

func createTyped(t *testing.T, e *env, typ, content string) *Entry {
	t.Helper()
	got, err := e.store.Create(CreateRequest{Project: "alpha", Type: typ, Visibility: "project", Content: content}, e.user)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ids(hits []Entry) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestSearchFresherWinsEqualMatch(t *testing.T) {
	e := newEnv(t)
	pinClock(e)
	const body = "quokka retry ladder 重试 lesson"
	// The stale one is created last so insertion order cannot explain
	// the answer.
	fresh := e.create(t, body)
	stale := e.create(t, body)
	age(t, e, fresh.ID, 2)
	age(t, e, stale.ID, 400)
	for _, m := range []struct{ mode, q string }{{MatchAll, "quokka"}, {MatchAny, "quokka ladder"}, {MatchAll, "重试"}} {
		hits, err := e.store.SearchMode(m.mode, "", m.q, "", nil, 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].ID != fresh.ID {
			t.Fatalf("%s %q: order %v, want fresh %s first", m.mode, m.q, ids(hits), fresh.ID)
		}
	}
}

func TestSearchRuleNeverDecays(t *testing.T) {
	e := newEnv(t)
	pinClock(e)
	const body = "wombat deploy freeze rule"
	exp := createTyped(t, e, "experience.deploy", body)
	rule := createTyped(t, e, "rule.deploy", body)
	doc := createTyped(t, e, "doc.design", body)
	age(t, e, exp.ID, 30)
	age(t, e, rule.ID, 3*365)
	age(t, e, doc.ID, 3*365+1)
	hits, err := e.store.Search("wombat", "", nil, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(hits)
	if len(got) != 3 || got[0] != rule.ID || got[1] != doc.ID || got[2] != exp.ID {
		t.Fatalf("order %v, want [rule %s, doc %s, experience %s]: rule.*/doc.* never decay", got, rule.ID, doc.ID, exp.ID)
	}
}

func TestSearchMuchBetterOldMatchBeatsWeakFresh(t *testing.T) {
	e := newEnv(t)
	pinClock(e)
	strong := e.create(t, "numbat numbat numbat numbat numbat")
	weak := e.create(t, "numbat "+strings.Repeat("unrelated filler words about other topics ", 30))
	age(t, e, strong.ID, 5*365)
	age(t, e, weak.ID, 0)
	// Precondition: the old match is far more than freshnessBoost better.
	var rs, rw float64
	q := `SELECT rank FROM memory_fts WHERE memory_fts MATCH 'body:"numbat"' AND mem_id=?1`
	if err := e.db.QueryRow(q, strong.ID).Scan(&rs); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(q, weak.ID).Scan(&rw); err != nil {
		t.Fatal(err)
	}
	if rs/rw <= 1+freshnessBoost {
		t.Fatalf("fixture too weak: bm25 strong %f vs weak %f", rs, rw)
	}
	for _, mode := range []string{MatchAll, MatchAny} {
		hits, err := e.store.SearchMode(mode, "", "numbat", "", nil, 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].ID != strong.ID {
			t.Fatalf("%s: order %v, want the much better old match %s first", mode, ids(hits), strong.ID)
		}
	}
}

func TestSearchFresherWinsNearTie(t *testing.T) {
	e := newEnv(t)
	pinClock(e)
	// Filler keeps the term rare: bm25's idf clamps to ~0 for a term in
	// most of the corpus, which would make every match a tie.
	for i := 0; i < 6; i++ {
		e.create(t, "unrelated filler about other topics")
	}
	stale := e.create(t, "quokka ladder note")
	fresh := e.create(t, "quokka ladder note, slightly longer")
	age(t, e, stale.ID, 365)
	age(t, e, fresh.ID, 1)
	// Precondition: on text alone the stale entry ranks (slightly) first.
	var rs, rf float64
	q := `SELECT rank FROM memory_fts WHERE memory_fts MATCH 'body:"quokka"' AND mem_id=?1`
	if err := e.db.QueryRow(q, stale.ID).Scan(&rs); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(q, fresh.ID).Scan(&rf); err != nil {
		t.Fatal(err)
	}
	if !(rs < rf && rs/rf < 1.4) {
		t.Fatalf("fixture is not a near-tie favouring the stale entry: bm25 stale %f fresh %f", rs, rf)
	}
	for _, mode := range []string{MatchAll, MatchAny} {
		hits, err := e.store.SearchMode(mode, "", "quokka", "", nil, 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].ID != fresh.ID {
			t.Fatalf("%s: order %v, want fresh %s first on a near-tie", mode, ids(hits), fresh.ID)
		}
	}
}
