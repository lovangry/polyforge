package daemon

// ship.go: the wrap→git delivery bridge (04 §5.1 ship row, baseline
// amendment polyforge#37; v1 shape). After a WI closes, its task-repo
// commits live only in the sealed capsule; ship reads the sealed bytes
// back out and pushes them to each inventory repo's git origin as the
// fixed-name branch polyforge/<wi>, then layers a PR on top when a PR
// credential exists (a missing PR credential is NOT an error; only the
// push credential is — DAEMON-512).
//
// v1 inventory: the capsule's repos/<name>/ subdirectories that are git
// worktrees (03: task repo checkouts carry their own histories); each
// one's origin URL is read from its own git config. READ-ONLY capsule
// access: the sealed scene stays read-only-reachable for ship, the
// write ban holds. The one command class that touches the capsule is
//
//	git -C <repo> -c core.hooksPath= push <origin-url> <sha>:refs/heads/<branch>
//
// which writes nothing under the capsule (explicit URL, raw-oid refspec,
// hooks neutralized). No aihub credential or endpoint is involved.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

const ghTimeout = 60 * time.Second

// shipError is one typed ship verdict; the route maps it onto the
// api.Error envelope verbatim.
type shipError struct {
	Code   string
	Detail string
}

func (e *shipError) Error() string { return "ship: " + e.Code + ": " + e.Detail }

func shipErr(code, format string, args ...any) *shipError {
	return &shipError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func (e *shipError) apiErr() *api.Error {
	return &api.Error{Code: e.Code, Detail: e.Detail}
}

type shipRepoOutcome struct {
	Name       string `json:"name"`
	OriginURL  string `json:"origin_url"`
	Head       string `json:"head"`
	Branch     string `json:"branch"`
	Pushed     bool   `json:"pushed"`
	Reused     bool   `json:"reused"`
	PRURL      string `json:"pr_url,omitempty"`
	CompareURL string `json:"compare_url,omitempty"`
}

type shipResponse struct {
	WorkItemID string            `json:"work_item_id"`
	Branch     string            `json:"branch"`
	Initiator  string            `json:"initiator,omitempty"`
	Repos      []shipRepoOutcome `json:"repos"`
}

func shipBranch(wi string) string { return "polyforge/" + wi }

// neverPushTarget structurally enforces that the delivery branch can
// never be a reserved name (main/master), however the branch computation
// is refactored later.
func neverPushTarget(branch string) bool {
	b := strings.TrimPrefix(branch, "refs/heads/")
	if i := strings.LastIndexByte(b, '/'); i >= 0 {
		b = b[i+1:]
	}
	return b == "main" || b == "master"
}

func (d *Daemon) shipRoute(w http.ResponseWriter, r *http.Request) {
	wi := d.canonWI(r.PathValue("wi")) // F2: the sealed capsule is canonical-named
	initiator := r.Header.Get("X-PF-User")
	out, err := d.Ship(r.Context(), wi, initiator)
	if err != nil {
		var se *shipError
		if errors.As(err, &se) {
			ae := se.apiErr()
			writeJSON(w, ae.HTTPStatus(), ae)
			return
		}
		writeErrOf(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Ship executes the ship operation: a purely local read of the sealed
// capsule plus one push per inventory repo to that repo's own origin.
func (d *Daemon) Ship(ctx context.Context, wi, initiator string) (*shipResponse, error) {
	branch := shipBranch(wi)
	if neverPushTarget(branch) {
		return nil, shipErr(api.ErrDaemonForbidden, "reserved branch target %q: ship never pushes main/master", branch)
	}
	if _, ok := d.rt.Info(wi); ok {
		return nil, shipErr(api.ErrDaemonConflict, "work item %s is still active on this daemon; ship applies to a sealed capsule only", wi)
	}
	// polyforge#113: the sealed capsule lives under its project's mapped
	// workspace. Resolve it across the mapped workspace set: the inverse
	// mapping first (a hub read names the WI's project), then any mapped
	// workspace whose capsules/<wi> exists — none → the existing
	// not-found shape. Never a guess outside the mapping.
	capDir := ""
	if item, err := d.hub.GetWorkItem(wi); err == nil && item != nil && item.ProjectName != "" {
		if ws, merr := d.cfg.Mapping.WorkspaceFor(item.ProjectName); merr == nil {
			capDir = filepath.Join(ws, "capsules", wi)
		}
	}
	if capDir == "" {
		for _, ws := range d.cfg.Mapping.Workspaces() {
			candidate := filepath.Join(ws, "capsules", wi)
			if st, err := os.Stat(candidate); err == nil && st.IsDir() {
				capDir = candidate
				break
			}
		}
	}
	if capDir != "" {
		if st, err := os.Stat(capDir); err != nil || !st.IsDir() {
			capDir = "" // the inverse-mapped base lost the scene: fall through to the set-wide probe result below
		}
	}
	if capDir == "" {
		return nil, shipErr(api.ErrDaemonNotFound, "no local sealed capsule for %s on this machine", wi)
	}
	names, err := inventory(capDir)
	if err != nil {
		return nil, shipErr(api.ErrDaemonInternal, "sealed capsule of %s is not shippable: %v", wi, err)
	}
	out := &shipResponse{WorkItemID: wi, Branch: branch, Initiator: initiator, Repos: []shipRepoOutcome{}}
	for _, name := range names {
		oc, err := d.shipOneRepo(ctx, capDir, name, branch, wi)
		if err != nil {
			return nil, err // first typed failure fails the op: no partial answer, no force-push
		}
		// The ship ledger (polyforge#123, P1b): record this repo's
		// branch-top SHA in aihub — accounting, not best-effort. A
		// recording failure fails the ship op with the typed DAEMON-514;
		// the pushes already made stay (git is the source of truth), but
		// the op never answers success while a row is missing.
		if err := d.hub.RecordShipLedger(wi, api.ShipLedgerRequest{
			Branch: branch,
			Repos:  []api.ShipLedgerEntry{{Repo: name, BranchTopSHA: oc.Head}},
		}); err != nil {
			return nil, shipErr(api.ErrDaemonShipLedger,
				"repo %s: the aihub ship-ledger recording failed — the ledger is accounting, not best-effort: %v", name, err)
		}
		out.Repos = append(out.Repos, *oc)
	}
	return out, nil
}

// inventory lists the capsule's repo worktrees (repos/<name> holding a
// .git entry), name-sorted for a stable answer.
func inventory(capDir string) ([]string, error) {
	root := filepath.Join(capDir, "repos")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // a capsule without task repos ships an empty set
		}
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), ".git")); err == nil {
			names = append(names, e.Name())
		}
	}
	// deterministic order
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names, nil
}

func (d *Daemon) shipOneRepo(ctx context.Context, capDir, name, branch, wi string) (*shipRepoOutcome, error) {
	dir := filepath.Join(capDir, "repos", name)
	origin, err := gitCombined(ctx, dir, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) == "" {
		return nil, shipErr(api.ErrDaemonShipPreflight, "repo %s: no origin remote in the sealed worktree", name)
	}
	origin = firstLine(origin)
	headOut, err := gitCombined(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, shipErr(api.ErrDaemonInternal, "repo %s: sealed worktree HEAD unreadable: %v", name, err)
	}
	head := firstLine(headOut)
	oc := &shipRepoOutcome{Name: name, OriginURL: origin, Head: head, Branch: branch}
	// Remote state, read WITHOUT touching the capsule (no -C at all).
	lsOut, err := gitCombined(ctx, "", "ls-remote", origin, "refs/heads/"+branch)
	if err != nil {
		return nil, shipErr(api.ErrDaemonShipPreflight,
			"repo %s: push preflight failed — origin %s unreachable or no usable git push identity: %s",
			name, origin, strings.TrimSpace(lsOut))
	}
	if remote := firstField(lsOut); remote != "" {
		if remote == head {
			oc.Reused = true // idempotent hit: the branch is already at the sealed commit
		} else {
			return nil, shipErr(api.ErrDaemonConflict,
				"repo %s: remote branch %s has moved to %s ≠ sealed commit %s; ship never force-pushes a moved branch",
				name, branch, remote, head)
		}
	} else {
		pushOut, perr := gitCombined(ctx, dir, "-c", "core.hooksPath=", "push", origin, head+":refs/heads/"+branch)
		if perr != nil {
			if gitops.IsNonFastForward(perr, pushOut) {
				return nil, shipErr(api.ErrDaemonConflict,
					"repo %s: remote branch %s moved past the sealed commit during the push; ship never force-pushes: %s",
					name, branch, strings.TrimSpace(pushOut))
			}
			return nil, shipErr(api.ErrDaemonShipPreflight,
				"repo %s: push failed — no usable git push identity or origin unreachable: %s",
				name, strings.TrimSpace(pushOut))
		}
		oc.Pushed = true
	}
	oc.PRURL, oc.CompareURL = prLayer(ctx, origin, branch, wi, head)
	return oc, nil
}

func gitCombined(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Env = gitops.CEnv(nil)
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func firstField(s string) string {
	s = firstLine(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// PR layering: gh when present AND usable; compare URL otherwise. A
// missing PR credential NEVER fails the op.

func prLayer(ctx context.Context, originURL, branch, wi, head string) (prURL, compareURL string) {
	owner, repo, ok := parseGitHubRepo(originURL)
	if !ok {
		return "", compareFallback(originURL, branch)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", compareFallback(originURL, branch)
	}
	gctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	// Reuse: an existing PR for this head branch is re-attached verbatim
	// (idempotent ship reuses the branch AND its PR).
	if url := ghPRView(gctx, owner, repo, branch); url != "" {
		return url, ""
	}
	title := "ship " + wi + ": " + branch
	body := "Sealed capsule delivery for work item " + wi + " (branch " + branch + ", sealed commit " + head + ")."
	if url := ghPRCreate(gctx, owner, repo, branch, title, body); url != "" {
		return url, ""
	}
	// gh present but not usable (no auth, API refused, ...): the machine
	// does not fail for lack of a PR credential.
	return "", compareFallback(originURL, branch)
}

func ghPRView(ctx context.Context, owner, repo, branch string) string {
	out, err := ghCombined(ctx, "pr", "view", branch, "--repo", owner+"/"+repo, "--json", "url")
	if err != nil {
		return ""
	}
	var v struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &v) != nil || v.URL == "" {
		return ""
	}
	return v.URL
}

func ghPRCreate(ctx context.Context, owner, repo, branch, title, body string) string {
	out, err := ghCombined(ctx, "pr", "create", "--repo", owner+"/"+repo,
		"--head", branch, "--title", title, "--body", body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func ghCombined(ctx context.Context, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "gh", args...)
	out, err := c.CombinedOutput()
	return string(out), err
}

func parseGitHubRepo(originURL string) (owner, repo string, ok bool) {
	s := originURL
	if strings.HasPrefix(s, "https://github.com/") {
		s = strings.TrimPrefix(s, "https://github.com/")
	} else if strings.HasPrefix(s, "git@github.com:") {
		s = strings.TrimPrefix(s, "git@github.com:")
	} else {
		return "", "", false
	}
	s = strings.TrimSuffix(s, ".git")
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func compareFallback(originURL, branch string) string {
	if owner, repo, ok := parseGitHubRepo(originURL); ok {
		return "https://github.com/" + owner + "/" + repo + "/compare/main..." + branch
	}
	return originURL
}
