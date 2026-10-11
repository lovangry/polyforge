package memory

// reconcile_steady_state_test.go (polyforge#129 repro): the startup
// reconcile's steady state. A restarted server with ZERO memory changes
// must reconcile incrementally — the owner ruling pins an mtime/size
// stamp fast path: entries whose stamp matches are skipped (zero reads,
// zero indexing), only changed entries are reindexed, and the whole
// steady-state pass is time-bounded at ≤1s (含全库 stat + 行扫描).
//
// The current implementation instead re-reads every authority file and
// rewrites every FTS row on every call (per-entry transaction in
// rebuildActiveEntry, then again batched in rebuildFTSBatched): an
// unconditional full FTS rebuild on every startup. Observed in
// production as ~14 minutes of unreachable server on every restart (the
// reconcile runs synchronously before ListenAndServe in cmd/aihub/
// main.go) with the "startup memory reconcile: N entries reindexed" log
// always reporting the full library count.
//
// These tests fail until the stamp fast path exists; after the fix they
// are the mechanical regression case (steady-state ≤1s, only changed
// entries reindexed, retrieval unchanged).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/aihub/core"
)

// steadyCorpusBody renders one authority body at the production shape:
// 3718 entries ≈ 95MB → ~25.6KB per entry average; the filler sentence is
// 111 bytes, so 230 repeats ≈ 25.5KB puts each entry on that average.
func steadyCorpusBody(i int) string {
	filler := strings.Repeat(
		"The trigram tokenizer indexes overlapping triplets so substring recall finds mid-word tokens like xyzzyplugh. ",
		230)
	return fmt.Sprintf("startup reconcile corpus entry %06d unique token Tok%06d.\n\n%s", i, i, filler)
}

// startupEnv builds the PRODUCTION layout: one data dir holding both the
// core database and the memory authority tree — main.go passes the same
// -data-dir to core.Open and memory.NewStore (newEnv keeps them apart,
// which would under-report the reconcile's file cost).
func startupEnv(t *testing.T, dir string) *env {
	t.Helper()
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
	return &env{db: db, store: NewStore(db, dir), admin: admin, user: u.ID}
}

// reopenStartupEnv models a process restart: every connection dropped,
// the same data dir reopened fresh (core.Open + NewStore) — exactly the
// sequence main.go runs on every boot. Seeded rows persist in the db.
func reopenStartupEnv(t *testing.T, dir string) *env {
	t.Helper()
	db, err := core.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	var uid string
	if err := db.QueryRow(`SELECT id FROM users WHERE display_name='root'`).Scan(&uid); err != nil {
		t.Fatalf("reopen user: %v", err)
	}
	return &env{db: db, store: NewStore(db, dir), admin: core.NewAdminManager(db), user: uid}
}

// searchOne asserts exactly one hit for a unique token.
func searchOne(t *testing.T, e *env, token string) {
	t.Helper()
	hits, err := e.store.Search(token, "", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search %q after reconcile: %d hits (err %v) — retrieval must be unchanged by the fast path", token, len(hits), err)
	}
}

// TestSteadyStateStartupReconcileSkipsUnchanged: with zero memory changes
// since the last boot, the startup reconcile must reindex nothing (the
// mtime/size stamp fast path); after exactly ONE entry changes, exactly
// that one is reindexed. The current implementation reindexes the whole
// library in both cases — the full-rebuild-on-every-startup failure.
func TestSteadyStateStartupReconcileSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	e := startupEnv(t, dir)
	const corpus = 120
	ids := make([]string, 0, corpus)
	for i := 0; i < corpus; i++ {
		entry, err := e.store.Create(CreateRequest{
			Project: "alpha", Type: "experience.debug", Visibility: "project",
			Content: steadyCorpusBody(i),
		}, e.user)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, entry.ID)
	}

	// Restart #1: zero changes since the corpus was written. The startup
	// reconcile must be a no-op on the index.
	e.db.Close()
	e = reopenStartupEnv(t, dir)
	n1, err := e.store.RebuildIndex()
	if err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	if n1 != 0 {
		t.Fatalf("startup reconcile with ZERO memory changes reindexed %d of %d entries — "+
			"the reconcile is an unconditional full FTS rebuild on every restart "+
			"(polyforge#129: production logs show the full library count on every boot); "+
			"want 0 reindexed via the mtime/size stamp fast path", n1, corpus)
	}
	searchOne(t, e, "Tok000003")

	// Exactly one entry changes — OUT OF BAND, bypassing the store's
	// write path. The change must be out-of-band because an API write
	// (Store.Update) reindexes and stamps its row transactionally (the
	// polyforge#129 owner ruling: the write path records the stamp in the
	// same transaction as the row), so a restart after an API change owes
	// the reconcile nothing — asserting a reindexed count for one would
	// demand exactly the redundant rewrite this WI removes. The out-of-band
	// edit is the drift class the reconcile exists to catch: restart #2
	// must reindex exactly that one entry.
	e7, err := e.store.load(ids[7])
	if err != nil {
		t.Fatalf("load entry 7: %v", err)
	}
	path7 := filepath.Join(e.store.dataDir, filepath.FromSlash(e7.apath), "memory.md")
	raw7, rerr := os.ReadFile(path7)
	if rerr != nil {
		t.Fatalf("read authority 7: %v", rerr)
	}
	updated := strings.Replace(string(raw7), "Tok000007.", "Tok000007 Tok000007v2.", 1)
	if updated == string(raw7) {
		t.Fatalf("out-of-band mutation did not apply")
	}
	if werr := os.WriteFile(path7, []byte(updated), 0o640); werr != nil {
		t.Fatalf("edit authority 7: %v", werr)
	}
	e.db.Close()
	e = reopenStartupEnv(t, dir)
	n2, err := e.store.RebuildIndex()
	if err != nil {
		t.Fatalf("one-change reconcile: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("startup reconcile after exactly ONE memory change reindexed %d of %d entries — "+
			"only the changed entry may be reindexed (polyforge#129 incremental ruling); want 1", n2, corpus)
	}
	searchOne(t, e, "Tok000007v2") // the changed entry is reindexed
	searchOne(t, e, "Tok000003")   // untouched entries stay retrievable
}

// TestSteadyStateStartupReconcileTimeBound: the owner-pinned budget —
// steady-state startup reconcile (zero changes) completes in ≤1s. This
// corpus is 400 entries ≈ 10MB (~10% of the production 3718-entry/95MB
// library, same ~25.6KB per-entry shape); the current implementation
// takes ~9-10s here (~24ms/entry: three full file reads + two trigram FTS
// rewrites per entry) and ~14 minutes at full production scale. After the
// stamp fast path lands the same corpus reconciles in milliseconds.
func TestSteadyStateStartupReconcileTimeBound(t *testing.T) {
	dir := t.TempDir()
	e := startupEnv(t, dir)
	const corpus = 400
	for i := 0; i < corpus; i++ {
		if _, err := e.store.Create(CreateRequest{
			Project: "alpha", Type: "experience.debug", Visibility: "project",
			Content: steadyCorpusBody(i),
		}, e.user); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Restart: zero changes. Measure the startup reconcile.
	e.db.Close()
	e = reopenStartupEnv(t, dir)
	start := time.Now()
	n, err := e.store.RebuildIndex()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("steady-state startup reconcile took %v for %d entries (~%v/entry); "+
			"owner-pinned budget is ≤1s (polyforge#129: ~14 minutes at the production "+
			"3718-entry corpus — the reconcile cost scales with the whole library, not with the change set)",
			elapsed, corpus, elapsed/corpus)
	}
	if n != 0 {
		t.Fatalf("steady-state reconcile reindexed %d of %d entries with zero changes — full rebuild, not the fast path", n, corpus)
	}
}
