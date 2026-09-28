package daemon

// ship_test.go: unit pins for the ship op's pure helpers (branch naming,
// the reserved-target guard, GitHub origin parsing, the compare-URL
// fallback, ls-remote output parsing). The full delivery flow is pinned
// end-to-end by test/usage_s10_ship_test.go against the real binaries.

import (
	"testing"
)

func TestShipBranchIsFixedName(t *testing.T) {
	// The delivery branch is always polyforge/<wi-slug>; v1: the wi id IS
	// the slug (server-minted, branch-safe alphabet).
	if got := shipBranch("wi_abc123def456"); got != "polyforge/wi_abc123def456" {
		t.Fatalf("shipBranch = %q", got)
	}
}

func TestShipNeverPushTarget(t *testing.T) {
	for _, branch := range []string{"main", "master", "refs/heads/main", "refs/heads/master"} {
		if !neverPushTarget(branch) {
			t.Fatalf("neverPushTarget(%q) must be true", branch)
		}
	}
	// The wi ids main/master trip the guard through the fixed-name scheme
	// (their branch's last path segment is the reserved name).
	for _, wi := range []string{"main", "master"} {
		if !neverPushTarget(shipBranch(wi)) {
			t.Fatalf("neverPushTarget(shipBranch(%q)) must be true", wi)
		}
	}
	for _, branch := range []string{
		"polyforge/wi_abc123def456", "polyforge/feature-main", "polyforge/masterplan",
		"develop", "polyforge/main-event2",
	} {
		if neverPushTarget(branch) {
			t.Fatalf("neverPushTarget(%q) must be false", branch)
		}
	}
}

func TestShipParseGitHubRepo(t *testing.T) {
	cases := []struct {
		url   string
		owner string
		repo  string
		ok    bool
	}{
		{"https://github.com/acme/app.git", "acme", "app", true},
		{"https://github.com/acme/app", "acme", "app", true},
		{"git@github.com:acme/app.git", "acme", "app", true},
		{"git@github.com:acme/app", "acme", "app", true},
		// Non-GitHub origins never reach gh.
		{url: "/tmp/origin.git"},
		{url: "https://gitlab.com/acme/app.git"},
		{url: "ssh://git@bitbucket.org/acme/app.git"},
		{url: "https://github.com/"},
	}
	for _, c := range cases {
		owner, repo, ok := parseGitHubRepo(c.url)
		if ok != c.ok || owner != c.owner || repo != c.repo {
			t.Errorf("parseGitHubRepo(%q) = %q,%q,%v", c.url, owner, repo, ok)
		}
	}
}

func TestShipCompareFallback(t *testing.T) {
	if got := compareFallback("https://github.com/acme/app.git", "polyforge/wi_abc"); got != "https://github.com/acme/app/compare/main...polyforge/wi_abc" {
		t.Fatalf("github compare fallback = %q", got)
	}
	// A non-GitHub origin carries the origin URL itself: the one place a
	// human can compare the pushed branch.
	if got := compareFallback("/srv/git/app.git", "polyforge/wi_abc"); got != "/srv/git/app.git" {
		t.Fatalf("non-github fallback = %q", got)
	}
}

func TestShipFirstShipField(t *testing.T) {
	if got := firstShipField("0123abcd\trefs/heads/polyforge/wi_x\n"); got != "0123abcd" {
		t.Fatalf("firstShipField = %q", got)
	}
	if got := firstShipField("0123abcd refs/heads/x\n"); got != "0123abcd" {
		t.Fatalf("firstShipField = %q", got)
	}
	if got := firstShipField(""); got != "" {
		t.Fatalf("firstShipField(empty) = %q", got)
	}
}

func TestShipIsNonFastForward(t *testing.T) {
	for _, out := range []string{
		" ! [rejected] polyforge/wi_x -> polyforge/wi_x (non-fast-forward)",
		"hint: Updates were rejected because the tip of your current branch is behind... fetch first",
	} {
		if !isNonFastForward(out) {
			t.Fatalf("isNonFastForward(%q) must be true", out)
		}
	}
	if isNonFastForward("fatal: unable to access '...': Could not resolve host") {
		t.Fatal("a transport failure is not a non-fast-forward verdict")
	}
}
