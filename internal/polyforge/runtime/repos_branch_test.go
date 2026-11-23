package runtime

// carries: internal/polyforge/runtime/repos.go, internal/polyforge/runtime/scene.go, internal/common/capsule/capsule.go
//
// repos_branch_test: the clone half of a branch pin (polyforge#45). A
// claim inventory entry with Branch set makes the capsule clone stand on
// the pinned branch (git clone --branch), records the pin's tip as the
// capsule baseline, and records the pin itself in capsule.json's new
// repo_branches fact. An unpinned entry keeps today's behavior exactly:
// the advertised default branch, no repo_branches key in capsule.json at
// all. A pin naming a branch the origin does not have fails the claim
// with the typed DAEMON-507 — the pin is a fact about the lane, not a
// hint, so landing on the server's default would be the silent accident
// the pin exists to prevent.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// fakeTaskRemoteBranches builds a real local git remote with two
// branches: main (one commit) and feat-cloud (a commit ahead of main).
// Answers the file:// URL, main's tip and feat-cloud's tip.
func fakeTaskRemoteBranches(t *testing.T) (url, mainHead, featHead string) {
	t.Helper()
	setGitIdentity(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	gitAt := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644)
	gitAt("add", ".")
	gitAt("commit", "-m", "init")
	mainHead = gitAt("rev-parse", "HEAD")
	gitAt("checkout", "-b", "feat-cloud")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644)
	gitAt("add", ".")
	gitAt("commit", "-m", "feature")
	featHead = gitAt("rev-parse", "HEAD")
	// the remote advertises main as its default: feat-cloud is the
	// pinned branch AHEAD of it, not the remote's own HEAD
	gitAt("checkout", "main")
	return "file://" + dir, mainHead, featHead
}

// symbolicRef answers git symbolic-ref --short HEAD of dir.
func symbolicRef(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("symbolic-ref in %s: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// capsuleBranchesOf answers capsule.json's repo_branches map.
func capsuleBranchesOf(t *testing.T, capDir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capDir, capsule.CapsuleJSON))
	if err != nil {
		t.Fatalf("capsule.json: %v", err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		t.Fatalf("capsule.json: %v", err)
	}
	return idn.RepoBranches
}

// A claim whose inventory pins a repo to feat-cloud clones it standing
// on feat-cloud (the criterion's own probe), records feat-cloud's tip as
// the capsule baseline, and records the pin in capsule.json.
func TestStartClonesPinnedRepoBranch(t *testing.T) {
	setGitIdentity(t)
	url, _, featHead := fakeTaskRemoteBranches(t)
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "app", URL: url, Branch: "feat-cloud"}}
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})

	if err := r.Start(context.Background(), "wi_pin1", StartOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	capDir := testCapsuleDir(r, "wi_pin1")
	repoDir := filepath.Join(capDir, capsule.DirRepos, "app")

	if got := symbolicRef(t, repoDir); got != "feat-cloud" {
		t.Fatalf("repos/app stands on %q, want the pinned branch feat-cloud", got)
	}
	if got := capsuleReposOf(t, capDir)["app"]; got != featHead {
		t.Fatalf("capsule baseline = %q, want the pinned branch's tip %q", got, featHead)
	}
	branches := capsuleBranchesOf(t, capDir)
	if len(branches) != 1 || branches["app"] != "feat-cloud" {
		t.Fatalf("capsule.json repo_branches = %v, want the one pin app=feat-cloud", branches)
	}
	if b, err := os.ReadFile(filepath.Join(repoDir, "feature.txt")); err != nil || string(b) != "feature\n" {
		t.Fatalf("the pinned branch's content is missing: %q, %v", b, err)
	}
	if err := r.Stop(context.Background(), "wi_pin1", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	unseal(t, capDir)
	unseal(t, repoDir)
}

// An unpinned inventory entry keeps today's materialization exactly: the
// server-advertised default branch checked out, its tip as the baseline,
// and no repo_branches fact in capsule.json at all (the pre-#45 capsule
// stays byte-identical in shape).
func TestStartUnpinnedCloneStaysOnServerDefault(t *testing.T) {
	setGitIdentity(t)
	url, mainHead, _ := fakeTaskRemoteBranches(t)
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "app", URL: url}}
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})

	if err := r.Start(context.Background(), "wi_pin2", StartOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	capDir := testCapsuleDir(r, "wi_pin2")
	repoDir := filepath.Join(capDir, capsule.DirRepos, "app")

	if got := symbolicRef(t, repoDir); got != "main" {
		t.Fatalf("repos/app stands on %q, want the advertised default main", got)
	}
	if got := capsuleReposOf(t, capDir)["app"]; got != mainHead {
		t.Fatalf("capsule baseline = %q, want the default branch's tip %q", got, mainHead)
	}
	if branches := capsuleBranchesOf(t, capDir); branches != nil {
		t.Fatalf("capsule.json repo_branches = %v, want the key absent for an unpinned capsule", branches)
	}
	raw, err := os.ReadFile(filepath.Join(capDir, capsule.CapsuleJSON))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "repo_branches") {
		t.Fatalf("an unpinned capsule.json carries repo_branches: %s", raw)
	}
	if err := r.Stop(context.Background(), "wi_pin2", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	unseal(t, capDir)
	unseal(t, repoDir)
}

// A pin naming a branch the origin does not have fails the claim with
// the typed DAEMON-507: the WI closes blocked, never a silent clone on
// the server's default branch.
func TestStartCloneOfMissingPinnedBranchFails(t *testing.T) {
	setGitIdentity(t)
	url, _, _ := fakeTaskRemoteBranches(t)
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "app", URL: url, Branch: "no-such-branch"}}
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})

	err := r.Start(context.Background(), "wi_pin3", StartOpts{})
	if err == nil {
		t.Fatal("a pin naming a missing branch must fail the claim")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonSpawnFail {
		t.Fatalf("want typed DAEMON-507, got %v (%T)", err, err)
	}
	if !strings.Contains(ae.Detail, "app") || !strings.Contains(ae.Detail, "no-such-branch") {
		t.Fatalf("the refusal must name the repo and the pinned branch, got %q", ae.Detail)
	}
	if len(hub.ends) == 0 || hub.lastEnd().Status != api.StatusBlocked {
		t.Fatalf("the claim must close blocked, got %+v", hub.lastEnd())
	}
	if r.jobOf("wi_pin3") != nil {
		t.Fatal("a failed pinned clone left a live job")
	}
}
