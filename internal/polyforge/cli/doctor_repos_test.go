package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// doctor_repos_test.go: the read-only git probe layer fixtures — real git
// repos built inside t.TempDir() (init, commit, branch, worktree add,
// dirty file, unborn HEAD); nothing is ever created outside the test's
// own temp directories.

// reposGit runs one repository-shaping git command (the fixtures write;
// the probe layer under test never does) and fails the test on error.
func reposGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=pf-test", "GIT_AUTHOR_EMAIL=pf-test@invalid",
		"GIT_COMMITTER_NAME=pf-test", "GIT_COMMITTER_EMAIL=pf-test@invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir %s): %v: %s", args, dir, err, out)
	}
}

// reposClone shapes ws/.repo/<name> as a real git clone of a fresh bare
// remote, on branch, with that branch pushed (a remote-tracking ref set):
// clean and pushed until the caller dirties it. Returns the clone dir.
func reposClone(t *testing.T, ws, name, branch string) string {
	t.Helper()
	bare := t.TempDir()
	reposGit(t, bare, "init", "-q", "--bare", "-b", "main")
	dir := filepath.Join(ws, ".repo", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reposGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/sh\necho deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reposGit(t, dir, "add", ".")
	reposGit(t, dir, "commit", "-q", "-m", "init")
	reposGit(t, dir, "remote", "add", "origin", bare)
	reposGit(t, dir, "push", "-q", "-u", "origin", "main")
	if branch != "main" {
		reposGit(t, dir, "checkout", "-q", "-b", branch)
		reposGit(t, dir, "push", "-q", "-u", "origin", branch)
	}
	return dir
}

// reposDirty modifies the tracked deploy.sh (` M deploy.sh`).
func reposDirty(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/sh\necho hand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reposUnit(t *testing.T, units []repoUnit, suffix string) repoUnit {
	t.Helper()
	for _, u := range units {
		if strings.HasSuffix(u.Dir, suffix) {
			return u
		}
	}
	t.Fatalf("no unit with dir suffix %q in %+v", suffix, units)
	return repoUnit{}
}

// Facts: a pushed branch is pushed; a dirty tracked file is dirty; a
// committed-but-unpushed commit flags like uncommitted changes. The same
// repo's unit answers all three shapes.
func TestRepoUnitsFactsOnBranch(t *testing.T) {
	ws := t.TempDir()
	dir := reposClone(t, ws, "one", "release/v0.0.27")

	units := workspaceRepoUnits(ws)
	if len(units) != 1 {
		t.Fatalf("units: %+v, want one clone", units)
	}
	u := reposUnit(t, units, ".repo/one")
	if u.Dir != dir || u.CloneName != "one" || u.Branch != "release/v0.0.27" || u.Dirty || u.Unpushed || u.Checked != "" {
		t.Fatalf("pushed clean clone: %+v", u)
	}

	reposDirty(t, dir)
	u = workspaceRepoUnits(ws)[0]
	if !u.Dirty || u.Unpushed {
		t.Fatalf("dirty clone: %+v", u)
	}

	reposGit(t, dir, "add", ".")
	reposGit(t, dir, "commit", "-q", "-m", "local only")
	u = workspaceRepoUnits(ws)[0]
	if u.Dirty || !u.Unpushed {
		t.Fatalf("unpushed local commit: %+v", u)
	}
}

// Enumeration: clones and only clones — a sibling without .git and the
// wrapper directory of the worktree shape are never units.
func TestRepoUnitsEnumeratesClonesOnly(t *testing.T) {
	ws := t.TempDir()
	reposClone(t, ws, "a", "main")
	reposClone(t, ws, "b", "main")
	if err := os.MkdirAll(filepath.Join(ws, ".repo", "not-a-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".repo", "not-a-repo", "notes.txt"), []byte("plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	units := workspaceRepoUnits(ws)
	if len(units) != 2 {
		t.Fatalf("units: %+v, want exactly clones a and b", units)
	}
	for _, u := range units {
		if !strings.HasPrefix(u.Dir, filepath.Join(ws, ".repo")) || u.Checked != "" {
			t.Fatalf("unexpected unit: %+v", u)
		}
	}
}

// Linked worktrees: reached through the clone's own worktree list, at any
// location (the nested wrap/cl-wtree shape of the acceptance suite); the
// wrapper directory itself is never a unit. A worktree sitting directly
// under .repo/ carries its .git pointer file and is enumerated as the
// clone it is — deduplicated against the linked face, one unit.
func TestRepoUnitsLinkedWorktreesReachedAndDeduped(t *testing.T) {
	ws := t.TempDir()
	parent := reposClone(t, ws, "parent", "main")
	nested := filepath.Join(ws, ".repo", "wrap", "cl-wtree")
	reposGit(t, parent, "worktree", "add", "-q", "-b", "feature/wt", nested)
	if err := os.WriteFile(filepath.Join(nested, "scratch-edit.txt"), []byte("untracked hand work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	units := workspaceRepoUnits(ws)
	if len(units) != 2 {
		t.Fatalf("units: %+v, want parent + nested worktree (wrapper dir included)", units)
	}
	wu := reposUnit(t, units, "wrap/cl-wtree")
	if wu.Branch != "feature/wt" || !wu.Dirty || wu.Unpushed || wu.CloneName != "parent" {
		t.Fatalf("nested worktree: %+v", wu)
	}
	pu := reposUnit(t, units, ".repo/parent")
	if pu.Dirty || pu.Branch != "main" || pu.CloneName != "parent" {
		t.Fatalf("parent clone: %+v (the worktree's dirt must not leak across units)", pu)
	}

	// The same git worktree added directly under .repo/: the pointer file
	// enumerates it as the clone it is; no second unit.
	ws2 := t.TempDir()
	parent2 := reposClone(t, ws2, "parent", "main")
	direct := filepath.Join(ws2, ".repo", "wt-direct")
	reposGit(t, parent2, "worktree", "add", "-q", "-b", "feature/direct", direct)
	units2 := workspaceRepoUnits(ws2)
	if len(units2) != 2 {
		t.Fatalf("units (direct worktree): %+v, want parent + wt-direct once", units2)
	}
	du := reposUnit(t, units2, ".repo/wt-direct")
	if du.Branch != "feature/direct" || du.CloneName != "wt-direct" || du.Checked != "" {
		t.Fatalf("direct worktree: %+v", du)
	}
}

// Detached HEAD: the branch is "" (the delivery decision has nothing to
// read); the dirt is still reported.
func TestRepoUnitsDetachedHead(t *testing.T) {
	ws := t.TempDir()
	reposClone(t, ws, "det", "main")
	reposGit(t, filepath.Join(ws, ".repo", "det"), "checkout", "-q", "--detach")

	u := reposUnit(t, workspaceRepoUnits(ws), ".repo/det")
	if u.Branch != "" || u.Checked != "" {
		t.Fatalf("detached clone: %+v", u)
	}
	reposDirty(t, filepath.Join(ws, ".repo", "det"))
	u = workspaceRepoUnits(ws)[0]
	if u.Branch != "" || !u.Dirty {
		t.Fatalf("detached dirty clone: %+v", u)
	}
}

// No remotes at all: HEAD flags as unpushed (the committed work is on no
// human face). Unborn HEAD: judged by status alone.
func TestRepoUnitsNoRemotesAndUnbornHead(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".repo", "local")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reposGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("hand work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reposGit(t, dir, "add", ".")
	reposGit(t, dir, "commit", "-q", "-m", "hand work, never pushed")

	u := reposUnit(t, workspaceRepoUnits(ws), ".repo/local")
	if !u.Unpushed || u.Dirty || u.Branch != "main" || u.Checked != "" {
		t.Fatalf("no-remotes clone: %+v", u)
	}

	unborn := filepath.Join(ws, ".repo", "fresh")
	if err := os.MkdirAll(unborn, 0o755); err != nil {
		t.Fatal(err)
	}
	reposGit(t, unborn, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(unborn, "draft.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u = reposUnit(t, workspaceRepoUnits(ws), ".repo/fresh")
	if u.Branch != "main" || !u.Dirty || u.Unpushed || u.Checked != "" {
		t.Fatalf("unborn HEAD clone: %+v", u)
	}
}

// Never-blank convention: a repo whose probe fails carries its exact
// reason (an unscannable unit is listed, never silently clean).
func TestRepoUnitsUnscannableCarriesReason(t *testing.T) {
	ws := t.TempDir()
	reposClone(t, ws, "good", "main")
	bad := filepath.Join(ws, ".repo", "bad")
	if err := os.MkdirAll(filepath.Join(bad, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	units := workspaceRepoUnits(ws)
	bu := reposUnit(t, units, ".repo/bad")
	if bu.Checked == "" {
		t.Fatalf(".git dir only: probe must not fake clean: %+v", bu)
	}
	// The good clone is unaffected: still scanned, still clean-and-pushed.
	gu := reposUnit(t, units, ".repo/good")
	if gu.Checked != "" || gu.Dirty || gu.Unpushed {
		t.Fatalf("good clone behind a bad one: %+v", gu)
	}
}

// A linked worktree whose checked-out directory is gone: git keeps listing
// it, the probe fails, the unit carries its reason (never a clean face);
// the listing clone stays scannable, its own dirt its own unit's business.
func TestRepoUnitsGoneWorktreeCarriesReason(t *testing.T) {
	ws := t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	clone := reposClone(t, ws, "lost", "main")
	reposGit(t, clone, "worktree", "add", "-q", "-b", "feature/x", wt)
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}

	units := workspaceRepoUnits(ws)
	if len(units) != 2 {
		t.Fatalf("units: %+v, want clone + gone worktree", units)
	}
	cu := reposUnit(t, units, ".repo/lost")
	if cu.Checked != "" && cu.Dir == clone {
		t.Fatalf("clone unit must stay scannable: %+v", cu)
	}
	wu := reposUnit(t, units, filepath.Base(wt))
	if wu.Checked == "" {
		t.Fatalf("gone worktree: probe must carry a reason, not fake clean: %+v", wu)
	}
}
