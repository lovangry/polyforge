package test

// v1_s19_ship_pr_test.go: the ship PR layer e2e against REAL github.com,
// driver/envelope edition. s04 pins the push layer against a local bare
// origin, so the PR layer (gh present → gh pr view reuse → gh pr create)
// is never reached, and v0 usage_s10 pins only its compare-URL fallback.
// This scenario runs the real thing: a private scratch repo created via
// gh under the authenticated account, a WI whose driver-pinned workflow
// lays the inventory repo (the fix step's envelope action) and completes,
// then the real `pf --json ship` — asserting the PR actually opens on
// github.com with head branch polyforge/<wi>, and that a second ship
// reuses the SAME single OPEN PR instead of duplicating it.
//
// The scenario SKIPS (never hard-fails) when a gate is missing:
// testing.Short() (no network in the short lane), the gh binary absent,
// or gh not authenticated. Each skip message names the missing gate.
// (The legacy lane's requireLegacyLane early guard is GONE: a
// driver-pinned scenario has no legacy lane to guard — the environmental
// gates are the whole story.)
import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// ghRun runs the real gh CLI and fails the test on a non-zero exit.
func ghRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("gh", args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ghScratchRepo creates a private github.com scratch repo under the
// authenticated account (name pf-e2e-<timestamp>), seeds a non-empty
// main baseline, and runs `gh auth setup-git` so pushes to the https
// origin resolve their credentials via gh's credential helper (the
// daemon inherits this machine's git configuration). The repo is
// deleted via t.Cleanup even when the test fails.
func ghScratchRepo(t *testing.T) (owner, repo, originURL string) {
	t.Helper()
	owner = ghRun(t, "api", "user", "--jq", ".login")
	repo = fmt.Sprintf("pf-e2e-%d", time.Now().UnixNano())
	full := owner + "/" + repo
	// Rate limiting is an availability gate, not a failure: repeated CI
	// runs trip github's repo-creation quota ("You have created too many
	// repositories, too quickly") — same skip-not-fail contract as a
	// missing gh binary or auth.
	if out, err := exec.Command("gh", "repo", "create", full, "--private").CombinedOutput(); err != nil {
		if strings.Contains(strings.ToLower(string(out)), "too many repositories") ||
			strings.Contains(strings.ToLower(string(out)), "rate limit") {
			t.Skipf("gh repo creation rate-limited (availability gate): %s", out)
		}
		t.Fatalf("gh repo create %s: %v: %s", full, err, out)
	}
	// Delete the scratch repo no matter how the test ends. Best effort:
	// a token without the delete_repo scope cannot delete (loud log, so
	// a leak is visible in the test output rather than silent).
	t.Cleanup(func() {
		out, err := exec.Command("gh", "repo", "delete", full, "--yes").CombinedOutput()
		if err != nil {
			t.Logf("cleanup: could not delete scratch repo %s (gh needs the delete_repo scope for that): %v: %s",
				full, err, strings.TrimSpace(string(out)))
		}
	})
	// The push credential for the https origin: gh's git credential
	// helper, wired into git's global configuration.
	ghRun(t, "auth", "setup-git")

	// Seed a non-empty main baseline (the PR needs a base branch).
	seed := t.TempDir()
	gitRun(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"),
		[]byte("pf e2e v1_s19 ship PR scratch repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "baseline for pf e2e ship PR")
	originURL = "https://github.com/" + full + ".git"
	gitRun(t, seed, "remote", "add", "origin", originURL)
	gitRun(t, seed, "push", "-u", "origin", "main")
	return owner, repo, originURL
}

// ghViewPR reads state, headRefName and url of one branch's PR.
func ghViewPR(t *testing.T, fullRepo, branch string) (state, headRefName, url string) {
	t.Helper()
	out := ghRun(t, "pr", "view", branch, "--repo", fullRepo,
		"--json", "state,headRefName,url")
	var pr struct {
		State       string `json:"state"`
		HeadRefName string `json:"headRefName"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		t.Fatalf("gh pr view answer undecodable: %v (%q)", err, out)
	}
	return pr.State, pr.HeadRefName, pr.URL
}

// ghOpenPRURLs lists the OPEN PR urls for one head branch.
func ghOpenPRURLs(t *testing.T, fullRepo, branch string) []string {
	t.Helper()
	out := ghRun(t, "pr", "list", "--repo", fullRepo, "--head", branch,
		"--state", "open", "--json", "url", "--limit", "10")
	var prs []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		t.Fatalf("gh pr list answer undecodable: %v (%q)", err, out)
	}
	urls := make([]string, 0, len(prs))
	for _, p := range prs {
		urls = append(urls, p.URL)
	}
	return urls
}

func TestV1S19ShipPR(t *testing.T) {
	// The environmental gates, in order; each skip names the missing
	// gate. This scenario performs real external work (gh auth, a real
	// GitHub scratch repository, credential git-config) before any
	// daemon boots — the gates run first thing, exactly as before.
	requireGh(t)

	// The real github.com origin: a private scratch repo this test owns.
	owner, repoName, origin := ghScratchRepo(t)
	fullRepo := owner + "/" + repoName

	inst := newV1Install(t)
	wi := inst.newWI(t, "s19: wrap then ship to real github and open the PR",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			// The fix step's envelope action lays the inventory repo (a
			// real worktree with its own github origin) before reporting
			// done.
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true,
				"name": "demo", "origin": origin},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst, optUseDriver())
	d.start()
	d.pf(t, "submit", wi)
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	// The driver-era completion face: the whole chain done before the
	// ship.
	waitWorkflowStatus(t, d, wi, "completed", 30*time.Second)
	env := archivedEnvelope(t, d.capsuleDir(wi), "fix", 2)
	if env["outcome"] != "completed" {
		t.Fatalf("fix envelope wrong: %v", env["outcome"])
	}

	// The sealed worktree carries the inventory repo and its head.
	repoDir := filepath.Join(d.capsuleDir(wi), "repos", "demo")
	sealedHead := strings.TrimSpace(gitRun(t, repoDir, "rev-parse", "HEAD"))

	// Ship through the real CLI; the PR layer must engage (gh present
	// AND authenticated here — the gates above guarantee it).
	out := d.pf(t, "--json", "ship", wi)
	var ship cli.ShipResponse
	if err := json.Unmarshal([]byte(out), &ship); err != nil {
		t.Fatalf("ship answer undecodable: %v (%q)", err, out)
	}
	if ship.Branch != "polyforge/"+wi {
		t.Fatalf("ship branch wrong: %q", ship.Branch)
	}
	if len(ship.Repos) != 1 || ship.Repos[0].Name != "demo" {
		t.Fatalf("ship inventory wrong: %+v", ship.Repos)
	}
	oc := ship.Repos[0]
	if !oc.Pushed || oc.Head != sealedHead {
		t.Fatalf("ship outcome wrong: %+v (sealed head %s)", oc, sealedHead)
	}
	if oc.PRURL == "" {
		t.Fatalf("ship PR layer did not engage: %+v (want a github PR url)", oc)
	}
	if oc.CompareURL != "" {
		t.Fatalf("ship compare fallback used despite gh: %+v", oc)
	}

	// Independent verification: the PR is real, OPEN, on the right
	// head branch, and its url is the ship-reported one.
	branch := "polyforge/" + wi
	state, headRefName, prURL := ghViewPR(t, fullRepo, branch)
	if state != "OPEN" {
		t.Fatalf("pr state %q, want OPEN (pr %s)", state, prURL)
	}
	if headRefName != branch {
		t.Fatalf("pr headRefName %q, want %q", headRefName, branch)
	}
	if prURL != oc.PRURL {
		t.Fatalf("pr url %q != ship-reported pr_url %q", prURL, oc.PRURL)
	}

	// A second ship is an idempotent reuse: no second push, the SAME
	// pr_url, and still the SAME single OPEN PR — not a duplicate.
	out2 := d.pf(t, "--json", "ship", wi)
	var ship2 cli.ShipResponse
	if err := json.Unmarshal([]byte(out2), &ship2); err != nil {
		t.Fatalf("second ship undecodable: %v (%q)", err, out2)
	}
	if len(ship2.Repos) != 1 {
		t.Fatalf("second ship inventory wrong: %+v", ship2.Repos)
	}
	oc2 := ship2.Repos[0]
	if oc2.Pushed || !oc2.Reused {
		t.Fatalf("second ship not an idempotent reuse: %+v", oc2)
	}
	if oc2.PRURL != oc.PRURL {
		t.Fatalf("second ship pr_url moved: %q -> %q", oc.PRURL, oc2.PRURL)
	}
	if oc2.CompareURL != "" {
		t.Fatalf("second ship compare fallback used despite gh: %+v", oc2)
	}
	state2, _, prURL2 := ghViewPR(t, fullRepo, branch)
	if state2 != "OPEN" || prURL2 != oc.PRURL {
		t.Fatalf("pr after second ship changed: state %q url %q (want OPEN %q)",
			state2, prURL2, oc.PRURL)
	}
	open := ghOpenPRURLs(t, fullRepo, branch)
	if len(open) != 1 || open[0] != oc.PRURL {
		t.Fatalf("expected exactly one OPEN PR %q for head %q, got %v", oc.PRURL, branch, open)
	}
}
