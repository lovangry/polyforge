package test

// E2e pin for wi_MuyoyHwOewS7, migrated onto the moved procedure file by
// wi_Qg8GLR3XS4o3 (polyforge#123): the release step's version-base
// computation must reflect every version already published by ANY capsule.
//
// The old release procedure (workflows/*/steps/release.md, step 1) computed
// the version floor inside the capsule's own worktree:
//
//	git tag -l 'v*' | sort -V | tail -1
//
// Parallel capsules each hold a private clone: a tag created by one capsule's
// release step stays local to that clone, so every other capsule still
// computed the origin-synced floor and the lanes collided on version numbers.
//
// polyforge#123 moved the procedure out of the shared templates (they are
// project-generic dispatchers now) into the polyforge repo itself:
// .polyforge/release.sh. The fixed procedure computes the floor from the
// union of the worktree's tags and the listing of the shared releases root
// (the same root the publish step claims into), and claims the version
// atomically by creating releases/<tag>/ with a plain mkdir, failing loudly
// when the version is already taken.
//
// This test does NOT hard-code a copy of the procedure's commands: it
// extracts the actual floor command line and the atomic claim line from the
// moved .polyforge/release.sh and executes them verbatim in a two-capsule
// fixture. It therefore pins the no-collision property end-to-end against
// the real procedure text — reverting the moved script to a clone-local
// floor (or dropping the atomic publish claim) makes this test fail.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releaseProcedureScript is the moved polyforge release procedure (the
// shared templates carry no procedure text anymore — they dispatch to this
// file), relative to this package's directory.
var releaseProcedureScript = filepath.Join("..", ".polyforge", "release.sh")

// procedureLineOf returns the first line of the script containing needle.
// It fails the test when no such line exists — that means the moved
// procedure no longer encodes the command the release contract needs.
func procedureLineOf(t *testing.T, needle string) string {
	t.Helper()
	raw, err := os.ReadFile(releaseProcedureScript)
	if err != nil {
		t.Fatalf("cannot read %s: %v", releaseProcedureScript, err)
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.Contains(ln, needle) {
			return strings.TrimSpace(ln)
		}
	}
	t.Fatalf("%s: no line containing %q - the moved release procedure lost a command the no-collision contract pins", releaseProcedureScript, needle)
	return ""
}

// procedureFloor executes the moved procedure's floor command line
// (extracted verbatim from .polyforge/release.sh) inside the given capsule
// worktree with RELEASES_ROOT pointing at the shared publish root.
func procedureFloor(t *testing.T, worktree, releasesRoot string) string {
	t.Helper()
	line := procedureLineOf(t, `sort -Vu`)
	cmd := exec.Command("sh", "-c", line)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), "RELEASES_ROOT="+releasesRoot)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("floor command %q from %s failed in %s: %v\n%s", line, releaseProcedureScript, worktree, err, out)
	}
	return strings.TrimSpace(string(out))
}

// procedureClaim executes the moved procedure's atomic version claim line
// (extracted verbatim from .polyforge/release.sh) with TAG set. It returns
// the claim error: nil means the version directory was claimed, non-nil
// means the version was already taken (the publish guard firing loudly).
func procedureClaim(t *testing.T, releasesRoot, tag string) error {
	t.Helper()
	line := procedureLineOf(t, `mkdir "$RELEASES_ROOT`)
	cmd := exec.Command("sh", "-c", line)
	cmd.Env = append(os.Environ(), "RELEASES_ROOT="+releasesRoot, "TAG="+tag)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("claim of %s failed: %v\n%s", tag, err, out)
	}
	return nil
}

// bumpPatch performs the procedure's "bump the patch segment" decision on a
// v<semver> floor: v0.3.1 -> v0.3.2.
func bumpPatch(t *testing.T, floor string) string {
	t.Helper()
	var mm, pp int
	if _, err := fmt.Sscanf(floor, "v0.%d.%d", &mm, &pp); err != nil {
		t.Fatalf("cannot parse floor %q as v0.minor.patch: %v", floor, err)
	}
	return fmt.Sprintf("v0.%d.%d", mm, pp+1)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=repro", "GIT_AUTHOR_EMAIL=repro@example.invalid",
		"GIT_COMMITTER_NAME=repro", "GIT_COMMITTER_EMAIL=repro@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func TestReleaseVersionBaseParallelCapsuleCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("repo-fixture scenario")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	capsuleA := filepath.Join(root, "capsuleA")
	capsuleB := filepath.Join(root, "capsuleB")
	releasesRoot := filepath.Join(root, "releases") // shared intranet publish root
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// Shared history: origin holds tags up to v0.3.1, matching the live
	// observation that `git ls-remote --tags origin` tops out at v0.3.1.
	gitIn(t, root, "init", "--bare", "-q", "-b", "main", origin)
	gitIn(t, root, "init", "-q", "-b", "main", capsuleA)
	gitIn(t, root, "clone", "-q", origin, capsuleB)
	gitIn(t, capsuleA, "commit", "--allow-empty", "-q", "-m", "base")
	gitIn(t, capsuleA, "remote", "add", "origin", origin)
	gitIn(t, capsuleA, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitIn(t, capsuleA, "tag", "-a", "v0.3.1", "-m", "floor as seen on origin")
	gitIn(t, capsuleA, "push", "-q", "origin", "refs/tags/v0.3.1")
	gitIn(t, capsuleB, "pull", "-q", "origin", "main")
	gitIn(t, capsuleB, "fetch", "-q", "--tags", "origin")

	// Capsule A's release step, following the moved procedure: compute the
	// floor with the script's union command, bump the patch segment, create
	// the annotated LOCAL tag, and claim+publish the version directory in
	// the shared releases root.
	floorA := procedureFloor(t, capsuleA, releasesRoot)
	if floorA != "v0.3.1" {
		t.Fatalf("fixture setup: capsule A floor = %q, want v0.3.1", floorA)
	}
	versA := bumpPatch(t, floorA)
	gitIn(t, capsuleA, "commit", "--allow-empty", "-q", "-m", "capsule A fix")
	gitIn(t, capsuleA, "tag", "-a", versA, "-m", "capsule A release")
	if err := procedureClaim(t, releasesRoot, versA); err != nil {
		t.Fatalf("capsule A: claiming fresh version %s failed: %v", versA, err)
	}
	for _, f := range []string{"aihub", "pf", "SHA256SUMS"} {
		if err := os.WriteFile(filepath.Join(releasesRoot, versA, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Capsule B's release step runs in parallel, later in wall-clock time
	// but with no visibility into capsule A's clone (A's tag is local to
	// A). It follows the same procedure verbatim.
	floorB := procedureFloor(t, capsuleB, releasesRoot)

	// The floor MUST reflect capsule A's already-published version: the
	// publish root is what carries it, since A's tag never entered B's
	// clone (nor origin).
	if floorB != versA {
		t.Fatalf("capsule B's floor = %q, want %q - the floor computation does not see versions already published to the shared releases root by parallel capsules", floorB, versA)
	}
	versB := bumpPatch(t, floorB)

	// Invariant the release step requires: the version a capsule decides
	// must not collide with a version a parallel capsule has already
	// published to the shared distribution root.
	published, err := os.ReadDir(releasesRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range published {
		if e.Name() == versB {
			t.Fatalf("version collision: capsule B's release step computed floor %q and chose version %q, but %q was already published to the shared releases root by parallel capsule A (its tag is local to capsule A's clone, invisible to capsule B's floor computation)",
				floorB, versB, e.Name())
		}
	}

	// Capsule B claims its version atomically - must succeed.
	if err := procedureClaim(t, releasesRoot, versB); err != nil {
		t.Fatalf("capsule B: claiming fresh version %s failed: %v", versB, err)
	}
	// Publish-time guard: an already-taken version must fail loudly.
	if err := procedureClaim(t, releasesRoot, versA); err == nil {
		t.Fatalf("publish guard missing: capsule B could claim already-published version %s - the procedure publishes over a parallel capsule's release instead of failing", versA)
	}
}
