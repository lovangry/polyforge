package core

// seq_test.go: the per-project seq semantics (seq-fix lane, owner
// ruling). The legacy library numbers WIs per project (aiterm#1 and
// defswitch#1 both exist), so:
//
//   - create numbers each project from 1 (was: one global counter);
//   - the import preserves a record's seq scoped to the TARGET project
//     (cross-project same-seq records coexist; a collision WITHIN the
//     target project is refused, naming the project);
//   - slugs always resolved by (project, seq) — now actually unique;
//   - databases created by the global-seq build are rebuilt on Open
//     with per-project renumbering, relative order preserved.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	_ "modernc.org/sqlite"
)

// mustProject creates a second project and returns its id.
func mustProject(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	p, err := env.Admin.CreateProject(CreateProjectRequest{Name: name})
	if err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
	return p.Project.ID
}

// TestCreateSeqPerProject: two projects each number from 1; slugs
// resolve by (project, seq); cross-project same-seq coexist.
func TestCreateSeqPerProject(t *testing.T) {
	env := newEnv(t)
	beta := mustProject(t, env, "beta")

	a1, err := env.WIs.Create(CreateRequest{Project: "alpha", Goal: "alpha one", Reporter: env.UserID})
	if err != nil {
		t.Fatal(err)
	}
	b1, err := env.WIs.Create(CreateRequest{Project: beta, Goal: "beta one", Reporter: env.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Seq != 1 || b1.Seq != 1 {
		t.Fatalf("first seqs = alpha %d, beta %d; both must be 1 (per-project numbering)", a1.Seq, b1.Seq)
	}
	if a1.Slug != "alpha#1" || b1.Slug != "beta#1" {
		t.Fatalf("slugs drifted: %q %q", a1.Slug, b1.Slug)
	}
	a2, err := env.WIs.Create(CreateRequest{Project: "alpha", Goal: "alpha two", Reporter: env.UserID})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := env.WIs.Create(CreateRequest{Project: beta, Goal: "beta two", Reporter: env.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if a2.Seq != 2 || b2.Seq != 2 {
		t.Fatalf("second seqs = alpha %d, beta %d; both must be 2", a2.Seq, b2.Seq)
	}

	// Lookup by (project, seq) resolves each project's OWN #1/#2.
	gotA, err := env.WIs.Get("alpha#1", GetOptions{})
	if err != nil || gotA.ID != a1.ID {
		t.Fatalf("alpha#1 resolved to %v (%v)", gotA, err)
	}
	gotB, err := env.WIs.Get("beta#1", GetOptions{})
	if err != nil || gotB.ID != b1.ID {
		t.Fatalf("beta#1 resolved to %v (%v)", gotB, err)
	}
	// A foreign prefix does not leak: beta#2 is beta's second, never
	// alpha's.
	gotB2, err := env.WIs.Get("beta#2", GetOptions{})
	if err != nil || gotB2.ID != b2.ID {
		t.Fatalf("beta#2 resolved to %v (%v)", gotB2, err)
	}
}

// TestImportSeqPerProject: preserved seqs are scoped to the target
// project; the same seq lands once per project; a within-project
// collision is refused with the project named; the auto seq is the
// project's MAX+1 (not the table's).
func TestImportSeqPerProject(t *testing.T) {
	env := newEnv(t)
	mustProject(t, env, "beta") // the import targets projects by NAME
	m := env.WIs
	closed := "2026-09-28T10:00:00Z"
	base := func(project, id string, seq int64) ImportRequest {
		return ImportRequest{
			Project: project, ID: id, Seq: &seq, Goal: "record " + id,
			Status: api.StatusWrapped, Reporter: env.UserID,
			CreatedAt: closed, UpdatedAt: closed, ClosedAt: strPtr(closed),
		}
	}
	// The legacy shape: aiterm#7 and defswitch#7 both exist.
	if _, err := m.Import(base("alpha", "wi_aiterm00007", 7)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Import(base("beta", "wi_defswitch7", 7)); err != nil {
		t.Fatalf("cross-project same seq must coexist: %v", err)
	}
	// Slugs stay distinct per project.
	gotA, err := m.Get("alpha#7", GetOptions{})
	if err != nil || gotA.ID != "wi_aiterm00007" {
		t.Fatalf("alpha#7: %v (%v)", gotA, err)
	}
	gotB, err := m.Get("beta#7", GetOptions{})
	if err != nil || gotB.ID != "wi_defswitch7" {
		t.Fatalf("beta#7: %v (%v)", gotB, err)
	}

	// The WITHIN-project collision is refused, naming the project.
	_, err = m.Import(base("alpha", "wi_aiterm0000b", 7))
	if !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "IMPORT_SEQ_TAKEN" {
		t.Fatalf("within-project seq collision: %v", err)
	}
	if !strings.Contains(err.Error(), `alpha`) {
		t.Errorf("refusal must name the project: %v", err)
	}
	// A THIRD project still takes seq 7 freely — the refusal above was
	// about alpha's own numbering, nothing global.
	mustProject(t, env, "gamma")
	if _, err := m.Import(base("gamma", "wi_gammaseq007", 7)); err != nil {
		t.Fatalf("seq 7 in a third project: %v", err)
	}

	// Auto seq: the project's MAX+1. beta carries seq 7 (wi_defswitch7);
	// alpha's next AUTO seq must be 8 (alpha's max is 7) — NOT
	// global-max+1, which the seq-7 rows of the other projects would
	// have pushed higher under the old numbering.
	res, err := m.Import(func() ImportRequest {
		r := base("alpha", "wi_autoalpha01", 0)
		r.Seq = nil
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkItem.Seq != 8 {
		t.Fatalf("auto seq = %d, want 8 (per-project MAX+1)", res.WorkItem.Seq)
	}
	// The refusal message also names the project on the overwrite path.
	if _, err := m.Import(func() ImportRequest {
		r := base("beta", "wi_defswitchb", 9)
		r.Overwrite = true
		r.ID = "wi_defswitch7" // exists; overwrite to seq 9 is a move — fine
		return r
	}()); err != nil {
		t.Fatalf("overwrite seq move within project: %v", err)
	}
	// ... but a collision with a DIFFERENT row of the same project is
	// still refused, with the project named (overwrite path; beta's seq 9
	// is held by the moved wi_defswitch7).
	if _, err := m.Import(func() ImportRequest {
		r := base("beta", "wi_autoalpha01", 0)
		r.Goal = "moved into beta"
		r.Seq = strPtrI(9)
		r.Overwrite = true
		return r
	}()); !IsCode(err, api.ErrCoreConflict) || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("overwrite-path collision must refuse naming the project: %v", err)
	}
}

// legacyWorkItemsDDL is the pre-seq-fix shape: seq globally UNIQUE, no
// session columns (the session migration adds those first, then the seq
// rebuild runs).
const legacyWorkItemsDDL = `CREATE TABLE work_items (
  id           TEXT PRIMARY KEY,
  seq          INTEGER NOT NULL UNIQUE,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  goal         TEXT NOT NULL,
  content      TEXT,
  parent_id    TEXT REFERENCES work_items(id),
  status       TEXT NOT NULL DEFAULT 'queued'
               CHECK (status IN ('queued','running','paused','blocked',
                                 'wrapped','failed','canceled')),
  priority     TEXT NOT NULL DEFAULT 'normal'
               CHECK (priority IN ('low','normal','high','urgent')),
  milestone    TEXT,
  labels       TEXT NOT NULL DEFAULT '[]',
  attrs        TEXT NOT NULL DEFAULT '{}',
  reporter     TEXT NOT NULL REFERENCES users(id),
  assignee     TEXT REFERENCES users(id),
  waiting_on   TEXT,
  bundle_name  TEXT,
  bundle_digest TEXT,
  workflow_budget_baseline TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  closed_at    TEXT
);`

// openLegacySeqDB builds a database at the pre-seq-fix shape: the old
// work_items (global UNIQUE seq), two projects, interleaved rows and one
// wi_events child row (the rebuild must carry children over verbatim).
func openLegacySeqDB(t *testing.T, withRows bool) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "aihub.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	for _, ddl := range []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY, display_name TEXT NOT NULL, email TEXT,
		 kind TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'writer', created_at TEXT NOT NULL)`,
		`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT NOT NULL DEFAULT '',
		 repos TEXT NOT NULL DEFAULT '[]', visible INTEGER NOT NULL DEFAULT 1, scenario TEXT,
		 identifier_hash TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		legacyWorkItemsDDL,
		`CREATE TABLE wi_events (
		 id INTEGER PRIMARY KEY AUTOINCREMENT,
		 work_item_id TEXT NOT NULL REFERENCES work_items(id),
		 created_at TEXT NOT NULL, actor TEXT, event_type TEXT NOT NULL,
		 payload TEXT NOT NULL DEFAULT '{}')`,
	} {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("legacy ddl: %v", err)
		}
	}
	for _, s := range []string{
		`INSERT INTO users(id, display_name, email, kind, created_at) VALUES ('u1','u one','','human','t')`,
		`INSERT INTO projects(id, name, identifier_hash, created_at, updated_at)
		 VALUES ('p1','alpha','h','t','t'), ('p2','beta','h','t','t')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("legacy seed: %v", err)
		}
	}
	if withRows {
		// Interleaved global seqs: alpha 1,3 and beta 2,5. Per-project
		// renumbering must preserve each project's relative order:
		// alpha 1→1, 3→2 and beta 2→1, 5→2.
		rows := [][2]any{
			{"wi_alphaold001", int64(1)}, {"wi_betaold0001", int64(2)},
			{"wi_alphaold003", int64(3)}, {"wi_betaold0005", int64(5)},
		}
		for i, r := range rows {
			proj := "p1"
			if strings.HasPrefix(r[0].(string), "wi_beta") {
				proj = "p2"
			}
			if _, err := raw.Exec(`INSERT INTO work_items(id, seq, project_id, goal, status, reporter, created_at, updated_at)
				VALUES (?1,?2,?3,?4,'wrapped','u1',?5,?5)`,
				r[0], r[1], proj, "legacy goal "+r[0].(string), "2026-09-28T0"+string(rune('1'+i))+":00:00Z"); err != nil {
				t.Fatalf("seed row: %v", err)
			}
		}
		if _, err := raw.Exec(`INSERT INTO wi_events(work_item_id, created_at, event_type)
			VALUES ('wi_alphaold001','t','work_item_filed')`); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
	return dir
}

// TestSeqMigrationRebuildsPerProject: a global-seq database renumbers to
// per-project seqs preserving relative order, carries child rows over,
// and re-opens idempotently.
func TestSeqMigrationRebuildsPerProject(t *testing.T) {
	dir := openLegacySeqDB(t, true)
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open (migration): %v", err)
	}
	defer db.Close()

	// The old shape is gone: no UNIQUE index on (seq) alone; a UNIQUE
	// (project_id, seq) index exists.
	needed, err := seqMigrationNeeded(db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if needed {
		t.Fatal("unique-on-seq index still present after migration")
	}
	haveProjectSeq := false
	idxRows, err := db.Query(`SELECT name FROM pragma_index_list('work_items') WHERE "unique"=1 AND origin='u'`)
	if err != nil {
		t.Fatal(err)
	}
	var idxNames []string
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		idxNames = append(idxNames, name)
	}
	idxRows.Close()
	// One-connection pool: inspect each index only after the pragma rows
	// are closed (a nested Query would deadlock the pool).
	for _, name := range idxNames {
		cols, err := indexColumns(db.DB, name)
		if err != nil {
			t.Fatal(err)
		}
		if len(cols) == 2 && cols[0] == "project_id" && cols[1] == "seq" {
			haveProjectSeq = true
		}
	}
	if !haveProjectSeq {
		t.Fatal("UNIQUE(project_id, seq) index missing after migration")
	}

	// Renumbering: per project 1..n, old relative order preserved.
	type row struct {
		id   string
		seq  int64
		goal string
	}
	want := map[string]row{
		"wi_alphaold001": {"wi_alphaold001", 1, "legacy goal wi_alphaold001"},
		"wi_alphaold003": {"wi_alphaold003", 2, "legacy goal wi_alphaold003"},
		"wi_betaold0001": {"wi_betaold0001", 1, "legacy goal wi_betaold0001"},
		"wi_betaold0005": {"wi_betaold0005", 2, "legacy goal wi_betaold0005"},
	}
	got, err := db.Query(`SELECT id, seq, goal FROM work_items ORDER BY project_id, seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	n := 0
	for got.Next() {
		var r row
		if err := got.Scan(&r.id, &r.seq, &r.goal); err != nil {
			t.Fatal(err)
		}
		w, ok := want[r.id]
		if !ok {
			t.Fatalf("unexpected row %s after migration", r.id)
		}
		if r.seq != w.seq || r.goal != w.goal {
			t.Errorf("%s: seq=%d goal=%q, want seq=%d goal=%q", r.id, r.seq, r.goal, w.seq, w.goal)
		}
		n++
	}
	if n != len(want) {
		t.Fatalf("rows after migration = %d, want %d", n, len(want))
	}
	// Child rows carried over verbatim.
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wi_events WHERE work_item_id='wi_alphaold001'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("child events lost: n=%d err=%v", events, err)
	}
	// The state machine works on the rebuilt table (a fresh create takes
	// the project's next seq).
	env := &testEnv{DB: db, Admin: NewAdminManager(db), WIs: NewWIManager(db)}
	if _, err := env.WIs.Create(CreateRequest{Project: "alpha", Goal: "post-migration", Reporter: "u1"}); err != nil {
		t.Fatalf("create on rebuilt table: %v", err)
	}
	var maxSeq int64
	if err := db.QueryRow(`SELECT MAX(seq) FROM work_items WHERE project_id='p1'`).Scan(&maxSeq); err != nil || maxSeq != 3 {
		t.Fatalf("post-migration create seq = %d (%v), want 3", maxSeq, err)
	}

	// Re-open: the migration is a no-op and the seqs are untouched.
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer db2.Close()
	var seqAgain int64
	if err := db2.QueryRow(`SELECT seq FROM work_items WHERE id='wi_alphaold003'`).Scan(&seqAgain); err != nil || seqAgain != 2 {
		t.Fatalf("re-open renumbered: seq=%d err=%v, want 2", seqAgain, err)
	}
}

// TestSeqMigrationEmptyDB: the rebuild is safe on an empty legacy
// database (nothing to copy, indexes rebuilt, creates then number from
// 1 per project).
func TestSeqMigrationEmptyDB(t *testing.T) {
	dir := openLegacySeqDB(t, false)
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open empty legacy db: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d (%v), want 0", n, err)
	}
	// A create on the rebuilt (empty) table takes seq 1.
	m := NewWIManager(db)
	if _, err := m.Create(CreateRequest{Project: "beta", Goal: "first", Reporter: "u1"}); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := db.QueryRow(`SELECT seq FROM work_items LIMIT 1`).Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("first seq on empty migrated db = %d (%v), want 1", seq, err)
	}
}

// TestImportGoalTruncationCJK: a CJK goal of 300 runes (900 bytes)
// imports with a rune-aligned ≤500-byte goal and the full original
// preserved under attrs.legacy_goal (seq-fix lane fix 2). Short goals
// gain no legacy_goal.
func TestImportGoalTruncationCJK(t *testing.T) {
	env := newEnv(t)
	m := env.WIs
	closed := "2026-09-28T10:00:00Z"

	big := strings.Repeat("调", 300) // 300 runes × 3 bytes = 900 bytes
	if len(big) != 900 {
		t.Fatalf("test setup: len(big)=%d", len(big))
	}
	res, err := m.Import(ImportRequest{
		Project: "alpha", ID: "wi_cjk00000001", Goal: big,
		Status: api.StatusWrapped, Reporter: env.UserID,
		CreatedAt: closed, UpdatedAt: closed, ClosedAt: strPtr(closed),
	})
	if err != nil {
		t.Fatalf("CJK goal refused: %v", err)
	}
	wi := res.WorkItem
	if len(wi.Goal) > api.MaxGoalBytes {
		t.Fatalf("goal still %d bytes, want ≤%d", len(wi.Goal), api.MaxGoalBytes)
	}
	if !utf8ValidString(wi.Goal) {
		t.Fatalf("goal not cut at a rune boundary: %q", wi.Goal)
	}
	wantTruncated := api.TruncateUTF8(big, api.MaxGoalBytes)
	if wi.Goal != wantTruncated {
		t.Fatalf("truncated goal drifted: got %d bytes, want the rune-boundary cut (%d bytes)",
			len(wi.Goal), len(wantTruncated))
	}
	legacy, ok := wi.Attrs["legacy_goal"].(string)
	if !ok || legacy != big {
		t.Fatalf("attrs.legacy_goal must carry the FULL original (%d bytes); got %v",
			len(big), wi.Attrs["legacy_goal"])
	}

	// A short goal gains no legacy_goal key.
	res2, err := m.Import(ImportRequest{
		Project: "alpha", ID: "wi_short000001", Goal: "short goal",
		Status: api.StatusWrapped, Reporter: env.UserID,
		CreatedAt: closed, UpdatedAt: closed, ClosedAt: strPtr(closed),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res2.WorkItem.Attrs["legacy_goal"]; ok {
		t.Fatal("short goal sprouted a legacy_goal attr")
	}

	// The idempotent re-run is a skip, not a divergence (the truncation
	// is deterministic).
	again, err := m.Import(ImportRequest{
		Project: "alpha", ID: "wi_cjk00000001", Goal: big,
		Status: api.StatusWrapped, Reporter: env.UserID,
		CreatedAt: closed, UpdatedAt: closed, ClosedAt: strPtr(closed),
	})
	if !IsImportAlreadyExists(err) || again.Outcome != ImportSkipped {
		t.Fatalf("re-import of truncated record: outcome=%s err=%v", again.Outcome, err)
	}

	// The overwrite path truncates the same way.
	res3, err := m.Import(ImportRequest{
		Project: "alpha", ID: "wi_short000001", Goal: big,
		Status: api.StatusFailed, Reporter: env.UserID,
		CreatedAt: closed, UpdatedAt: closed, ClosedAt: strPtr(closed),
		Overwrite: true,
	})
	if err != nil || res3.Outcome != ImportOverwritten {
		t.Fatalf("overwrite with CJK goal: outcome=%s err=%v", res3.Outcome, err)
	}
	if len(res3.WorkItem.Goal) > api.MaxGoalBytes || res3.WorkItem.Attrs["legacy_goal"] != big {
		t.Fatalf("overwrite truncation drifted: %d bytes, legacy=%v",
			len(res3.WorkItem.Goal), res3.WorkItem.Attrs["legacy_goal"])
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}
