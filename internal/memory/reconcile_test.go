package memory

// B5 crash-injection tests (06 §9) and §5.3 reconcile tests.
//
// The crash hook (crashHook in memory.go) fires at every numbered boundary
// of the §5.2 write order. A panic at that boundary unwinds through the
// same defers a hard process death would leave behind (SQLite rolling an
// uncommitted transaction back, mutex release), so the durable state after
// recover() is exactly the crash window the assertions target:
//
//   - create/update: pre-write (no byte written), post-tmp (① done, ②
//     not started), post-commit (② done, ③ not started — the B5 window);
//   - redact: pre-move / post-move (① done, ② not started).
//
// Each test asserts the INV-7f serving behavior in the window (old state
// served, or 404 for the missing-authority windows — never a
// half-committed state) and the convergence path afterwards (read-path
// single-entry reconcile and/or the full §5.3 RebuildIndex).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/core/store"
)

// ---------- crash-injection scaffolding (06 §9 B5) ----------

// crashSentinel is the panic value injected at an armed crash point.
type crashSentinel struct{ stage string }

// injectCrash arms crashHook to panic exactly at the given stage. The
// hook is a package-level variable, so the cleanup always disarms it.
func injectCrash(t *testing.T, stage string) {
	t.Helper()
	t.Cleanup(func() { crashHook = nil })
	crashHook = func(fired string) {
		if fired == stage {
			panic(crashSentinel{stage: stage})
		}
	}
}

// recordStages arms crashHook to only record every stage it observes —
// the §9 B5 item 3 control shape: the hooks must not change the
// observable normal path.
func recordStages(t *testing.T) *[]string {
	t.Helper()
	stages := &[]string{}
	t.Cleanup(func() { crashHook = nil })
	crashHook = func(fired string) { *stages = append(*stages, fired) }
	return stages
}

// expectCrash runs fn and fails the test unless the injected crash fired.
func expectCrash(t *testing.T, stage string, fn func()) {
	t.Helper()
	fired := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(crashSentinel); ok {
					fired = true
					return
				}
				panic(r) // not our sentinel: propagate
			}
		}()
		fn()
	}()
	if !fired {
		t.Fatalf("crash point %q never fired", stage)
	}
}

// ---------- assertions and small query helpers ----------

// want404 fails unless err is a MEM-404 APIError.
func want404(t *testing.T, err error) {
	t.Helper()
	if ae, ok := err.(*APIError); ok && ae.Code == "MEM-404" {
		return
	}
	t.Fatalf("want MEM-404, got %v", err)
}

// countMemRows returns the number of memories rows.
func countMemRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// onlyMemID returns the id of the single memories row of a fresh store.
func onlyMemID(t *testing.T, s *Store) string {
	t.Helper()
	if n := countMemRows(t, s); n != 1 {
		t.Fatalf("want exactly 1 memories row, got %d", n)
	}
	var id string
	if err := s.db.QueryRow(`SELECT id FROM memories`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ftsCount returns the number of memory_fts rows of one mem id.
func ftsCount(t *testing.T, s *Store, memID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE mem_id=?1`, memID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// writeAuthority hand-writes an authority document (frontmatter + body)
// into dir — the §10.1 manual-placement shape the §5.3 backfill consumes.
// Returns the written bytes.
func writeAuthority(t *testing.T, dir string, fm *frontmatter, body string) []byte {
	t.Helper()
	raw, err := buildAuthorityFile(fm, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.md"), raw, 0o640); err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---------- B5 crash-injection tests (06 §9) ----------

// TestB5CreatePreWrite: death before any authority byte — no row, no FTS
// trace (the transaction never opened), no tmp file. The reader-visible
// state is simply "does not exist", and RebuildIndex has nothing to do.
func TestB5CreatePreWrite(t *testing.T) {
	s := newTestStore(t)
	injectCrash(t, "create:pre-write")
	expectCrash(t, "create:pre-write", func() {
		_, _ = s.Create(CreateRequest{
			Project: "p1", Type: "experience.debug", Visibility: "project",
			Content: "never written body",
		}, "usr_worker")
	})

	if n := countMemRows(t, s); n != 0 {
		t.Fatalf("row committed before any write: %d rows", n)
	}
	for _, pat := range []string{"memory.md", "memory.md.tmp"} {
		m, _ := filepath.Glob(filepath.Join(s.dataDir, "memory", "pj1", "*", pat))
		if len(m) != 0 {
			t.Fatalf("%s survived a pre-write crash: %v", pat, m)
		}
	}
	if n, err := s.RebuildIndex(); err != nil || n != 0 {
		t.Fatalf("rebuild after pre-write crash: n=%d err=%v", n, err)
	}
	hits, err := s.Search("never", "", nil, 50, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("search must be empty, got %v (err %v)", hits, err)
	}
}

// TestB5CreatePostTmp: ① done, ② not started — the tmp file is inert
// crash residue (the authority path was never touched), so no row exists
// and nothing is servable. RebuildIndex sweeps the tmp and that is the
// whole convergence.
func TestB5CreatePostTmp(t *testing.T) {
	s := newTestStore(t)
	injectCrash(t, "create:post-tmp")
	expectCrash(t, "create:post-tmp", func() {
		_, _ = s.Create(CreateRequest{
			Project: "p1", Type: "experience.debug", Visibility: "project",
			Content: "half written body",
		}, "usr_worker")
	})

	if n := countMemRows(t, s); n != 0 {
		t.Fatalf("row committed before ②: %d rows", n)
	}
	tmps, _ := filepath.Glob(filepath.Join(s.dataDir, "memory", "pj1", "*", "memory.md.tmp"))
	if len(tmps) != 1 {
		t.Fatalf("want exactly 1 crash-residue tmp, got %v", tmps)
	}
	if n, err := s.RebuildIndex(); err != nil || n != 0 {
		t.Fatalf("rebuild: n=%d err=%v", n, err)
	}
	tmps, _ = filepath.Glob(filepath.Join(s.dataDir, "memory", "pj1", "*", "memory.md.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("tmp not swept by the §5.3 step-1 walk: %v", tmps)
	}
	if n := countMemRows(t, s); n != 0 {
		t.Fatalf("rebuild invented a row: %d", n)
	}
}

// TestB5CreatePostCommit: ② done, ③ not started — the B5 window. The row
// and FTS are committed but the authority file does not exist, so the
// entry is inconsistent (INV-7f): no gated read path serves it. Search
// excludes the candidate and its inline single-entry reconcile deletes
// the orphan row together with its FTS row and sweeps the tmp — the whole
// create rolls back (06 §5.3 step 6).
func TestB5CreatePostCommit(t *testing.T) {
	s := newTestStore(t)
	injectCrash(t, "create:post-commit")
	expectCrash(t, "create:post-commit", func() {
		_, _ = s.Create(CreateRequest{
			Project: "p1", Type: "experience.debug", Visibility: "project",
			Content: "postcommit crashwindow body",
		}, "usr_worker")
	})

	id := onlyMemID(t, s)
	raw, err := s.load(id)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Status != "active" || s.entryConsistent(raw) {
		t.Fatalf("post-commit window not detected: status=%s consistent=%v", raw.Status, s.entryConsistent(raw))
	}
	if n := ftsCount(t, s, id); n != 1 {
		t.Fatalf("FTS row missing at ②-done: %d", n)
	}
	tmps, _ := filepath.Glob(filepath.Join(s.dataDir, "memory", "pj1", "*", "memory.md.tmp"))
	if len(tmps) != 1 {
		t.Fatalf("want the ① tmp residue in the window, got %v", tmps)
	}

	// INV-7f: the inconsistent candidate is excluded from this result set
	// (the single-entry reconcile is dispatched inside serveConsistent).
	hits, serr := s.Search("postcommit", "", nil, 50, nil)
	if serr != nil || len(hits) != 0 {
		t.Fatalf("inconsistent entry served from search: %v (err %v)", hits, serr)
	}
	// The inline reconcile rolled the whole create back.
	want404(t, func() error { _, err := s.load(id); return err }())
	if n := ftsCount(t, s, id); n != 0 {
		t.Fatalf("FTS row survived the orphan rollback: %d", n)
	}
	tmps, _ = filepath.Glob(filepath.Join(s.dataDir, "memory", "pj1", "*", "memory.md.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("tmp not swept by the rollback: %v", tmps)
	}
	_, err = s.Get(id)
	want404(t, err)
	if n, rerr := s.RebuildIndex(); rerr != nil || n != 0 {
		t.Fatalf("rebuild after convergence: n=%d err=%v", n, rerr)
	}
}

// TestB5UpdatePreWrite: death before any byte, including the history
// snapshot — nothing changed at all: the old state keeps serving.
func TestB5UpdatePreWrite(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "old body stays")
	old := mustContent(t, s, e.ID)
	oldRow, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}

	injectCrash(t, "update:pre-write")
	expectCrash(t, "update:pre-write", func() {
		_, _ = s.Update(e.ID, UpdateRequest{Content: "new body never", Title: strp("ghost")})
	})

	if got := mustContent(t, s, e.ID); string(got) != string(old) {
		t.Fatal("authority changed before any byte was written")
	}
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ContentSHA != oldRow.ContentSHA || row.UpdatedAt != oldRow.UpdatedAt {
		t.Fatalf("row drifted on a pre-write crash: sha=%s updated_at=%s", row.ContentSHA, row.UpdatedAt)
	}
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	if _, terr := os.Stat(filepath.Join(dir, "memory.md.tmp")); !os.IsNotExist(terr) {
		t.Fatal("tmp file survived a pre-write crash")
	}
	if _, herr := os.Stat(filepath.Join(dir, "history")); !os.IsNotExist(herr) {
		t.Fatal("history snapshot taken before the pre-write crash point")
	}
	hits, _ := s.Search("stays", "", nil, 50, nil)
	if len(hits) != 1 {
		t.Fatalf("old body lost from the index: %v", hits)
	}
}

// TestB5UpdatePostTmp: ① done, ② not started — the row still matches the
// untouched authority file, so the OLD content keeps serving. The residue
// (the new tmp + an orphan history snapshot of the still-current
// authority) is swept by RebuildIndex without touching the served state.
func TestB5UpdatePostTmp(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "old body stays")
	old := mustContent(t, s, e.ID)

	injectCrash(t, "update:post-tmp")
	expectCrash(t, "update:post-tmp", func() {
		_, _ = s.Update(e.ID, UpdateRequest{Content: "new body never"})
	})

	// The window serves the old state normally (row == file).
	if got := mustContent(t, s, e.ID); string(got) != string(old) {
		t.Fatal("served content changed while the row still matches the file")
	}
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	if _, terr := os.Stat(filepath.Join(dir, "memory.md.tmp")); terr != nil {
		t.Fatal("want the ① tmp residue in the window")
	}
	hist, _ := filepath.Glob(filepath.Join(dir, "history", "*.md"))
	if len(hist) != 1 {
		t.Fatalf("want the outgoing-version snapshot in the window, got %v", hist)
	}

	n, err := s.RebuildIndex()
	if err != nil || n != 1 {
		t.Fatalf("rebuild: n=%d err=%v", n, err)
	}
	if _, terr := os.Stat(filepath.Join(dir, "memory.md.tmp")); !os.IsNotExist(terr) {
		t.Fatal("tmp not swept by §5.3 step 1")
	}
	hist, _ = filepath.Glob(filepath.Join(dir, "history", "*.md"))
	if len(hist) != 0 {
		t.Fatalf("orphan history snapshot (== current authority) not swept: %v", hist)
	}
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ContentSHA != shaHex(old) {
		t.Fatal("row drifted from the surviving authority")
	}
	if got := mustContent(t, s, e.ID); string(got) != string(old) {
		t.Fatal("served content changed across the rebuild")
	}
}

// TestB5UpdatePostCommit: ② done, ③ not started — the B5 window. The row
// carries the NEW metadata while the authority file still holds the OLD
// content. INV-7f excludes the entry from the list-shaped reads, and the
// single-entry reconcile rolls the whole PATCH back to the file: old
// content, old metadata, FTS rebuilt from the old body, tmp and orphan
// history snapshot swept.
func TestB5UpdatePostCommit(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "old body baseline")
	old := mustContent(t, s, e.ID)
	oldRow, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}

	injectCrash(t, "update:post-commit")
	expectCrash(t, "update:post-commit", func() {
		_, _ = s.Update(e.ID, UpdateRequest{Content: "---\ntitle: ghost\n---\nnew body patchunique"})
	})

	// The window: committed metadata is NEW, the file is OLD.
	raw, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.ContentSHA == oldRow.ContentSHA {
		t.Fatal("row not committed at the post-commit crash point")
	}
	if s.entryConsistent(raw) {
		t.Fatal("the ②→③ window must be inconsistent (INV-7f)")
	}
	// INV-7f: List excludes the entry from THIS result set (and the
	// exclusion dispatches the single-entry reconcile).
	lst, lerr := s.List("pj1", nil)
	if lerr != nil || len(lst) != 0 {
		t.Fatalf("half-committed entry served by list: %v (err %v)", lst, lerr)
	}
	// The reconcile rolled the PATCH back to the file's authority.
	got := mustContent(t, s, e.ID)
	if string(got) != string(old) {
		t.Fatal("rollback did not restore the old authority content")
	}
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ContentSHA != oldRow.ContentSHA || row.UpdatedAt != oldRow.UpdatedAt {
		t.Fatalf("rollback incomplete: sha=%s updated_at=%s", row.ContentSHA, row.UpdatedAt)
	}
	if row.Title == nil || *row.Title != "t" {
		t.Fatalf("title not rolled back to the file: %v", row.Title)
	}
	// FTS follows the rollback: old token hits, new token misses.
	hits, _ := s.Search("baseline", "", nil, 50, nil)
	if len(hits) != 1 {
		t.Fatalf("old body lost from the index after rollback: %v", hits)
	}
	hits, _ = s.Search("patchunique", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("new body still indexed after rollback: %v", hits)
	}
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	if _, terr := os.Stat(filepath.Join(dir, "memory.md.tmp")); !os.IsNotExist(terr) {
		t.Fatal("tmp not swept by the rollback reconcile")
	}
	hist, _ := filepath.Glob(filepath.Join(dir, "history", "*.md"))
	if len(hist) != 0 {
		t.Fatalf("orphan history snapshot (== current authority) not swept: %v", hist)
	}
}

// TestB5RedactPreMove: death before ① — nothing moved, the entry is
// untouched and keeps serving.
func TestB5RedactPreMove(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "redact window")

	injectCrash(t, "redact:pre-move")
	expectCrash(t, "redact:pre-move", func() { _ = s.Redact(e.ID) })

	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "active" {
		t.Fatalf("status drifted on a pre-move crash: %s", row.Status)
	}
	if _, gerr := s.Get(e.ID); gerr != nil {
		t.Fatalf("entry must keep serving: %v", gerr)
	}
	if _, serr := os.Stat(filepath.Join(s.dataDir, "memory", "pj1", e.ID, "memory.md")); serr != nil {
		t.Fatal("authority file missing after a pre-move crash")
	}
	if _, rerr := os.Stat(filepath.Join(s.dataDir, "memory", "pj1", "_redacted", e.ID)); !os.IsNotExist(rerr) {
		t.Fatal("nothing may move before the redact ① crash point")
	}
}

// TestB5RedactPostMove: ① done, ② not started — the file location IS the
// authoritative state, so no gated read serves the half-committed window:
// Get returns MEM-404 and its single-entry reconcile forward-completes
// the redact (status='redacted' + FTS delete), never a rollback.
func TestB5RedactPostMove(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "redact window target")

	injectCrash(t, "redact:post-move")
	expectCrash(t, "redact:post-move", func() { _ = s.Redact(e.ID) })

	// The window: file already in _redacted/, row still active.
	raw, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Status != "active" || s.entryConsistent(raw) {
		t.Fatalf("window not detected: status=%s consistent=%v", raw.Status, s.entryConsistent(raw))
	}
	if _, serr := os.Stat(filepath.Join(s.dataDir, "memory", "pj1", "_redacted", e.ID, "memory.md")); serr != nil {
		t.Fatal("want the moved authority in _redacted/ at the post-move crash point")
	}
	// INV-7f: the half-committed state is never served; Get 404s and the
	// inline reconcile forward-completes the redact.
	_, gerr := s.Get(e.ID)
	want404(t, gerr)
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "redacted" {
		t.Fatalf("redact not forward-completed from the file position: %s", row.Status)
	}
	if n := ftsCount(t, s, e.ID); n != 0 {
		t.Fatalf("FTS row survived the forward completion: %d", n)
	}
	hits, serr := s.Search("target", "", nil, 50, nil)
	if serr != nil || len(hits) != 0 {
		t.Fatalf("redacted entry still searchable: %v (err %v)", hits, serr)
	}
	if n, rerr := s.RebuildIndex(); rerr != nil || n != 0 {
		t.Fatalf("rebuild must be idempotent here: n=%d err=%v", n, rerr)
	}
}

// TestB5HooksDoNotAlterNormalPath is the §9 B5 item 3 control: with the
// hook only RECORDING, the full write path runs unchanged — every stage
// fires exactly once, in the §5.2 order, and the observable end state is
// fully consistent.
func TestB5HooksDoNotAlterNormalPath(t *testing.T) {
	s := newTestStore(t)
	stages := recordStages(t)

	e := mustCreate(t, s, "control body one")
	if _, err := s.Update(e.ID, UpdateRequest{Content: "control body two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HTML(e.ID); err != nil {
		t.Fatal(err)
	}
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !s.entryConsistent(row) || row.smsSHA.String != row.ContentSHA {
		t.Fatal("normal path left an inconsistent or stale-derived entry")
	}
	if err := s.Redact(e.ID); err != nil {
		t.Fatal(err)
	}
	_, gerr := s.Get(e.ID)
	want404(t, gerr)

	want := []string{
		"create:pre-write", "create:post-tmp", "create:post-commit",
		"update:pre-write", "update:post-tmp", "update:post-commit",
		"redact:pre-move", "redact:post-move",
	}
	if len(*stages) != len(want) {
		t.Fatalf("stage sequence %v != %v", *stages, want)
	}
	for i := range want {
		if (*stages)[i] != want[i] {
			t.Fatalf("stage sequence %v != %v", *stages, want)
		}
	}
}

// ---------- §5.3 reconcile tests ----------

// TestRebuildSweepsTransients pins §5.3 step 1: the walk removes every
// write-order transient — tmp files anywhere in the memory tree (even in
// a directory with no row) and the crash-residue history snapshots (bytes
// equal to the CURRENT authority) — while a snapshot of a genuinely
// superseded version survives and the served entry is untouched.
func TestRebuildSweepsTransients(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "sweep target body")
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	auth, err := os.ReadFile(filepath.Join(dir, "memory.md"))
	if err != nil {
		t.Fatal(err)
	}

	// Crash residue: tmp files and an orphan history snapshot.
	if err := os.WriteFile(filepath.Join(dir, "memory.md.tmp"), []byte("junk md tmp"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.html.tmp"), []byte("junk html tmp"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history", "orphan.md"), auth, 0o640); err != nil {
		t.Fatal(err)
	}
	// A snapshot of a genuinely superseded version: NOT residue.
	if err := os.WriteFile(filepath.Join(dir, "history", "keep.md"), []byte("older version bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Residue in a directory that has no row at all.
	ghost := filepath.Join(s.dataDir, "memory", "pj1", "mem_ghost")
	if err := os.MkdirAll(ghost, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ghost, "memory.md.tmp"), []byte("junk ghost"), 0o640); err != nil {
		t.Fatal(err)
	}

	n, err := s.RebuildIndex()
	if err != nil || n != 1 {
		t.Fatalf("rebuild: n=%d err=%v", n, err)
	}
	for _, p := range []string{
		filepath.Join(dir, "memory.md.tmp"),
		filepath.Join(dir, "memory.html.tmp"),
		filepath.Join(ghost, "memory.md.tmp"),
		filepath.Join(dir, "history", "orphan.md"),
	} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Fatalf("transient not swept: %s", p)
		}
	}
	if _, kerr := os.Stat(filepath.Join(dir, "history", "keep.md")); kerr != nil {
		t.Fatalf("legitimate history snapshot swept: %v", kerr)
	}
	if got := mustContent(t, s, e.ID); string(got) != string(auth) {
		t.Fatal("sweep touched the served authority")
	}
	hits, _ := s.Search("sweep", "", nil, 50, nil)
	if len(hits) != 1 {
		t.Fatalf("entry lost from the index across the sweep: %v", hits)
	}
}

// TestRebuildBackfillsFileWithoutRow pins §5.3 step 3 branch 1 and step 5:
// a hand-planted active-tree file (06 §10.1) is backfilled as a fully
// servable, searchable row; migrated redacted stock under _redacted/ is
// backfilled with status='redacted' — no FTS row, never served, never
// searchable.
func TestRebuildBackfillsFileWithoutRow(t *testing.T) {
	s := newTestStore(t)

	fm := &frontmatter{ID: "mem_planted", Type: "experience.rule", OwnerUserID: "usr_worker",
		Title: strp("planted"), Visibility: "project", ProjectID: "pj1",
		Tags: []string{}, Related: []string{}, CreatedAt: store.Now(), UpdatedAt: store.Now()}
	raw := writeAuthority(t, filepath.Join(s.dataDir, "memory", "pj1", "mem_planted"), fm, "planted body trigramword")

	fm2 := &frontmatter{ID: "mem_oldred", Type: "experience.rule", OwnerUserID: "usr_worker",
		Title: strp("oldred"), Visibility: "project", ProjectID: "pj1",
		Tags: []string{}, Related: []string{}, CreatedAt: store.Now(), UpdatedAt: store.Now()}
	writeAuthority(t, filepath.Join(s.dataDir, "memory", "pj1", "_redacted", "mem_oldred"), fm2, "redactedbody hidden prose")

	n, err := s.RebuildIndex()
	if err != nil || n != 1 { // only the active-tree file counts as indexed
		t.Fatalf("rebuild: n=%d err=%v", n, err)
	}
	// Active stock: backfilled, served, searchable.
	got, gerr := s.Get("mem_planted")
	if gerr != nil {
		t.Fatalf("planted file not backfilled: %v", gerr)
	}
	if got.ContentSHA != shaHex(raw) {
		t.Fatal("backfilled content_sha does not match the file bytes")
	}
	hits, _ := s.Search("planted", "", nil, 50, nil)
	if len(hits) != 1 {
		t.Fatalf("planted file not searchable after backfill: %v", hits)
	}
	// Redacted stock: backfilled terminal, invisible to every read path.
	row, lerr := s.load("mem_oldred")
	if lerr != nil || row.Status != "redacted" {
		t.Fatalf("redacted stock not backfilled redacted: %v %s", lerr, row.Status)
	}
	if c := ftsCount(t, s, "mem_oldred"); c != 0 {
		t.Fatalf("redacted stock must never be indexed: %d", c)
	}
	_, gerr = s.Get("mem_oldred")
	want404(t, gerr)
	hits, _ = s.Search("redactedbody", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("redacted stock searchable: %v", hits)
	}
}

// TestRebuildDeletesOrphanRow pins §5.3 step 6 for the create ②→③ crash
// window: an active row whose authority file exists nowhere dies together
// with its FTS row, and the crash-residue tmp is swept.
func TestRebuildDeletesOrphanRow(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "orphan row body")
	dir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)

	// Simulate the crash residue: row committed, file never placed.
	if err := os.Remove(filepath.Join(dir, "memory.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.md.tmp"), []byte("crash residue"), 0o640); err != nil {
		t.Fatal(err)
	}

	n, err := s.RebuildIndex()
	if err != nil || n != 0 {
		t.Fatalf("rebuild: n=%d err=%v", n, err)
	}
	want404(t, func() error { _, err := s.load(e.ID); return err }())
	if c := ftsCount(t, s, e.ID); c != 0 {
		t.Fatalf("FTS row survived the orphan deletion: %d", c)
	}
	if _, terr := os.Stat(filepath.Join(dir, "memory.md.tmp")); !os.IsNotExist(terr) {
		t.Fatal("crash-residue tmp not swept")
	}
	hits, _ := s.Search("orphan", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("orphan row still searchable: %v", hits)
	}
}

// TestRebuildForwardCompletesRedact pins §5.3 step 5: a directory already
// sitting in _redacted/ with the row still active is a redact that crashed
// between ① and ② — the file location is authoritative, so the row is
// forward-completed to 'redacted' with the FTS row removed.
func TestRebuildForwardCompletesRedact(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "forward complete me")
	src := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	dst := filepath.Join(s.dataDir, "memory", "pj1", "_redacted", e.ID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}

	raw, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Status != "active" {
		t.Fatalf("precondition: row must still be active, got %s", raw.Status)
	}
	n, err := s.RebuildIndex()
	if err != nil || n != 0 {
		t.Fatalf("rebuild: n=%d err=%v", n, err)
	}
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "redacted" {
		t.Fatalf("redact not forward-completed: %s", row.Status)
	}
	if c := ftsCount(t, s, e.ID); c != 0 {
		t.Fatalf("FTS row survived the forward completion: %d", c)
	}
	_, gerr := s.Get(e.ID)
	want404(t, gerr)
	hits, _ := s.Search("forward", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("forward-completed redact still searchable: %v", hits)
	}
}

// TestRebuildOutOfBandEditFollowsFile pins the INV-7c direction inside
// §5.3 step 3: when the row and a hand-edited authority file disagree,
// the FILE wins — every metadata column, the FTS body and the derived
// pair all follow the file.
func TestRebuildOutOfBandEditFollowsFile(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "first body original")
	row, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	fm := &frontmatter{ID: e.ID, Type: row.Type, OwnerUserID: row.OwnerUserID,
		Title: strp("hand-edited"), Visibility: "team", WorkItemID: row.WorkItemID,
		ProjectID: row.ProjectID, Tags: []string{"fresh"}, Related: row.Related,
		CreatedAt: row.CreatedAt, UpdatedAt: store.Now()}
	raw := writeAuthority(t, filepath.Join(s.dataDir, "memory", "pj1", e.ID), fm, "second body edited")

	if n, rerr := s.RebuildIndex(); rerr != nil || n != 1 {
		t.Fatalf("rebuild: n=%d err=%v", n, rerr)
	}
	got, gerr := s.Get(e.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.ContentSHA != shaHex(raw) {
		t.Fatal("row did not follow the out-of-band edited file")
	}
	if got.Title == nil || *got.Title != "hand-edited" {
		t.Fatalf("title did not follow the file: %v", got.Title)
	}
	if got.Visibility != "team" || len(got.Tags) != 1 || got.Tags[0] != "fresh" {
		t.Fatalf("visibility/tags did not follow the file: %s %v", got.Visibility, got.Tags)
	}
	if got.UpdatedAt != fm.UpdatedAt {
		t.Fatalf("updated_at did not follow the file: %s != %s", got.UpdatedAt, fm.UpdatedAt)
	}
	hits, _ := s.Search("edited", "", nil, 50, nil)
	if len(hits) != 1 {
		t.Fatalf("FTS did not follow the file: %v", hits)
	}
	hits, _ = s.Search("original", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("stale body still indexed: %v", hits)
	}
	// The derived pair is re-derived for the changed authority (INV-7d).
	after, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.smsSHA.Valid || after.smsSHA.String != shaHex(raw) {
		t.Fatal("source_markdown_sha not re-derived after the file won")
	}
}

// TestRebuildPureFTSLoss pins the 06 §9 仅FTS丢失专项: when the row and
// the authority file are fully consistent but the FTS row is gone, the
// §5.3 step-4 unconditional per-entry rebuild restores it WITHOUT
// touching the memories row (no re-derivation, updated_at unchanged).
func TestRebuildPureFTSLoss(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "purely indexed body")
	before, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, derr := s.db.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, e.ID); derr != nil {
		t.Fatal(derr)
	}
	hits, _ := s.Search("purely", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("precondition: search must miss without the FTS row: %v", hits)
	}

	n, rerr := s.RebuildIndex()
	if rerr != nil || n != 1 {
		t.Fatalf("rebuild: n=%d err=%v", n, rerr)
	}
	hits, _ = s.Search("purely", "", nil, 50, nil)
	if len(hits) != 1 {
		t.Fatalf("pure FTS loss not repaired: %v", hits)
	}
	after, err := s.load(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatal("memories row rewritten on a pure FTS repair")
	}
	if after.smsSHA != before.smsSHA || after.hSHA != before.hSHA {
		t.Fatal("derived digests rewritten on a pure FTS repair")
	}
}

// TestRebuildSkipsInvalidFrontmatter pins §5.3 step 3: an active-tree file
// whose frontmatter lacks required fields is skipped — it never becomes
// an illegal row and is never indexed.
func TestRebuildSkipsInvalidFrontmatter(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.dataDir, "memory", "pj1", "mem_bad")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.md"), []byte("---\nid: mem_bad\n---\nbody"), 0o640); err != nil {
		t.Fatal(err)
	}

	n, err := s.RebuildIndex()
	if err != nil || n != 0 {
		t.Fatalf("invalid frontmatter produced a rebuild result: n=%d err=%v", n, err)
	}
	want404(t, func() error { _, err := s.load("mem_bad"); return err }())
	hits, _ := s.Search("body", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("invalid frontmatter became searchable: %v", hits)
	}
}

// TestRebuildRedactedRowNotResurrected pins the recorded inference behind
// §5.3 step 3: a copy of the authority file planted back into the active
// tree of an already-redacted entry must NOT resurrect the terminal row —
// redact is 终态不可写 (06 §5.4), so the file is ignored, never indexed,
// and every gated read keeps answering MEM-404.
func TestRebuildRedactedRowNotResurrected(t *testing.T) {
	s := newTestStore(t)
	e := mustCreate(t, s, "resurrection guard body")
	if err := s.Redact(e.ID); err != nil {
		t.Fatal(err)
	}
	redacted, err := os.ReadFile(filepath.Join(s.dataDir, "memory", "pj1", "_redacted", e.ID, "memory.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Plant the redacted bytes back into the active tree.
	activeDir := filepath.Join(s.dataDir, "memory", "pj1", e.ID)
	if err := os.MkdirAll(activeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "memory.md"), redacted, 0o640); err != nil {
		t.Fatal(err)
	}

	n, rerr := s.RebuildIndex()
	if rerr != nil || n != 0 {
		t.Fatalf("redacted row resurrected into the index: n=%d err=%v", n, rerr)
	}
	row, lerr := s.load(e.ID)
	if lerr != nil || row.Status != "redacted" {
		t.Fatalf("redacted row resurrected: %v %s", lerr, row.Status)
	}
	if c := ftsCount(t, s, e.ID); c != 0 {
		t.Fatalf("resurrected file indexed: %d", c)
	}
	hits, _ := s.Search("resurrection", "", nil, 50, nil)
	if len(hits) != 0 {
		t.Fatalf("resurrected file searchable: %v", hits)
	}
	_, gerr := s.Get(e.ID)
	want404(t, gerr)
}
