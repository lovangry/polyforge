package daemon

// ship.go: the ship(wi_id) operation (04 §5.1 ship row, baseline amendment
// polyforge#37 / PR #39) — the wrap→git delivery bridge. After a WI has
// wrapped, its git commits live only in the sealed capsule (detached HEAD)
// and the Revision chain; ship reads the sealed bytes back out and pushes
// them to each inventory repo's git origin as the fixed-name branch
// polyforge/<wi-slug>, then layers a PR on top when a PR credential exists.
//
// Preflight (both checked BEFORE any push is initiated):
//   - this machine holds the WI's sealed capsule → otherwise DAEMON-404;
//   - the local git push identity / origin reachability hold → otherwise
//     DAEMON-512 (the push attempt itself is the preflight: an auth or
//     transport failure maps to 512 with the git verdict in the detail).
//
// Semantics:
//   - the target branch is ALWAYS polyforge/<wi-slug> (v1: the wi id IS the
//     slug — 00 §3.4a has no separate slug resolution on the wire);
//   - main/master are NEVER pushed: a reserved-name target is refused
//     DAEMON-403 before the remote is ever contacted;
//   - idempotency: remote branch already at the sealed commit → the branch
//     is reused and the PR (or compare URL) re-attached, success; remote
//     branch MOVED past the sealed commit → DAEMON-409, never a force-push;
//   - PR layering: gh CLI present and authenticated → open/reuse a PR and
//     carry its URL; otherwise the response carries a compare URL and the
//     op STILL SUCCEEDS — a missing PR credential is never an error (the
//     PR credential and the push credential are two independent premises;
//     only the latter feeds DAEMON-512).
//
// READ-ONLY capsule access (03 §2.1 amendment: the sealed scene stays
// read-only-reachable for ship, the write ban holds — ship is NOT a write
// exception channel). The whole op touches the capsule with exactly one
// command class:
//
//	git -C <capsule-repo> -c core.hooksPath= push <origin-url> <sha>:refs/heads/<branch>
//
// which writes NOTHING under the capsule:
//   - pushing to an EXPLICIT URL (never a named remote) updates no local
//     remote-tracking refs and touches no local config;
//   - the refspec's source is a raw object id, its destination lives on the
//     remote side — no local ref moves;
//   - `-c core.hooksPath=` disables the pre-push hook, the only local code
//     a push would otherwise run inside the source repository;
//   - git's push path reads objects and config only; it creates no lock
//     file in the source repository.
// The remaining capsule reads (rev-parse HEAD, repos/inventory.json) are
// pure reads. `git ls-remote` is run WITHOUT -C from the daemon's own cwd:
// it does not touch any local repository at all.
//
// The push credential is the machine's EXISTING git identity (SSH agent /
// credential helper, inherited via the daemon process environment — the
// same identity the capsule checkout already required); ship stores no
// credential bytes and never touches the aihub credential domain.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// ErrCodeShipPreflight is DAEMON-512 SHIP_PUSH_PREFLIGHT_FAILED (04 §5.2,
// baseline amendment polyforge#37): no usable local git push identity
// (SSH/credential helper) or the origin is unreachable — the push was not
// initiated. Strictly distinct from a missing PR credential, which is NOT
// an error (compare-URL fallback, still a success).
const ErrCodeShipPreflight = "DAEMON-512"

// ghTimeout bounds every gh CLI invocation: a hanging gh (interactive
// prompt on a weird terminal, wedged auth) must never hang the ship op.
const ghTimeout = 60 * time.Second

// ShipError is one typed ship verdict (DAEMON-404/403/409/512/400/500);
// the handler maps Code→HTTP status, Detail→the envelope's detail.
type ShipError struct {
	Code   string
	Detail string
}

func (e *ShipError) Error() string { return "ship: " + e.Code + ": " + e.Detail }

func shipErr(code, format string, args ...any) *ShipError {
	return &ShipError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ShipRepoOutcome is the per-repo result of one ship call.
type ShipRepoOutcome struct {
	Name      string `json:"name"`
	OriginURL string `json:"origin_url"`
	Head      string `json:"head"` // the sealed commit this machine pushed
	Branch    string `json:"branch"`
	// Pushed: a push happened in THIS call (the remote branch did not
	// exist at the sealed commit yet).
	Pushed bool `json:"pushed"`
	// Reused: the remote branch was already at the sealed commit — the
	// idempotent path; nothing was pushed.
	Reused bool `json:"reused"`
	// PRURL is set when a PR was opened or reused via the gh CLI;
	// CompareURL is the URL-fallback carried when no PR credential was
	// available. Exactly one of the two is set on success.
	PRURL      string `json:"pr_url,omitempty"`
	CompareURL string `json:"compare_url,omitempty"`
}

// ShipOutcome is the ship(wi_id) response: one line per inventory repo.
type ShipOutcome struct {
	WorkItemID string `json:"work_item_id"`
	Branch     string `json:"branch"`
	// Initiator is the self-declared X-PF-User identity (the same v1
	// degraded-local-trust ruling as pause/wrap, 04 §5.1).
	Initiator string            `json:"initiator,omitempty"`
	Repos     []ShipRepoOutcome `json:"repos"`
}

// shipBranch is the fixed-name delivery branch: polyforge/<wi-slug>. The
// wi id IS the slug in v1 (server-minted wi_+random ids, validated to the
// branch-safe alphabet by capsule id validation).
func shipBranch(wiID string) string { return "polyforge/" + wiID }

// neverPushTarget reports whether branch names a reserved target the ship
// bridge must never create or move: main or master — as the full ref name
// or as its last path segment. The fixed-name scheme (polyforge/<slug>)
// can never produce these by construction; this guard is the structural
// enforcement so no future refactor of the branch computation can regress
// into pushing a reserved name. A wi id of main/master trips it.
func neverPushTarget(branch string) bool {
	b := strings.TrimPrefix(branch, "refs/heads/")
	if b == "main" || b == "master" {
		return true
	}
	if i := strings.LastIndexByte(b, '/'); i >= 0 {
		b = b[i+1:]
	}
	return b == "main" || b == "master"
}

// Ship executes the ship(wi_id) operation for one work item (04 §5.1).
// It is a purely LOCAL operation: the sealed capsule on this machine is
// the only execution-scene input, git origin is the only remote touched,
// and no aihub credential or endpoint is involved.
func (d *Daemon) Ship(ctx context.Context, wi, initiator string) (*ShipOutcome, error) {
	branch := shipBranch(wi)
	// Reserved-name refusal BEFORE anything else — in particular before
	// the remote is ever contacted (04 §5.1: 永不推送 main/master →
	// DAEMON-403).
	if neverPushTarget(branch) {
		return nil, shipErr(ErrCodeForbidden, "reserved branch target %q: ship never pushes main/master", branch)
	}
	// Local sealed capsule (03 §2.1: sealed scenes stay read-only reachable
	// for ship). A wi with no capsule on this machine answers DAEMON-404.
	capc, _, err := capsule.Open(d.cfg.WorkspaceRoot, wi)
	if err != nil {
		var capErr *capsule.Error
		if errors.As(err, &capErr) && capErr.Code == "CAP-400" {
			return nil, shipErr("DAEMON-400", "invalid work item id: %v", err)
		}
		return nil, shipErr(ErrCodeNotFound, "no local sealed capsule for %s on this machine", wi)
	}
	inv, err := loadShipInventory(capc)
	if err != nil {
		return nil, shipErr("DAEMON-500", "sealed capsule of %s is not shippable: %v", wi, err)
	}
	out := &ShipOutcome{WorkItemID: wi, Branch: branch, Initiator: initiator, Repos: []ShipRepoOutcome{}}
	for _, r := range inv.Repos {
		oc, err := d.shipOneRepo(ctx, capc, r, branch, wi)
		if err != nil {
			return nil, err // first typed failure fails the op (no force, no partial answer)
		}
		out.Repos = append(out.Repos, *oc)
	}
	return out, nil
}

// loadShipInventory reads the sealed capsule's repos/inventory.json (the
// checkout snapshot 03 §4.3 wrote; ship reads it back for the origin URLs
// and repo names — it never re-derives them). Reading a 0444 file in a
// 0555 tree is exactly the sealed-scene read access ship is granted.
func loadShipInventory(capc *capsule.Capsule) (*capsule.InventorySnapshot, error) {
	raw, err := os.ReadFile(filepath.Join(capc.Dir, "repos", "inventory.json"))
	if err != nil {
		return nil, fmt.Errorf("repos/inventory.json unreadable: %w", err)
	}
	var snap capsule.InventorySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("repos/inventory.json unparsable: %w", err)
	}
	return &snap, nil
}

// shipOneRepo ships one inventory repo: read the sealed worktree HEAD,
// compare it with the remote branch, push (or reuse), then layer the PR.
func (d *Daemon) shipOneRepo(ctx context.Context, capc *capsule.Capsule, r capsule.InventoryRepo, branch, wi string) (*ShipRepoOutcome, error) {
	dir := filepath.Join(capc.Dir, "repos", r.Name)
	// The sealed commit: the worktree HEAD the WI Agent left behind.
	// rev-parse is a pure read.
	headOut, err := gitCombined(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, shipErr("DAEMON-500", "repo %s: sealed worktree HEAD unreadable: %v", r.Name, err)
	}
	head := firstShipLine(headOut)
	if head == "" {
		return nil, shipErr("DAEMON-500", "repo %s: sealed worktree HEAD is empty", r.Name)
	}
	oc := &ShipRepoOutcome{Name: r.Name, OriginURL: r.URL, Head: head, Branch: branch}
	// Remote state, read WITHOUT touching the capsule (no -C at all).
	// An unreachable origin / unusable transport is the DAEMON-512
	// preflight failure — the push is never initiated.
	lsOut, err := gitCombined(ctx, "", "ls-remote", r.URL, "refs/heads/"+branch)
	if err != nil {
		return nil, shipErr(ErrCodeShipPreflight, "repo %s: push preflight failed — origin %s unreachable or no usable git push identity: %s",
			r.Name, r.URL, strings.TrimSpace(lsOut))
	}
	if remote := firstShipField(lsOut); remote != "" {
		switch remote {
		case head:
			// Idempotent hit: the remote branch is already at the sealed
			// commit — reuse it, attach the PR/compare layer, succeed.
			oc.Reused = true
		default:
			// The remote branch moved past the sealed commit: a typed
			// refusal — ship NEVER force-pushes over a moved branch.
			return nil, shipErr(ErrCodeConflict,
				"repo %s: remote branch %s has moved to %s ≠ sealed commit %s; ship never force-pushes a moved branch",
				r.Name, branch, remote, head)
		}
	} else {
		// No remote branch yet: the push IS the preflight — an auth or
		// transport failure surfaces as DAEMON-512 with git's verdict.
		// Read-only push: explicit URL source repo never written (see the
		// file header for the full argument).
		pushOut, perr := gitCombined(ctx, dir, "-c", "core.hooksPath=", "push", r.URL, head+":refs/heads/"+branch)
		if perr != nil {
			if isNonFastForward(pushOut) {
				// Lost a race: the branch moved between ls-remote and the
				// push. Same verdict as the moved-branch check above.
				return nil, shipErr(ErrCodeConflict,
					"repo %s: remote branch %s moved past the sealed commit during the push; ship never force-pushes: %s",
					r.Name, branch, strings.TrimSpace(pushOut))
			}
			return nil, shipErr(ErrCodeShipPreflight, "repo %s: push failed — no usable git push identity or origin unreachable: %s",
				r.Name, strings.TrimSpace(pushOut))
		}
		oc.Pushed = true
	}
	// PR layering — NEVER fails the op (04 §5.1: a missing PR credential
	// is not an error; the compare-URL fallback keeps the op a success).
	oc.PRURL, oc.CompareURL = prLayer(ctx, r.URL, branch, wi, head)
	return oc, nil
}

// isNonFastForward recognizes git's rejection wording for a push onto a
// branch that has moved (the late-discovered variant of the moved-branch
// refusal).
func isNonFastForward(out string) bool {
	return strings.Contains(out, "non-fast-forward") ||
		strings.Contains(out, "fetch first") ||
		strings.Contains(out, "(rejected)")
}

// gitCombined runs one git command (dir == "" → the daemon's own cwd, no
// repository involved) and returns the combined output. Errors carry the
// combined output so callers can surface git's own verdict.
func gitCombined(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

// firstShipLine trims a git command's output to its first line.
func firstShipLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// firstShipField takes the first whitespace field (ls-remote prints
// "<sha>\t<ref>"; absent refs print nothing).
func firstShipField(s string) string {
	s = firstShipLine(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// PR layering (04 §5.1): gh when present AND authenticated; compare URL
// otherwise. Two independent premises from the push credential — only
// the latter can ever produce DAEMON-512.

// prLayer resolves the URL layer for one shipped repo: a PR URL when the
// gh CLI is available, usable, and the origin is a GitHub repository;
// otherwise the compare-URL fallback. Every gh failure (absent binary, no
// auth, API error, non-GitHub origin) degrades to the fallback — never an
// op failure.
func prLayer(ctx context.Context, originURL, branch, wi, head string) (prURL, compareURL string) {
	owner, repo, ok := parseGitHubRepo(originURL)
	if !ok {
		// Non-GitHub origin (local path, GitLab, ...): gh is GitHub-only,
		// so there is no PR face to try at all.
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
	// Open: title/body preclude any interactive prompt.
	if url := ghPRCreate(gctx, owner, repo, branch, wi, head); url != "" {
		return url, ""
	}
	// gh present but not usable (no auth, API refused, ...): the machine
	// does not fail for lack of a PR credential.
	return "", compareFallback(originURL, branch)
}

// ghCombined runs one gh CLI command and returns the combined output.
// The daemon inherits its environment, so gh reads whatever PR credential
// the machine already has (GH_TOKEN, gh auth hosts file) — ship stores
// nothing.
func ghCombined(ctx context.Context, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "gh", args...)
	out, err := c.CombinedOutput()
	return string(out), err
}

// ghPRView returns the URL of the existing PR for head branch, or "".
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

// ghPRCreate opens a PR for the pushed branch and returns its URL, or "".
func ghPRCreate(ctx context.Context, owner, repo, branch, wi, head string) string {
	title := "ship " + wi + ": " + branch
	body := "Sealed capsule delivery for work item " + wi + " (branch " + branch + ", sealed commit " + head + ")."
	out, err := ghCombined(ctx, "pr", "create",
		"--repo", owner+"/"+repo, "--head", branch, "--title", title, "--body", body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// parseGitHubRepo extracts owner/repo from a GitHub origin URL (https or
// ssh forms, optional .git suffix). Non-GitHub origins answer ok=false.
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

// compareFallback builds the URL-fallback carried when no PR was opened:
// a GitHub compare link against main when the origin is a GitHub repo,
// otherwise the origin URL itself (the one place a human can compare the
// pushed branch against whatever the remote defaults to).
func compareFallback(originURL, branch string) string {
	if owner, repo, ok := parseGitHubRepo(originURL); ok {
		return "https://github.com/" + owner + "/" + repo + "/compare/main..." + branch
	}
	return originURL
}
