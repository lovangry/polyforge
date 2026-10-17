package runtime

// crash_window_test.go: kill -9 windows of the runtime. Each test builds
// the on-disk state a daemon killed at that exact point leaves (or kills
// the in-process state there), restarts from disk against the fake hub
// and asserts convergence.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// TestRestartClearsIndexLockLeftByKilledCommit (CR-3): the daemon died
// inside a git commit, leaving a fresh index.lock. The restart reaps the
// WI's orphans, removes the lock and the recovery's commit-dirty push
// lands — the lock's age does not decide anything.
func TestRestartClearsIndexLockLeftByKilledCommit(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	wi := "wi_cr3_lock"
	j := seedJob(r, wi)
	bare := initGitCapsule(t, j)
	r.mu.Lock()
	delete(r.jobs, wi) // the restart seeds from state.json
	r.mu.Unlock()

	rec := matrixRecord(r, wi, 0, "")
	rec.RemoteURL = bare
	writeStateFile(t, r, []*jobRecord{rec})
	hub.hold(wi, rec.SessionID, api.StatusRunning)
	if err := os.WriteFile(filepath.Join(rec.CapsuleDir, "dirty.txt"), []byte("unflushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(rec.GitDir, "index.lock")
	if err := os.WriteFile(lock, []byte("torn index"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for sp.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sp.callCount() == 0 {
		t.Fatal("the restart did not recover the work item")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("the dead commit's index.lock must be gone after the reap: %v", err)
	}
	out, err := exec.Command("git", "--git-dir", bare, "log", "--name-only", "--format=%s", "main").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(out), "dirty.txt") {
		t.Fatalf("the recovery's commit-dirty push must land; hub log:\n%s", out)
	}
}

// separateGitScene builds a worktree with an external git-dir, the
// production capsule layout.
func separateGitScene(t *testing.T) (capDir, gitDir string) {
	t.Helper()
	base := t.TempDir()
	capDir = filepath.Join(base, "capsules", "wi_q6")
	gitDir = filepath.Join(base, "git", "wi_q6.git")
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "--separate-git-dir="+gitDir, capDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return capDir, gitDir
}

// gitDirOf answers the git-dir git resolves for a worktree.
func gitDirOf(t *testing.T, worktree string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(worktree))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	return p
}

func quarantinedSibling(t *testing.T, path string) string {
	t.Helper()
	m, _ := filepath.Glob(path + ".quarantined.*")
	if len(m) != 1 {
		t.Fatalf("want one quarantined sibling of %s, got %v", path, m)
	}
	p, _ := filepath.EvalSymlinks(m[0])
	return p
}

// TestQuarantinedWorktreePointsAtItsOwnGitDir (CR-6): after a quarantine
// and the fresh clone at the old git-dir path, git run in the
// quarantined tree must reach the quarantined git-dir, never the live one.
func TestQuarantinedWorktreePointsAtItsOwnGitDir(t *testing.T) {
	capDir, gitDir := separateGitScene(t)
	if err := quarantineCapsule(capDir, gitDir); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "--separate-git-dir="+gitDir, capDir).CombinedOutput(); err != nil {
		t.Fatalf("fresh clone stand-in: %v: %s", err, out)
	}
	qTree := quarantinedSibling(t, capDir)
	if got, want := gitDirOf(t, qTree), quarantinedSibling(t, gitDir); got != want {
		t.Fatalf("the quarantined tree resolves git-dir %q, want its own %q", got, want)
	}
}

// TestCrashMidQuarantineKeepsPairing (CR-6): killed after the git-dir
// moved and the pointer followed it, before the worktree moved: the next
// claim's quarantine moves the worktree still paired with that git-dir.
func TestCrashMidQuarantineKeepsPairing(t *testing.T) {
	capDir, gitDir := separateGitScene(t)
	moved := gitDir + ".quarantined.crash"
	if err := os.Rename(gitDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, ".git"), []byte("gitdir: "+moved+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := quarantineCapsule(capDir, gitDir); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "--separate-git-dir="+gitDir, capDir).CombinedOutput(); err != nil {
		t.Fatalf("fresh clone stand-in: %v: %s", err, out)
	}
	want, _ := filepath.EvalSymlinks(moved)
	if got := gitDirOf(t, quarantinedSibling(t, capDir)); got != want {
		t.Fatalf("the quarantined tree resolves git-dir %q, want its pair %q", got, want)
	}
}

// TestCarrierFetchBlipDoesNotBlock (CR-10): a re-claim whose carrier read
// meets one hub blip retries it instead of failing the claim (which
// closed the work item blocked until a human resumed it).
func TestCarrierFetchBlipDoesNotBlock(t *testing.T) {
	origFetch, origWait := fetchTaskRefFn, carrierFetchBackoff
	defer func() { fetchTaskRefFn, carrierFetchBackoff = origFetch, origWait }()
	carrierFetchBackoff = time.Millisecond
	calls := 0
	fetchTaskRefFn = func(context.Context, gitops.Opts, string, string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("ls-remote: connection reset by peer")
		}
		return "", nil
	}
	r := &Runtime{}
	j := &job{wi: "wi_cr10"}
	if _, err := r.restoreTaskRepoState(context.Background(), j, "app", t.TempDir()); err != nil {
		t.Fatalf("one hub blip failed the carrier restore: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the blip must be retried once, got %d reads", calls)
	}
}
