package gitops

// gitops_taskref_test.go: the task-repo durability lane (polyforge#108)
// against REAL git — every publish mirrors each repos/<name> HEAD into
// the WI's bare repo as refs/pf-task/<name>, best-effort (a rejected
// carrier never fails the chain publish), and FetchTaskRef reads the
// carrier back for the claim-time restore.

import (
	"errors"
	"os"
	"os/exec"
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

// The task repo is agent-controlled: a pre-push hook planted in it must
// not run during the carrier push (it would run as the daemon with the
// credential askpass in its environment).
func TestTaskRefPushSkipsRepoHooks(t *testing.T) {
	setIdentity(t)
	o, base := taskOpts(t, "wi_taskhook")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	workHead := mkTaskRepoWithWork(t, o.CapsuleDir, "demo-src", filepath.Join(base, "origin"))
	marker := filepath.Join(base, "pre-push-ran")
	hook := filepath.Join(o.CapsuleDir, capsule.DirRepos, "demo-src", ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Control: the planted hook is live for a plain push.
	ctl := filepath.Join(base, "ctl.git")
	mkBare(t, ctl, false)
	mustRun(t, "git", "-C", filepath.Join(o.CapsuleDir, capsule.DirRepos, "demo-src"), "push", "-q", ctl, "HEAD:refs/heads/x")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: a plain push should have run the planted hook")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := CommitPush(o, "boundary"); err != nil {
		t.Fatalf("CommitPush: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName("demo-src")); !ok || got != workHead {
		t.Fatalf("carrier ref = %q (%v), want %q", got, ok, workHead)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the task repo's pre-push hook ran during the carrier push")
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

// Never force: a re-claim that started from a clone LACKING the carried
// commits publishes a diverged head. The carrier push is rejected
// non-fast-forward, the carrier on the hub stays on the WI's commits, and
// the divergence is raised as a task_ref_diverged error event (not only
// stderr).
func TestTaskRefDivergenceIsRejectedAndRaisedAsEvent(t *testing.T) {
	setIdentity(t)
	o, base := taskOpts(t, "wi_task3")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	workHead := mkTaskRepoWithWork(t, o.CapsuleDir, "demo-src", filepath.Join(base, "origin"))
	if _, _, _, err := CommitPush(o, "boundary"); err != nil {
		t.Fatalf("first CommitPush: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName("demo-src")); !ok || got != workHead {
		t.Fatalf("carrier = %q (%v), want %q", got, ok, workHead)
	}

	// Diverge: drop the work commit, commit something else.
	repoDir := filepath.Join(o.CapsuleDir, capsule.DirRepos, "demo-src")
	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(repoDir, "other.txt"), []byte("diverged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", repoDir, "add", "-A")
	mustRun(t, "git", "-C", repoDir, "commit", "-q", "-m", "diverged work")

	var kinds []string
	var body map[string]any
	o.EventSink = func(kind string, b map[string]any) {
		kinds = append(kinds, kind)
		body = b
	}
	if _, _, _, err := CommitPush(o, "boundary 2"); err != nil {
		t.Fatalf("a rejected carrier must not fail the chain publish: %v", err)
	}
	if got, _ := bareRefSHA(t, o.RemoteURL, TaskRefName("demo-src")); got != workHead {
		t.Fatalf("carrier moved to %q: a diverged push must never overwrite it (want %q)", got, workHead)
	}
	if len(kinds) != 1 || kinds[0] != EventTaskRefDiverged {
		t.Fatalf("events = %v, want one %s", kinds, EventTaskRefDiverged)
	}
	if body["repo"] != "demo-src" || body["ref"] != TaskRefName("demo-src") {
		t.Fatalf("event body = %v", body)
	}
}

// nonEnglishLocale returns an installed non-C/POSIX/English locale name
// (as `locale -a` lists it), or "" when none is available.
func nonEnglishLocale() string {
	out, err := exec.Command("locale", "-a").Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Fields(string(out)) {
		if l == "C" || l == "POSIX" || strings.HasPrefix(l, "C.") || strings.HasPrefix(l, "en") {
			continue
		}
		return l
	}
	return ""
}

// CEnv is deterministic: it drops every inherited LC_ALL/LANGUAGE and
// pins LC_ALL=C last, whatever the daemon's own environment says.
func TestCEnvPinsCLocale(t *testing.T) {
	got := CEnv([]string{"PATH=/bin", "LC_ALL=zh_CN.UTF-8", "LANGUAGE=zh_CN", "LANG=zh_CN.UTF-8"})
	var lcAll []string
	for _, kv := range got {
		if strings.HasPrefix(kv, "LANGUAGE=") {
			t.Fatalf("LANGUAGE leaked: %v", got)
		}
		if strings.HasPrefix(kv, "LC_ALL=") {
			lcAll = append(lcAll, kv)
		}
	}
	if len(lcAll) != 1 || lcAll[0] != "LC_ALL=C" || got[len(got)-1] != "LC_ALL=C" {
		t.Fatalf("LC_ALL must be exactly one trailing LC_ALL=C: %v", got)
	}
}

// IsNonFastForward must match REAL git push output, and git must run
// under the C locale however the daemon's environment is set.
func TestIsNonFastForwardOnRealGitOutput(t *testing.T) {
	setIdentity(t)
	loc := nonEnglishLocale()
	if loc == "" {
		t.Skip("no non-English locale installed (locale -a): the C-locale guarantee cannot be exercised against real git")
	}
	t.Setenv("LC_ALL", loc)
	t.Setenv("LANGUAGE", "zh_CN")
	base := t.TempDir()
	bare := filepath.Join(base, "b.git")
	mustRun(t, "git", "init", "-q", "--bare", "-b", "main", bare)
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	mustRun(t, "git", "clone", "-q", bare, a)
	mustRun(t, "git", "-C", a, "commit", "-q", "--allow-empty", "-m", "one")
	mustRun(t, "git", "-C", a, "push", "-q", "origin", "HEAD:main")
	mustRun(t, "git", "clone", "-q", bare, b)
	mustRun(t, "git", "-C", b, "commit", "-q", "--allow-empty", "-m", "b-side")
	mustRun(t, "git", "-C", a, "commit", "-q", "--allow-empty", "-m", "a-side")
	mustRun(t, "git", "-C", a, "push", "-q", "origin", "HEAD:main")

	out, err := git(nil, b, "push", "origin", "HEAD:main")
	if err == nil {
		t.Fatal("the diverged push must be rejected")
	}
	if !IsNonFastForward(err, out) {
		t.Fatalf("real git rejection not recognised: err=%v out=%q", err, out)
	}
	if !strings.Contains(err.Error(), "[rejected]") {
		t.Fatalf("git did not run under the C locale: %v", err)
	}
	if IsNonFastForward(errors.New("git push: Could not resolve host"), "") {
		t.Fatal("an unreachable remote is not a non-fast-forward")
	}
	// The carrier-absent answer is the ls-remote exit code, locale-free.
	o := Opts{WorkItemID: "wi_loc", RemoteURL: bare, Username: "u", Secret: "s"}
	if sha, err := FetchTaskRef(o, b, "nothing"); err != nil || sha != "" {
		t.Fatalf("absent carrier under a foreign locale: %q, %v", sha, err)
	}
}
