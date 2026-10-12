package core

// seq_test.go: the per-project seq semantics (seq-fix lane, owner
// ruling). The legacy library numbers WIs per project (aiterm#1 and
// defswitch#1 both exist), so:
//
//   - create numbers each project from 1 (was: one global counter);
//   - slugs always resolved by (project, seq) — now actually unique;
//   - databases still on the global-seq shape are refused on Open.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// mustProject creates a second project and returns its id.
func mustProject(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	p, err := env.Admin.CreateProject(CreateProjectRequest{Name: name})
	if err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
	return p.ID
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

// TestOpenRefusesGlobalSeqDB: a database still carrying the old global
// UNIQUE(seq) is refused with a clear error (no in-place migration).
func TestOpenRefusesGlobalSeqDB(t *testing.T) {
	for _, withRows := range []bool{true, false} {
		dir := openLegacySeqDB(t, withRows)
		db, err := Open(dir)
		if err == nil {
			db.Close()
			t.Fatal("Open must refuse a global-seq database")
		}
		if !strings.Contains(err.Error(), "globally UNIQUE") {
			t.Fatalf("error does not name the shape problem: %v", err)
		}
	}
}
