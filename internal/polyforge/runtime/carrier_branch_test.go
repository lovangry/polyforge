package runtime

// carries: internal/polyforge/runtime/repos.go
//
// carrier_branch_test: the carrier restore's branch under a pin
// (polyforge#45, design point 5). A pinned repo's carrier restore
// attaches HEAD to the PIN by name — the clone already stands on it
// (--branch), and a detached HEAD (the shape where the symbolic-ref
// read cannot name a branch) must not drift the restore to the
// deterministic fallback either. The carried SHA still rules: checkout
// -B moves the pin to it. An unpinned restore keeps today's behavior
// exactly: the clone's own symbolic branch when it has one, the
// fallback branch (pf-carrier) when HEAD is detached.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// workHead answers git rev-parse HEAD of a worktree.
func workHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// carrierBranchRig answers a clone of a two-branch remote checked out
// DETACHED at feat-cloud's tip (adversarial: symbolic-ref names nothing),
// with one carrier commit ahead of that tip pushed to a bare hub's
// refs/pf-task/app, plus the gitops store that reads it.
func carrierBranchRig(t *testing.T) (dir, hubBare, carrierTip string) {
	t.Helper()
	setGitIdentity(t)
	url, _, _ := fakeTaskRemoteBranches(t)

	dir = t.TempDir()
	if out, err := exec.Command("git", "clone", "--quiet", url, dir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "--quiet", "--detach", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("detach the fresh clone: %v: %s", err, out)
	}
	gitAt := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := exec.Command("git", "-C", dir, "symbolic-ref", "HEAD").Output(); err == nil {
		t.Fatal("test setup: the clone must be DETACHED (symbolic-ref must fail)")
	}
	// the previous session's one commit on the pinned branch
	os.WriteFile(filepath.Join(dir, "work.txt"), []byte("durable work\n"), 0o644)
	gitAt("add", "-A")
	gitAt("commit", "-m", "repo work")
	carrierTip = gitAt("rev-parse", "HEAD")

	hubBare = t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", hubBare).CombinedOutput(); err != nil {
		t.Fatalf("hub bare init: %v: %s", err, out)
	}
	gitAt("push", "--quiet", hubBare, "HEAD:refs/pf-task/app")
	// the clone no longer stands on the carrier tip: a fresh clone
	// would be at the branch tip, not the previous session's head
	gitAt("checkout", "--quiet", "--detach", "HEAD~1")
	return dir, hubBare, carrierTip
}

// A pinned repo's carrier restore attaches HEAD to the pin by name, with
// the carried SHA still ruling the branch's tip.
func TestCarrierRestoreAttachesToPinnedBranch(t *testing.T) {
	dir, hubBare, carrierTip := carrierBranchRig(t)
	j := &job{wi: "wi_carrier_pin",
		repos: []api.ProjectRepo{{Name: "app", URL: "unused", Branch: "feat-cloud"}},
		git:   gitops.Opts{WorkItemID: "wi_carrier_pin", RemoteURL: hubBare, GitDir: filepath.Join(t.TempDir(), "git")}}
	r := &Runtime{}

	sha, err := r.restoreTaskRepoState(context.Background(), j, j.git, "app", dir)
	if err != nil {
		t.Fatalf("restoreTaskRepoState: %v", err)
	}
	if sha != carrierTip {
		t.Fatalf("restored SHA = %q, want the carrier tip %q", sha, carrierTip)
	}
	if got := symbolicRef(t, dir); got != "feat-cloud" {
		t.Fatalf("the restored clone stands on %q, want the pinned branch feat-cloud", got)
	}
	if got := workHead(t, dir); got != carrierTip {
		t.Fatalf("the pinned branch's tip after restore = %q, want the carrier tip %q (the carried SHA rules)", got, carrierTip)
	}
}

// The same restore without a pin keeps today's behavior: a detached
// clone falls back to the deterministic branch.
func TestCarrierRestoreUnpinnedKeepsTheFallback(t *testing.T) {
	dir, hubBare, carrierTip := carrierBranchRig(t)
	j := &job{wi: "wi_carrier_plain",
		repos: []api.ProjectRepo{{Name: "app", URL: "unused"}},
		git:   gitops.Opts{WorkItemID: "wi_carrier_plain", RemoteURL: hubBare, GitDir: filepath.Join(t.TempDir(), "git")}}
	r := &Runtime{}

	if _, err := r.restoreTaskRepoState(context.Background(), j, j.git, "app", dir); err != nil {
		t.Fatalf("restoreTaskRepoState: %v", err)
	}
	if got := symbolicRef(t, dir); got != carrierFallbackBranch {
		t.Fatalf("an unpinned detached restore stands on %q, want the fallback %q", got, carrierFallbackBranch)
	}
	if got := workHead(t, dir); got != carrierTip {
		t.Fatalf("the restored HEAD = %q, want the carrier tip %q", got, carrierTip)
	}
}
