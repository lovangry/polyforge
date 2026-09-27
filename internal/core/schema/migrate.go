package schema

import (
	"database/sql"
	"fmt"
)

// migration is one schema step. Each migration runs inside a single
// transaction together with the user_version bump, so a crash mid-migration
// leaves the previous version fully intact (02 §7: WAL 回放，事务原子性保证无半写).
type migration struct {
	version int
	// stmts are executed verbatim in order. DDL in SQLite is transactional.
	stmts []string
	// post runs after stmts within the same transaction, for data backfills.
	post func(tx *sql.Tx) error
}

var migrations = []migration{
	{version: 1, stmts: []string{DDLV1}},
}

// CurrentVersion returns the target schema version.
func CurrentVersion() int { return migrations[len(migrations)-1].version }

// Migrate brings the database at db up to CurrentVersion. It is idempotent:
// already-applied versions are skipped. The applied-version counter lives in
// PRAGMA user_version (transactional with the DDL) and is mirrored into
// core_meta.schema_version ("1", 02 §4.10 固定键) on every apply.
//
// First boot: the database is empty, version 1 installs the full schema
// (02 §11: 一期从无到有，首次启动建库 v1).
func Migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("schema: read user_version: %w", err)
	}
	if current > CurrentVersion() {
		return fmt.Errorf("schema: database version %d is newer than this binary (%d); refusing to downgrade", current, CurrentVersion())
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("schema: apply version %d: %w", m.version, err)
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range m.stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
	}
	if m.post != nil {
		if err := m.post(tx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return fmt.Errorf("bump user_version: %w", err)
	}
	// Mirror into core_meta.schema_version. core_meta exists from v1 on; for
	// version 1 itself the DDL created it earlier in this same transaction.
	if _, err := tx.Exec(`INSERT INTO core_meta(key, value) VALUES ('schema_version', ?1)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmt.Sprintf("%d", m.version)); err != nil {
		return fmt.Errorf("mirror schema_version: %w", err)
	}
	return tx.Commit()
}

// ExecutionStateColumnTokens is the INV-2c/INV-1 mechanical audit vocabulary
// (02 §5.1a 执行态键名黑名单 + execution-state column families). No table
// column name in the Core schema may contain any of these tokens.
var ExecutionStateColumnTokens = []string{
	"current_step", "step_id", "step_attempt", "attempt", "review_round",
	"review_verdict", "progress", "workflow_status", "rewind_stat",
	"budget_consumed", "command_count", "step_start_ref", "pending_question",
}
