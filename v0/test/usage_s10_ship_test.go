package test

// usage_s10_ship_test.go: S10 — the ship delivery bridge (04 §5.1 ship
// row, baseline amendment polyforge#37) through the REAL binaries: a
// wrapped WI's sealed capsule is pushed to fake git origins (bare repos
// on local disk) as the fixed-name branch polyforge/<wi>, with the
// idempotency, moved-branch, reserved-name, preflight and read-only
// pins the ship row promises.
//
// The gh PR layer is pinned by its fallback: the origins here are local
// paths, and gh is GitHub-only, so the PR layer deterministically
// degrades to the compare URL (never an error) regardless of whether the
// host running the tests happens to have an authenticated gh.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/cli"
)

// shipGit runs one git command (dir == "" → the test process cwd) and
// fails the test on a non-zero exit.
func shipGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir %q): %v: %s", args, dir, err, out)
	}
	return string(out)
}

// shipTrim is the shared output trim.
func shipTrim(s string) string { return strings.TrimSpace(s) }

// shipBareOrigin creates one fake git remote: a bare repository on local
// disk seeded with a single base commit. Returns the origin path and the
// base sha (the frozen base the WI's inventory checkout pins).
func shipBareOrigin(t *testing.T, name string) (originPath, base string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	shipGit(t, "", "init", "-q", src)
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("base of "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shipGit(t, src, "add", "README.md")
	shipGit(t, src, "-c", "user.name=pf-test", "-c", "user.email=pf-test@example.com",
		"commit", "-q", "-m", "base")
	base = shipTrim(shipGit(t, src, "rev-parse", "HEAD"))
	originPath = filepath.Join(t.TempDir(), name+".git")
	shipGit(t, "", "clone", "-q", "--bare", src, originPath)
	return originPath, base
}

// shipRepoDecl names one inventory repo and its fake origin.
type shipRepoDecl struct {
	name   string
	origin string
}

// shipCreateClosedWI runs the full pre-ship story on one machine: file a
// WI carrying the given repo inventory, submit it, wait for the WI Agent,
// leave ONE sealed commit per repo worktree (what the agent "did"), then
// wrap terminal. Returns the wi id and the sealed commit per repo.
func shipCreateClosedWI(t *testing.T, inst *usageInstall, m *usageMachine, goal string, repos []shipRepoDecl) (string, map[string]string) {
	t.Helper()
	args := []string{"--json", "file", "create", "--project", "usage",
		"--goal", goal, "--bundle", "feature-full"}
	for _, r := range repos {
		base := shipTrim(shipGit(t, "", "--git-dir", r.origin, "rev-parse", "HEAD"))
		args = append(args, "--repo", r.name+"="+r.origin, "--repo-base", r.name+"="+base)
	}
	raw := inst.pf(t, args...)
	var created struct {
		ID string `json:"id"`
	}
	if err := jsonUnmarshalStrict(raw, &created); err != nil || created.ID == "" {
		t.Fatalf("file create (with repos): %q (%v)", raw, err)
	}
	wi := created.ID

	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() >= 1 })

	// The agent's work: one empty commit per inventory worktree — the
	// sealed commit ship must later deliver.
	sealed := map[string]string{}
	for _, r := range repos {
		dir := filepath.Join(m.home, "workspace", "capsules", wi, "repos", r.name)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			t.Fatalf("inventory repo %s not checked out at %s: %v", r.name, dir, err)
		}
		shipGit(t, dir, "-c", "user.name=pf-test", "-c", "user.email=pf-test@example.com",
			"commit", "-q", "--allow-empty", "-m", "sealed work for "+wi)
		sealed[r.name] = shipTrim(shipGit(t, dir, "rev-parse", "HEAD"))
	}

	m.pfOn("wrap", wi)
	waitUntilUse(t, "the WI to be wrapped", func() bool { return inst.wiStatus(t, wi) == "wrapped" })
	return wi, sealed
}

// shipHashTree fingerprints an entire directory tree (the sealed capsule,
// .git included): relative path → (kind, mode, content hash | link
// target). Ship must leave this byte-identical.
func shipHashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	walk := func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			out[rel] = "link " + info.Mode().String() + " -> " + tgt
		case info.IsDir():
			out[rel] = "dir " + info.Mode().String()
		default:
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(raw)
			out[rel] = "file " + info.Mode().String() + " " + hex.EncodeToString(sum[:])
		}
		return nil
	}
	if err := filepath.Walk(root, walk); err != nil {
		t.Fatalf("hash tree %s: %v", root, err)
	}
	return out
}

// shipOriginBranch reads a branch sha out of a bare fake origin.
func shipOriginBranch(t *testing.T, origin, branch string) string {
	t.Helper()
	return shipTrim(shipGit(t, "", "--git-dir", origin, "rev-parse", "refs/heads/"+branch))
}

func TestUsageS10ShipSealedDelivery(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s10")
	m.start("FAKEPI_MODE=ask")

	// Two fake origins: the capsule is multi-repo and the response must
	// report per-repo outcomes.
	appOrigin, _ := shipBareOrigin(t, "app")
	libOrigin, _ := shipBareOrigin(t, "lib")
	wi, sealed := shipCreateClosedWI(t, inst, m, "S10: ship the sealed capsule",
		[]shipRepoDecl{{"app", appOrigin}, {"lib", libOrigin}})
	branch := "polyforge/" + wi
	capDir := filepath.Join(m.home, "workspace", "capsules", wi)

	// The sealed scene is byte-frozen BEFORE the ship: after a successful
	// ship the capsule tree must be identical (03 §2.1: sealed stays
	// read-only for ship; ship is no write exception).
	before := shipHashTree(t, capDir)

	// First ship (--json): every inventory repo pushed to its origin.
	raw := m.pfOn("--json", "ship", wi)
	var resp cli.ShipResponse
	if err := jsonUnmarshalStrict(raw, &resp); err != nil {
		t.Fatalf("ship answer not JSON: %q (%v)", raw, err)
	}
	if resp.WorkItemID != wi || resp.Branch != branch {
		t.Fatalf("ship answer ids: %+v (want wi %s branch %s)", resp, wi, branch)
	}
	if len(resp.Repos) != 2 {
		t.Fatalf("ship must report both inventory repos: %s", raw)
	}
	for _, r := range resp.Repos {
		if !r.Pushed || r.Reused {
			t.Fatalf("repo %s: first ship must push (pushed, not reused): %+v", r.Name, r)
		}
		if r.Head != sealed[r.Name] {
			t.Fatalf("repo %s: pushed head %s ≠ sealed commit %s", r.Name, r.Head, sealed[r.Name])
		}
		// gh-absent fallback: no PR credential on a local-path origin →
		// the compare URL, and SUCCESS (pr_url stays empty).
		if r.PRURL != "" {
			t.Fatalf("repo %s: no gh credential exists for a local origin; pr_url must be empty: %+v", r.Name, r)
		}
		want := ""
		switch r.Name {
		case "app":
			want = appOrigin
		case "lib":
			want = libOrigin
		}
		if r.CompareURL != want {
			t.Fatalf("repo %s: compare fallback = %q, want the origin %q", r.Name, r.CompareURL, want)
		}
		// The origin now carries the branch at the sealed commit.
		if got := shipOriginBranch(t, want, branch); got != sealed[r.Name] {
			t.Fatalf("repo %s: origin branch %s is at %s, want the sealed commit %s", r.Name, branch, got, sealed[r.Name])
		}
	}

	// Idempotency: a second ship with the same sha REUSES the remote
	// branch (no duplicate-push failure, op success, URL present) — and
	// pins the human render on the way.
	out := m.pfOn("ship", wi)
	for _, want := range []string{"shipped " + wi, "branch " + branch, "reused"} {
		if !strings.Contains(out, want) {
			t.Fatalf("second ship output must contain %q:\n%s", want, out)
		}
	}
	for _, r := range []struct{ name, origin string }{{"app", appOrigin}, {"lib", libOrigin}} {
		if got := shipOriginBranch(t, r.origin, branch); got != sealed[r.name] {
			t.Fatalf("repo %s: idempotent ship must not move the branch: %s", r.name, got)
		}
	}

	// Read-only assertion: the capsule tree (including .git) is identical
	// after both ships.
	after := shipHashTree(t, capDir)
	if len(after) != len(before) {
		t.Fatalf("ship changed the capsule tree: %d entries before, %d after", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("ship wrote into the sealed capsule at %s: %q → %q", k, v, after[k])
		}
	}
}

func TestUsageS10ShipMovedBranchAndUnreachableOrigin(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s10b")
	m.start("FAKEPI_MODE=ask")

	// Moved remote branch: someone pushed the delivery branch forward —
	// ship must refuse DAEMON-409 and NEVER force over it.
	originA, _ := shipBareOrigin(t, "app")
	wiA, sealedA := shipCreateClosedWI(t, inst, m, "S10b: moved remote branch",
		[]shipRepoDecl{{"app", originA}})
	branchA := "polyforge/" + wiA
	scratch := filepath.Join(t.TempDir(), "scratch")
	shipGit(t, "", "clone", "-q", originA, scratch)
	shipGit(t, scratch, "-c", "user.name=pf-test", "-c", "user.email=pf-test@example.com",
		"commit", "-q", "--allow-empty", "-m", "moved by someone else")
	moved := shipTrim(shipGit(t, scratch, "rev-parse", "HEAD"))
	if moved == sealedA["app"] {
		t.Fatal("the moved commit must differ from the sealed commit")
	}
	shipGit(t, scratch, "push", "-q", originA, "HEAD:refs/heads/"+branchA)

	errs := m.pfOnExit(1, "ship", wiA)
	if !strings.Contains(errs, "DAEMON-409") {
		t.Fatalf("ship over a moved branch must refuse DAEMON-409, got:\n%s", errs)
	}
	if got := shipOriginBranch(t, originA, branchA); got != moved {
		t.Fatalf("ship must never force-push: branch %s is at %s, want the moved commit %s", branchA, got, moved)
	}

	// Unreachable origin → DAEMON-512 (the push preflight): the push is
	// never initiated. Renaming the fake origin away simulates the
	// unreachable origin / unusable push transport.
	originB, _ := shipBareOrigin(t, "app2")
	wiB, _ := shipCreateClosedWI(t, inst, m, "S10b: unreachable origin",
		[]shipRepoDecl{{"app", originB}})
	if err := os.Rename(originB, originB+".gone"); err != nil {
		t.Fatal(err)
	}
	errs = m.pfOnExit(1, "ship", wiB)
	if !strings.Contains(errs, "DAEMON-512") {
		t.Fatalf("ship against an unreachable origin must refuse DAEMON-512, got:\n%s", errs)
	}
}

func TestUsageS10ShipNoCapsuleAndReservedTarget(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s10c")
	m.start("FAKEPI_MODE=ask")

	// No local capsule on this machine → DAEMON-404.
	errs := m.pfOnExit(1, "ship", "wi_nope00000001")
	if !strings.Contains(errs, "DAEMON-404") {
		t.Fatalf("ship without a local capsule must refuse DAEMON-404, got:\n%s", errs)
	}

	// Reserved branch target: main/master are never pushed. The refusal
	// fires before any remote contact (there is nothing to contact).
	for _, id := range []string{"main", "master"} {
		errs = m.pfOnExit(1, "ship", id)
		if !strings.Contains(errs, "DAEMON-403") {
			t.Fatalf("ship of %q must refuse DAEMON-403 (reserved branch target), got:\n%s", id, errs)
		}
	}
}
