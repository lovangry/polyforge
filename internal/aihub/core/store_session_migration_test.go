package core

// store_session_migration_test.go: the v1.5 additive migration — a
// database created by a v1 server (work_items WITHOUT the session
// columns) gains them in place on Open, existing rows defaulting to
// clean_pause=0; a fresh database already carries them via the DDL and
// the migration is a no-op.

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSessionColumnMigrationFromV1Database(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aihub.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// The v1-era work_items shape: no session columns, one live row.
	if _, err := raw.Exec(`CREATE TABLE work_items (
  id           TEXT PRIMARY KEY,
  seq          INTEGER NOT NULL,
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
  closed_at    TEXT,
  UNIQUE (project_id, seq)
);`); err != nil {
		t.Fatalf("v1 table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO work_items(id, seq, project_id, goal, reporter, created_at, updated_at)
		VALUES ('wi_legacyrow', 1, 'p1', 'legacy row', 'u1', 't', 't')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	raw.Close()

	// Open through Core: the DDL is a no-op (table exists), the migration
	// adds the session columns.
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var cleanPause int
	var hash, fp sql.NullString
	err = db.QueryRow(`SELECT session_hash, session_fingerprint, clean_pause FROM work_items WHERE id='wi_legacyrow'`).
		Scan(&hash, &fp, &cleanPause)
	if err != nil {
		t.Fatalf("legacy row unreadable after migration: %v", err)
	}
	if hash.Valid || fp.Valid || cleanPause != 0 {
		t.Fatalf("legacy row session defaults wrong: hash=%v fp=%v clean_pause=%d", hash, fp, cleanPause)
	}
	// A re-open (idempotent migration) must not fail on the now-present
	// columns.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('work_items') WHERE name IN
		('session_hash','session_fingerprint','session_machine','session_owner','session_started_at','clean_pause')`).Scan(&n); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if n != 6 {
		t.Fatalf("session columns = %d, want 6", n)
	}
}
