package memory

// Focused tests for the 06-memory-search semantics: the three-step write
// order artifacts (INV-7a), the double-digest HTML staleness check
// (INV-7d, B12/B16), the §4.4 body extraction, short-query widening, the
// two-step redact shape, the A18 share ruling and the backupBarrier
// coordination (B08).

import (
	"errors"
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

// ---------- lock-discipline regressions for the INV-7f serving gate ----------

// simulatingPatchCommits performs, under a test-held memLock, everything a
// finished PATCH leaves behind: new authority bytes, row metadata and FTS
// from the file, and (when derive is true) the fresh html + digest pair.
// It is the state the blocked read must observe once the lock releases.
func simulatingPatchCommits(t *testing.T, s *Store, memID, body string, derive bool) []byte {
	t.Helper()
	row, err := s.load(memID)
	if err != nil {
		t.Fatal(err)
	}
	fm := &frontmatter{ID: memID, Type: row.Type, OwnerUserID: row.OwnerUserID,
		Title: row.Title, Tags: row.Tags, Visibility: row.Visibility,
		WorkItemID: row.WorkItemID, ProjectID: row.ProjectID, Related: row.Related,
		CreatedAt: row.CreatedAt, UpdatedAt: store.Now()}
	raw := writeAuthority(t, filepath.Join(s.dataDir, "memory", "pj1", memID), fm, body)
	if err := s.updateRowFromFile(row, fm, raw, shaHex(raw)); err != nil {
		t.Fatal(err)
	}
	if derive {
		_, fmBody, _ := splitFrontmatter(string(raw))
		if _, derr := s.deriveHTML(row, raw, fmBody); derr != nil {
			t.Fatal(derr)
		}
	}
	return raw
}

// TestContentServesUnderEntryLock pins the FB-3 ordering for Content: the
// INV-7f validation and the authority-byte read happen inside ONE hold of
// the entry's per-mem write mutex, so the bytes served always pair with
// the row the gate validated. The pre-fix shape validated without the
// lock and read the file afterwards — a PATCH completing in between
// served its new bytes against the validated old row.
func TestContentServesUnderEntryLock(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "original gate body")

	// A writer holding the entry's critical section.
	unlock := s.memLock(e.ID)

	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := s.Content(e.ID)
		done <- result{b, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("Content completed while the entry lock was held (gate/read not lock-protected): err=%v", r.err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked: the validate+read pair is atomic against writers.
	}

	raw := simulatingPatchCommits(t, s, e.ID, "replacement gate body", false)
	unlock()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Content: %v", r.err)
		}
		if shaHex(r.b) != shaHex(raw) {
			t.Fatal("Content served bytes other than the post-lock committed state")
		}
		row, err := s.load(e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.ContentSHA != shaHex(r.b) {
			t.Fatal("served bytes do not match the row the gate validated (TOCTOU)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Content did not resume after the lock was released")
	}
}

// TestHTMLDerivesUnderEntryLock pins the FB-3 ordering for HTML: the
// freshness judgment (row digests + live file digests) and any
// re-derivation all read state captured INSIDE the entry lock, so the
// digest pair can never be judged against or overwritten from a
// superseded committed state. The pre-fix shape captured authority bytes
// and the row's digests BEFORE the lock and re-derived from those bytes
// under it — a PATCH completing in between had its fresh
// source_markdown_sha/html_sha pair clobbered with stale digests.
func TestHTMLDerivesUnderEntryLock(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "original html body")
	// Prime the derived pair so the pre-fix fast path would return
	// immediately instead of blocking on the lock.
	if _, err := s.HTML(e.ID); err != nil {
		t.Fatal(err)
	}

	// A writer holding the entry's critical section.
	unlock := s.memLock(e.ID)

	type htmlResult struct {
		b   []byte
		err error
	}
	done := make(chan htmlResult, 1)
	go func() {
		b, err := s.HTML(e.ID)
		done <- htmlResult{b, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("HTML completed while the entry lock was held (freshness judgment not lock-protected): err=%v", r.err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked: the whole read runs under the entry mutex.
	}

	simulatingPatchCommits(t, s, e.ID, "replacement html body", true)
	unlock()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("HTML: %v", r.err)
		}
		doc := string(r.b)
		if !strings.Contains(doc, "replacement html body") {
			t.Fatalf("HTML derived from pre-lock (superseded) bytes: %s", doc)
		}
		if strings.Contains(doc, "original html body") {
			t.Fatalf("stale derived view served: %s", doc)
		}
		// The digest pair must reflect the post-lock authority, not a
		// stale overwrite (INV-7d).
		row, err := s.load(e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !row.smsSHA.Valid || row.smsSHA.String != row.ContentSHA {
			t.Fatalf("source_markdown_sha %v != content_sha %s (stale digest overwrite)", row.smsSHA, row.ContentSHA)
		}
		if !row.hSHA.Valid || row.hSHA.String != fileSHA(t, filepath.Join(s.dataDir, "memory", "pj1", e.ID, "memory.html")) {
			t.Fatal("html_sha does not match the on-disk derived view")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTML did not resume after the lock was released")
	}
}

// TestReadViewAuthorizesAndServesOneSnapshot (R2C-F3 regression): the
// detail route's authorization decision and the bytes it serves must come
// from ONE snapshot, captured inside a single hold of the entry's per-mem
// write mutex. The pre-fix server shape authorized against Get's metadata
// and then read Content under a separate lock window: a PATCH landing in
// between (visibility→private + new body) disclosed the NEW private body
// to a caller whose authorization was based on the OLD metadata. Here the
// PATCH commits while the read is blocked on the lock; the read must then
// judge the post-lock state and refuse — never serve the private bytes.
func TestReadViewAuthorizesAndServesOneSnapshot(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "public body snapshot probe")

	// The server layer's tier predicate shape (06 §7 layer 2): a
	// non-owner is entitled exactly while the entry is not private.
	tierAllowed := func(e *Entry) (bool, error) { return e.Visibility != "private", nil }

	// A writer holding the entry's critical section.
	unlock := s.memLock(e.ID)

	type result struct {
		entry   *Entry
		content []byte
		html    []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		entry, content, html, err := s.ReadView(e.ID, tierAllowed, true)
		done <- result{entry, content, html, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("ReadView completed while the entry lock was held (authorize/read not one lock window): err=%v", r.err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked: decision and byte read share one critical section.
	}

	// Commit everything a finished PATCH leaves behind — with the entry
	// now flipped to visibility=private and a brand-new body.
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	fm := &frontmatter{ID: e.ID, Type: row.Type, OwnerUserID: row.OwnerUserID,
		Title: row.Title, Tags: row.Tags, Visibility: "private",
		WorkItemID: row.WorkItemID, ProjectID: row.ProjectID, Related: row.Related,
		CreatedAt: row.CreatedAt, UpdatedAt: store.Now()}
	raw := writeAuthority(t, filepath.Join(s.dataDir, "memory", "pj1", e.ID), fm, "private secret body")
	if err := s.updateRowFromFile(row, fm, raw, shaHex(raw)); err != nil {
		t.Fatal(err)
	}
	unlock()

	select {
	case r := <-done:
		ae, ok := r.err.(*APIError)
		if !ok || ae.Code != "MEM-404" {
			t.Fatalf("ReadView served a snapshot that turned private mid-read: err=%v entry=%v content=%q", r.err, r.entry, r.content)
		}
		if r.content != nil || r.html != nil {
			t.Fatal("refused read still returned bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadView did not resume after the lock was released")
	}

	// The owner-shaped predicate (private allowed) reads the SAME
	// post-lock snapshot fine: entry, markdown and derived view all pair.
	entry, content, html, err := s.ReadView(e.ID, nil, true)
	if err != nil {
		t.Fatalf("owner-shaped ReadView: %v", err)
	}
	if entry.Visibility != "private" || !strings.Contains(string(content), "private secret body") {
		t.Fatalf("post-lock snapshot wrong: vis=%s content=%q", entry.Visibility, content)
	}
	if !strings.Contains(string(html), "private secret body") {
		t.Fatalf("derived view not from the same snapshot: %s", html)
	}
	// A predicate error is propagated verbatim (the server layer's typed
	// CORE-403 scope rejection must reach the client, not collapse to 404).
	_, _, _, err = s.ReadView(e.ID, func(*Entry) (bool, error) { return false, errTestReject }, false)
	if err != errTestReject {
		t.Fatalf("predicate error not propagated: %v", err)
	}
}

var errTestReject = errors.New("test typed rejection")
