package capsule

// bootstrap.go: the capsule_bootstrap payload arrives in the claim response
// (02 §8: bundle reference + budget baseline + repo inventory, so the
// machine needs NO data-plane reads to execute). This file defines the
// Daemon-side parse of that payload.

import (
	"encoding/json"
	"strings"
)

// RepoEntry is one inventory repo as frozen in the WI creation transaction
// (02 §4.6): {name, url, subdir?, base}. Base is immutable after creation.
// Subdir is declared/frozen but NOT consumed by the v1 Capsule layout —
// see the SUBDIR RULING in inventory.go (full-repo checkout is the
// intended v1 shape; subdir stays reserved metadata).
type RepoEntry struct {
	Position int64   `json:"position"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Subdir   *string `json:"subdir"`
	Base     string  `json:"base"`
}

// Bootstrap is the claim response's capsule_bootstrap payload.
type Bootstrap struct {
	WorkItemID             string          `json:"work_item_id"`
	BundleName             string          `json:"bundle_name"`
	BundleDigest           string          `json:"bundle_digest"`
	WorkflowBudgetBaseline json.RawMessage `json:"workflow_budget_baseline"`
	RepoInventory          []RepoEntry     `json:"repo_inventory"`
}

// ParseBootstrap decodes the raw capsule_bootstrap JSON from a claim /
// force-takeover response.
func ParseBootstrap(raw json.RawMessage) (*Bootstrap, error) {
	if len(raw) == 0 {
		return nil, errf("CAP-400", "empty capsule_bootstrap")
	}
	var b Bootstrap
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, errf("CAP-400", "capsule_bootstrap unparsable: %v", err)
	}
	if b.WorkItemID == "" {
		return nil, errf("CAP-400", "capsule_bootstrap lacks work_item_id")
	}
	// NOTE: an EMPTY inventory is legal here: 02 §4.6 (the storage authority
	// 03 §4.1 defers to) pins "缺省为空数组（非仓库类 WI）" — non-repository
	// WIs carry zero entries by design. Only the > 8 upper bound is enforced
	// here; the 1..8 range of 03 §4.1 describes the repo-backed declaration,
	// not the non-repo class.
	if len(b.RepoInventory) > 8 {
		// 03 §4.1: at most 8 entries.
		return nil, errf("CAP-400", "repo inventory has %d entries (max 8)", len(b.RepoInventory))
	}
	if err := validateInventoryNames(b.RepoInventory); err != nil {
		return nil, err
	}
	return &b, nil
}

// validateInventoryNames enforces the per-entry name rules both the parse
// and the checkout must hold (03 §4.1/§4.3): a repo name becomes the path
// segment <capsule>/repos/<name>, so it must be a SINGLE safe segment — no
// separators, no "."/".." traversal, no NUL — and unique within the WI
// (02 §4.6 UNIQUE(work_item_id, name); a duplicate would alias one
// checkout under two declarations).
func validateInventoryNames(entries []RepoEntry) error {
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Name == "" || e.URL == "" || e.Base == "" {
			return errf("CAP-400", "repo inventory entry %q lacks name/url/base", e.Name)
		}
		if e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\\\x00") {
			return errf("CAP-400", "repo inventory name %q is not a single safe path segment", e.Name)
		}
		if seen[e.Name] {
			return errf("CAP-400", "repo inventory name %q declared twice", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}
