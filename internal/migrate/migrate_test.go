package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/core/testsupport"
)

// WAL read-only probe: the verification opens the Core SQLite file while
// the live server holds it open in WAL mode — this must work with no
// write path of our own.
func TestOpenCoreReadOnlyWhileWriterHoldsWAL(t *testing.T) {
	db, dataDir := testsupport.NewDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO core_meta(key, value) VALUES ('probe', 'ok')`); err != nil {
		t.Fatal(err)
	}
	ro, err := openCoreReadOnly(dataDir)
	if err != nil {
		t.Fatalf("read-only open against a live WAL database: %v", err)
	}
	defer ro.Close()
	var v string
	if err := ro.QueryRow(`SELECT value FROM core_meta WHERE key='probe'`).Scan(&v); err != nil {
		t.Fatalf("query: %v", err)
	}
	if v != "ok" {
		t.Fatalf("value: %q", v)
	}
	// A write attempt on the read-only handle must fail, never silently
	// succeed.
	if _, err := ro.Exec(`INSERT INTO core_meta(key, value) VALUES ('x', 'y')`); err == nil {
		t.Fatal("read-only handle accepted a write")
	}
}

// JSONL parsing: full-shape records, defaults for optional fields,
// --since filtering, and the input-corruption refusals.
func TestReadRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.jsonl")
	lines := []string{
		`{"id":"mem_aaa000111222","type":"experience.debug","title":"T","tags":["a"],"visibility":"project","content":"body one","project":"p1","owner":"alice@example.com","owner_kind":"human","work_item_id":"wi_old","related":["mem_bbb000111222"],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","status":"active"}`,
		`{"id":"mem_bbb000111222","type":"fact.note","visibility":"private","content":"body two","project":"p1","owner":"ci-agent","created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-02T00:00:00Z","status":"redacted"}`,
		``, // blank line tolerated
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, dropped, total, err := readRecords(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || dropped != 0 || len(recs) != 2 {
		t.Fatalf("counts: total=%d dropped=%d len=%d", total, dropped, len(recs))
	}
	if recs[0].WorkItemID == nil || *recs[0].WorkItemID != "wi_old" {
		t.Fatalf("work_item_id: %+v", recs[0].WorkItemID)
	}
	if recs[1].Title != nil || recs[1].Tags != nil || recs[1].Related != nil {
		t.Fatalf("absent optional fields must stay nil: %+v", recs[1])
	}
	if recs[1].humanOwner() {
		t.Fatal("owner without owner_kind and no '@' should infer machine")
	}

	// --since delta filter: only the record with updated_at >= since.
	recs, dropped, total, err = readRecords(path, "2026-01-15T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || dropped != 1 || len(recs) != 1 || recs[0].ID != "mem_bbb000111222" {
		t.Fatalf("delta: total=%d dropped=%d recs=%+v", total, dropped, recs)
	}

	// Corruption refusals.
	bad := map[string]string{
		"unknown key":    `{"id":"mem_x","nope":1}`,
		"bad id":         `{"id":"wi_x","type":"t","visibility":"project","content":"c","project":"p","owner":"o","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"active"}`,
		"bad visibility": `{"id":"mem_x","type":"t","visibility":"nope","content":"c","project":"p","owner":"o","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"active"}`,
		"bad status":     `{"id":"mem_x","type":"t","visibility":"project","content":"c","project":"p","owner":"o","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"archived"}`,
		"blank content":  `{"id":"mem_x","type":"t","visibility":"project","content":"  ","project":"p","owner":"o","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"active"}`,
		"bad timestamp":  `{"id":"mem_x","type":"t","visibility":"project","content":"c","project":"p","owner":"o","created_at":"not-a-time","updated_at":"2026-01-01T00:00:00Z","status":"active"}`,
		"dup id": strings.Join([]string{
			`{"id":"mem_dup","type":"t","visibility":"project","content":"c","project":"p","owner":"o","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"active"}`,
			`{"id":"mem_dup","type":"t","visibility":"project","content":"c","project":"p","owner":"o","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"active"}`,
		}, "\n"),
	}
	for name, body := range bad {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := readRecords(path, ""); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The rendered authority document must round-trip through the same
// framing the server's parseAuthorityDoc uses: leading "---" block, the
// exact field set, omitempty on absent optionals.
func TestRenderAuthorityFileShape(t *testing.T) {
	title := "Some title"
	tags := []string{"a", "b"}
	related := []string{"mem_zzz000111222"}
	fm := &authorityFM{
		ID: "mem_abc123def456", Type: "experience.debug", OwnerUserID: "usr_1",
		Title: &title, Tags: tags, Visibility: "project", ProjectID: "pj_1",
		Related: related, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	}
	raw, err := renderAuthorityFile(fm, "body text")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		"---\nid: mem_abc123def456\n", "type: experience.debug\n",
		"owner_user_id: usr_1\n", "title: Some title\n", "tags: [a, b]\n",
		"visibility: project\n", "project_id: pj_1\n",
		"related: [mem_zzz000111222]\n",
		"created_at: \"2026-01-01T00:00:00Z\"\n", "updated_at: \"2026-01-02T00:00:00Z\"\n",
		"---\nbody text",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "work_item_id") || strings.Contains(s, "idempotency_key") {
		t.Fatalf("omitted optionals must not appear:\n%s", s)
	}

	// Empty lists are omitted (the row stores "[]"; the parse side treats
	// nil and empty as equal).
	fm2 := &authorityFM{
		ID: "mem_abc123def456", Type: "t", OwnerUserID: "u", Visibility: "private",
		ProjectID: "p", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	raw2, err := renderAuthorityFile(fm2, "b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw2), "tags:") || strings.Contains(string(raw2), "related:") {
		t.Fatalf("empty lists must be omitted:\n%s", raw2)
	}
}

// Placement + read-back: files land at the contract paths, and the
// readPlacedFM used by verification step ④ sees what was written.
func TestPlaceAndReadBack(t *testing.T) {
	dir := t.TempDir()
	fm := &authorityFM{
		ID: "mem_abc123def456", Type: "t", OwnerUserID: "u", Visibility: "project",
		ProjectID: "pj_9", Related: []string{"mem_other"}, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	raw, err := renderAuthorityFile(fm, "hello world")
	if err != nil {
		t.Fatal(err)
	}
	rel := authorityRelPath("pj_9", "mem_abc123def456", false)
	if rel != "memory/pj_9/mem_abc123def456" {
		t.Fatalf("rel path: %s", rel)
	}
	if err := placeAuthorityFile(dir, rel, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory", "pj_9", "mem_abc123def456", "memory.md")); err != nil {
		t.Fatal(err)
	}
	back, err := readPlacedFM(dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != fm.ID || back.ProjectID != fm.ProjectID || len(back.Related) != 1 || back.Related[0] != "mem_other" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if back.WorkItemID != nil {
		t.Fatalf("work_item_id should be absent: %+v", back.WorkItemID)
	}

	// Redacted placement lands under _redacted/ and stays parseable.
	relRed := authorityRelPath("pj_9", "mem_abc123def456", true)
	if relRed != "memory/pj_9/_redacted/mem_abc123def456" {
		t.Fatalf("redacted rel path: %s", relRed)
	}
	if err := placeAuthorityFile(dir, relRed, raw); err != nil {
		t.Fatal(err)
	}

	// scanPlacedIDs sees both trees.
	ids, err := scanPlacedIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ids["mem_abc123def456"] || len(ids) != 1 {
		t.Fatalf("scan: %+v", ids)
	}
}

// The retrieval probe: leading 3 runes of the longest alphanumeric word,
// across ASCII and CJK bodies.
func TestSmokeSubstring(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"deployment scripts record rollback steps", "dep"},
		{"部署脚本记录了回滚步骤", "部署脚"},
		{"ok", "ok"},
		{"# Heading\n\n- item alpha", "Hea"},
	}
	for _, c := range cases {
		got, err := smokeSubstring(c.body)
		if err != nil {
			t.Fatalf("%q: %v", c.body, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %q want %q", c.body, got, c.want)
		}
	}
	if _, err := smokeSubstring("* * *"); err == nil {
		t.Fatal("no-alphanumeric body must not yield a probe")
	}
}

// The sampler: fixed ratio, deterministic across runs, minimum one.
func TestSampleIDs(t *testing.T) {
	var placed []placedMem
	for i := 0; i < 20; i++ {
		placed = append(placed, placedMem{rec: Record{ID: "mem_x" + string(rune('a'+i))}})
	}
	a := sampleIDs(placed, false)
	b := sampleIDs(placed, false)
	if len(a) != 2 { // 10% of 20
		t.Fatalf("sample size: %d", len(a))
	}
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("not deterministic: %v vs %v", a, b)
	}
	one := sampleIDs(placed[:1], false)
	if len(one) != 1 {
		t.Fatalf("minimum sample: %v", one)
	}
	if sampleIDs(nil, false) != nil {
		t.Fatal("empty input must not sample")
	}
}
