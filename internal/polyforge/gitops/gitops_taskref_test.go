package gitops

// gitops_taskref_test.go: the task-repo durability lane (polyforge#108)
// against REAL git — every publish mirrors each repos/<name> HEAD into
// the WI's bare repo as refs/pf-task/<name>, best-effort (a rejected
// carrier never fails the chain publish), and FetchTaskRef reads the
// carrier back for the claim-time restore.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// taskOpts is testOpts with the base dir answered, so a test can lay a
// task-repo origin next to the capsule and the hub bare.
func taskOpts(t *testing.T, wi string) (Opts, string) {
	t.Helper()
	base := t.TempDir()
	o := Opts{
		WorkItemID: wi,
		CapsuleDir: filepath.Join(base, "capsules", wi),
		GitDir:     filepath.Join(base, "git", wi+".git"),
		RemoteURL:  filepath.Join(base, "hub", wi+".git"),
		Username:   wi,
		Secret:     "push-secret",
	}
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return o, base
}

// mkTaskCapsule is mkCapsule plus the capsule's own .gitignore — exactly
// what materialize writes before cloneTaskRepos runs — so the task clone
// under repos/ stays out of the capsule index the way production lays it
// (without it the gitlink precheck correctly refuses the whole publish).
func mkTaskCapsule(t *testing.T, o Opts) {
	t.Helper()
	mkCapsule(t, o)
	if err := os.WriteFile(filepath.Join(o.CapsuleDir, ".gitignore"), []byte(capsule.GitignoreContent), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkTaskRepoWithWork lays a task-repo origin (one init commit), clones it
// under the capsule's repos/<name>, and makes one local unpushed commit
// on top (the agent's work). Answers the work head SHA.
func mkTaskRepoWithWork(t *testing.T, capsuleDir, name, originDir string) string {
	t.Helper()
	origin := filepath.Join(originDir, name)
	if out, err := run("git", "init", "-q", "-b", "main", origin); err != nil {
		t.Fatalf("task origin init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(origin, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", origin, "add", "-A")
	mustRun(t, "git", "-C", origin, "commit", "-q", "-m", "init")
	repoDir := filepath.Join(capsuleDir, capsule.DirRepos, name)
	mustRun(t, "git", "clone", "-q", origin, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "work.txt"), []byte("agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", repoDir, "add", "-A")
	mustRun(t, "git", "-C", repoDir, "commit", "-q", "-m", "repo work")
	return strings.TrimSpace(mustRun(t, "git", "-C", repoDir, "rev-parse", "HEAD"))
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := run(args...)
	if err != nil {
		t.Fatalf("%v: %s", args, out)
	}
	return out
}

func bareRefSHA(t *testing.T, bare, ref string) (string, bool) {
	t.Helper()
	out, err := run("git", "--git-dir", bare, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// A chain publish must carry the task-repo state with it: each
// repos/<name> HEAD lands in the WI's bare repo under
// refs/pf-task/<name> (the session-independent carrier a re-claim
// restores from), next to the chain tip.
func TestCommitPushCarriesTaskRefs(t *testing.T) {
	setIdentity(t)
	o, base := taskOpts(t, "wi_task1")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	workHead := mkTaskRepoWithWork(t, o.CapsuleDir, "demo-src", filepath.Join(base, "origin"))
	// A task repo with no commits carries nothing (unborn HEAD: honest skip).
	mustRun(t, "git", "init", "-q", "-b", "main", filepath.Join(o.CapsuleDir, capsule.DirRepos, "empty"))

	sha, _, _, err := CommitPush(o, "boundary")
	if err != nil {
		t.Fatalf("CommitPush: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, "refs/heads/main"); !ok || got != sha {
		t.Fatalf("chain tip %q (%v), want the commit SHA %q", got, ok, sha)
	}
	got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName("demo-src"))
	if !ok || got != workHead {
		t.Fatalf("carrier ref = %q (%v), want the task work head %q", got, ok, workHead)
	}
	if _, ok := bareRefSHA(t, o.RemoteURL, TaskRefName("empty")); ok {
		t.Fatal("an unborn task repo must not produce a carrier ref")
	}
}

// Best-effort-loud, mechanically: a carrier push REJECTED by the remote
// (this test's simplified hook enforces the seq trailer on every ref —
// stricter than production's, which exempts refs/pf-task/*; pinned in
// package git) must not fail the chain publish. The chain tip lands; the
// rejected carrier does not; CommitPush answers success.
func TestCommitPushTaskRefRejectionNeverBlocksTheChain(t *testing.T) {
	setIdentity(t)
	o, base := taskOpts(t, "wi_task2")
	mkBare(t, o.RemoteURL, true) // hook: seq enforced on EVERY ref
	mkTaskCapsule(t, o)
	mkTaskRepoWithWork(t, o.CapsuleDir, "demo-src", filepath.Join(base, "origin"))

	sha, _, _, err := CommitPush(o, "boundary")
	if err != nil {
		t.Fatalf("a rejected task-ref push must never fail the chain publish: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, "refs/heads/main"); !ok || got != sha {
		t.Fatalf("chain tip missing after a rejected carrier: %q (%v)", got, ok)
	}
	if _, ok := bareRefSHA(t, o.RemoteURL, TaskRefName("demo-src")); ok {
		t.Fatal("the rejected carrier ref must not be present")
	}
}

// The claim-side read: a fresh clone of the task origin (the re-claim
// shape — the previous session's capsule directory is gone) plus
// FetchTaskRef answers the carried head, and the update-ref + reset
// recipe lands the worktree on it, work files and all. With no carrier
// recorded, FetchTaskRef answers "" quietly.
func TestFetchTaskRefRoundTrip(t *testing.T) {
	setIdentity(t)
	o, base := taskOpts(t, "wi_task3")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	origin := filepath.Join(base, "origin", "demo-src")
	workHead := mkTaskRepoWithWork(t, o.CapsuleDir, "demo-src", filepath.Join(base, "origin"))
	if _, _, _, err := CommitPush(o, "boundary"); err != nil {
		t.Fatalf("CommitPush: %v", err)
	}

	// No carrier for a repo that was never published: quiet absence.
	emptyDir := filepath.Join(base, "fresh-empty")
	mustRun(t, "git", "clone", "-q", origin, emptyDir)
	sha, err := FetchTaskRef(o, emptyDir, "demo-src-nothing")
	if err != nil || sha != "" {
		t.Fatalf("absent carrier: sha %q err %v, want \"\" nil", sha, err)
	}

	// The re-claim shape: a FRESH clone of the bare origin, previous
	// session gone.
	fresh := filepath.Join(base, "fresh-clone")
	mustRun(t, "git", "clone", "-q", origin, fresh)
	sha, err = FetchTaskRef(o, fresh, "demo-src")
	if err != nil {
		t.Fatalf("FetchTaskRef: %v", err)
	}
	if sha != workHead {
		t.Fatalf("carrier answered %q, want the work head %q", sha, workHead)
	}
	// The restore recipe (runtime.restoreTaskRepoState's git side): point
	// the checked-out branch at the carried head, sync index+worktree.
	mustRun(t, "git", "-C", fresh, "update-ref", "-m", "pf: restore task-repo state across re-claim", "refs/heads/main", sha)
	mustRun(t, "git", "-C", fresh, "reset", "--hard", "--quiet")
	if got := strings.TrimSpace(mustRun(t, "git", "-C", fresh, "rev-parse", "HEAD")); got != workHead {
		t.Fatalf("restored HEAD %q, want %q", got, workHead)
	}
	if b, err := os.ReadFile(filepath.Join(fresh, "work.txt")); err != nil || string(b) != "agent work\n" {
		t.Fatalf("restored work.txt wrong: %q, %v", b, err)
	}
}

// The lane is a no-op on a capsule with no repos/ tree (the common
// no-inventory shape): nothing pushed, nothing logged, publish unaffected.
func TestPushTaskRefsWithoutReposIsANoop(t *testing.T) {
	setIdentity(t)
	o, _ := taskOpts(t, "wi_task4")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	writeDirty(t, o, "state.txt", "one\n") // a publishable chain commit
	if _, _, _, err := CommitPush(o, "boundary"); err != nil {
		t.Fatalf("CommitPush: %v", err)
	}
	if _, ok := bareRefSHA(t, o.RemoteURL, TaskCarrierNamespace+"x"); ok {
		t.Fatal("no carrier ref may exist without a repos/ tree")
	}
}
