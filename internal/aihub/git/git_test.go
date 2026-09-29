package git

// Full-chain tests through the real git binary: httptest server + the
// git-http-backend proxy + clone/push. Each pre-receive violation gets a
// dedicated test: non-FF, ref deletion, gitlink, seq regression, blob
// over-limit; plus legal push/clone and credential fencing (old
// credential rejected after takeover).

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
)

// gitEnv is one fully wired environment.
type gitEnv struct {
	db     *core.DB
	owners *core.OwnerManager
	admin  *core.AdminManager
	wis    *core.WIManager
	mgr    *Manager
	ts     *httptest.Server
	userID string
}

func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not found: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	dataDir := t.TempDir()
	db, err := core.Open(dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	env := &gitEnv{
		db:     db,
		owners: core.NewOwnerManager(db),
		admin:  core.NewAdminManager(db),
		wis:    core.NewWIManager(db),
	}
	u, err := env.admin.CreateUser("root", "root@example.com", "admin", "human")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	env.userID = u.ID
	if _, err := env.admin.CreateProject(core.CreateProjectRequest{Name: "alpha"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	mgr, err := NewManager(dataDir, env.owners)
	if err != nil {
		t.Fatalf("git manager: %v", err)
	}
	env.mgr = mgr
	mux := http.NewServeMux()
	mux.Handle("/git/", http.HandlerFunc(mgr.HandleGit))
	env.ts = httptest.NewServer(mux)
	t.Cleanup(env.ts.Close)
	return env
}

// bundleNameTest pins the workflow bundle for git-chain test tickets
// (registration tickets are not claimable).
var bundleNameTest = "git-chain-bundle"

// claimWI creates + claims a WI, installs the bare repo, and returns the
// claim result.
func (e *gitEnv) claimWI(t *testing.T, caller string) (wiID string, res *core.ClaimResult) {
	t.Helper()
	wi, err := e.wis.Create(core.CreateRequest{
		Project: "alpha", Goal: "git chain test", Reporter: e.userID,
		BundleName: &bundleNameTest, // execution ticket: claimable
	})
	if err != nil {
		t.Fatalf("create wi: %v", err)
	}
	res, err = e.owners.Claim(wi.ID, core.ClaimRequest{Intent: core.IntentClaim, IdempotencyKey: "idem-" + wi.ID}, caller)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := e.mgr.EnsureBare(wi.ID); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	return wi.ID, res
}

// credURL embeds the credential the way the pf gitops layer does.
func (e *gitEnv) credURL(wiID, secret string) string {
	base := fmt.Sprintf("%s/git/%s.git", e.ts.URL, wiID)
	return strings.Replace(base, "http://", "http://"+wiID+":"+secret+"@", 1)
}

// runGit runs git in dir with the given env additions.
func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// mustGit fails the test on a git error.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// worktree initializes a local worktree whose HEAD is main.
func newWorktree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGit(t, dir, "init", "-b", "main", ".")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	return dir
}

// commitSeq writes a file and commits it with the seq trailer.
func commitSeq(t *testing.T, dir, name, content string, seq int64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-m", fmt.Sprintf("%s\n\nPolyforge-Seq: %d", name, seq))
}

// push pushes HEAD:main to the given remote URL.
func push(t *testing.T, dir, remote string) error {
	t.Helper()
	_, err := runGit(t, dir, "push", remote, "HEAD:refs/heads/main")
	return err
}

// pushWithAttempt pushes HEAD:main carrying --push-option=attempt-id=<id>,
// exactly the way the pf gitops choke point does.
func pushWithAttempt(t *testing.T, dir, remote, attemptID string) (string, error) {
	t.Helper()
	return runGit(t, dir, "push", "--push-option=attempt-id="+attemptID, remote, "HEAD:refs/heads/main")
}

// TestPushOptionAdvertisedAndFenced (§六 备层): the bare repo advertises
// push options (without the advertisement git rejects every push that
// carries one — and the gitops choke point now always does), a push whose
// attempt-id option matches the credential's attempt succeeds, and one
// with a foreign id is rejected by the pre-receive hook.
func TestPushOptionAdvertisedAndFenced(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	attempt := res.Claim.AttemptID
	if attempt == "" {
		t.Fatalf("claim result carries no attempt id")
	}
	// The bare repo advertises push options — both on fresh creation and
	// idempotently for a pre-existing repo (re-EnsureBare must not regress
	// it).
	out := mustGit(t, e.mgr.RepoDir(wiID), "config", "receive.advertisePushOptions")
	if strings.TrimSpace(out) != "true" {
		t.Fatalf("receive.advertisePushOptions = %q, want true", strings.TrimSpace(out))
	}
	if err := e.mgr.EnsureBare(wiID); err != nil {
		t.Fatalf("re-ensure bare: %v", err)
	}
	out = mustGit(t, e.mgr.RepoDir(wiID), "config", "receive.advertisePushOptions")
	if strings.TrimSpace(out) != "true" {
		t.Fatalf("receive.advertisePushOptions after re-ensure = %q, want true", strings.TrimSpace(out))
	}

	cred := e.credURL(wiID, res.Credential.Secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	// A push WITHOUT the option still succeeds through the proxy (the
	// hook only compares when both sides of the fence exist).
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("option-less push rejected: %v", err)
	}
	// The matching attempt-id option succeeds.
	commitSeq(t, wt, "b.txt", "two\n", 2)
	if out, err := pushWithAttempt(t, wt, cred, attempt); err != nil {
		t.Fatalf("push with the matching attempt-id option failed: %v (%s)", err, out)
	}
	// A foreign attempt id is rejected by the pre-receive hook.
	commitSeq(t, wt, "c.txt", "three\n", 3)
	out, err := pushWithAttempt(t, wt, cred, "ra_someone_else")
	if err == nil {
		t.Fatalf("push with a foreign attempt-id accepted: %s", out)
	}
	if !strings.Contains(out, "does not match the credential's attempt") {
		t.Fatalf("wrong rejection: %s", out)
	}
	// The rejected push moved nothing: the remote tip still ends at b.txt,
	// and a corrected push (same commits, right id) lands cleanly.
	second := filepath.Join(t.TempDir(), "second")
	mustGit(t, t.TempDir(), "clone", cred, second)
	if _, err := os.Stat(filepath.Join(second, "c.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected push leaked c.txt into the bare repo: %v", err)
	}
	if out, err := pushWithAttempt(t, wt, cred, attempt); err != nil {
		t.Fatalf("corrected push failed: %v (%s)", err, out)
	}
}

// TestPushOptionFenceAbsentExpected: when PF_EXPECTED_ATTEMPT is absent
// (a push that never traveled through the proxy — e.g. a direct local
// push against the bare repo for testing) the hook must NOT fence, even
// though the push carries an attempt-id option: there is nothing to
// compare against.
func TestPushOptionFenceAbsentExpected(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	// Direct push to the bare repo path (no proxy, no PF_EXPECTED_ATTEMPT)
	// carrying an attempt-id option: accepted.
	out, err := pushWithAttempt(t, wt, e.mgr.RepoDir(wiID), res.Claim.AttemptID)
	if err != nil {
		t.Fatalf("local option-carrying push rejected: %v (%s)", err, out)
	}
}

func TestLegalPushAndClone(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	cred := e.credURL(wiID, res.Credential.Secret)

	// Clone the empty bare repo (clone works while empty).
	wt := newWorktree(t)
	mustGit(t, wt, "clone", cred, filepath.Join(wt, "clone"))
	clone := filepath.Join(wt, "clone")

	// Worktree against the remote: commit seq 1, push, commit seq 2, push.
	commitSeq(t, clone, "capsule.txt", "step one\n", 1)
	if err := push(t, clone, cred); err != nil {
		t.Fatalf("first push failed: %v", err)
	}
	commitSeq(t, clone, "step two.txt", "step two\n", 2)
	if err := push(t, clone, cred); err != nil {
		t.Fatalf("second push failed: %v", err)
	}

	// A fresh clone sees the pushed history.
	second := filepath.Join(wt, "second")
	mustGit(t, wt, "clone", cred, second)
	if data, err := os.ReadFile(filepath.Join(second, "step two.txt")); err != nil || string(data) != "step two\n" {
		t.Fatalf("fresh clone missing content: %v %q", err, data)
	}
	// Both commits carry their seq trailers.
	log := mustGit(t, second, "log", "--format=%(trailers:key=Polyforge-Seq,valueonly)")
	if !strings.Contains(strings.ReplaceAll(log, " ", ""), "1") || !strings.Contains(strings.ReplaceAll(log, " ", ""), "2") {
		t.Fatalf("seq trailers missing from history: %q", log)
	}
	// The pre-receive hook file exists and is executable.
	hook := filepath.Join(e.mgr.RepoDir(wiID), "hooks", "pre-receive")
	info, err := os.Stat(hook)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("pre-receive hook missing or not executable: %v", err)
	}
}

func TestPushWithoutCredentialRejected(t *testing.T) {
	e := newGitEnv(t)
	wiID, _ := e.claimWI(t, e.userID)
	anon := fmt.Sprintf("%s/git/%s.git", e.ts.URL, wiID)
	wt := newWorktree(t)
	commitSeq(t, wt, "f.txt", "x\n", 1)
	if err := push(t, wt, anon); err == nil {
		t.Fatalf("push without credentials accepted")
	}
}

func TestNonFastForwardRejected(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	cred := e.credURL(wiID, res.Credential.Secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("base push: %v", err)
	}
	// Rewrite history: amend the pushed commit (same seq, new sha via a
	// changed message so the commit id genuinely differs).
	mustGit(t, wt, "commit", "--amend", "-m", "a.txt rewritten\n\nPolyforge-Seq: 1")
	err := push(t, wt, cred)
	if err == nil {
		t.Fatalf("non-FF push accepted")
	}
	out, _ := runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if !strings.Contains(out, "non-fast-forward") {
		t.Fatalf("wrong rejection: %s", out)
	}
}

func TestRefDeletionRejected(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	cred := e.credURL(wiID, res.Credential.Secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("base push: %v", err)
	}
	out, err := runGit(t, wt, "push", cred, ":refs/heads/main")
	if err == nil {
		t.Fatalf("ref deletion accepted")
	}
	if !strings.Contains(out, "ref deletion") {
		t.Fatalf("wrong rejection: %s", out)
	}
}

func TestGitlinkRejected(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	cred := e.credURL(wiID, res.Credential.Secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("base push: %v", err)
	}
	// Stage a gitlink (submodule commit reference) without the object.
	mustGit(t, wt, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendored-thing")
	commitSeq(t, wt, "marker.txt", "two\n", 2)
	// Re-stage the gitlink entry lost by commitSeq's add -A.
	mustGit(t, wt, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendored-thing")
	mustGit(t, wt, "commit", "--amend", "--no-edit")
	out, err := runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("gitlink push accepted: %s", out)
	}
	if !strings.Contains(out, "gitlink") {
		t.Fatalf("wrong rejection: %s", out)
	}
}

func TestSeqRegressionRejected(t *testing.T) {
	e := newGitEnv(t)
	wiID, res := e.claimWI(t, e.userID)
	cred := e.credURL(wiID, res.Credential.Secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("base push: %v", err)
	}
	// Regression: a NEW commit whose seq does not increase past the tip.
	commitSeq(t, wt, "b.txt", "two\n", 1)
	out, err := runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("seq regression accepted: %s", out)
	}
	if !strings.Contains(out, "Polyforge-Seq must increase") {
		t.Fatalf("wrong rejection: %s", out)
	}
	// Missing trailer entirely.
	mustGit(t, wt, "reset", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(wt, "c.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "-m", "no trailer at all")
	out, err = runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("missing trailer accepted: %s", out)
	}
	if !strings.Contains(out, "missing an integer Polyforge-Seq trailer") {
		t.Fatalf("wrong rejection: %s", out)
	}
}

func TestBlobOverLimitRejected(t *testing.T) {
	e := newGitEnv(t)
	// Shrink the limit so the test stays fast; the hook reads it via the
	// env the proxy sets.
	e.mgr.MaxBlobBytes = 512
	wiID, res := e.claimWI(t, e.userID)
	cred := e.credURL(wiID, res.Credential.Secret)
	wt := newWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "big.bin"), bytes.Repeat([]byte{0}, 2048), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "-m", "big\n\nPolyforge-Seq: 1")
	out, err := runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("oversized blob accepted: %s", out)
	}
	if !strings.Contains(out, "blob exceeds") {
		t.Fatalf("wrong rejection: %s", out)
	}
}

func TestOldCredentialRejectedAfterTakeover(t *testing.T) {
	e := newGitEnv(t)
	wiID, res1 := e.claimWI(t, e.userID)
	cred1 := e.credURL(wiID, res1.Credential.Secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred1); err != nil {
		t.Fatalf("push with the live credential failed: %v", err)
	}
	// Takeover: generation 2, new credential; the old one must be dead
	// at the git face (main fencing layer).
	res2, err := e.owners.Claim(wiID, core.ClaimRequest{Intent: core.IntentTakeover, MachineID: "machine-2"}, e.userID)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	commitSeq(t, wt, "b.txt", "two\n", 2)
	out, perr := runGit(t, wt, "push", cred1, "HEAD:refs/heads/main")
	if perr == nil {
		t.Fatalf("push with the REVOKED credential accepted: %s", out)
	}
	if !strings.Contains(out, "Authentication") && !strings.Contains(out, "401") {
		t.Fatalf("wrong rejection for revoked credential: %s", out)
	}
	// The new credential works and continues the seq chain.
	cred2 := e.credURL(wiID, res2.Credential.Secret)
	if err := push(t, wt, cred2); err != nil {
		t.Fatalf("push with the new credential failed: %v", err)
	}
	// And a fetch with the old credential is refused too.
	out, cerr := runGit(t, t.TempDir(), "clone", cred1, filepath.Join(t.TempDir(), "x"))
	if cerr == nil {
		t.Fatalf("clone with the revoked credential accepted: %s", out)
	}
}
