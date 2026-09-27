package capsule

// bootstrap.go: the capsule_bootstrap payload arrives in the claim response
// (02 §8: bundle reference + budget baseline + repo inventory, so the
// machine needs NO data-plane reads to execute). This file defines the
// Daemon-side parse of that payload.

import "encoding/json"

// RepoEntry is one inventory repo as frozen in the WI creation transaction
// (02 §4.6): {name, url, subdir?, base}. Base is immutable after creation.
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
	if len(b.RepoInventory) > 8 {
		// 03 §4.1: 1 ≤ entries ≤ 8.
		return nil, errf("CAP-400", "repo inventory has %d entries (max 8)", len(b.RepoInventory))
	}
	for _, e := range b.RepoInventory {
		if e.Name == "" || e.URL == "" || e.Base == "" {
			return nil, errf("CAP-400", "repo inventory entry %q lacks name/url/base", e.Name)
		}
	}
	return &b, nil
}
