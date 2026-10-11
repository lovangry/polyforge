package memory

// reconcile_stamp_test.go (polyforge#129): the freshness-stamp gate's
// backward-compatibility and refresh paths, complementary to the steady-
// state tests in reconcile_steady_state_test.go.
//
//   - A pre-#129 database (rows whose fs_mtime_ns/fs_size are NULL) must
//     degrade to the old full pass exactly ONCE — every row verified by
//     content, every FTS row rewritten, every row stamped — and be
//     incremental from the next pass on.
//   - A touched-but-unmodified authority file (fresh mtime, identical
//     bytes — what migrate-memories' re-placement does on every run, and
//     what its verify step's rebuilt==tree count depends on) is
//     re-verified by content, its stamp refreshed, and it IS counted as
//     brought current; no index write is needed and retrieval is
//     unchanged.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
)

func stampTestEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := core.Open(dir)
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
	return &env{db: db, store: NewStore(db, dir), admin: admin, user: u.ID}, dir
}

func stampSeed(t *testing.T, e *env, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		entry, err := e.store.Create(CreateRequest{
			Project: "alpha", Type: "experience.debug", Visibility: "project",
			Content: steadyCorpusBody(i),
		}, e.user)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, entry.ID)
	}
	return ids
}

// TestReconcileUnstampedRowsFullPassOnce: rows written before the #129
// stamp existed carry NULL stamps; the first reconcile verifies each by
// content and stamps it — one full pass (every FTS row rewritten) — and
// the next pass, same process or restarted, answers 0.
func TestReconcileUnstampedRowsFullPassOnce(t *testing.T) {
	e, _ := stampTestEnv(t)
	const corpus = 20
	stampSeed(t, e, corpus)
	if _, err := e.db.Exec(`UPDATE memories SET fs_mtime_ns=NULL, fs_size=NULL`); err != nil {
		t.Fatalf("clear stamps: %v", err)
	}
	n1, err := e.store.RebuildIndex()
	if err != nil {
		t.Fatalf("first pass over unstamped rows: %v", err)
	}
	if n1 != corpus {
		t.Fatalf("first pass over %d unstamped rows brought %d current, want %d — "+
			"the pre-#129 database must degrade to the old full pass exactly once", n1, corpus, corpus)
	}
	hits, err := e.store.Search("Tok000003", "", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("retrieval after the stamping pass: %d hits (err %v)", len(hits), err)
	}
	n2, err := e.store.RebuildIndex()
	if err != nil || n2 != 0 {
		t.Fatalf("second pass brought %d current (err %v), want 0 — stamped rows must be skipped", n2, err)
	}
}

// TestReconcileTouchedFileRefreshesStamp: rewriting an authority file
// with IDENTICAL content (fresh mtime, same size) breaks the stamp but
// not the content: the pass re-verifies by sha, refreshes the stamp,
// counts the entry as brought current, and writes no index row. The pass
// after it is incremental again.
func TestReconcileTouchedFileRefreshesStamp(t *testing.T) {
	e, dir := stampTestEnv(t)
	const corpus = 5
	ids := stampSeed(t, e, corpus)
	e7 := ids[1]
	row, err := e.store.load(e7)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	path := filepath.Join(dir, filepath.FromSlash(row.apath), "memory.md")
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read authority: %v", rerr)
	}
	if werr := os.WriteFile(path, raw, 0o640); werr != nil { // identical bytes, fresh mtime
		t.Fatalf("touch authority: %v", werr)
	}
	n, err := e.store.RebuildIndex()
	if err != nil {
		t.Fatalf("reconcile after touch: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconcile after touching one file brought %d current, want 1", n)
	}
	hits, err := e.store.Search("Tok000001", "", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("retrieval after stamp refresh: %d hits (err %v)", len(hits), err)
	}
	n2, err := e.store.RebuildIndex()
	if err != nil || n2 != 0 {
		t.Fatalf("pass after the refresh brought %d current (err %v), want 0", n2, err)
	}
}
