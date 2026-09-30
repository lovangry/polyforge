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

// DDL is the v1 Core schema. First boot installs it as-is; no migrations
// (fresh product, no stock). Compared to v0: capsule revision tables and
// the service-secret machinery are gone; owner_generations gains the
// attempt identity and the push-credential digest (v1 generation fencing,
// main layer).
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
  closed_at    TEXT
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

CREATE TABLE IF NOT EXISTS owner_generations (
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  generation   INTEGER NOT NULL,
  attempt_id   TEXT NOT NULL,
  owner        TEXT NOT NULL,
  -- owner/machine_id carry the v1 wire strings (display owner, daemon
  -- machine id); they are deliberately NOT FK columns: the claim face
  -- accepts a display owner and machines may not be registered yet.
  machine_id   TEXT,
  secret_hash  TEXT NOT NULL,
  started_at   TEXT NOT NULL,
  ended_at     TEXT,
  end_reason   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_owner_gen_active
  ON owner_generations(work_item_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_owner_gen_seq
  ON owner_generations(work_item_id, generation);

CREATE TABLE IF NOT EXISTS claim_idempotency (
  idempotency_key TEXT PRIMARY KEY,
  work_item_id    TEXT NOT NULL REFERENCES work_items(id),
  caller_identity TEXT NOT NULL,
  response_json   TEXT NOT NULL,
  created_at      TEXT NOT NULL
);

-- M5 service-secret epoch ledger (02 §4.10/§6.6). v1 minted credentials
-- are per-attempt random secrets (hash-only in owner_generations), so no
-- validation path consumes a shared service secret; the ledger exists for
-- the rotate-secret wire contract and its audit trail.
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
	return &DB{sqlDB}, nil
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
