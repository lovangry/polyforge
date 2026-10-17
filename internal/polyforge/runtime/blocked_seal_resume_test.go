package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// sealBlockedWithUnpushedWork runs wi on r, commits unpushed.txt into its
// capsule after a first published commit and closes it while the hub refuses every push: the close
// degrades to blocked carrying the local head, which only this machine
// holds. It answers the sealed head and the hub's bare repo; the push
// refusal is lifted again before it returns.
func sealBlockedWithUnpushedWork(t *testing.T, hub *fakeHub, r *Runtime, wi string) (head, bare string) {
	t.Helper()
	startRunning(t, r, wi)
	j := r.jobOf(wi)
	if j == nil {
		t.Fatal("no job after Start")
	}
	if err := os.WriteFile(filepath.Join(j.capsuleDir, "pushed.txt"), []byte("published\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := gitops.CommitPush(context.Background(), j.git, "published step"); err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	bare = filepath.Join(hub.capsuleRepoBase, wi+".git")
	// aihub's bare repos name main as HEAD, so a clone checks the chain out
	if out, err := exec.Command("git", "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("hub HEAD: %v: %s", err, out)
	}
	hook := filepath.Join(bare, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.capsuleDir, "unpushed.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	head, err := gitops.Head(j.git)
	if err != nil || head == "" {
		t.Fatalf("local head: %q %v", head, err)
	}
	if end := hub.lastEnd(); end.Status != api.StatusBlocked || end.HeadSHA != head {
		t.Fatalf("the degraded close = %+v, want blocked carrying the local head %s", end, head)
	}
	if hubHasCommit(bare, head) {
		t.Fatal("setup: the sealed head must not have reached the hub")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	return head, bare
}

func hubHasCommit(bare, sha string) bool {
	return exec.Command("git", "--git-dir="+bare, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

func hubMain(t *testing.T, bare string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+bare, "rev-parse", "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("hub main: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// A blocked resume on the sealing machine continues FROM the sealed head:
// the fresh capsule adopts the unpushed commits from the quarantined
// scene, and the next publish pushes them to the hub.
func TestSameMachineBlockedResumeContinuesFromSealedHead(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	a, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	const wi = "wi_seal_same_machine"
	head, bare := sealBlockedWithUnpushedWork(t, hub, a, wi)

	startRunning(t, a, wi)
	j := a.jobOf(wi)
	if j == nil {
		t.Fatal("no job after the resume")
	}
	if got, err := gitops.Head(j.git); err != nil || got != head {
		t.Fatalf("resumed capsule head = %q (%v), want the sealed head %s", got, err, head)
	}
	if _, err := os.Stat(filepath.Join(j.capsuleDir, "unpushed.txt")); err != nil {
		t.Fatalf("the unpushed work must be in the resumed capsule: %v", err)
	}
	if qs := quarantinedGitDirs(j.git.GitDir); len(qs) == 0 {
		t.Fatal("the quarantined scene must stay on disk")
	}

	if err := a.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop after the resume: %v", err)
	}
	if end := hub.lastEnd(); end.Status != api.StatusWrapped {
		t.Fatalf("close after the resume = %+v, want wrapped", end)
	}
	main := hubMain(t, bare)
	if err := exec.Command("git", "--git-dir="+bare, "merge-base", "--is-ancestor", head, main).Run(); err != nil {
		t.Fatalf("the hub chain (main %s) must carry the sealed head %s: %v", main, head, err)
	}
}

// A blocked resume on the sealing machine whose quarantined scenes do not
// hold the sealed head is refused like another machine's (DAEMON-518);
// the owner's --accept-lost-work (the hub clears the seal) then
// continues from the hub's chain.
func TestSameMachineBlockedResumeWithoutSealedCommitIsRefused(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	a, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	const wi = "wi_seal_same_machine_lost"
	head, bare := sealBlockedWithUnpushedWork(t, hub, a, wi)
	// the sealed scene is gone from this machine
	if err := os.RemoveAll(a.gitDirFor(wi)); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { terminateJobAgent(a, wi) })
	err := a.Start(context.Background(), wi, StartOpts{})
	if err == nil || !strings.Contains(err.Error(), api.ErrDaemonSealedHeadMismatch) {
		t.Fatalf("a same-machine resume without the sealed commit = %v, want %s", err, api.ErrDaemonSealedHeadMismatch)
	}
	if end := hub.lastEnd(); end.Status != api.StatusBlocked {
		t.Fatalf("the refused claim must end blocked, got %+v", end)
	}
	hub.mu.Lock()
	still := hub.seals[wi].SealedHead
	delete(hub.seals, wi) // pf resume --accept-lost-work: the door clears the seal
	hub.mu.Unlock()
	if still != head {
		t.Fatalf("the refusal must keep the seal at %s, got %q", head, still)
	}

	startRunning(t, a, wi)
	j := a.jobOf(wi)
	if j == nil {
		t.Fatal("no job after --accept-lost-work")
	}
	if got, err := gitops.Head(j.git); err != nil || got != hubMain(t, bare) {
		t.Fatalf("the accepted-loss continue must start from the hub head, got %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(j.capsuleDir, "unpushed.txt")); !os.IsNotExist(err) {
		t.Fatalf("the abandoned work must not be in the capsule: %v", err)
	}
}

// A blocked resume on the sealing machine whose close reached aihub with
// nothing (every push refused) restores the task repo from the sealed
// scene: the unpushed task-repo commit and the uncommitted work both come
// back, from the same quarantined generation the capsule chain is
// adopted from.
func TestSameMachineBlockedResumeRestoresTaskReposFromSealedScene(t *testing.T) {
	g := newWIPRig(t, "wi_seal_task_repos")
	g.dirty(t)
	hook := filepath.Join(g.hubBare, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g.block(t)
	if last := g.hub.lastEnd(); last.HeadSHA == "" {
		t.Fatalf("setup: the blocked close must carry the sealed head, got %+v", last)
	}
	for _, ref := range []string{gitops.TaskRefName("app"), gitops.TaskWIPRefName("app")} {
		if _, ok := g.hubRef(t, ref); ok {
			t.Fatalf("setup: %s must not have reached aihub", ref)
		}
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	if err := g.r.Start(context.Background(), g.wi, StartOpts{}); err != nil {
		t.Fatalf("resume on the sealing machine: %v", err)
	}
	t.Cleanup(func() { terminateJobAgent(g.r, g.wi) })
	repoB := filepath.Join(testCapsuleDir(g.r, g.wi), capsule.DirRepos, "app")
	if got := carrierGit(t, repoB, "rev-parse", "HEAD"); got != g.workHead {
		t.Fatalf("resumed task repo HEAD = %q, want the unpushed commit %q", got, g.workHead)
	}
	if b, err := os.ReadFile(filepath.Join(repoB, "e2e_test.go")); err != nil || !strings.Contains(string(b), "never committed") {
		t.Fatalf("the uncommitted file is missing: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(repoB, "work.txt")); err != nil || string(b) != "committed work\nplus uncommitted edit\n" {
		t.Fatalf("the uncommitted edit is missing: %q, %v", b, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(g.r.gitDirFor(g.wi)), "pf-carrier-*")); len(left) != 0 {
		t.Fatalf("the sealed-scene store must be removed after the restore: %v", left)
	}
}
