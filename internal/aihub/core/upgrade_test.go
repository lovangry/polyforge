package core

// upgrade_test.go — the polyforge#149 documented-manual-SQL upgrade pins:
// the fixture builds an OLD-schema database (the pre-#149 dead tables and
// columns), applies the SQL parsed DIRECTLY out of docs/upgrade.md (the
// document is the single source), and asserts the dead shapes are gone,
// the live rows survive, the optional waiting_on cleanup touches only the
// dead values, and `Open` runs NO auto migration (an un-upgraded
// old-schema database keeps its dead shapes).

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

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
func upgradeSQLBlocks(t *testing.T, body string) []string {
	t.Helper()
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

// loadUpgradeDoc reads docs/upgrade.md and answers its sql blocks plus
// the body (backup-first wording is asserted too).
func loadUpgradeDoc(t *testing.T) ([]string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "upgrade.md"))
	if err != nil {
		t.Fatalf("read docs/upgrade.md: %v", err)
	}
	body := string(raw)
	blocks := upgradeSQLBlocks(t, body)
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
	if !strings.Contains(strings.ToLower(body), "backup") {
		t.Fatalf("docs/upgrade.md never tells the operator to back up first")
	}
	return blocks, body
}

// tableExists and columnExists mirror the w149 probes on a raw handle.
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

// oldSchemaFixture turns a freshly-opened (current-schema) database into
// an OLD-schema one: the dead tables and columns exactly as the
// pre-change DDL created them. populate fills every affected
// table/column with rows, including all four waiting_on value classes.
func oldSchemaFixture(t *testing.T, env *testEnv, populate bool) {
	t.Helper()
	db := env.DB.DB
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS core_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS wi_ship_ledger (
			work_item_id   TEXT NOT NULL REFERENCES work_items(id),
			repo           TEXT NOT NULL,
			branch         TEXT NOT NULL DEFAULT '',
			branch_top_sha TEXT NOT NULL,
			pushed_at      TEXT NOT NULL,
			PRIMARY KEY (work_item_id, repo)
		)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture: old table: %v", err)
		}
	}
	addCol := func(table, column, decl string) {
		if rawColumnExists(t, db, table, column) {
			return
		}
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl); err != nil {
			t.Fatalf("fixture: add %s.%s: %v", table, column, err)
		}
	}
	addCol("projects", "scenario", "TEXT")
	addCol("projects", "identifier_hash", "TEXT NOT NULL DEFAULT ''")
	addCol("work_items", "bundle_digest", "TEXT")
	addCol("work_items", "workflow_budget_baseline", "TEXT")
	addCol("work_items", "session_owner", "TEXT")
	if !populate {
		return
	}
	// every affected table/column carries rows; the four waiting_on
	// value classes each hold one row.
	gate := mustCreateExecWI(t, env, "upgrade stale gate row", nil, "")
	runtimeRow := mustCreateExecWI(t, env, "upgrade stale runtime row", nil, "")
	owner := mustCreateExecWI(t, env, "upgrade live owner row", nil, "")
	dep := mustCreateExecWI(t, env, "upgrade live dependency row", nil, "")
	fills := []struct {
		stmt string
		args []any
	}{
		{`INSERT OR REPLACE INTO core_meta(key,value) VALUES('schema_version','1')`, nil},
		{`UPDATE projects SET scenario='legacy', identifier_hash='deadbeef'`, nil},
		{`INSERT OR REPLACE INTO wi_ship_ledger(work_item_id,repo,branch,branch_top_sha,pushed_at)
			VALUES(?1,'demo-src','polyforge/old',?2,'2026-01-01T00:00:00Z')`,
			[]any{gate, strings.Repeat("e", 40)}},
		{`UPDATE work_items SET bundle_digest=?1, workflow_budget_baseline='{}', session_owner='m-old'`,
			[]any{strings.Repeat("d", 64)}},
		{`UPDATE work_items SET waiting_on='gate' WHERE id=?1`, []any{gate}},
		{`UPDATE work_items SET waiting_on='runtime:spawn' WHERE id=?1`, []any{runtimeRow}},
		{`UPDATE work_items SET waiting_on='owner' WHERE id=?1`, []any{owner}},
		{`UPDATE work_items SET waiting_on='dependency:demo-bundle' WHERE id=?1`, []any{dep}},
	}
	for _, f := range fills {
		if _, err := db.Exec(f.stmt, f.args...); err != nil {
			t.Fatalf("fixture: populate (%s): %v", f.stmt, err)
		}
	}
}

// assertDeadGone asserts every dead table/column is absent.
func assertDeadGone(t *testing.T, db *sql.DB, when string) {
	t.Helper()
	for _, table := range []string{"core_meta", "wi_ship_ledger"} {
		if rawTableExists(t, db, table) {
			t.Errorf("%s: dead table %s still present", when, table)
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
			t.Errorf("%s: dead column %s.%s still present", when, d.table, d.column)
		}
	}
}

// applyUpgradeDoc applies the doc's mandatory (DROP-carrying) block and
// the optional waiting_on cleanup block, in that order, answering the
// ids of the stale gate/runtime rows (empty when unpopulated).
func applyUpgradeDoc(t *testing.T, db *sql.DB, blocks []string) {
	t.Helper()
	applied := 0
	for _, blk := range blocks {
		up := strings.ToUpper(blk)
		if !strings.Contains(up, "DROP") {
			continue // the cleanup block is exercised below
		}
		for _, stmt := range upgradeStatements(blk) {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("upgrade statement failed: %v\n%s", err, stmt)
			}
			applied++
		}
	}
	if applied == 0 {
		t.Fatalf("the upgrade doc carries no applicable DROP statements")
	}
}

// upgradeRun is the shared body of the two flavors.
func upgradeRun(t *testing.T, populate bool) {
	blocks, _ := loadUpgradeDoc(t)

	env := newEnv(t) // fresh current-schema DB, admin user, project alpha
	oldSchemaFixture(t, env, populate)

	// Apply the mandatory SQL from the document.
	applyUpgradeDoc(t, env.DB.DB, blocks)
	assertDeadGone(t, env.DB.DB, "after the documented upgrade SQL")

	if populate {
		// the live rows survive; the cleanup UPDATE touches ONLY the
		// stale values.
		var n int
		if err := env.DB.QueryRow(`SELECT COUNT(*) FROM work_items WHERE waiting_on IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 4 {
			t.Fatalf("upgrade lost waiting_on rows: %d left, want the 4 seeded", n)
		}
		for _, blk := range blocks {
			if !strings.Contains(strings.ToUpper(blk), "UPDATE") || !strings.Contains(blk, "waiting_on") {
				continue
			}
			for _, stmt := range upgradeStatements(blk) {
				if _, err := env.DB.Exec(stmt); err != nil {
					t.Fatalf("cleanup statement failed: %v\n%s", err, stmt)
				}
			}
		}
		var stale int
		if err := env.DB.QueryRow(`SELECT COUNT(*) FROM work_items
			WHERE waiting_on='gate' OR waiting_on LIKE 'runtime:%'`).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if stale != 0 {
			t.Fatalf("the cleanup UPDATE left %d stale rows", stale)
		}
		var live int
		if err := env.DB.QueryRow(`SELECT COUNT(*) FROM work_items
			WHERE waiting_on='owner' OR waiting_on LIKE 'dependency:%'`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 2 {
			t.Fatalf("the cleanup UPDATE touched LIVE rows: %d live rows left, want 2", live)
		}
		// every affected live row survived
		if err := env.DB.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n < 4 {
			t.Fatalf("the upgrade lost rows: %d work items left, want at least the 4 seeded", n)
		}
	}

	// Core smoke on the upgraded database: create, claim, list.
	id := mustCreateExecWI(t, env, "post-upgrade smoke ticket", nil, "")
	m := NewSessionManager(env.DB)
	if _, err := m.Claim(id, api.SessionClaimRequest{
		SessionID: strings.Repeat("7", 32), MachineID: "m1", IdempotencyKey: "up-1",
	}, env.UserID); err != nil {
		t.Fatalf("post-upgrade claim: %v", err)
	}
	wi, err := env.WIs.Get(id, GetOptions{})
	if err != nil || wi.Status != api.StatusRunning {
		t.Fatalf("post-upgrade claim left status %s (%v)", wi.Status, err)
	}
	if _, err := env.WIs.ReadyQueue("alpha"); err != nil {
		t.Fatalf("post-upgrade ready queue: %v", err)
	}
}

func TestUpgradeOldEmptyDB(t *testing.T)     { upgradeRun(t, false) }
func TestUpgradeOldPopulatedDB(t *testing.T) { upgradeRun(t, true) }

// TestOpenRunsNoAutoMigration: `Open` on an old-schema database leaves
// the dead shapes exactly as they are — the upgrade path is the
// documented manual SQL, never a migration at boot.
func TestOpenRunsNoAutoMigration(t *testing.T) {
	env := newEnv(t)
	oldSchemaFixture(t, env, true)
	// Copy the fixture's database (WAL checkpointed by VACUUM INTO) into
	// a fresh data dir and reopen it with Open.
	dataDir := t.TempDir()
	if _, err := env.DB.Exec(`VACUUM INTO ?1`, filepath.Join(dataDir, "aihub.db")); err != nil {
		t.Fatalf("fixture copy (VACUUM INTO): %v", err)
	}
	db2, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if !rawTableExists(t, db2.DB, "wi_ship_ledger") {
		t.Fatal("Open migrated the old schema (wi_ship_ledger is gone): no auto migration is the contract")
	}
	if !rawTableExists(t, db2.DB, "core_meta") {
		t.Fatal("Open migrated the old schema (core_meta is gone): no auto migration is the contract")
	}
	if !rawColumnExists(t, db2.DB, "work_items", "session_owner") {
		t.Fatal("Open dropped work_items.session_owner: no auto migration is the contract")
	}
}
