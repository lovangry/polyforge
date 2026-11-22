package runtime

// repo_hooks_test.go — polyforge#41 AC1: a capsule clone runs the target
// repo's own commit-msg validation. A repo that ships .husky (the husky
// convention commitlint projects use) has the clone's core.hooksPath
// wired to it at materialize time: a commit whose message violates the
// repo's hook is rejected inside the capsule clone, a conforming one
// lands. The daemon's hardened reads (taskrepo.Git's core.hooksPath
// pin) are a different path and stay untouched.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// repoHooksGit is one git in dir under the test's identity, answering its
// combined output and error (a violating commit's failure is the
// assertion's subject, so it must be returnable, not fatal).
func repoHooksGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=pf-test", "GIT_AUTHOR_EMAIL=pf-test@invalid",
		"GIT_COMMITTER_NAME=pf-test", "GIT_COMMITTER_EMAIL=pf-test@invalid")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestCloneRunsTheTargetRepoCommitMsgHook: the fixture repo ships a
// .husky/commit-msg whose rule ("reject any message carrying VIOLATE")
// is authored by the test, independent of any engine code — the capsule
// clone must run it: the violating message commits successfully without
// the wiring, is rejected with it, and a conforming message still lands.
func TestCloneRunsTheTargetRepoCommitMsgHook(t *testing.T) {
	hub := newFakeHub()
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	source := filepath.Join(ws, ".repo", "app")
	w079GitRepo(t, source, "local.txt", "local source\n")
	if err := os.MkdirAll(filepath.Join(source, ".husky"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\n" +
		"case \"$(cat \"$1\")\" in *VIOLATE*)\n" +
		"  echo \"commit-msg: rejected by the repo's commitlint (VIOLATE)\" >&2\n" +
		"  exit 1;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(source, ".husky", "commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := repoHooksGit(source, "add", ".husky"); err != nil {
		t.Fatalf("staging the repo's .husky: %v: %s", err, out)
	}
	if out, err := repoHooksGit(source, "commit", "-m", "chore: ship the repo's husky hooks"); err != nil {
		t.Fatalf("committing the repo's .husky: %v: %s", err, out)
	}

	// The claim's inventory names the repo; the WI's carrier store (the
	// hub bare, holding no carrier ref — a first claim) answers nothing
	// to restore, so the clone stands on the source's own HEAD.
	hub.repos = []api.ProjectRepo{{Name: "app", URL: "file://" + source}}
	hubBare := filepath.Join(r.cfg.GitRemoteBase, "wi_hooks.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", hubBare).CombinedOutput(); err != nil {
		t.Fatalf("hub bare init: %v: %s", err, out)
	}
	j := &job{wi: "wi_hooks", capsuleDir: testCapsuleDir(r, "wi_hooks")}
	j.git = gitops.Opts{WorkItemID: "wi_hooks", Username: "wi_hooks", Secret: "s",
		CapsuleDir: j.capsuleDir, RemoteURL: hubBare, GitDir: r.gitDirFor("wi_hooks")}
	j.repos = []api.ProjectRepo{{Name: "app", URL: "file://" + source}}
	if _, err := r.cloneTaskRepos(context.Background(), j); err != nil {
		t.Fatalf("cloneTaskRepos: %v", err)
	}
	dir := filepath.Join(j.capsuleDir, capsule.DirRepos, "app")

	// The wiring: the clone's core.hooksPath names the clone's own .husky.
	out, err := repoHooksGit(dir, "config", "core.hooksPath")
	if err != nil || strings.TrimSpace(out) != filepath.Join(dir, ".husky") {
		t.Fatalf("the capsule clone must wire the repo's .husky as core.hooksPath (got %q, err %v) — without it a commitlint-violating message commits successfully",
			strings.TrimSpace(out), err)
	}

	// A violating message is rejected by the repo's own hook.
	if err := os.WriteFile(filepath.Join(dir, "violate.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := repoHooksGit(dir, "add", "violate.txt"); err != nil {
		t.Fatalf("staging: %v: %s", err, out)
	}
	out, err = repoHooksGit(dir, "commit", "-m", "test: VIOLATE the repo's commitlint")
	if err == nil {
		t.Fatalf("the commit-msg violating message must be rejected inside the capsule clone, it committed:\n%s", out)
	}
	if !strings.Contains(out, "commit-msg: rejected by the repo's commitlint") {
		t.Fatalf("the rejection must be the repo hook's own, got:\n%s", out)
	}

	// A conforming message lands.
	if out, err := repoHooksGit(dir, "commit", "-m", "test: follows the repo's commitlint"); err != nil {
		t.Fatalf("a conforming commit must land in the capsule clone: %v: %s", err, out)
	}
}
