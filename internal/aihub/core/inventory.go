package core

// WI repo inventory (02 §4.6): the repos one work item declares it works
// in. It is stored as ONE JSON array in work_items.repo_inventory (NULL =
// no inventory), shaped like projects.repos: [{name, url, subdir?}].
// Frozen at create — there is no UPDATE path (a changed inventory means a
// new WI). The claim answer serves it instead of the project's full repo
// list, so a ticket only clones the repos it declared.
//
// A frozen base commit per repo is NOT supported: nothing on the work
// machine checks one out, so a `base` is refused at create instead of
// being stored and ignored.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// RepoDecl is one repo_inventory entry. URL may be omitted on create:
// the entry then names one of the project's repos and takes its URL from
// projects.repos. Base exists on the type only so a create carrying one
// is refused (INVENTORY_BASE_UNSUPPORTED) instead of silently dropped;
// a stored entry never has one.
type RepoDecl struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Subdir *string `json:"subdir,omitempty"`
	Base   string  `json:"base,omitempty"`
}

// maxRepoInventory is the §4.6 cardinality bound: 1..8 entries per WI.
const maxRepoInventory = 8

// resolveRepoInventory validates a create's declared inventory against
// the §4.6 rules and fills each URL-less entry from the project's repos.
// It answers the array to freeze (nil when none was declared).
func resolveRepoInventory(inv []RepoDecl, projectRepos []api.ProjectRepo) ([]RepoDecl, error) {
	if len(inv) == 0 {
		return nil, nil
	}
	if len(inv) > maxRepoInventory {
		return nil, Fail(api.ErrCoreBadInput, "", "repo_inventory allows 1..8 entries")
	}
	urlOf := map[string]string{}
	for _, pr := range projectRepos {
		urlOf[pr.Name] = pr.URL
	}
	out := make([]RepoDecl, 0, len(inv))
	seen := map[string]bool{}
	for _, r := range inv {
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			return nil, Fail(api.ErrCoreBadInput, "", "repo_inventory entry without a name")
		}
		if seen[r.Name] {
			return nil, Fail(api.ErrCoreBadInput, "INVENTORY_DUPLICATE_NAME", "duplicate inventory name %q", r.Name)
		}
		seen[r.Name] = true
		if r.Base != "" {
			return nil, Fail(api.ErrCoreBadInput, "INVENTORY_BASE_UNSUPPORTED",
				"repo %q: a frozen base commit is not supported; omit base (the work machine clones the repo's default branch)", r.Name)
		}
		if r.URL == "" {
			u, ok := urlOf[r.Name]
			if !ok {
				return nil, Fail(api.ErrCoreBadInput, "REPO_NOT_IN_PROJECT",
					"repo %q is not one of the project's repos; name a project repo or give its url", r.Name)
			}
			r.URL = u
		}
		out = append(out, r)
	}
	return out, nil
}

// repoInventoryJSON is the column value of a resolved inventory: NULL
// when none was declared.
func repoInventoryJSON(inv []RepoDecl) (any, error) {
	if len(inv) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(inv)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// repoInventoryOf reads a WI's frozen inventory in declaration order
// (nil when it declared none).
func repoInventoryOf(q queryRower, wiID string) ([]RepoDecl, error) {
	var raw sql.NullString
	err := q.QueryRow(`SELECT repo_inventory FROM work_items WHERE id=?1`, wiID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
	}
	if err != nil || !raw.Valid || raw.String == "" {
		return nil, err
	}
	var inv []RepoDecl
	if err := json.Unmarshal([]byte(raw.String), &inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// claimReposTx answers the claim's task repos: the WI's inventory when
// it declared one, else every repo of its project (WIs filed without an
// inventory keep the pre-§4.6 behavior).
func claimReposTx(tx *sql.Tx, wiID, projectID string) ([]api.ProjectRepo, error) {
	inv, err := repoInventoryOf(tx, wiID)
	if err != nil {
		return nil, err
	}
	if len(inv) == 0 {
		return projectReposTx(tx, projectID)
	}
	repos := make([]api.ProjectRepo, 0, len(inv))
	for _, r := range inv {
		repos = append(repos, api.ProjectRepo{Name: r.Name, URL: r.URL})
	}
	return repos, nil
}
