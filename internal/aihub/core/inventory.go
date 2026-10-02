package core

// WI repo inventory (02 §4.6): the repo-reference triple (name/url/subdir
// declaration + frozen base sha) whose Core-authoritative store is the
// wi_repo_inventory table. The two input arrays arrive on the WI
// create/patch body and are validated as ONE triple; the base is frozen
// inside the create transaction and immutable after (no UPDATE path — a
// changed inventory means a new WI). This file ports the v0 validation
// block (v0/internal/core/workitem/workitem.go:186-212) into the v1
// core, closing the TODO(v1) §4.5 checkout-hash gap the WI goal names:
// a ProjectRepo-style entry with name+URL but a missing/blank HEAD sha
// can no longer slip through as a half-empty triple.

import (
	"database/sql"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// RepoDecl is one declared inventory entry (the §4.6 wire shape of
// repo_inventory). It is deliberately NOT api.ProjectRepo: that type is
// the project-level inventory (name/url/description, no sha) and stays
// untouched; this is the WI-level triple's declaration half.
type RepoDecl struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Subdir *string `json:"subdir"`
}

// RepoBaseDecl is one repo_inventory_base entry: the frozen base commit
// sha for a declared repo name.
type RepoBaseDecl struct {
	Name string `json:"name"`
	Base string `json:"base"`
}

// WIRepoEntry is one wi_repo_inventory row as served on the WI view
// (base included — frozen at create, never null for a declared repo).
type WIRepoEntry struct {
	Position int64   `json:"position"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Subdir   *string `json:"subdir"`
	Base     string  `json:"base"`
}

// maxRepoInventory is the §4.6 cardinality bound: 1..8 entries per WI.
const maxRepoInventory = 8

// validateRepoInventory enforces the §4.6 triple on the two input arrays:
// 1..8 entries, unique names per WI, and a base sha for every declared
// repo. A declared entry whose base is missing (no base row at all) or
// blank draws INVENTORY_BASE_MISSING naming the offending repo — the
// same refusal vocabulary as v0 workitem.go:211 and doc line 276; the
// create/patch carrying it must fail whole. Duplicate names and a base
// set that does not match the declaration set are INVENTORY_MISMATCH.
// On success it returns the name→base map for the freeze insert.
func validateRepoInventory(inv []RepoDecl, bases []RepoBaseDecl) (map[string]string, error) {
	if len(inv) > maxRepoInventory {
		return nil, Fail(api.ErrCoreBadInput, "", "repo_inventory allows 1..8 entries")
	}
	baseByName := map[string]string{}
	for _, b := range bases {
		if _, dup := baseByName[b.Name]; dup {
			return nil, Fail(api.ErrCoreBadInput, "INVENTORY_MISMATCH", "duplicate base entry for %q", b.Name)
		}
		baseByName[b.Name] = b.Base
	}
	seenNames := map[string]bool{}
	for _, r := range inv {
		if seenNames[r.Name] {
			return nil, Fail(api.ErrCoreBadInput, "INVENTORY_MISMATCH", "duplicate inventory name %q", r.Name)
		}
		seenNames[r.Name] = true
	}
	// Per-entry base check FIRST: a declared entry with name+URL whose
	// base row is absent entirely (the whole repo_inventory_base array
	// missing, or just this name) is exactly the half-empty triple §4.6
	// refuses — it draws the base-naming refusal, not the generic
	// count-mismatch one.
	for _, r := range inv {
		base, ok := baseByName[r.Name]
		if !ok || strings.TrimSpace(base) == "" {
			return nil, Fail(api.ErrCoreBadInput, "INVENTORY_BASE_MISSING", "base missing for %q", r.Name)
		}
		baseByName[r.Name] = strings.TrimSpace(base)
	}
	// What remains of the set comparison: base entries naming repos the
	// declaration does not carry (extra or foreign names).
	if len(baseByName) != len(inv) {
		return nil, Fail(api.ErrCoreBadInput, "INVENTORY_MISMATCH",
			"repo_inventory_base does not match repo_inventory (count/name set)")
	}
	return baseByName, nil
}

// insertRepoInventoryTx freezes the validated triple into
// wi_repo_inventory inside the caller's create transaction (§4.6: the
// declaration and the base land in the SAME transaction as the WI row;
// there is no UPDATE path afterwards).
func insertRepoInventoryTx(tx *sql.Tx, wiID string, inv []RepoDecl, baseByName map[string]string) error {
	for i, r := range inv {
		var subdir any
		if r.Subdir != nil {
			subdir = *r.Subdir
		}
		if _, err := tx.Exec(`INSERT INTO wi_repo_inventory(work_item_id, position, name, url, subdir, base)
			VALUES (?1,?2,?3,?4,?5,?6)`, wiID, i+1, r.Name, r.URL, subdir, baseByName[r.Name]); err != nil {
			return err
		}
	}
	return nil
}

// repoInventoryOf reads the frozen inventory (base included) in
// declaration order. WIs without an inventory serve an empty array.
func (m *WIManager) repoInventoryOf(wiID string) ([]WIRepoEntry, error) {
	rows, err := m.db.Query(`SELECT position, name, url, subdir, base FROM wi_repo_inventory
		WHERE work_item_id=?1 ORDER BY position`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WIRepoEntry{}
	for rows.Next() {
		var e WIRepoEntry
		var subdir sql.NullString
		if err := rows.Scan(&e.Position, &e.Name, &e.URL, &subdir, &e.Base); err != nil {
			return nil, err
		}
		if subdir.Valid {
			e.Subdir = &subdir.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
