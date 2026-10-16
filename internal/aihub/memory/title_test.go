package memory

// title_test.go: the derived row title — the derivation itself, create
// filling a blank title while keeping an explicit one, and the startup
// backfill of rows written without one (idempotent, kept by the
// reconcile afterwards).

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveTitle(t *testing.T) {
	long := strings.Repeat("abcdefghij", 10) // 100 runes
	cjkLong := strings.Repeat("记忆标题派生", 20)  // 120 runes
	cases := []struct {
		name, body string
		want       *string
	}{
		{"heading", "\n\n# Retry ladder lesson\nbody", sp("Retry ladder lesson")},
		{"deep heading", "### Deep heading ###", sp("Deep heading ###")},
		{"list", "- first item\n- second", sp("first item")},
		{"star list", "*   starred", sp("starred")},
		{"quote list", "> - quoted item", sp("quoted item")},
		{"numbered", "12. numbered step", sp("numbered step")},
		{"numbered paren", "3) paren step", sp("paren step")},
		{"task", "- [ ] make the backoff exponential", sp("make the backoff exponential")},
		{"hashtag kept", "#hashtag is text", sp("#hashtag is text")},
		{"emphasis kept", "**Bold** start", sp("**Bold** start")},
		{"fence skipped", "```go\nfunc main() {}\n```", sp("func main() {}")},
		{"rule skipped", "---\n***\nafter rules", sp("after rules")},
		{"empty markers skipped", "#\n- \n>\nreal line", sp("real line")},
		{"cjk", "## 重试阶梯必须指数退避", sp("重试阶梯必须指数退避")},
		{"long", long, sp(long[:79] + "…")},
		{"cjk long", cjkLong, sp(string([]rune(cjkLong)[:79]) + "…")},
		{"exactly max", long[:80], sp(long[:80])},
		{"empty", "", nil},
		{"blank", " \n\t\n", nil},
		{"markers only", "#\n- \n---", nil},
		{"bare numbered marker", "1.\n2)\n- [ ]\nthe real step", sp("the real step")},
		{"decimal kept", "3.14 is pi", sp("3.14 is pi")},
	}
	for _, c := range cases {
		got := deriveTitle(c.body)
		if !strPtrEq(got, c.want) {
			t.Errorf("%s: deriveTitle = %v, want %v", c.name, deref(got), deref(c.want))
		}
		if got != nil && len([]rune(*got)) > titleMaxRunes {
			t.Errorf("%s: %d runes > %d", c.name, len([]rune(*got)), titleMaxRunes)
		}
	}
}

func TestCreateDerivesBlankTitle(t *testing.T) {
	e := newEnv(t)
	for _, title := range []*string{nil, sp(""), sp("  \t")} {
		got, err := e.store.Create(CreateRequest{
			Project: "alpha", Type: "experience.debug", Visibility: "project",
			Title: title, Content: "# Provider retry ladder\nback off exponentially",
		}, e.user)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title == nil || *got.Title != "Provider retry ladder" {
			t.Fatalf("title %q: row title = %v, want derived", deref(title), deref(got.Title))
		}
	}
	got, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "experience.debug", Visibility: "project",
		Title: sp(" Explicit "), Content: "# Heading",
	}, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title == nil || *got.Title != " Explicit " {
		t.Fatalf("explicit title = %v, want kept verbatim", deref(got.Title))
	}
	// The derived title is a projection of the file: a forced
	// re-verification pass must not correct it away.
	nullAllStamps(t, e)
	if _, err := e.store.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	var blank int
	if err := e.db.QueryRow(`SELECT count(*) FROM memories WHERE title IS NULL OR title=''`).Scan(&blank); err != nil {
		t.Fatal(err)
	}
	if blank != 0 {
		t.Fatalf("%d rows lost their title after a verifying reconcile", blank)
	}
}

func TestBackfillTitlesIdempotent(t *testing.T) {
	e := newEnv(t)
	a := e.create(t, "- legacy lesson one\nmore")
	b := e.create(t, "### 遗留记忆二\n正文")
	kept, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "fact.note", Visibility: "project",
		Title: sp("Keep me"), Content: "# not this",
	}, e.user)
	if err != nil {
		t.Fatal(err)
	}
	// Rows written before titles were derived: empty or NULL titles.
	if _, err := e.db.Exec(`UPDATE memories SET title=NULL WHERE id=?1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE memories SET title='  ' WHERE id=?1`, b.ID); err != nil {
		t.Fatal(err)
	}
	titles := func() map[string]string {
		out := map[string]string{}
		for _, id := range []string{a.ID, b.ID, kept.ID} {
			got, err := e.store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			out[id] = deref(got.Title)
		}
		return out
	}
	if _, err := e.store.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{a.ID: "legacy lesson one", b.ID: "遗留记忆二", kept.ID: "Keep me"}
	first := titles()
	for id, w := range want {
		if first[id] != w {
			t.Fatalf("after backfill %s title = %q, want %q", id, first[id], w)
		}
	}
	// Second run: nothing left to select, nothing written.
	var pending int
	if err := e.db.QueryRow(`SELECT count(*) FROM memories WHERE status='active'
		AND (title IS NULL OR trim(title) = '')`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d rows still selected after the backfill", pending)
	}
	var before int64
	if err := e.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := e.store.backfillTitles(); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := e.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("second backfill wrote %d rows, want 0", after-before)
	}
	if second := titles(); second[a.ID] != first[a.ID] || second[b.ID] != first[b.ID] {
		t.Fatalf("second pass changed titles: %v → %v", first, second)
	}
}

func TestBackfillRowRedactedKeepsNoTitle(t *testing.T) {
	e := newEnv(t)
	var alphaID string
	if err := e.db.QueryRow(`SELECT id FROM projects WHERE name='alpha'`).Scan(&alphaID); err != nil {
		t.Fatal(err)
	}
	placeAuthority(t, e, alphaID, "mem_redactedtitle", "# would be a title")
	src := filepath.Join(e.store.dataDir, "memory", alphaID, "mem_redactedtitle")
	dst := filepath.Join(e.store.dataDir, "memory", alphaID, "_redacted", "mem_redactedtitle")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	var status string
	var title sql.NullString
	if err := e.db.QueryRow(`SELECT status, title FROM memories WHERE id='mem_redactedtitle'`).Scan(&status, &title); err != nil {
		t.Fatal(err)
	}
	if status != "redacted" || title.Valid {
		t.Fatalf("redacted backfill: status %q title %v, want redacted with no title", status, title)
	}
}

func sp(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
