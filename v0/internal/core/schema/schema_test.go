package schema_test

import (
	"database/sql"
	"github.com/silgrid/polyforge/internal/core/schema"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/core/store"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMigrateIdempotent asserts repeated Migrate calls on an up-to-date
// database are no-ops and that the version bookkeeping is visible in both
// PRAGMA user_version and core_meta.schema_version (02 §4.10 / §11).
func TestMigrateIdempotent(t *testing.T) {
	db := openTestDB(t)
	if got := schemaVersion(t, db); got != 1 {
		t.Fatalf("schema_version = %d, want 1", got)
	}
	if err := schema.Migrate(db.DB); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := schemaVersion(t, db); got != 1 {
		t.Fatalf("schema_version after re-migrate = %d, want 1", got)
	}
}

// TestNoExecutionStateColumns is the INV-1 mechanical audit assertion
// (02 §10 DDL 审计测试): no table column in the Core schema carries any
// workflow execution-state token. Progress is only ever read through from
// the Capsule's latest published Revision (02 §5.2).
func TestNoExecutionStateColumns(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) < 19 { // 18 user tables + memory_fts shadow pieces
		t.Fatalf("expected the full v1 schema, got %d tables: %v", len(tables), tables)
	}
	for _, table := range tables {
		if strings.HasPrefix(table, "sqlite_") || strings.HasPrefix(table, "memory_fts_") {
			continue
		}
		cols, err := columnNames(db.DB, table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, col := range cols {
			lower := strings.ToLower(col)
			for _, tok := range schema.ExecutionStateColumnTokens {
				if strings.Contains(lower, tok) {
					t.Errorf("INV-1 violation: table %q column %q contains execution-state token %q", table, col, tok)
				}
			}
		}
	}
}

// TestInvariantColumns asserts a few load-bearing schema shapes that later
// logic depends on: the three-column NULL coupling CHECK on work_items
// (02 §4.2), the unique partial index on active generations (INV-2b) and the
// capsule_revisions parent chain CHECK (02 §4.8).
func TestInvariantColumns(t *testing.T) {
	db := openTestDB(t)
	seed := func(stmt string, args ...any) {
		t.Helper()
		if _, err := db.Exec(stmt, args...); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	seed(`INSERT INTO projects(id, name) VALUES ('pj1','p1')`)
	seed(`INSERT INTO users(id, display_name, kind) VALUES ('usr1','u','human')`)
	seed(`INSERT INTO users(id, display_name, kind) VALUES ('usr2','u2','human')`)
	seed(`INSERT INTO work_items(id, seq, project_id, goal, reporter, created_at, updated_at)
		VALUES ('wi_x', 1, 'pj1', 'g', 'usr1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)

	// Partial NULL coupling: bundle_name set without digest must fail.
	_, err := db.Exec(`INSERT INTO work_items(id, seq, project_id, goal, reporter, bundle_name, created_at, updated_at)
		VALUES ('wi_y', 2, 'pj1', 'g', 'usr1', 'feature-full', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected CHECK failure for bundle columns not all-NULL or all-set")
	}

	seed(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at) VALUES ('wi_x', 1, 'usr1', '2026-01-01T00:00:00Z')`)
	_, err = db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES ('wi_x', 2, 'usr2', '2026-01-01T00:01:00Z')`)
	if err == nil {
		t.Fatal("expected unique violation for second active generation row (INV-2b)")
	}

	seed(`INSERT INTO capsule_revisions(work_item_id, seq, parent, generation, manifest_sha, data_bytes, published_at)
		VALUES ('wi_x', 1, 0, 1, 'a', 1, '2026-01-01T00:00:00Z')`)
	_, err = db.Exec(`INSERT INTO capsule_revisions(work_item_id, seq, parent, generation, manifest_sha, data_bytes, published_at)
		VALUES ('wi_x', 2, 0, 1, 'b', 1, '2026-01-01T00:00:01Z')`)
	if err == nil {
		t.Fatal("expected CHECK failure: seq=2 must have parent=1 (chain rule)")
	}
}

func schemaVersion(t *testing.T, db *store.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	var mirrored string
	if err := db.QueryRow(`SELECT value FROM core_meta WHERE key='schema_version'`).Scan(&mirrored); err != nil {
		t.Fatalf("core_meta.schema_version: %v", err)
	}
	if fmtInt(v) != mirrored {
		t.Fatalf("user_version=%d but core_meta.schema_version=%s", v, mirrored)
	}
	return v
}

func fmtInt(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func columnNames(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`PRAGMA table_info(` + "`" + table + "`" + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}
