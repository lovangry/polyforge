package memory

// Focused tests for the 06-memory-search semantics: the three-step write
// order artifacts (INV-7a), the double-digest HTML staleness check
// (INV-7d, B12/B16), the §4.4 body extraction, short-query widening, the
// two-step redact shape, the A18 share ruling and the backupBarrier
// coordination (B08).

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO projects(id, name) VALUES ('pj1','p1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_worker','w','human','writer')`); err != nil {
		t.Fatal(err)
	}
	return NewStore(db, dir)
}

func mustCreate(t *testing.T, s *Store, content string) *Entry {
	t.Helper()
	e, err := s.Create(CreateRequest{
		Project: "p1", Type: "experience.debug", Visibility: "project",
		Content: content, Title: strp("t"), Tags: []string{"a"},
	}, "usr_worker")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return e
}

func strp(s string) *string { return &s }

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return shaHex(b)
}

// TestCreateWriteOrder pins INV-7a end state: the tmp file is gone, the
// authority file carries the frontmatter self-identification, the row's
// content_sha equals the on-disk bytes, and the derived pair (memory.html
// + both digests) is consistent.
func TestCreateWriteOrder(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "# Heading\n\n你好世界 polyforge trigram probe")

	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	if _, err := os.Stat(filepath.Join(dir, "memory.md.tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp file survived the write order: %v", err)
	}
	authority, err := os.ReadFile(filepath.Join(dir, "memory.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(authority), "id: "+e.ID) {
		t.Fatalf("authority file does not self-identify:\n%s", authority)
	}
	if sha := shaHex(authority); sha != e.ContentSHA {
		t.Fatalf("content_sha %s != on-disk sha %s (INV-7c)", e.ContentSHA, sha)
	}

	// Derived pair: html file exists, both digests recorded and consistent.
	fresh, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.smsSHA.Valid || fresh.smsSHA.String != e.ContentSHA {
		t.Fatalf("source_markdown_sha = %v, want %s", fresh.smsSHA, e.ContentSHA)
	}
	if !fresh.hSHA.Valid || fresh.hSHA.String != fileSHA(t, filepath.Join(dir, "memory.html")) {
		t.Fatalf("html_sha %v does not match the html file", fresh.hSHA)
	}
}

// TestCreateFrontmatterRules covers 06 §2.2: a client-supplied id is
// CLIENT_ID_NOT_ALLOWED; malformed frontmatter is FRONTMATTER_INVALID.
func TestCreateFrontmatterRules(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
		Visibility: "project", Content: "---\nid: mem_hacked\n---\nbody"}, "usr_worker")
	ae, ok := err.(*APIError)
	if !ok || ae.Reason != "CLIENT_ID_NOT_ALLOWED" {
		t.Fatalf("client id: got %v", err)
	}
	_, err = s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
		Visibility: "project", Content: "---\n: :: bad yaml [\n---\nbody"}, "usr_worker")
	if ae, ok := err.(*APIError); !ok || ae.Reason != "FRONTMATTER_INVALID" {
		t.Fatalf("bad frontmatter: got %v", err)
	}
	// A leading "---" with no close is a thematic break, not frontmatter.
	e, err := s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
		Visibility: "project", Content: "---\njust a thematic break body"}, "usr_worker")
	if err != nil {
		t.Fatalf("thematic break misparsed as frontmatter: %v", err)
	}
	if !strings.Contains(string(mustContent(t, s, e.ID)), "just a thematic break body") {
		t.Fatal("body lost")
	}
}

func mustContent(t *testing.T, s *Store, memID string) []byte {
	t.Helper()
	b, err := s.Content(memID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUpdateWriteOrder covers the PATCH path: history snapshot, full
// metadata follow (incl. visibility from the new frontmatter), and
// re-derivation of the HTML pair.
func TestUpdateWriteOrder(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "old body 你好世界")

	upd, err := s.Update(e.ID, UpdateRequest{
		Content: "---\nvisibility: private\nrelated: [mem_other]\n---\nnew body 学习笔记",
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Visibility != "private" {
		t.Fatalf("visibility not followed from frontmatter: %q", upd.Visibility)
	}
	if len(upd.Related) != 1 || upd.Related[0] != "mem_other" {
		t.Fatalf("related not followed: %v", upd.Related)
	}
	if !strings.Contains(string(mustContent(t, s, e.ID)), "new body 学习笔记") {
		t.Fatal("authority not replaced")
	}
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	matches, _ := filepath.Glob(filepath.Join(dir, "history", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("history snapshot missing: %v", matches)
	}
	if !strings.Contains(string(mustContent(t, s, e.ID)), "id: "+e.ID) {
		t.Fatal("updated authority lost its self-identification")
	}
	// Derived pair follows the update.
	fresh, _ := s.load(e.ID)
	if fresh.smsSHA.String != upd.ContentSHA {
		t.Fatalf("source_markdown_sha %s != content_sha %s", fresh.smsSHA.String, upd.ContentSHA)
	}
	if fresh.hSHA.String != fileSHA(t, filepath.Join(dir, "memory.html")) {
		t.Fatal("html_sha does not follow the re-derived html")
	}
	// Frontmatter id mismatch is refused.
	_, err = s.Update(e.ID, UpdateRequest{Content: "---\nid: mem_other\n---\nx"})
	if ae, ok := err.(*APIError); !ok || ae.Reason != "FRONTMATTER_ID_MISMATCH" {
		t.Fatalf("id mismatch: got %v", err)
	}
}

// TestHTMLStalenessReDerive covers both INV-7d windows: (B12) the
// "metadata committed, derivation never ran" crash point — html missing or
// source digest stale → re-derive; (B16) html tampered out-of-band →
// re-derive even though the Markdown never changed.
func TestHTMLStalenessReDerive(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "# One\n\nbody one")
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	htmlPath := filepath.Join(dir, "memory.html")

	// B12: simulate ③ done, derivation not run: drop the artifact and the
	// digests (what a crash between rename and derive leaves behind).
	if err := os.Remove(htmlPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET source_markdown_sha=NULL, html_sha=NULL WHERE id=?1`, e.ID); err != nil {
		t.Fatal(err)
	}
	out, err := s.HTML(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "<h1>One</h1>") {
		t.Fatalf("re-derivation produced %s", out)
	}
	fresh, _ := s.load(e.ID)
	if !fresh.hSHA.Valid || fresh.hSHA.String != fileSHA(t, htmlPath) {
		t.Fatal("digests not recorded after re-derivation")
	}

	// B16: tamper with memory.html out-of-band while the Markdown is intact.
	if err := os.WriteFile(htmlPath, []byte("<h1>tampered</h1>"), 0o640); err != nil {
		t.Fatal(err)
	}
	out, err = s.HTML(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tampered") {
		t.Fatal("stale (tampered) html served despite html_sha mismatch")
	}
	if fileSHA(t, htmlPath) != shaHex(out) {
		t.Fatal("html file not rewritten by re-derivation")
	}
}

// TestExtractBody pins the 06 §4.4 rule set: frontmatter stripped,
// structural characters removed from prose, code blocks kept verbatim.
func TestExtractBody(t *testing.T) {
	content := "---\ntitle: t\n---\n# Heading *bold* _em_\n\n> quoted\n\n```go\n# not a heading\nx := *ptr\n```\n"
	body := extractBody(content)
	if strings.Contains(body, "title") {
		t.Fatalf("frontmatter leaked into the index body: %q", body)
	}
	for _, structural := range []string{"#", "*", "_", ">"} {
		if strings.Contains(strings.Split(body, "# not a heading")[0], structural) {
			t.Fatalf("structural %q survived in prose: %q", structural, body)
		}
	}
	if !strings.Contains(body, "# not a heading") || !strings.Contains(body, "x := *ptr") {
		t.Fatalf("code block not preserved verbatim: %q", body)
	}
}

// TestSearchShortQuery exercises the retrieval widening: 2-rune CJK and
// 2-char ASCII terms that a trigram MATCH alone cannot answer still hit;
// mixed long+short queries intersect correctly.
func TestSearchShortQuery(t *testing.T) {
	s := newTestStore(t)
	a := mustCreate(t, s, "你好世界 polyforge trigram probe")
	b := mustCreate(t, s, "golang 学习笔记 experience")

	for _, q := range []string{"好世", "学习", "笔记"} {
		hits, err := s.Search(q, "", nil, 50, nil)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(hits) == 0 {
			t.Fatalf("short query %q returned nothing", q)
		}
	}
	// Mixed: long token narrows via FTS, short token filters in the scan.
	hits, err := s.Search("polyforge 好世", "", nil, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != a.ID {
		t.Fatalf("mixed query: got %v", hits)
	}
	// A short token that matches nothing excludes the candidate.
	hits, err = s.Search("polyforge 学习", "", nil, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("mixed query should not hit: %v", hits)
	}
	// Long-only queries keep working through FTS (3+ runes).
	hits, err = s.Search("你好世界", "", nil, 50, nil)
	if err != nil || len(hits) != 1 || hits[0].ID != a.ID {
		t.Fatalf("long query: %v %v", err, hits)
	}
	_ = b
	// Type filter still applies on the widened path.
	hits, err = s.Search("好世", "experience.debug", nil, 50, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("type filter on widened path: %v %v", err, hits)
	}
}

// TestRedactTwoStep pins the 06 §5.2 redact shape: dir moved first, then
// one tx for status + FTS removal; a second redact is MEM-409.
func TestRedactTwoStep(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "你好世界 redact me")
	if err := s.Redact(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "memory", "pj1", e.ID, "memory.md")); !os.IsNotExist(err) {
		t.Fatal("authority dir not moved to _redacted/")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "memory", "pj1", "_redacted", e.ID, "memory.md")); err != nil {
		t.Fatal("authority dir missing under _redacted/")
	}
	row, _ := s.load(e.ID)
	if row.Status != "redacted" {
		t.Fatalf("status = %s", row.Status)
	}
	hits, err := s.Search("你好世界", "", nil, 50, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("redacted memory still searchable: %v %v", err, hits)
	}
	err = s.Redact(e.ID)
	if ae, ok := err.(*APIError); !ok || ae.Code != "MEM-409" {
		t.Fatalf("second redact: %v", err)
	}
	// Update on a redacted entry is refused (terminal not writable).
	_, err = s.Update(e.ID, UpdateRequest{Content: "zombie"})
	if ae, ok := err.(*APIError); !ok || ae.Code != "MEM-409" {
		t.Fatalf("update on redacted: %v", err)
	}
}

// TestShareRuling: stateless HMAC token — verifiable, single-entry, and a
// forged signature is rejected (06 §5.1 A18; ruling recorded in Share).
func TestShareRuling(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "shared body")
	secret := []byte("test-secret")
	tok, err := s.Share(e.ID, secret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveShare(tok, secret)
	if err != nil || got != e.ID {
		t.Fatalf("resolve: %v %s", err, got)
	}
	_, err = s.ResolveShare(e.ID+".deadbeefdeadbeefdeadbe", secret)
	if ae, ok := err.(*APIError); !ok || ae.Code != "MEM-404" {
		t.Fatalf("forged token accepted: %v", err)
	}
	_, err = s.ResolveShare(tok, []byte("other-secret"))
	if ae, ok := err.(*APIError); !ok || ae.Code != "MEM-404" {
		t.Fatalf("token verified under the wrong secret: %v", err)
	}
}

// TestBackupBarrierCoordination (B08): while the backup procedure holds
// the barrier's write side, a memory create blocks; once released, the
// write completes. Same test shape covers update/redact — they take the
// same read side at the same point.
func TestBackupBarrierCoordination(t *testing.T) {
	s := newTestStore(t)
	BackupBarrier.Lock()

	done := make(chan error, 1)
	go func() {
		_, err := s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
			Visibility: "project", Content: "blocked write"}, "usr_worker")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("memory write completed while the backup held the barrier: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked: the coordination holds.
	}
	BackupBarrier.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write failed after barrier release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("memory write did not resume after barrier release")
	}
}

// TestConcurrentUpdateSerialization (06 §6): concurrent updates of one
// entry serialize on the per-mem mutex; the authority file is never a
// half-written mix and the row matches the surviving file.
func TestConcurrentUpdateSerialization(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "base body")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Update(e.ID, UpdateRequest{Content: "writer " + string(rune('A'+i)) + " 学习"})
			if err != nil {
				t.Errorf("update %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	content := string(mustContent(t, s, e.ID))
	row, _ := s.load(e.ID)
	if shaHex(mustContent(t, s, e.ID)) != row.ContentSHA {
		t.Fatal("row content_sha does not match the surviving authority file")
	}
	if !strings.Contains(content, "id: "+e.ID) {
		t.Fatal("authority file corrupted by concurrent updates")
	}
}

// TestContentLimit: the 06 §5.4 MEM-413 boundary (≤ 1 MiB).
func TestContentLimit(t *testing.T) {
	s := newTestStore(t)
	big := strings.Repeat("x", maxContentBytes+1)
	_, err := s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
		Visibility: "project", Content: big}, "usr_worker")
	if ae, ok := err.(*APIError); !ok || ae.Code != "MEM-413" {
		t.Fatalf("oversized create: %v", err)
	}
	e := mustCreate(t, s, "small")
	_, err = s.Update(e.ID, UpdateRequest{Content: big})
	if ae, ok := err.(*APIError); !ok || ae.Code != "MEM-413" {
		t.Fatalf("oversized update: %v", err)
	}
}

// TestIdempotentCreate: the wrap-summary idempotency key dedups creates.
func TestIdempotentCreate(t *testing.T) {
	s := newTestStore(t)
	key := "wrap-summary-wi_1-3"
	e1, err := s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
		Visibility: "project", Content: "summary body", IdempotencyKey: &key}, "usr_worker")
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.Create(CreateRequest{Project: "p1", Type: "experience.debug",
		Visibility: "project", Content: "summary body", IdempotencyKey: &key}, "usr_worker")
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID != e2.ID {
		t.Fatalf("idempotency key produced two entries: %s vs %s", e1.ID, e2.ID)
	}
}

// TestRawHTMLEscaped: the derived view never passes raw HTML through
// (06 §4.3 unsafe=false / §7: no script-injection surface).
func TestRawHTMLEscaped(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "# T\n\n<script>alert(1)</script>\n\n**bold** and a [link](https://example.com)")
	out, err := s.HTML(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	if strings.Contains(doc, "<script>") {
		t.Fatalf("raw script passed through: %s", doc)
	}
	// goldmark's safe mode drops raw HTML with an "omitted" comment rather
	// than escaping it entity-by-entity — the 06 §4.3 rule (raw HTML
	// disabled, no injection surface) is satisfied either way.
	if !strings.Contains(doc, "raw HTML omitted") || !strings.Contains(doc, "<strong>bold</strong>") {
		t.Fatalf("unexpected derivation: %s", doc)
	}
	if !strings.Contains(doc, `<a href="https://example.com">link</a>`) {
		t.Fatalf("markdown links not rendered: %s", doc)
	}
}
