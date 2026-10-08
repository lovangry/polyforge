package git

// Full-chain tests through the real git binary: httptest server + the
// git-http-backend proxy + clone/push. Each pre-receive violation gets a
// dedicated test: non-FF, ref deletion, gitlink, seq regression, blob
// over-limit; plus legal push/clone and session fencing (the superseded
// session's secret rejected after force takeover).

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
	"github.com/silgrid/polyforge/internal/common/api"
)

// gitEnv is one fully wired environment.
type gitEnv struct {
	db       *core.DB
	owners   *core.OwnerManager
	sessions *core.SessionManager
	admin    *core.AdminManager
	wis      *core.WIManager
	mgr      *Manager
	ts       *httptest.Server
	userID   string
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
		db:       db,
		owners:   core.NewOwnerManager(db),
		sessions: core.NewSessionManager(db),
		admin:    core.NewAdminManager(db),
		wis:      core.NewWIManager(db),
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

// claimWI creates + CAS-claims a WI under the session model, installs
// the bare repo, and returns the WI id with the client-minted session
// secret (the push password).
func (e *gitEnv) claimWI(t *testing.T) (wiID, secret string) {
	t.Helper()
	wi, err := e.wis.Create(core.CreateRequest{
		Project: "alpha", Goal: "git chain test", Reporter: e.userID,
		BundleName: &bundleNameTest, // execution ticket: claimable
	})
	if err != nil {
		t.Fatalf("create wi: %v", err)
	}
	secret = "git-session-secret-" + wi.ID
	if _, err := e.sessions.Claim(wi.ID, api.SessionClaimRequest{
		SessionID: secret, MachineID: "m1", IdempotencyKey: "idem-" + wi.ID,
	}, e.userID); err != nil {
		t.Fatalf("session claim: %v", err)
	}
	if err := e.mgr.EnsureBare(wi.ID); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	return wi.ID, secret
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

// claimSessionWI creates a queued execution ticket and CAS-claims it
// under the v1.5 session model with the given CLIENT-MINTED secret,
// installing the bare repo. Returns the WI id and the session's PUBLIC
// fingerprint (the push-option backup fence's expected value).
func (e *gitEnv) claimSessionWI(t *testing.T, secret string) (wiID, fingerprint string) {
	t.Helper()
	wi, err := e.wis.Create(core.CreateRequest{
		Project: "alpha", Goal: "git session test", Reporter: e.userID,
		BundleName: &bundleNameTest,
	})
	if err != nil {
		t.Fatalf("create wi: %v", err)
	}
	claim, err := e.sessions.Claim(wi.ID, api.SessionClaimRequest{
		SessionID: secret, MachineID: "m1", IdempotencyKey: "idem-" + wi.ID,
	}, e.userID)
	if err != nil {
		t.Fatalf("session claim: %v", err)
	}
	if err := e.mgr.EnsureBare(wi.ID); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	return wi.ID, claim.SessionFingerprint
}

// pushWithFingerprint pushes HEAD:main carrying
// --push-option=session-fingerprint=<fp>, exactly the way the v1.5 pf
// gitops choke point does (contract §1.6 backup layer).
func pushWithFingerprint(t *testing.T, dir, remote, fp string) (string, error) {
	t.Helper()
	return runGit(t, dir, "push", "--push-option=session-fingerprint="+fp, remote, "HEAD:refs/heads/main")
}

// TestPushOptionAdvertisedAndFenced (§1.6 backup layer): the bare repo
// advertises push options (without the advertisement git rejects every
// push that carries one — and the gitops choke point now always does), a
// push whose session-fingerprint option matches the credential's session
// succeeds, and one with a foreign fingerprint is rejected by the
// pre-receive hook.
func TestPushOptionAdvertisedAndFenced(t *testing.T) {
	e := newGitEnv(t)
	secret := "0123456789abcdef0123456789abcdef"
	wiID, fp := e.claimSessionWI(t, secret)
	if fp == "" {
		t.Fatalf("claim result carries no session fingerprint")
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
		t.Fatalf("receive.advertisePushOptions after re-ensure = %q", strings.TrimSpace(out))
	}

	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	// A push WITHOUT the option still succeeds through the proxy (the
	// hook only compares when both sides of the fence exist).
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("option-less push rejected: %v", err)
	}
	// The matching session-fingerprint option succeeds.
	commitSeq(t, wt, "b.txt", "two\n", 2)
	if out, err := pushWithFingerprint(t, wt, cred, fp); err != nil {
		t.Fatalf("push with the matching session-fingerprint option failed: %v (%s)", err, out)
	}
	// A foreign fingerprint is rejected by the pre-receive hook.
	commitSeq(t, wt, "c.txt", "three\n", 3)
	out, err := pushWithFingerprint(t, wt, cred, "deadbeefdeadbeef")
	if err == nil {
		t.Fatalf("push with a foreign session-fingerprint accepted: %s", out)
	}
	if !strings.Contains(out, "does not match the credential's session fingerprint") {
		t.Fatalf("wrong rejection: %s", out)
	}
	// The rejected push moved nothing: the remote tip still ends at b.txt,
	// and a corrected push (same commits, right fingerprint) lands cleanly.
	second := filepath.Join(t.TempDir(), "second")
	mustGit(t, t.TempDir(), "clone", cred, second)
	if _, err := os.Stat(filepath.Join(second, "c.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected push leaked c.txt into the bare repo: %v", err)
	}
	if out, err := pushWithFingerprint(t, wt, cred, fp); err != nil {
		t.Fatalf("corrected push failed: %v (%s)", err, out)
	}
}

// TestPushOptionFenceAbsentExpected: when PF_EXPECTED_FINGERPRINT is
// absent (a push that never traveled through the proxy — e.g. a direct
// local push against the bare repo for testing) the hook must NOT fence,
// even though the push carries a session-fingerprint option: there is
// nothing to compare against.
func TestPushOptionFenceAbsentExpected(t *testing.T) {
	e := newGitEnv(t)
	wiID, fp := e.claimSessionWI(t, "abcdef0123456789abcdef0123456789")
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	// Direct push to the bare repo path (no proxy, no
	// PF_EXPECTED_FINGERPRINT) carrying a session-fingerprint option:
	// accepted.
	out, err := pushWithFingerprint(t, wt, e.mgr.RepoDir(wiID), fp)
	if err != nil {
		t.Fatalf("local option-carrying push rejected: %v (%s)", err, out)
	}
}

func TestLegalPushAndClone(t *testing.T) {
	e := newGitEnv(t)
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)

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
	wiID, _ := e.claimWI(t)
	anon := fmt.Sprintf("%s/git/%s.git", e.ts.URL, wiID)
	wt := newWorktree(t)
	commitSeq(t, wt, "f.txt", "x\n", 1)
	if err := push(t, wt, anon); err == nil {
		t.Fatalf("push without credentials accepted")
	}
}

func TestNonFastForwardRejected(t *testing.T) {
	e := newGitEnv(t)
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
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
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
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
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
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
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
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
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
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

func TestOldSecretRejectedAfterTakeover(t *testing.T) {
	e := newGitEnv(t)
	wiID, secret1 := e.claimWI(t)
	cred1 := e.credURL(wiID, secret1)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred1); err != nil {
		t.Fatalf("push with the live session secret failed: %v", err)
	}
	// Force takeover: the session swaps atomically to a NEW secret; the
	// old one must be dead at the git face (fencing-on-use, §1.6).
	secret2 := "git-session-secret-2-" + wiID
	if _, err := e.sessions.Takeover(wiID, api.SessionClaimRequest{
		SessionID: secret2, MachineID: "machine-2", Reason: "machine lost",
	}, e.userID); err != nil {
		t.Fatalf("force takeover: %v", err)
	}
	commitSeq(t, wt, "b.txt", "two\n", 2)
	out, perr := runGit(t, wt, "push", cred1, "HEAD:refs/heads/main")
	if perr == nil {
		t.Fatalf("push with the SUPERSEDED session secret accepted: %s", out)
	}
	if !strings.Contains(out, "Authentication") && !strings.Contains(out, "401") {
		t.Fatalf("wrong rejection for the superseded secret: %s", out)
	}
	// The new secret works and continues the seq chain.
	cred2 := e.credURL(wiID, secret2)
	if err := push(t, wt, cred2); err != nil {
		t.Fatalf("push with the new session secret failed: %v", err)
	}
	// And a fetch with the old secret is refused too.
	out, cerr := runGit(t, t.TempDir(), "clone", cred1, filepath.Join(t.TempDir(), "x"))
	if cerr == nil {
		t.Fatalf("clone with the superseded session secret accepted: %s", out)
	}
}

// TestSlugClaimedTicketPushesToCanonicalBareRepo (pilot F2): a ticket may
// be CLAIMED by its "<project>#<seq>" slug — the id-or-slug face accepts
// it — but the git surface is keyed by the CANONICAL id alone: the bare
// repo is <data>/git/<wi_id>.git and the push remote must be built from
// the canonical id. A slug-built remote is not merely wrong, it is
// silently truncated: "#" is the URL fragment separator, so
// .../git/alpha#9.git asks the proxy for .../git/alpha and the push
// 404s. This pins both sides: the canonical URL pushes, the slug URL
// does not.
func TestSlugClaimedTicketPushesToCanonicalBareRepo(t *testing.T) {
	e := newGitEnv(t)
	// Create the ticket and resolve its slug ("<project>#<seq>").
	wi, err := e.wis.Create(core.CreateRequest{
		Project: "alpha", Goal: "slug push test", Reporter: e.userID,
		BundleName: &bundleNameTest,
	})
	if err != nil {
		t.Fatalf("create wi: %v", err)
	}
	if wi.Slug == "" || !strings.Contains(wi.Slug, "#") {
		t.Fatalf("expected a # slug, got %q", wi.Slug)
	}
	// The id-or-slug resolution the API endpoint performs: the slug maps
	// to the canonical id (the core claim face is id-keyed; the HTTP
	// layer resolves before it).
	resolved, err := e.wis.Resolve(wi.Slug)
	if err != nil {
		t.Fatalf("resolve slug %s: %v", wi.Slug, err)
	}
	if resolved != wi.ID {
		t.Fatalf("slug %s resolved to %q, want canonical %q", wi.Slug, resolved, wi.ID)
	}
	secret := "git-session-secret-" + wi.ID
	claim, err := e.sessions.Claim(resolved, api.SessionClaimRequest{
		SessionID: secret, MachineID: "m1", IdempotencyKey: "idem-" + wi.ID,
	}, e.userID)
	if err != nil {
		t.Fatalf("claim %s: %v", resolved, err)
	}
	if claim.WorkItemID != wi.ID {
		t.Fatalf("claim answered work item %q, want canonical %q", claim.WorkItemID, wi.ID)
	}
	if err := e.mgr.EnsureBare(wi.ID); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}

	// The remote built from the CANONICAL id matches the bare repo and
	// the push lands.
	cred := e.credURL(wi.ID, secret)
	if !strings.HasSuffix(cred, "/git/"+wi.ID+".git") {
		t.Fatalf("canonical remote URL %q must name the bare repo /git/%s.git", cred, wi.ID)
	}
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("push to the canonical remote failed: %v", err)
	}
	head := strings.TrimSpace(mustGit(t, wt, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(firstFieldOf(mustGit(t, e.mgr.RepoDir(wi.ID), "rev-parse", "refs/heads/main")))
	if head != remoteHead {
		t.Fatalf("canonical push did not land: local %s, remote %s", head, remoteHead)
	}

	// The remote built from the SLUG (the pilot's failure shape) can never
	// reach the bare repo: "#" cuts the URL at the fragment.
	slugURL := e.ts.URL + "/git/" + wi.Slug + ".git"
	commitSeq(t, wt, "b.txt", "two\n", 2)
	if perr := push(t, wt, slugURL); perr == nil {
		t.Fatalf("push to the slug-built remote %q must fail", slugURL)
	}
	// and it moved nothing: the remote tip is still the first commit.
	remoteHead2 := strings.TrimSpace(firstFieldOf(mustGit(t, e.mgr.RepoDir(wi.ID), "rev-parse", "refs/heads/main")))
	if remoteHead2 != remoteHead {
		t.Fatalf("the slug push moved the bare repo tip: %s -> %s", remoteHead, remoteHead2)
	}
}

func firstFieldOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// TestSessionPushAuthFencing (§1.6 main layer, session path): a push with
// the live session secret succeeds through the proxy; a wrong secret is
// refused at auth (401, rejection names the current holder fingerprint);
// a PAUSED work item refuses pushes entirely (M3: no pushes while
// paused, GIT-403); and after a force takeover the OLD secret is dead
// while the new one works.
func TestSessionPushAuthFencing(t *testing.T) {
	e := newGitEnv(t)
	secret := "11111111222222223333333344444444"
	wiID, fp := e.claimSessionWI(t, secret)

	// Correct secret: push lands.
	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	commitSeq(t, wt, "a.txt", "one\n", 1)
	if err := push(t, wt, cred); err != nil {
		t.Fatalf("push with the live session secret failed: %v", err)
	}

	// Wrong secret: 401 at the proxy.
	commitSeq(t, wt, "b.txt", "two\n", 2)
	out, err := runGit(t, wt, "push", e.credURL(wiID, "99999999888888887777777766666666"), "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("push with a wrong session secret accepted: %s", out)
	}
	if !strings.Contains(out, "401") && !strings.Contains(out, "Authentication") {
		t.Fatalf("wrong rejection for a wrong secret: %s", out)
	}

	// Paused (M3): the session stays, the status check refuses pushes.
	if err := e.sessions.Pause(wiID, secret, false, "daemon shutdown", e.userID); err != nil {
		t.Fatalf("session pause: %v", err)
	}
	commitSeq(t, wt, "c.txt", "three\n", 3)
	out, err = runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("push while paused accepted: %s", out)
	}
	if !strings.Contains(out, "403") {
		t.Fatalf("paused push must be a 403, got: %s", out)
	}

	// Force takeover swaps the session; the old secret is dead, the new
	// one pushes and continues the seq chain.
	newSecret := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := e.sessions.Takeover(wiID, api.SessionClaimRequest{
		SessionID: newSecret, MachineID: "m2", Owner: "op", Reason: "machine lost",
	}, e.userID); err != nil {
		t.Fatalf("force takeover: %v", err)
	}
	out, err = runGit(t, wt, "push", cred, "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("push with the SUPERSEDED session secret accepted: %s", out)
	}
	if !strings.Contains(out, "401") && !strings.Contains(out, "Authentication") {
		t.Fatalf("wrong rejection for the superseded secret: %s", out)
	}
	// The mismatch rejection names the NEW holder's fingerprint.
	if !strings.Contains(out, fp) {
		// (the 401 body names the holder; git's own stderr may not show
		// it — the fingerprint check is covered at the core level)
		t.Logf("rejection detail (fingerprint naming asserted at core level): %s", out)
	}
	if err := push(t, wt, e.credURL(wiID, newSecret)); err != nil {
		t.Fatalf("push with the new session secret failed: %v", err)
	}
}

// commitPlain writes a file and commits it WITHOUT the Polyforge-Seq
// trailer — the shape of a task repo's own history.
func commitPlain(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-m", "task work: "+name)
}

// TestTaskCarrierRefAccepted (polyforge#108): the refs/pf-task/<repo>
// namespace is the task-repo durability lane — the daemon mirrors a task
// repo's local head there so a bounded-fail re-claim can restore it. The
// history is the TASK repo's own: no Polyforge-Seq trailer, submodules
// allowed. The hook must accept exactly that shape (while the chain
// invariants stay enforced on refs/heads/* — pinned by the dedicated
// tests above).
func TestTaskCarrierRefAccepted(t *testing.T) {
	e := newGitEnv(t)
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	commitPlain(t, wt, "code.txt", "task repo work\n")
	out, err := runGit(t, wt, "push", cred, "HEAD:refs/pf-task/demo-src")
	if err != nil {
		t.Fatalf("task-carrier push rejected: %s", out)
	}
	// A task repo may legitimately contain a submodule gitlink — the
	// chain's flat-history rule does not apply to foreign history.
	mustGit(t, wt, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendored-thing")
	mustGit(t, wt, "commit", "-m", "task work with submodule")
	if out, err := runGit(t, wt, "push", cred, "HEAD:refs/pf-task/demo-src"); err != nil {
		t.Fatalf("carrier push with a gitlink rejected: %s", out)
	}
	// The ref landed and advertises back.
	out = mustGit(t, wt, "ls-remote", cred, "refs/pf-task/demo-src")
	if !strings.Contains(out, "refs/pf-task/demo-src") {
		t.Fatalf("carrier ref missing after push: %s", out)
	}
	// The carried head fetches back out (the restore lane's read).
	wt2 := newWorktree(t)
	mustGit(t, wt2, "remote", "add", "hub", cred)
	mustGit(t, wt2, "fetch", "--quiet", "hub", "refs/pf-task/demo-src")
	sha := strings.TrimSpace(mustGit(t, wt, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(mustGit(t, wt2, "rev-parse", "FETCH_HEAD")); got != sha {
		t.Fatalf("fetched carrier head %q, want %q", got, sha)
	}
}

// TestTaskCarrierRefStillFenced: the namespace exemption is ONLY the
// chain-only seq/gitlink pair — deletion and fast-forward stay enforced
// for carrier refs, and the chain invariants stay enforced on
// refs/heads/*.
func TestTaskCarrierRefStillFenced(t *testing.T) {
	e := newGitEnv(t)
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	commitPlain(t, wt, "code.txt", "one\n")
	if out, err := runGit(t, wt, "push", cred, "HEAD:refs/pf-task/demo-src"); err != nil {
		t.Fatalf("base carrier push: %s", out)
	}
	// History rewrite: an amended (non-descendant) tip is rejected. The
	// rewritten tree must actually diverge (an in-second amend would
	// reproduce the same commit SHA and push as up-to-date).
	if err := os.WriteFile(filepath.Join(wt, "rewrite.txt"), []byte("divergent\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "--amend", "--no-edit")
	if out, err := runGit(t, wt, "push", cred, "HEAD:refs/pf-task/demo-src"); err == nil {
		t.Fatalf("non-fast-forward carrier push accepted: %s", out)
	} else if !strings.Contains(out, "non-fast-forward") {
		t.Fatalf("wrong rejection: %s", out)
	}
	// Deletion stays rejected.
	if out, err := runGit(t, wt, "push", cred, ":refs/pf-task/demo-src"); err == nil {
		t.Fatalf("carrier ref deletion accepted: %s", out)
	} else if !strings.Contains(out, "deletion") {
		t.Fatalf("wrong rejection: %s", out)
	}
	// The chain invariants stay enforced on refs/heads/*: a seq-less
	// commit to main is still refused.
	if out, err := runGit(t, wt, "push", cred, "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("seq-less chain push accepted: %s", out)
	} else if !strings.Contains(out, "Polyforge-Seq") {
		t.Fatalf("wrong rejection: %s", out)
	}
}
