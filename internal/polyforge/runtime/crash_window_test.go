package runtime

// crash_window_test.go: kill -9 windows of the runtime. Each test builds
// the on-disk state a daemon killed at that exact point leaves (or kills
// the in-process state there), restarts from disk against the fake hub
// and asserts convergence.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
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
