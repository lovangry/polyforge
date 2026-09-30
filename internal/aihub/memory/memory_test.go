package memory

// Memory tests: FTS round trip (create → search, update reindex, redact
// removal, short CJK token widening) and the three reconcile scenarios
// (orphan row rollback, authority file backfill, redacted forward
// completion).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
)

type env struct {
	db    *core.DB
	store *Store
	admin *core.AdminManager
	user  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := core.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	admin := core.NewAdminManager(db)
	u, err := admin.CreateUser("root", "root@example.com", "admin", "human")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := admin.CreateProject(core.CreateProjectRequest{Name: "alpha"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	return &env{db: db, store: NewStore(db, t.TempDir()), admin: admin, user: u.ID}
}

func (e *env) create(t *testing.T, content string) *Entry {
	t.Helper()
	entry, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "experience.debug",
		Visibility: "project", Content: content,
	}, e.user)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return entry
}

func TestFtsRoundTrip(t *testing.T) {
	e := newEnv(t)
	entry := e.create(t, "The trigram index finds arbitrary substrings like xyzzyplugh.")

	// ≥3-rune token via FTS MATCH.
	hits, err := e.store.Search("xyzzyplugh", "", nil, 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != entry.ID {
		t.Fatalf("round trip miss: %+v", hits)
	}
	// Arbitrary substring (mid-word) works too.
	hits, err = e.store.Search("zzyplu", "", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("substring miss: %v %+v", err, hits)
	}
	// A 2-rune token exercises the short-query widening.
	hits, err = e.store.Search("中", "", nil, 10, nil)
	if err != nil || len(hits) != 0 {
		// no CJK content yet: expect zero, not an error
		t.Fatalf("short token on empty: %v %+v", err, hits)
	}
	e.create(t, "中文检索必须覆盖两字词。")
	hits, err = e.store.Search("中文", "", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("CJK 2-rune widening miss: %v %+v", err, hits)
	}
	// Type filter.
	hits, err = e.store.Search("trigram", "experience.debug", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("type filter: %v %+v", err, hits)
	}
	hits, err = e.store.Search("trigram", "fact.other", nil, 10, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("type filter should exclude: %v", hits)
	}

	// Update: the FTS row follows the new content (delete-then-insert).
	if _, err := e.store.Update(entry.ID, UpdateRequest{Content: "rewritten body talks about frobnicators now"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if hits, _ = e.store.Search("xyzzyplugh", "", nil, 10, nil); len(hits) != 0 {
		t.Fatalf("stale FTS row survived the update")
	}
	if hits, _ = e.store.Search("frobnicators", "", nil, 10, nil); len(hits) != 1 {
		t.Fatalf("updated content not indexed")
	}
	// The authority file content matches what was written.
	content, err := e.store.Content(entry.ID)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	if !strings.Contains(string(content), "frobnicators") {
		t.Fatalf("authority file stale: %s", content)
	}
	if !strings.HasPrefix(string(content), "---\n") {
		t.Fatalf("authority file missing frontmatter: %q", string(content)[:40])
	}

	// Redact: gone from search and from Get.
	if err := e.store.Redact(entry.ID); err != nil {
		t.Fatalf("redact: %v", err)
	}
	if _, err := e.store.Get(entry.ID); err == nil || err.(*APIError).Code != "MEM-404" {
		t.Fatalf("redacted entry still served: %v", err)
	}
	if hits, _ = e.store.Search("frobnicators", "", nil, 10, nil); len(hits) != 0 {
		t.Fatalf("redacted entry still searchable")
	}
}

// TestSearchTypePrefixFilter: the type filter's wildcard form — a
// trailing ".*" is a prefix match on "X." (experience.* matches
// experience.debug and experience.approach, not fact.note and not the
// bare type experience); a non-wildcard value keeps exact-match
// semantics (fact.* never matches experience.* rows).
func TestSearchTypePrefixFilter(t *testing.T) {
	e := newEnv(t)
	mk := func(memType string) {
		t.Helper()
		if _, err := e.store.Create(CreateRequest{
			Project: "alpha", Type: memType,
			Visibility: "project", Content: "the shared xylophone token",
		}, e.user); err != nil {
			t.Fatalf("create %s: %v", memType, err)
		}
	}
	mk("experience.debug")
	mk("experience.approach")
	mk("fact.note")
	mk("experience")

	types := func(hits []Entry) []string {
		out := make([]string, 0, len(hits))
		for _, h := range hits {
			out = append(out, h.Type)
		}
		return out
	}

	// experience.*: both experience.* rows, never fact.note and never
	// the bare type experience.
	hits, err := e.store.Search("xylophone", "experience.*", nil, 10, nil)
	if err != nil {
		t.Fatalf("search experience.*: %v", err)
	}
	got := types(hits)
	if len(got) != 2 {
		t.Fatalf("experience.* hits = %v, want the two experience.* types", got)
	}
	for _, ty := range got {
		if !strings.HasPrefix(ty, "experience.") {
			t.Fatalf("experience.* matched non-experience type %q", ty)
		}
	}

	// fact.*: only fact.note — an experience.* row never matches.
	hits, err = e.store.Search("xylophone", "fact.*", nil, 10, nil)
	if err != nil {
		t.Fatalf("search fact.*: %v", err)
	}
	if got = types(hits); len(got) != 1 || got[0] != "fact.note" {
		t.Fatalf("fact.* hits = %v, want only fact.note", got)
	}

	// Non-wildcard values keep exact-match semantics.
	hits, err = e.store.Search("xylophone", "experience.debug", nil, 10, nil)
	if err != nil || len(hits) != 1 || hits[0].Type != "experience.debug" {
		t.Fatalf("exact hits = %+v err=%v, want only experience.debug", hits, err)
	}
	hits, err = e.store.Search("xylophone", "experience", nil, 10, nil)
	if err != nil || len(hits) != 1 || hits[0].Type != "experience" {
		t.Fatalf("bare-type hits = %+v err=%v, want only the bare experience row", hits, err)
	}
}

func TestReconcileOrphanRowRollback(t *testing.T) {
	// Scenario: the row committed (step ②) but the authority file was
	// never placed (crash before ③). Reconcile deletes the whole create.
	e := newEnv(t)
	entry := e.create(t, "about to be orphaned")
	dir := filepath.Join(e.store.dataDir, "memory", entry.ProjectID, entry.ID)
	// Simulate the crash window: file gone, row alive.
	if err := os.Remove(filepath.Join(dir, "memory.md")); err != nil {
		t.Fatalf("remove authority: %v", err)
	}
	// The INV-7f gate refuses to serve it.
	if _, err := e.store.Get(entry.ID); err == nil {
		t.Fatalf("orphan row served")
	}
	// And it is excluded from search.
	if hits, _ := e.store.Search("orphaned", "", nil, 10, nil); len(hits) != 0 {
		t.Fatalf("orphan row searchable")
	}
	// Rebuild: the row dies with its FTS row.
	if _, err := e.store.RebuildIndex(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?1`, entry.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan row survived rebuild: %v %d", err, n)
	}
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE mem_id=?1`, entry.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan FTS row survived rebuild: %v %d", err, n)
	}
}

func TestReconcileFileBackfill(t *testing.T) {
	// Scenario: an authority file exists on disk with NO row (manual
	// placement / restore-from-backup). Rebuild backfills the row
	// entirely from the frontmatter (file wins, INV-7c).
	e := newEnv(t)
	entry := e.create(t, "the canonical content")
	dir := filepath.Join(e.store.dataDir, "memory", entry.ProjectID, entry.ID)
	raw, err := os.ReadFile(filepath.Join(dir, "memory.md"))
	if err != nil {
		t.Fatalf("read authority: %v", err)
	}
	// Plant a copy under a new mem id with a corrected frontmatter id.
	// Simpler: nuke the row of the existing entry, keep the file.
	if _, err := e.db.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, entry.ID); err != nil {
		t.Fatalf("del fts: %v", err)
	}
	if _, err := e.db.Exec(`DELETE FROM memories WHERE id=?1`, entry.ID); err != nil {
		t.Fatalf("del row: %v", err)
	}
	if n, err := e.store.RebuildIndex(); err != nil || n != 1 {
		t.Fatalf("rebuild: %v %d", err, n)
	}
	back, err := e.store.load(entry.ID)
	if err != nil {
		t.Fatalf("backfill missing: %v", err)
	}
	if back.ContentSHA != shaHex(raw) || back.Status != "active" {
		t.Fatalf("backfill wrong: %+v", back)
	}
	// And it is searchable again.
	hits, err := e.store.Search("canonical", "", nil, 10, nil)
	if err != nil || len(hits) != 1 || hits[0].ID != entry.ID {
		t.Fatalf("backfilled entry not searchable: %v %+v", err, hits)
	}
}

func TestReconcileRedactedForwardCompletion(t *testing.T) {
	// Scenario: redact moved the directory into _redacted/ but crashed
	// before committing step ②. Rebuild forward-completes from the file
	// position — never rolls back.
	e := newEnv(t)
	entry := e.create(t, "to be redacted incompletely")
	dir := filepath.Join(e.store.dataDir, "memory", entry.ProjectID, entry.ID)
	dst := filepath.Join(e.store.dataDir, "memory", entry.ProjectID, "_redacted", entry.ID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(dir, dst); err != nil {
		t.Fatalf("move: %v", err)
	}
	// Row is still active: the read-path gate must heal it, not serve it.
	if _, err := e.store.Get(entry.ID); err == nil {
		t.Fatalf("half-redacted entry served")
	}
	if _, err := e.store.RebuildIndex(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var status string
	if err := e.db.QueryRow(`SELECT status FROM memories WHERE id=?1`, entry.ID).Scan(&status); err != nil || status != "redacted" {
		t.Fatalf("redact not forward-completed: %v %q", err, status)
	}
	if _, err := e.store.Get(entry.ID); err == nil {
		t.Fatalf("forward-completed redaction still served")
	}
}

func TestIdempotentCreate(t *testing.T) {
	e := newEnv(t)
	key := "wrap-summary-1"
	e1, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "experience.debug", Visibility: "project",
		Content: "first write", IdempotencyKey: &key,
	}, e.user)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same key + same owner + same project returns the original entry.
	again, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "experience.debug", Visibility: "project",
		Content: "different content entirely", IdempotencyKey: &key,
	}, e.user)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if again.ID != e1.ID {
		t.Fatalf("idempotent replay created a new entry: %s vs %s", again.ID, e1.ID)
	}
	// Same key, different owner: rejected.
	other := "other-owner"
	if _, err := e.store.Create(CreateRequest{
		Project: "alpha", Type: "experience.debug", Visibility: "project",
		Content: "x", IdempotencyKey: &key,
	}, other); err == nil || err.(*APIError).Reason != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("cross-owner key reuse not rejected: %v", err)
	}
}
