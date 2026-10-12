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

// DDL is the v1.5 Core schema. First boot installs it as-is; migrations
// for pre-existing databases run in Open: the additive session-columns
// migration, then the per-project seq rebuild (seq-fix lane: the legacy
// library numbers WIs per project, so seq is UNIQUE(project_id, seq),
// not globally unique). Compared to v0: capsule revision tables and the
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
  -- M3 heartbeat wiring (lane hb-m3, additive): the daemon's liveness
  -- payload carries its current mode and the WI ids it holds live, so the
  -- machines read face (GET /api/v1/machines) answers not just "alive?"
  -- but "alive and running what?". live_wi_ids is a JSON array string.
  mode        TEXT,
  live_wi_ids TEXT,
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
  -- seq is the per-project display number (<project>#<seq>): unique per
  -- project, NOT globally (the legacy library numbers per project —
  -- aiterm#1 and defswitch#1 both exist). The constraint lives at the
  -- bottom of this table definition.
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
  clean_pause         INTEGER NOT NULL DEFAULT 0,
  -- per-project display seq (seq-fix lane owner ruling).
  UNIQUE (project_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_wi_status     ON work_items(status, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_wi_assignee   ON work_items(assignee) WHERE status IN ('running','paused','blocked');
CREATE INDEX IF NOT EXISTS idx_wi_waiting_on ON work_items(waiting_on) WHERE waiting_on IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wi_parent     ON work_items(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wi_milestone  ON work_items(project_id, milestone) WHERE milestone IS NOT NULL;

CREATE TABLE IF NOT EXISTS wi_repo_inventory (
  -- §4.6: the WI repo-reference triple. The declaration (name/url/
  -- subdir) and the frozen base sha are written ONLY inside the WI
  -- create transaction; there is no UPDATE path (base immutable
  -- after create — a changed inventory means a new WI).
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  position     INTEGER NOT NULL,            -- declaration order, 1..8
  name         TEXT NOT NULL,               -- repo short name
  url          TEXT NOT NULL,
  subdir       TEXT,                        -- in-repo subdirectory anchor, nullable
  base         TEXT,                        -- commit sha, frozen at create
  PRIMARY KEY (work_item_id, position)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_inventory_name ON wi_repo_inventory(work_item_id, name);

CREATE TABLE IF NOT EXISTS wi_ship_ledger (
  -- polyforge#123 (P1b): the ship-time WI → repo → branch-top SHA
  -- ledger. One row per (WI, repo); ship upserts the branch top it
  -- pushed (or re-used), so the ledger always answers "where this WI's
  -- copy of the repo lives in git". The queryable face is
  -- GET /api/v1/work-items/{id}/ship-ledger; the recording face is the
  -- ship bridge's POST. Not execution state (INV-1): it is delivery
  -- accounting about git remotes, written by the daemon's ship bridge.
  work_item_id   TEXT NOT NULL REFERENCES work_items(id),
  repo           TEXT NOT NULL,               -- repo short name (inventory spelling)
  branch         TEXT NOT NULL DEFAULT '',    -- the delivery branch (polyforge/<wi>)
  branch_top_sha TEXT NOT NULL,               -- the pushed/re-used branch top
  pushed_at      TEXT NOT NULL,               -- RFC3339, server-stamped at record time
  PRIMARY KEY (work_item_id, repo)
);

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
  updated_at   TEXT NOT NULL,
  -- polyforge#129 freshness stamp: the authority file's observed state
  -- (Stat mtime in nanoseconds + byte size) at the moment the row was
  -- written, recorded by the write path in the same transaction as the
  -- row. The reconcile consults it before any per-entry work — stamp
  -- match ⇒ skip (no read, no hash, no FTS write); mismatch ⇒ verify by
  -- content sha. NULL means "unverified" (pre-#129 rows, backfills
  -- awaiting their batched index write): the next reconcile pass
  -- verifies the row against the file and stamps it.
  fs_mtime_ns  INTEGER,
  fs_size      INTEGER
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
	// M3 heartbeat wiring: machines gains mode/live_wi_ids in place for
	// databases created before the daemon heartbeat face was wired.
	if err := migrateMachines(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("core: migrate machines: %w", err)
	}
	// polyforge#129: memories gains the freshness-stamp columns in place
	// (guarded ALTER TABLE, same discipline as migrateMachines). Rows in
	// a pre-#129 database carry NULL stamps; the first reconcile after
	// the upgrade verifies each row against its authority file and
	// stamps it — one full pass, after which the steady state is
	// incremental.
	if err := migrateMemoryStamps(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("core: migrate memory stamps: %w", err)
	}
	// work_items.seq is numbered per project: UNIQUE(project_id, seq). A
	// database still carrying the old global UNIQUE(seq) is refused — there
	// is no in-place migration; rebuild it offline.
	if err := requirePerProjectSeq(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{sqlDB}, nil
}

// machineColumns are the M3 heartbeat additions to machines (DDL above).
// Fresh installs get them from the DDL; existing databases get them from
// migrateMachines' guarded ALTER TABLE, same discipline as migrateSessions.
var machineColumns = []struct{ name, decl string }{
	{"mode", "TEXT"},
	{"live_wi_ids", "TEXT"},
}

// migrateMachines adds the machines heartbeat columns when missing. Every
// guard makes re-runs (and fresh DDL installs) no-ops.
func migrateMachines(db *sql.DB) error {
	for _, c := range machineColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('machines') WHERE name=?1`, c.name).Scan(&n); err != nil {
			return fmt.Errorf("probe column %s: %w", c.name, err)
		}
		if n != 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE machines ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}

// memoryColumns are the polyforge#129 freshness-stamp additions to
// memories (DDL above). Fresh installs get them from the DDL; existing
// databases get them from migrateMemoryStamps' guarded ALTER TABLE,
// same discipline as migrateMachines/migrateSessions.
var memoryColumns = []struct{ name, decl string }{
	{"fs_mtime_ns", "INTEGER"},
	{"fs_size", "INTEGER"},
}

// migrateMemoryStamps adds the memories freshness-stamp columns when
// missing. Every guard makes re-runs (and fresh DDL installs) no-ops.
func migrateMemoryStamps(db *sql.DB) error {
	for _, c := range memoryColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('memories') WHERE name=?1`, c.name).Scan(&n); err != nil {
			return fmt.Errorf("probe column %s: %w", c.name, err)
		}
		if n != 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE memories ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
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

// ---------------------------------------------------------------------------
// Per-project seq shape check (no migration)

// requirePerProjectSeq fails loudly when work_items still has the old
// global UNIQUE index on (seq) alone.
func requirePerProjectSeq(db *sql.DB) error {
	old, err := hasGlobalSeqUnique(db)
	if err != nil {
		return fmt.Errorf("core: probe seq uniqueness: %w", err)
	}
	if old {
		return fmt.Errorf("core: work_items.seq is globally UNIQUE (pre per-project numbering); this build requires UNIQUE(project_id, seq) and does not migrate — rebuild the table offline")
	}
	return nil
}

// hasGlobalSeqUnique reports whether work_items carries a UNIQUE
// index on (seq) alone (the old global shape). The index names are
// collected BEFORE inspecting each one: Core runs a one-connection
// pool, and a nested Query while the outer rows are still open would
// deadlock the pool.
func hasGlobalSeqUnique(db *sql.DB) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_index_list('work_items')
		WHERE "unique"=1 AND origin='u'`)
	if err != nil {
		return false, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return false, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, name := range names {
		cols, err := indexColumns(db, name)
		if err != nil {
			return false, err
		}
		if len(cols) == 1 && cols[0] == "seq" {
			return true, nil
		}
	}
	return false, nil
}

// indexColumns answers one index's column list in position order.
func indexColumns(db *sql.DB, index string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_index_info(?1) ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
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
