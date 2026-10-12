package core

// ship_ledger.go: the ship-time WI → repo → branch-top SHA ledger
// (polyforge#123, P1b). The daemon's ship bridge reports, per repo it
// pushed (or idempotently re-used), the branch-top SHA it left in the
// repo's git origin; this manager upserts the row so the ledger always
// answers "where does this WI's copy of the repo live in git". There is no
// HTTP read face: the table is read from SQLite by operators.
//
// Accounting, not execution state (INV-1): rows describe git remotes'
// branch tops, never workflow progress. Recording is the ship bridge's
// duty and it treats a recording failure as a typed ship failure (the
// daemon side's contract), so a ledger row exists for every repo whose
// push was reported.

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// ShipLedgerManager keeps the wi_ship_ledger table.
type ShipLedgerManager struct {
	db  *DB
	wis *WIManager
}

// NewShipLedgerManager assembles the manager (the WI manager is shared
// for id-or-slug resolution — one resolver, no second spelling rule).
func NewShipLedgerManager(db *DB, wis *WIManager) *ShipLedgerManager {
	return &ShipLedgerManager{db: db, wis: wis}
}

// validateShipLedgerEntries enforces the recording shape: 1..64 entries,
// non-blank repo names (inventory spelling), non-blank branch-top SHAs.
func validateShipLedgerEntries(entries []api.ShipLedgerEntry) error {
	if len(entries) == 0 {
		return Fail(api.ErrCoreBadInput, "", "ship-ledger recording carries no repos")
	}
	if len(entries) > 64 {
		return Fail(api.ErrCoreBadInput, "", "ship-ledger recording carries %d repos, over the 64 bound", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if strings.TrimSpace(e.Repo) == "" {
			return Fail(api.ErrCoreBadInput, "", "ship-ledger entry carries no repo name")
		}
		if seen[e.Repo] {
			return Fail(api.ErrCoreBadInput, "", "ship-ledger recording carries repo %q twice", e.Repo)
		}
		seen[e.Repo] = true
		if strings.TrimSpace(e.BranchTopSHA) == "" {
			return Fail(api.ErrCoreBadInput, "", "ship-ledger entry for repo %q carries no branch_top_sha", e.Repo)
		}
	}
	return nil
}

// Record upserts one ship run's per-repo branch tops. Idempotent per
// (WI, repo): a re-ship overwrites the row with the newly pushed top —
// the ledger answers the LATEST delivery, not the history (the push
// history lives in git itself).
func (m *ShipLedgerManager) Record(wiIDOrSlug, branch string, entries []api.ShipLedgerEntry) error {
	if err := validateShipLedgerEntries(entries); err != nil {
		return err
	}
	id, err := m.wis.Resolve(wiIDOrSlug)
	if err != nil {
		return err
	}
	// Resolve passes any wi_-prefixed string through; the FK needs a real
	// row — an unknown WI is CORE-404, never a raw constraint failure.
	var one int
	if err := m.db.QueryRow(`SELECT 1 FROM work_items WHERE id=?1`, id).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Fail(api.ErrCoreNotFound, "", "work item %q", wiIDOrSlug)
		}
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if strings.TrimSpace(branch) == "" {
		branch = "" // the daemon normally reports polyforge/<wi>; blank stays blank
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO wi_ship_ledger(work_item_id, repo, branch, branch_top_sha, pushed_at)
			VALUES (?1, ?2, ?3, ?4, ?5)
			ON CONFLICT(work_item_id, repo) DO UPDATE SET
				branch = excluded.branch,
				branch_top_sha = excluded.branch_top_sha,
				pushed_at = excluded.pushed_at`, id, e.Repo, branch, strings.TrimSpace(e.BranchTopSHA), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
