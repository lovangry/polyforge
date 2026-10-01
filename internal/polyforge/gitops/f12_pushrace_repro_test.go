package gitops

// f12_pushrace_repro_test.go — reproduction for work item polyforge#46
// (F12 shutdown-window commit-dirty push race):
//
//   daemon-b's graceful-stop logs showed git push failures
//     "cannot lock ref 'refs/heads/main': is at X but expected Y"
//     / "cannot lock ref 'refs/heads/main': reference already exists"
//   for the commit-dirty-on-recover pushes of three work items — the
//   pushes were REJECTED while the local commits stood (no data lost).
//
// This file reproduces that failure mode end-to-end through the REAL
// surfaces involved: the aihub git Manager (bare repo + pre-receive
// hook), the aihub git HTTP proxy (HandleGit → git http-backend →
// git-receive-pack) and the gitops push argv/credential shape
// (pushLocked's exact command line). Two push lanes of the SAME work
// item race:
//
//   - lane 1 goes through gitops.Push (the runtime seal / retry entry
//     point, in-process, per-WI mutex held);
//   - lane 2 is an overlapping push invocation carrying the identical
//     argv (gitops' per-WI mutex serializes in-process gitops entry
//     points, so the overlapping lane is driven as the raw `git push`
//     exec that both lanes ultimately are on the wire).
//
// Both lanes advertise the SAME old ref value (git-receive-pack
// compare-and-swaps against the value each client saw at advertisement
// time); when the ref moves server-side between advertisement and the
// ref transaction, the losing lane is rejected with exactly the logged
// message while its local commit stands. The pre-receive hook's sleep
// (test-only window widening, installed AFTER the baseline publish)
// makes the overlap deterministic; without it the same race is purely
// timing-dependent.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aihubgit "github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/common/capsule"
)

const (
	reproFingerprint = "0123456789abcdef"
	reproSecret      = "race-secret"
)

// reproChecker is a CredentialChecker stub: username must be the WI id,
// the secret must match. It answers the session's public fingerprint the
// pre-receive fence expects.
type reproChecker struct{}

func (reproChecker) CheckPushCredential(wiID, username, secret string) (string, error) {
	if username != wiID || secret != reproSecret {
		return "", fmt.Errorf("bad push credential for %s", wiID)
	}
	return reproFingerprint, nil
}

// reproEnv wires the real aihub git surface: Manager (bare-repo tree +
// hook), proxy handler on an httptest server.
type reproEnv struct {
	mgr *aihubgit.Manager
	ts  *httptest.Server
}

func newReproEnv(t *testing.T) *reproEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not found: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	mgr, err := aihubgit.NewManager(t.TempDir(), reproChecker{})
	if err != nil {
		t.Fatalf("git manager: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/git/", http.HandlerFunc(mgr.HandleGit))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &reproEnv{mgr: mgr, ts: ts}
}

// slowHook prepends a sleep to the bare repo's real aihub pre-receive
// hook, widening the server-side window between ref advertisement and
// the ref transaction so the two lanes deterministically overlap.
// Test scaffolding only — it changes WHEN the race window closes, not
// WHAT the surfaces do.
func (e *reproEnv) slowHook(t *testing.T, wi string, d time.Duration) {
	t.Helper()
	path := filepath.Join(e.mgr.RepoDir(wi), "hooks", "pre-receive")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	lines := strings.SplitN(string(body), "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("unexpected hook shape: %q", string(body))
	}
	widened := lines[0] + "\nsleep " + fmt.Sprint(int(d.Seconds())) + "\n" + lines[1]
	if err := os.WriteFile(path, []byte(widened), 0o755); err != nil {
		t.Fatalf("write widened hook: %v", err)
	}
}

// reproOpts builds the gitops Opts for one capsule of the WI, remote
// pointing at the real proxy.
func (e *reproEnv) reproOpts(t *testing.T, wi string) Opts {
	t.Helper()
	base := t.TempDir()
	return Opts{
		WorkItemID: wi,
		Fingerprint: reproFingerprint,
		CapsuleDir: filepath.Join(base, "capsules", wi),
		GitDir:     filepath.Join(base, "git", wi+".git"),
		RemoteURL:  e.ts.URL + "/git/" + wi + ".git",
		Username:   wi,
		Secret:     reproSecret,
	}
}


// lanePushResult is one lane's outcome.
type lanePushResult struct {
	label string
	ok    bool
	err   error
	out   string
}

// rawPush execs the EXACT argv pushLocked builds (push, session-fingerprint
// push option, remote URL, HEAD:refs/heads/main) with gitops' env shape
// (external GIT_DIR/GIT_WORK_TREE + one-shot GIT_ASKPASS credential).
// It bypasses the in-process per-WI mutex the way an overlapping push
// lane does on the wire.
func rawPush(t *testing.T, o Opts) lanePushResult {
	t.Helper()
	args := []string{"push"}
	if o.Fingerprint != "" {
		args = append(args, "--push-option=session-fingerprint="+o.Fingerprint)
	}
	args = append(args, o.RemoteURL, "HEAD:refs/heads/main")
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		t.Fatalf("credential env: %v", err)
	}
	defer cleanup()
	cmd := exec.Command("git", args...)
	cmd.Env = append(o.Env(), additions...)
	cmd.Dir = o.CapsuleDir
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return lanePushResult{label: "raw lane", ok: err == nil, err: err, out: out.String()}
}

// localCommit makes the standing HEAD a new seq-tagged commit WITHOUT
// pushing (the state both racing lanes then publish).
func localCommit(t *testing.T, o Opts, name string, seq int64) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(o.CapsuleDir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := o.Env()
	if _, err := git(env, o.CapsuleDir, "add", "-A"); err != nil {
		t.Fatalf("add: %v", err)
	}
	msg := fmt.Sprintf("%s\n\n%s: %d", name, capsule.SeqTrailer, seq)
	if _, err := git(env, o.CapsuleDir, "commit", "-m", msg); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sha, err := git(env, o.CapsuleDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(sha)
}

func headSHA(t *testing.T, o Opts) string {
	t.Helper()
	out, err := git(o.Env(), o.CapsuleDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(out)
}

func bareMainSHA(t *testing.T, e *reproEnv, wi string) string {
	t.Helper()
	out, err := git(nil, e.mgr.RepoDir(wi), "rev-parse", "--verify", "refs/heads/main")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func cleanTree(t *testing.T, o Opts) bool {
	t.Helper()
	out, err := git(o.Env(), o.CapsuleDir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return strings.TrimSpace(out) == ""
}

// TestReproShutdownPushRaceCannotLockRef: the WI's ref is concurrently
// advanced between the losing lane's advertisement and its ref
// transaction; the losing lane's push is rejected with the reported
// graceful-stop log line while its local commit stands.
//
// THIS TEST PINS THE REPORTED FAILURE MODE: it passes while the race
// rejects a commit-dirty push (one lane rejected with "cannot lock ref
// ... is at X but expected Y", the pushed commit standing locally, no
// data lost). A fix that makes same-WI pushes idempotent makes the
// "rejected" assertion fail — that flip is the verification signal.
func TestReproShutdownPushRaceCannotLockRef(t *testing.T) {
	setIdentity(t)
	e := newReproEnv(t)
	const wi = "wi_race1"
	if err := e.mgr.EnsureBare(wi); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	o := e.reproOpts(t, wi)
	if err := os.MkdirAll(filepath.Dir(o.CapsuleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mkCapsule(t, o)

	// Baseline: first commit published through the production choke
	// point (seq 1), server ref = A.
	writeDirty(t, o, "base.txt", "base\n")
	if sha, pushed, _, err := CommitPush(o, "baseline commit"); err != nil || !pushed {
		t.Fatalf("baseline CommitPush = (%q, %v, %v)", sha, pushed, err)
	}
	baseline := headSHA(t, o)
	if got := bareMainSHA(t, e, wi); got != baseline {
		t.Fatalf("baseline never landed: server=%s local=%s", got, baseline)
	}

	// The commit-dirty state both lanes race to publish (seq 2).
	raced := localCommit(t, o, "dirty.txt", 2)

	// Widen the server-side window deterministically (test scaffolding).
	e.slowHook(t, wi, 3*time.Second)

	// Lane 1: the runtime seal / retry entry point, in-process.
	type pushOut struct {
		pushed bool
		err    error
	}
	ch1 := make(chan pushOut, 1)
	go func() {
		pushed, err := Push(o)
		ch1 <- pushOut{pushed, err}
	}()
	time.Sleep(500 * time.Millisecond)

	// Lane 2: the overlapping push of the SAME work item, same standing
	// HEAD, identical argv — on the wire both lanes looked identical
	// when daemon-b logged the rejection.
	res2 := rawPush(t, o)
	var res1 pushOut
	select {
	case res1 = <-ch1:
	case <-time.After(30 * time.Second):
		t.Fatal("lane 1 push never returned")
	}

	// Exactly one lane was rejected, and for the REPORTED reason: the
	// CAS text from the graceful-stop logs.
	lane1Rejected := res1.err != nil
	lane2Rejected := res2.err != nil
	if lane1Rejected == lane2Rejected {
		t.Fatalf("expected exactly one lane rejected, got lane1=%v lane2=%v\nlane1: %v\nlane2 out: %s",
			lane1Rejected, lane2Rejected, res1.err, res2.out)
	}
	rejOut := ""
	if lane1Rejected {
		rejOut = fmt.Sprintf("%v", res1.err)
	} else {
		rejOut = res2.out
	}
	if !strings.Contains(rejOut, "cannot lock ref 'refs/heads/main'") ||
		!strings.Contains(rejOut, "is at") || !strings.Contains(rejOut, "but expected") {
		t.Fatalf("rejection lacks the reported 'cannot lock ref: is at X but expected Y' text:\n%s", rejOut)
	}
	t.Logf("rejected lane output:\n%s", rejOut)

	// The local commit stands and nothing is lost: the capsule's HEAD is
	// still the raced commit, the server holds it (the winner delivered
	// it), and the worktree is clean.
	if got := headSHA(t, o); got != raced {
		t.Fatalf("local commit did not stand: HEAD=%s want %s", got, raced)
	}
	if got := bareMainSHA(t, e, wi); got != raced {
		t.Fatalf("server main=%s, want the raced commit %s (data loss)", got, raced)
	}
	if !cleanTree(t, o) {
		t.Fatal("capsule worktree unexpectedly dirty")
	}
}

// TestReproShutdownPushRaceReferenceAlreadyExists: the create-race shape
// of the same failure — both lanes advertise the ref as nonexistent,
// one lane's create wins, the other is rejected with the second message
// from the graceful-stop logs ("reference already exists") while both
// local commits stand.
func TestReproShutdownPushRaceReferenceAlreadyExists(t *testing.T) {
	setIdentity(t)
	e := newReproEnv(t)
	const wi = "wi_race2"
	if err := e.mgr.EnsureBare(wi); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	// Two capsules of the SAME work item (e.g. a recovered capsule and
	// a concurrently derived one), each holding its own first commit.
	o1 := e.reproOpts(t, wi)
	o1.CapsuleDir += "-a"
	o1.GitDir += "-a"
	o2 := e.reproOpts(t, wi)
	o2.CapsuleDir += "-b"
	o2.GitDir += "-b"
	for _, o := range []Opts{o1, o2} {
		if err := os.MkdirAll(filepath.Dir(o.CapsuleDir), 0o755); err != nil {
			t.Fatal(err)
		}
		mkCapsule(t, o)
	}
	sha1 := localCommit(t, o1, "one.txt", 1)
	sha2 := localCommit(t, o2, "two.txt", 1)

	// Widen the window deterministically.
	e.slowHook(t, wi, 3*time.Second)

	type pushOut struct {
		pushed bool
		err    error
	}
	ch1 := make(chan pushOut, 1)
	go func() {
		pushed, err := Push(o1)
		ch1 <- pushOut{pushed, err}
	}()
	time.Sleep(500 * time.Millisecond)
	res2 := rawPush(t, o2)
	var res1 pushOut
	select {
	case res1 = <-ch1:
	case <-time.After(30 * time.Second):
		t.Fatal("lane 1 push never returned")
	}

	lane1Rejected, lane2Rejected := res1.err != nil, res2.err != nil
	if lane1Rejected == lane2Rejected {
		t.Fatalf("expected exactly one lane rejected, got lane1=%v lane2=%v\nlane1: %v\nlane2 out: %s",
			lane1Rejected, lane2Rejected, res1.err, res2.out)
	}
	rejOut := ""
	if lane1Rejected {
		rejOut = fmt.Sprintf("%v", res1.err)
	} else {
		rejOut = res2.out
	}
	if !strings.Contains(rejOut, "cannot lock ref 'refs/heads/main'") ||
		!strings.Contains(rejOut, "reference already exists") {
		t.Fatalf("rejection lacks the reported 'reference already exists' text:\n%s", rejOut)
	}
	t.Logf("rejected lane output:\n%s", rejOut)

	// Both local commits stand (no data lost); the server holds the
	// winner's commit.
	if got := headSHA(t, o1); got != sha1 {
		t.Fatalf("capsule A commit did not stand: %s want %s", got, sha1)
	}
	if got := headSHA(t, o2); got != sha2 {
		t.Fatalf("capsule B commit did not stand: %s want %s", got, sha2)
	}
	got := bareMainSHA(t, e, wi)
	if got != sha1 && got != sha2 {
		t.Fatalf("server main=%s holds neither local commit", got)
	}
}
