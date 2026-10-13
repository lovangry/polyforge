package core

// upgrade_test.go — the polyforge#149 documented-manual-SQL upgrade pins.
// The fixture is a REAL pre-#149 database: the literal old DDL (preW149DDL,
// copied verbatim from `git show 85cf926:internal/aihub/core/store.go`),
// populated through raw SQL because the current code cannot write to it.
// The SQL is parsed DIRECTLY out of docs/upgrade.md (the document is the
// single source); after it runs, the current server's write paths must
// succeed on the upgraded database, and without it they must not.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

// preW149DDL is the Core schema exactly as shipped before polyforge#149
// (85cf926). Do not edit: it is the shape real old databases have.
const preW149DDL = `
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
  -- copy of the repo lives in git". It is
  -- recorded through POST /api/v1/work-items/{id}/ship-ledger by the ship
  -- bridge (no HTTP read face). Not execution state (INV-1): it is delivery
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

// repoRoot walks up from the working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root (go.mod) not found")
		}
		dir = parent
	}
}

// upgradeSQLBlocks extracts the fenced ```sql blocks of a markdown body.
func upgradeSQLBlocks(body string) []string {
	var out []string
	rest := body
	for {
		i := strings.Index(rest, "```sql")
		if i < 0 {
			return out
		}
		rest = rest[i+len("```sql"):]
		j := strings.Index(rest, "```")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j+3:]
	}
}

// upgradeStatements splits one SQL block into executable statements,
// dropping `--` comments and blank statements.
func upgradeStatements(block string) []string {
	var clean []string
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		clean = append(clean, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(clean, "\n"), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// loadUpgradeDoc reads docs/upgrade.md and answers its sql blocks.
func loadUpgradeDoc(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "upgrade.md"))
	if err != nil {
		t.Fatalf("read docs/upgrade.md: %v", err)
	}
	blocks := upgradeSQLBlocks(string(raw))
	if len(blocks) == 0 {
		t.Fatalf("docs/upgrade.md carries no fenced sql block")
	}
	var hasDropLedger, hasCleanup bool
	for _, blk := range blocks {
		up := strings.ToUpper(blk)
		if strings.Contains(up, "DROP") && strings.Contains(blk, "wi_ship_ledger") {
			hasDropLedger = true
		}
		if strings.Contains(up, "UPDATE") && strings.Contains(blk, "waiting_on") {
			hasCleanup = true
		}
	}
	if !hasDropLedger {
		t.Fatalf("docs/upgrade.md carries no mandatory block dropping wi_ship_ledger")
	}
	if !hasCleanup {
		t.Fatalf("docs/upgrade.md carries no optional waiting_on cleanup UPDATE")
	}
	return blocks
}

// rawTableExists and rawColumnExists probe the schema on a raw handle.
func rawTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1`, table).Scan(&n); err != nil {
		t.Fatalf("probe table %s: %v", table, err)
	}
	return n > 0
}

func rawColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("pragma scan %s: %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma walk %s: %v", table, err)
	}
	return false
}

// oldSchemaDB builds a data dir holding a pre-#149 database (the literal
// old DDL) and answers it with an open raw handle. populate fills every
// affected table/column through raw SQL, including the four waiting_on
// value classes (two dead, two live).
func oldSchemaDB(t *testing.T, populate bool) (string, *sql.DB) {
	t.Helper()
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "aihub.db"))
	if err != nil {
		t.Fatalf("fixture: open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(preW149DDL); err != nil {
		t.Fatalf("fixture: old DDL: %v", err)
	}
	if !populate {
		return dataDir, db
	}
	now := Now()
	stmts := []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO core_meta(key,value) VALUES('schema_version','1')`, nil},
		{`INSERT INTO users(id,display_name,kind,role,created_at) VALUES('u-old','old','human','admin',?1)`, []any{now}},
		{`INSERT INTO projects(id,name,scenario,identifier_hash,created_at,updated_at)
			VALUES('p-old','legacy','legacy','deadbeef',?1,?1)`, []any{now}},
	}
	for i, w := range []string{"gate", "runtime:spawn", "owner", "dependency:demo-bundle"} {
		stmts = append(stmts, struct {
			stmt string
			args []any
		}{`INSERT INTO work_items(id,seq,project_id,goal,reporter,waiting_on,bundle_name,
			bundle_digest,workflow_budget_baseline,session_owner,created_at,updated_at)
			VALUES(?1,?2,'p-old',?3,'u-old',?4,'demo-bundle',?5,'{}','m-old',?6,?6)`,
			[]any{oldWIID(i), i + 1, "old row " + w, w, strings.Repeat("d", 64), now}})
	}
	stmts = append(stmts, struct {
		stmt string
		args []any
	}{`INSERT INTO wi_ship_ledger(work_item_id,repo,branch,branch_top_sha,pushed_at)
		VALUES(?1,'demo-src','polyforge/old',?2,?3)`, []any{oldWIID(0), strings.Repeat("e", 40), now}})
	for _, s := range stmts {
		if _, err := db.Exec(s.stmt, s.args...); err != nil {
			t.Fatalf("fixture: populate (%s): %v", s.stmt, err)
		}
	}
	return dataDir, db
}

// oldWIID is the id of the i-th seeded old work item.
func oldWIID(i int) string { return "wi_old" + string(rune('a'+i)) }

// assertDeadGone asserts every dead table/column is absent.
func assertDeadGone(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"core_meta", "wi_ship_ledger"} {
		if rawTableExists(t, db, table) {
			t.Errorf("dead table %s still present after the upgrade SQL", table)
		}
	}
	for _, d := range []struct{ table, column string }{
		{"projects", "scenario"},
		{"projects", "identifier_hash"},
		{"work_items", "bundle_digest"},
		{"work_items", "workflow_budget_baseline"},
		{"work_items", "session_owner"},
	} {
		if rawColumnExists(t, db, d.table, d.column) {
			t.Errorf("dead column %s.%s still present after the upgrade SQL", d.table, d.column)
		}
	}
}

// applyUpgradeDoc applies the statements of every doc block that passes
// keep, in document order.
func applyUpgradeDoc(t *testing.T, db *sql.DB, blocks []string, keep func(upper string) bool) {
	t.Helper()
	applied := 0
	for _, blk := range blocks {
		if !keep(strings.ToUpper(blk)) {
			continue
		}
		for _, stmt := range upgradeStatements(blk) {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("upgrade statement failed: %v\n%s", err, stmt)
			}
			applied++
		}
	}
	if applied == 0 {
		t.Fatalf("docs/upgrade.md carries no applicable statements")
	}
}

func isMandatoryBlock(upper string) bool { return strings.Contains(upper, "DROP") }
func isCleanupBlock(upper string) bool {
	return strings.Contains(upper, "UPDATE") && strings.Contains(upper, "WAITING_ON")
}

// upgradeRun builds a real old database, applies the documented SQL,
// reopens it with the current Open and drives the live write paths.
func upgradeRun(t *testing.T, populate bool) {
	blocks := loadUpgradeDoc(t)
	dataDir, raw := oldSchemaDB(t, populate)

	applyUpgradeDoc(t, raw, blocks, isMandatoryBlock)
	assertDeadGone(t, raw)

	if populate {
		// The optional cleanup touches ONLY the dead waiting_on values.
		applyUpgradeDoc(t, raw, blocks, isCleanupBlock)
		var stale, live, total int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM work_items
			WHERE waiting_on='gate' OR waiting_on LIKE 'runtime:%'`).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM work_items
			WHERE waiting_on='owner' OR waiting_on LIKE 'dependency:%'`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if stale != 0 || live != 2 || total != 4 {
			t.Fatalf("cleanup: stale=%d live=%d total=%d, want 0/2/4", stale, live, total)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// The current server on the upgraded file: user, project (the write
	// the old identifier_hash NOT NULL column broke), work item, claim,
	// patch, ready queue.
	env := newEnvAt(t, dataDir)
	id := mustCreateExecWI(t, env, "post-upgrade smoke ticket", nil, "")
	m := NewSessionManager(env.DB)
	if _, err := m.Claim(id, api.SessionClaimRequest{
		SessionID: strings.Repeat("7", 32), MachineID: "m1", IdempotencyKey: "up-1",
	}, env.UserID); err != nil {
		t.Fatalf("post-upgrade claim: %v", err)
	}
	wi, err := env.WIs.Get(id, GetOptions{})
	if err != nil || wi.Status != api.StatusRunning {
		t.Fatalf("post-upgrade claim left status %v (%v)", wi, err)
	}
	if _, err := env.WIs.ReadyQueue("alpha"); err != nil {
		t.Fatalf("post-upgrade ready queue: %v", err)
	}

	if populate {
		// The old rows read and write through the live columns.
		old := oldWIID(2)
		if _, err := env.WIs.Patch(old, map[string]json.RawMessage{"priority": json.RawMessage(`"high"`)}); err != nil {
			t.Fatalf("post-upgrade patch of an old row: %v", err)
		}
		if _, err := env.WIs.Cancel(oldWIID(0), env.UserID); err != nil {
			t.Fatalf("post-upgrade cancel of an old row: %v", err)
		}
		if _, err := env.WIs.Create(CreateRequest{Project: "legacy", Goal: "new in old project", Reporter: env.UserID}); err != nil {
			t.Fatalf("post-upgrade create in an old project: %v", err)
		}
		if _, err := env.WIs.ReadyQueue("legacy"); err != nil {
			t.Fatalf("post-upgrade ready queue of an old project: %v", err)
		}
	}
}

func TestUpgradeOldEmptyDB(t *testing.T)     { upgradeRun(t, false) }
func TestUpgradeOldPopulatedDB(t *testing.T) { upgradeRun(t, true) }

// TestOpenRunsNoAutoMigration: `Open` on a real old-schema database
// leaves the dead shapes exactly as they are, and the current write
// path fails on it — the documented SQL is mandatory before the new
// server starts.
func TestOpenRunsNoAutoMigration(t *testing.T) {
	dataDir, raw := oldSchemaDB(t, true)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"wi_ship_ledger", "core_meta"} {
		if !rawTableExists(t, db.DB, table) {
			t.Fatalf("Open migrated the old schema (%s is gone): no auto migration is the contract", table)
		}
	}
	if !rawColumnExists(t, db.DB, "projects", "identifier_hash") {
		t.Fatal("Open dropped projects.identifier_hash: no auto migration is the contract")
	}
	if _, err := NewAdminManager(db).CreateProject(CreateProjectRequest{Name: "alpha"}); err == nil {
		t.Fatal("CreateProject succeeded on an un-upgraded database; docs/upgrade.md says it fails until the SQL runs")
	}
}
