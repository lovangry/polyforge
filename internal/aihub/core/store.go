// Package core is the v1 AIHub persistent layer: SQLite store (single
// writer, WAL), work-item state machine, claim/generation arbitration with
// attempt-scoped push credentials, the ready queue and the admin face.
//
// Design authority: docs/design/polyforge-vnext/v1-architecture-spec.md
// (rulings C1–C3), v1-spec-full.md §六, and the v0 business-semantic
// baseline in 02-aihub-core.md where v1 has not superseded it. The v0
// revision chain (capsule_revisions / capsule_heads / SYNC-*) is deleted:
// git IS the revision chain; aihub serves one bare repo per WI.
package core

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB is the Core database handle. All writes go through one connection
// (single writer); reads are concurrent under WAL.
type DB struct {
	*sql.DB
}

// DDL is the v1.5 Core schema. First boot installs it as-is; the one
// additive migration (the session columns on work_items) runs in Open for
// pre-v1.5 databases. Compared to v0: capsule revision tables and the
// service-secret machinery are gone; ownership lives ON the work_items
// row itself (the v1.5 session model — session_hash/session_fingerprint/
// clean_pause; the v1 owner_generations table was deleted with the model
// it served).
const DDL = `
CREATE TABLE IF NOT EXISTS core_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id           TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  email        TEXT,
  kind         TEXT NOT NULL CHECK (kind IN ('human','machine')),
  role         TEXT NOT NULL DEFAULT 'writer' CHECK (role IN ('admin','writer')),
  created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id),
  project_id  TEXT REFERENCES projects(id),
  name        TEXT NOT NULL DEFAULT '',
  key_hash    TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  revoked_at  TEXT
);

CREATE TABLE IF NOT EXISTS machines (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id),
  runtime_version TEXT,
  last_heartbeat_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  repos       TEXT NOT NULL DEFAULT '[]',
  visible     INTEGER NOT NULL DEFAULT 1,
  scenario    TEXT,
  identifier_hash TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id TEXT NOT NULL REFERENCES projects(id),
  user_id    TEXT NOT NULL REFERENCES users(id),
  role       TEXT NOT NULL CHECK (role IN ('viewer','writer','maintainer')),
  created_at TEXT NOT NULL,
  PRIMARY KEY (project_id, user_id)
);

CREATE TABLE IF NOT EXISTS work_items (
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
  closed_at    TEXT,
  -- v1.5 session model (v1.5-session-model.md / v1.5-api-contract.md):
  -- the ownership epoch lives ON the row. session_hash is
  -- hex(sha256(session_id)) — the SECRET itself never lands here;
  -- session_fingerprint is its 16-hex public prefix (B3 split).
  -- clean_pause marks an OPERATOR pause (M3): a restart must not
  -- auto-resume it, unlike a daemon-shutdown pause (0).
  session_hash        TEXT,
  session_fingerprint TEXT,
  session_machine     TEXT,
  session_owner       TEXT,
  session_started_at  TEXT,
  clean_pause         INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_wi_status     ON work_items(status, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_wi_assignee   ON work_items(assignee) WHERE status IN ('running','paused','blocked');
CREATE INDEX IF NOT EXISTS idx_wi_waiting_on ON work_items(waiting_on) WHERE waiting_on IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wi_parent     ON work_items(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wi_milestone  ON work_items(project_id, milestone) WHERE milestone IS NOT NULL;

CREATE TABLE IF NOT EXISTS wi_dependencies (
  blocking_wi_id TEXT NOT NULL REFERENCES work_items(id),
  blocked_wi_id  TEXT NOT NULL REFERENCES work_items(id),
  kind           TEXT NOT NULL CHECK (kind IN ('blocks','supersedes','related')),
  created_at     TEXT NOT NULL,
  PRIMARY KEY (blocking_wi_id, blocked_wi_id, kind),
  CHECK (blocking_wi_id <> blocked_wi_id)
);

CREATE TABLE IF NOT EXISTS claim_idempotency (
  idempotency_key TEXT PRIMARY KEY,
  work_item_id    TEXT NOT NULL REFERENCES work_items(id),
  caller_identity TEXT NOT NULL,
  response_json   TEXT NOT NULL,
  created_at      TEXT NOT NULL
);

-- M5 service-secret epoch ledger (02 §4.10/§6.6). Session-model pushes
-- are fenced by the per-WI session hash on work_items, so no validation
-- path consumes a shared service secret; the ledger exists for the
-- rotate-secret wire contract and its audit trail.
CREATE TABLE IF NOT EXISTS service_secret_epochs (
  epoch       INTEGER PRIMARY KEY,
  mode        TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  retired_at  TEXT,
  rotated_by  TEXT
);

CREATE TABLE IF NOT EXISTS wi_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  created_at   TEXT NOT NULL,
  actor        TEXT REFERENCES users(id),
  event_type   TEXT NOT NULL,
  payload      TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_wi ON wi_events(work_item_id, id);

CREATE TABLE IF NOT EXISTS memories (
  id           TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  owner_user_id TEXT NOT NULL REFERENCES users(id),
  work_item_id TEXT REFERENCES work_items(id),
  type         TEXT NOT NULL,
  title        TEXT,
  visibility   TEXT NOT NULL,
  authority_path TEXT NOT NULL,
  content_sha  TEXT NOT NULL,
  content_len  INTEGER NOT NULL,
  tags         TEXT NOT NULL DEFAULT '[]',
  related      TEXT NOT NULL DEFAULT '[]',
  idempotency_key TEXT UNIQUE,
  status       TEXT NOT NULL DEFAULT 'active',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
  mem_id UNINDEXED, body,
  tokenize='trigram'
);
`

// Open opens (creating if needed) the Core database at
// <data_dir>/aihub.db and installs the schema.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("core: create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "aihub.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("core: open: %w", err)
	}
	// Single writer: one connection serializes every write; WAL still
	// serves concurrent external readers on the same file.
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(DDL); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("core: schema: %w", err)
	}
	// v1.5 additive migration: a database created by a v1 server gains
	// the session-model columns in place (nothing is ever dropped here —
	// the post-merge cleanup commit owns the deletions).
	if err := migrateSessions(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("core: migrate sessions: %w", err)
	}
	return &DB{sqlDB}, nil
}

// sessionColumns are the v1.5 work_items additions (schema section in the
// DDL above). Fresh installs get them from the DDL; existing v1 databases
// get them from migrateSessions' guarded ALTER TABLE.
var sessionColumns = []struct{ name, decl string }{
	{"session_hash", "TEXT"},
	{"session_fingerprint", "TEXT"},
	{"session_machine", "TEXT"},
	{"session_owner", "TEXT"},
	{"session_started_at", "TEXT"},
	{"clean_pause", "INTEGER NOT NULL DEFAULT 0"},
}

// migrateSessions adds the v1.5 session columns when missing. ALTER TABLE
// ADD COLUMN cannot add an un-DEFAULTed NOT NULL column, so clean_pause
// carries its default in the declaration; every guard makes re-runs (and
// fresh DDL installs) no-ops.
func migrateSessions(db *sql.DB) error {
	for _, c := range sessionColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('work_items') WHERE name=?1`, c.name).Scan(&n); err != nil {
			return fmt.Errorf("probe column %s: %w", c.name, err)
		}
		if n != 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE work_items ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}

// Now returns the canonical RFC3339 UTC timestamp for all Core columns.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// NewID returns prefix + 12 random alphanumeric characters.
func NewID(prefix string) (string, error) {
	var b [12]byte
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("core: rand: %w", err)
	}
	out := make([]byte, 12)
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return prefix + string(out), nil
}

// NewSecret returns n cryptographically random bytes, hex encoded.
func NewSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("core: rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}
