package gitops

// gitops_test.go: REAL-git regressions for the capsule chain choke point —
// init→commit→push against a local bare repo, seq trailer monotonicity
// (validated by a pre-receive hook shaped like aihub's), the clean-tree
// path, the attempt-id push option, credential delivery over http via the
// one-shot askpass helper (never argv), and stale-lock aging.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// gitArgsProbe, when set, observes every git argv the package builds
// (test seam: proves the push credential never rides argv).
var gitArgsProbe func(args []string)

func init() {
	wrapped := git
	git = func(env []string, dir string, args ...string) (string, error) {
		if gitArgsProbe != nil {
			gitArgsProbe(args)
		}
		return wrapped(env, dir, args...)
	}
}

func setIdentity(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "pf-test"},
		{"GIT_AUTHOR_EMAIL", "pf-test@invalid"},
		{"GIT_COMMITTER_NAME", "pf-test"},
		{"GIT_COMMITTER_EMAIL", "pf-test@invalid"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	// keep the machine's credential helpers out of the http tests
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
}

// mkBare creates the aihub-side bare repo with push options advertised
// (every push from the choke point carries one) and records what the
// pre-receive hook saw.
type testBare struct {
	dir      string
	optsFile string
}

func mkBare(t *testing.T, dir string, hook bool) *testBare {
	t.Helper()
	if out, err := run("git", "init", "--bare", dir); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"git", "--git-dir", dir, "config", "receive.advertisePushOptions", "true"},
	} {
		if out, err := run(args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	b := &testBare{dir: dir, optsFile: filepath.Join(dir, "push-options.log")}
	if hook {
		writeTestHook(t, b)
	}
	return b
}

// writeTestHook installs a pre-receive hook shaped like aihub's: seq
// trailer present + strictly increasing, and every push option recorded.
func writeTestHook(t *testing.T, b *testBare) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
TRAILER=Polyforge-Seq
prev=""
while read old new ref; do
  range="$new"
  if [ "$old" != "0000000000000000000000000000000000000000" ]; then
    prev=$(git log -1 --format="%%(trailers:key=$TRAILER,valueonly)" "$old" | tr -d '[:space:]')
    case "$prev" in ''|*[!0-9]*) prev="" ;; esac
    range="$old..$new"
  fi
  for c in $(git rev-list --reverse "$range"); do
    seq=$(git log -1 --format="%%(trailers:key=$TRAILER,valueonly)" "$c" | tr -d '[:space:]')
    case "$seq" in ''|*[!0-9]*) echo "commit $c missing $TRAILER" >&2; exit 1 ;; esac
    if [ -n "$prev" ]; then
      [ "$seq" -gt "$prev" ] || { echo "$TRAILER must increase: $c has $seq, prev $prev" >&2; exit 1; }
    fi
    prev="$seq"
  done
done
echo "$GIT_PUSH_OPTION_COUNT|$GIT_PUSH_OPTION_0" >> %q
exit 0
`, b.optsFile)
	path := filepath.Join(b.dir, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (b *testBare) pushesSeen(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(b.optsFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// tipSeq reads the newest main commit's Polyforge-Seq on the bare repo.
func (b *testBare) tipSeq(t *testing.T) int64 {
	t.Helper()
	out, err := run("git", "--git-dir", b.dir, "log", "-1",
		"--format=%(trailers:key="+capsule.SeqTrailer+",valueonly)", "main")
	if err != nil {
		t.Fatalf("bare log: %v: %s", err, out)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		t.Fatalf("tip trailer unparsable: %q", out)
	}
	return n
}

func (b *testBare) hasMain(t *testing.T) bool {
	t.Helper()
	out, err := run("git", "--git-dir", b.dir, "rev-parse", "--verify", "refs/heads/main")
	return err == nil && strings.TrimSpace(out) != ""
}

func run(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	return string(out), err
}

// mkCapsule builds an initialized, externalized capsule worktree.
func mkCapsule(t *testing.T, o Opts) {
	t.Helper()
	if err := InitLocal(o); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	if err := ExternalizeGitDir(o); err != nil {
		t.Fatalf("ExternalizeGitDir: %v", err)
	}
	if !isPointerFile(t, o.CapsuleDir) {
		t.Fatal("capsule .git must be a pointer file after externalization")
	}
}

func isPointerFile(t *testing.T, capsuleDir string) bool {
	t.Helper()
	st, err := os.Stat(filepath.Join(capsuleDir, ".git"))
	return err == nil && st.Mode().IsRegular()
}

func writeDirty(t *testing.T, o Opts, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(o.CapsuleDir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testOpts(t *testing.T, wi string) (Opts, string) {
	t.Helper()
	base := t.TempDir()
	o := Opts{
		WorkItemID: wi,
		AttemptID:  "ra_" + wi,
		CapsuleDir: filepath.Join(base, "capsules", wi),
		GitDir:     filepath.Join(base, "git", wi+".git"),
		RemoteURL:  filepath.Join(base, "hub", wi+".git"),
		Username:   wi,
		Secret:     "push-secret",
	}
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return o, o.RemoteURL
}

// ---------------------------------------------------------------------------
// seq trailer: derived in-lock, strictly increasing, accepted by the hook

func TestCommitPushSeqMonotonicAndPushLands(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_seq")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)

	for i := 1; i <= 3; i++ {
		writeDirty(t, o, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("round %d\n", i))
		sha, pushed, _, err := CommitPush(o, fmt.Sprintf("observe: execution v%d", i))
		if err != nil || !pushed {
			t.Fatalf("round %d: CommitPush = (%q, %v, %v)", i, sha, pushed, err)
		}
		if sha == "" {
			t.Fatalf("round %d: local SHA missing", i)
		}
		if got := bare.tipSeq(t); got != int64(i) {
			t.Fatalf("round %d: bare tip seq = %d, want %d", i, got, i)
		}
	}
	if n := len(bare.pushesSeen(t)); n != 3 {
		t.Fatalf("pushes seen by hook: %d, want 3", n)
	}
}

func TestCommitPushRetryKeepsSeqConsistent(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_retry")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)

	// Round 1: the remote is dead — the commit lands locally, push fails.
	dead := o
	dead.RemoteURL = filepath.Join(t.TempDir(), "nowhere.git")
	writeDirty(t, o, "a.txt", "a\n")
	sha1, pushed, _, err := CommitPush(dead, "first")
	if err == nil || pushed {
		t.Fatalf("push to a dead remote must fail: (%q, %v, %v)", sha1, pushed, err)
	}
	if sha1 == "" {
		t.Fatal("the local commit must still be reported on push failure")
	}
	// Round 2: a new change against the live remote — the seq continues
	// above the standing commit, and the push carries BOTH commits
	// through the hook's strict-increase check.
	writeDirty(t, o, "b.txt", "b\n")
	sha2, pushed, _, err := CommitPush(o, "second")
	if err != nil || !pushed {
		t.Fatalf("CommitPush = (%q, %v, %v)", sha2, pushed, err)
	}
	if sha2 == sha1 {
		t.Fatal("the second commit must be a new SHA")
	}
	if !bare.hasMain(t) {
		t.Fatal("the push never landed")
	}
}

func TestCommitPushCleanTreePushesStandingHead(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_clean")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)

	writeDirty(t, o, "a.txt", "a\n")
	sha, pushed, _, err := CommitPush(o, "one")
	if err != nil || !pushed {
		t.Fatalf("CommitPush = (%q, %v, %v)", sha, pushed, err)
	}
	pushes := len(bare.pushesSeen(t))

	// Clean tree: no error, the standing HEAD — and an HONEST push
	// result. The re-push of an already-pushed HEAD is an idempotent
	// no-op ("Everything up-to-date": the hook saw nothing more), but
	// pushed reports the truthful server-reached state, not a
	// presumed one.
	sha2, pushed, pt, err := CommitPush(o, "wrap: wrapped")
	if err != nil {
		t.Fatalf("clean tree must not error: %v", err)
	}
	if !pushed {
		t.Fatal("clean tree must report the standing HEAD's push honestly (pushed=true)")
	}
	// Clean tree performs no commit call: commit_ms is the honest zero,
	// while the push round-trip is timed just the same.
	if pt.CommitMS != 0 {
		t.Fatalf("clean tree must carry commit_ms=0 (no commit call), got %d", pt.CommitMS)
	}
	if pt.PushMS < 0 {
		t.Fatalf("push_ms must be non-negative, got %d", pt.PushMS)
	}
	if sha2 != sha {
		t.Fatalf("clean tree must answer the standing HEAD: %q vs %q", sha2, sha)
	}
	if got := len(bare.pushesSeen(t)); got != pushes {
		t.Fatalf("clean tree re-push must be an idempotent no-op: %d pushes, want %d", got, pushes)
	}
}

// TestCommitPushCleanTreeRetryIsHonestAfterFailedPush is the N1
// regression: a commit lands locally, the push fails, and the retry hits
// a CLEAN tree. The retry must attempt the push of the standing HEAD
// (so a still-dead server answers with its error, not a fake success)
// and once the server is back it must report pushed=true with the
// standing SHA — never let a clean tree fake a publish.
func TestCommitPushCleanTreeRetryIsHonestAfterFailedPush(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_cleantree")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)

	// Round 1: the remote is dead — the commit lands locally, push fails.
	dead := o
	dead.RemoteURL = filepath.Join(t.TempDir(), "nowhere.git")
	writeDirty(t, o, "a.txt", "a\n")
	sha1, pushed, _, err := CommitPush(dead, "first")
	if err == nil || pushed {
		t.Fatalf("push to a dead remote must fail: (%q, %v, %v)", sha1, pushed, err)
	}
	if sha1 == "" {
		t.Fatal("the local commit must still be reported on push failure")
	}

	// Round 2: the tree is clean, the remote STILL dead — the retry
	// must NOT fake success. It pushes the standing HEAD, fails, and
	// reports the error.
	sha2, pushed, _, err := CommitPush(dead, "retry on clean tree")
	if err == nil {
		t.Fatal("a clean-tree retry against a dead remote must surface the push error")
	}
	if pushed {
		t.Fatal("a failed clean-tree retry must not report pushed=true")
	}
	if sha2 != sha1 {
		t.Fatalf("clean tree must answer the standing HEAD: %q vs %q", sha2, sha1)
	}
	if bare.hasMain(t) {
		t.Fatal("nothing may have reached the server through the dead remote")
	}

	// Round 3: the tree is still clean, the server is back — the retry
	// pushes the standing HEAD through and reports pushed=true.
	sha3, pushed, _, err := CommitPush(o, "retry once the server is back")
	if err != nil || !pushed {
		t.Fatalf("clean-tree retry against the live remote = (%q, %v, %v)", sha3, pushed, err)
	}
	if sha3 != sha1 {
		t.Fatalf("the standing HEAD must be the one pushed: %q vs %q", sha3, sha1)
	}
	if got := bare.tipSeq(t); got != 1 {
		t.Fatalf("bare tip seq = %d, want 1", got)
	}
}

// TestCommitPushCleanTreeWithNoHistoryIsTyped covers the practically
// unreachable shape (materialization always leaves untracked files): a
// clean porcelain with no standing HEAD must answer the typed
// ErrEmptyHistory, not raw rev-parse noise.
func TestCommitPushCleanTreeWithNoHistoryIsTyped(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_nohist")
	mkBare(t, barePath, false)
	mkCapsule(t, o) // initialized, unborn branch, empty tree: clean porcelain

	sha, pushed, _, err := CommitPush(o, "nothing anywhere")
	if err == nil {
		t.Fatalf("clean tree with no history must error: (%q, %v)", sha, pushed)
	}
	if !errors.Is(err, ErrEmptyHistory) {
		t.Fatalf("the error must be the typed ErrEmptyHistory, got %v", err)
	}
}

func TestCommitPushFirstCommitIsSeqOneOnUnbornBranch(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_unborn")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)

	writeDirty(t, o, "first.txt", "first\n")
	if _, pushed, _, err := CommitPush(o, "first"); err != nil || !pushed {
		t.Fatalf("first CommitPush = (%v, %v)", pushed, err)
	}
	if got := bare.tipSeq(t); got != 1 {
		t.Fatalf("first commit seq = %d, want 1", got)
	}
}

// TestPushOptionSessionFingerprintReachedServer (v1.5 §1.6): when the
// opts carry the PUBLIC session fingerprint, the push option becomes
// session-fingerprint=<fp> — the fencing-on-use backup layer — and the
// legacy attempt-id option is NOT sent alongside it.
func TestPushOptionSessionFingerprintReachedServer(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_fp")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)
	o.AttemptID = "ra_wi_fp" // legacy value must NOT ride once a fingerprint is set
	o.Fingerprint = "ab12cd34ef56aa00"

	writeDirty(t, o, "a.txt", "a\n")
	if _, pushed, _, err := CommitPush(o, "one"); err != nil || !pushed {
		t.Fatalf("CommitPush = (%v, %v)", pushed, err)
	}
	seen := bare.pushesSeen(t)
	if len(seen) != 1 {
		t.Fatalf("hook invocations: %d, want 1", len(seen))
	}
	if seen[0] != "1|session-fingerprint=ab12cd34ef56aa00" {
		t.Fatalf("push options seen: %q, want count=1 option session-fingerprint=ab12cd34ef56aa00", seen[0])
	}
}

func TestPushOptionAttemptIDReachedServer(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_popt")
	bare := mkBare(t, barePath, true)
	mkCapsule(t, o)

	writeDirty(t, o, "a.txt", "a\n")
	if _, pushed, _, err := CommitPush(o, "one"); err != nil || !pushed {
		t.Fatalf("CommitPush = (%v, %v)", pushed, err)
	}
	seen := bare.pushesSeen(t)
	if len(seen) != 1 {
		t.Fatalf("hook invocations: %d, want 1", len(seen))
	}
	if seen[0] != "1|attempt-id=ra_wi_popt" {
		t.Fatalf("push options seen: %q, want count=1 option attempt-id=ra_wi_popt", seen[0])
	}
}

// ---------------------------------------------------------------------------
// credential delivery: askpass helper, http:// transport, argv hygiene

func TestAskpassScriptShapeAndCleanup(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_askpass")
	o.RemoteURL = "http://127.0.0.1:1/wi_askpass.git"

	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	defer cleanup()
	script := ""
	prompt := ""
	for _, kv := range additions {
		if strings.HasPrefix(kv, "GIT_ASKPASS=") {
			script = strings.TrimPrefix(kv, "GIT_ASKPASS=")
		}
		if kv == "GIT_TERMINAL_PROMPT=0" {
			prompt = kv
		}
	}
	if script == "" || prompt == "" {
		t.Fatalf("env additions wrong: %v", additions)
	}
	// the helper lives OUTSIDE the capsule, in a 0700 dir
	if strings.HasPrefix(script, o.CapsuleDir) {
		t.Fatal("the askpass helper must not live inside the capsule")
	}
	st, err := os.Stat(filepath.Dir(script))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("askpass dir must be 0700, got %v (%v)", st, err)
	}
	sst, err := os.Stat(script)
	if err != nil || sst.Mode().Perm() != 0o700 {
		t.Fatalf("askpass script must be 0700, got %v (%v)", sst, err)
	}
	// the helper answers username and password prompts
	for _, tc := range []struct{ ask, want string }{
		{"Username for 'http://x': ", o.Username},
		{"Password for 'http://x': ", o.Secret},
	} {
		out, err := run(script, tc.ask)
		if err != nil {
			t.Fatalf("askpass %q: %v", tc.ask, err)
		}
		if strings.TrimSpace(out) != tc.want {
			t.Fatalf("askpass %q = %q, want %q", tc.ask, out, tc.want)
		}
	}
	cleanup()
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the script, stat err = %v", err)
	}
}

func TestCredentialEnvSkipsNonHTTPRemotes(t *testing.T) {
	o, barePath := testOpts(t, "wi_local")
	o.RemoteURL = barePath // local path: no credential consulted
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(additions) != 0 {
		t.Fatalf("local remote must not build an askpass helper: %v", additions)
	}
}

// TestAskpassFallbackBaseIsCleanedUp: the MkdirTemp fallback (no usable
// GitDir parent) must clean up the whole temp base directory with the
// call, not leak one fresh base per invocation.
func TestAskpassFallbackBaseIsCleanedUp(t *testing.T) {
	o, _ := testOpts(t, "wi_fallback")
	o.GitDir = "" // forces the MkdirTemp("", "pf-askpass-") fallback
	o.RemoteURL = "http://127.0.0.1:1/wi_fallback.git"

	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(additions) != 2 {
		t.Fatalf("expected GIT_ASKPASS + GIT_TERMINAL_PROMPT additions: %v", additions)
	}
	script := strings.TrimPrefix(additions[0], "GIT_ASKPASS=")
	base := filepath.Dir(filepath.Dir(script)) // <temp>/pf-askpass-xxx/askpass/<script>
	if _, serr := os.Stat(base); serr != nil {
		t.Fatalf("fallback base must exist before cleanup: %v", serr)
	}
	cleanup()
	if _, serr := os.Stat(base); !os.IsNotExist(serr) {
		t.Fatalf("cleanup must remove the fallback base dir %s", base)
	}
}

// TestSweepAskpassRemovesCrashResidue: a killed daemon leaves one-shot
// askpass scripts (holding push secrets) under <gitRoot>/askpass; the
// startup sweep must remove them, leave unrelated entries alone, and be
// a no-op when the directory does not exist.
func TestSweepAskpassRemovesCrashResidue(t *testing.T) {
	o, _ := testOpts(t, "wi_sweep")
	gitRoot := filepath.Dir(o.GitDir) // <base>/git
	askpassDir := filepath.Join(gitRoot, "askpass")
	if err := os.MkdirAll(askpassDir, 0o700); err != nil {
		t.Fatal(err)
	}
	residue := filepath.Join(askpassDir, "askpass-wi_dead-0123456789abcdef")
	if err := os.WriteFile(residue, []byte("#!/bin/sh\nsecret\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(askpassDir, "keep-me.txt")
	if err := os.WriteFile(unrelated, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SweepAskpass(gitRoot); err != nil {
		t.Fatalf("SweepAskpass: %v", err)
	}
	if _, serr := os.Stat(residue); !os.IsNotExist(serr) {
		t.Fatal("the crash-residue script must be swept")
	}
	if _, serr := os.Stat(unrelated); serr != nil {
		t.Fatalf("unrelated entries must survive the sweep: %v", serr)
	}
	// a gitRoot without an askpass dir is a clean no-op, not an error
	if err := SweepAskpass(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("SweepAskpass on an absent dir must be a no-op: %v", err)
	}
}

func TestPushArgsNeverCarrySecret(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_argv")
	mkBare(t, barePath, false)
	mkCapsule(t, o)

	var mu atomic.Value // []string
	mu.Store([]string{})
	gitArgsProbe = func(args []string) {
		prev := mu.Load().([]string)
		mu.Store(append(prev, strings.Join(args, "\x00")))
	}
	t.Cleanup(func() { gitArgsProbe = nil })

	writeDirty(t, o, "a.txt", "a\n")
	if _, pushed, _, err := CommitPush(o, "one"); err != nil || !pushed {
		t.Fatalf("CommitPush = (%v, %v)", pushed, err)
	}
	var sawPush bool
	for _, blob := range mu.Load().([]string) {
		args := strings.Split(blob, "\x00")
		for _, a := range args {
			if strings.Contains(a, o.Secret) {
				t.Fatalf("secret found in git argv %q", args)
			}
			if strings.Contains(a, "attempt-id=ra_wi_argv") {
				sawPush = true
			}
		}
	}
	if !sawPush {
		t.Fatal("the push must carry --push-option=attempt-id=…")
	}
}

// TestPushAndCloneOverHTTPWithCredential drives the whole chain against a
// REAL git http server: clone over http:// (the default stack's scheme)
// must receive the attempt credential through the askpass helper, and a
// CommitPush must reach the same server.
func TestPushAndCloneOverHTTPWithCredential(t *testing.T) {
	setIdentity(t)
	root := t.TempDir()
	repo := filepath.Join(root, "wi_http.git")
	if out, err := run("git", "init", "--bare", repo); err != nil {
		t.Fatalf("bare init: %v: %s", err, out)
	}
	if out, err := run("git", "--git-dir", repo, "config", "http.receivepack", "true"); err != nil {
		t.Fatalf("receivepack: %v: %s", err, out)
	}
	if out, err := run("git", "--git-dir", repo, "config", "receive.advertisePushOptions", "true"); err != nil {
		t.Fatalf("advertisePushOptions: %v: %s", err, out)
	}
	url := startGitHTTP(t, root)

	base := t.TempDir()
	o := Opts{
		WorkItemID: "wi_http",
		AttemptID:  "ra_wi_http",
		CapsuleDir: filepath.Join(base, "capsules", "wi_http"),
		GitDir:     filepath.Join(base, "git", "wi_http.git"),
		RemoteURL:  url + "/wi_http.git",
		Username:   "wi_http",
		Secret:     "http-push-secret",
	}
	if err := os.MkdirAll(filepath.Dir(o.CapsuleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Clone(o); err != nil {
		t.Fatalf("clone over http:// must receive the credential: %v", err)
	}
	// re-externalize: Clone leaves the git-dir inside the worktree
	if err := os.Rename(filepath.Join(o.CapsuleDir, ".git"), o.GitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.CapsuleDir, ".git"),
		[]byte("gitdir: "+o.GitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDirty(t, o, "http.txt", "over http\n")
	sha, pushed, _, err := CommitPush(o, "http push")
	if err != nil || !pushed {
		t.Fatalf("CommitPush over http:// = (%q, %v, %v)", sha, pushed, err)
	}
	out, err := run("git", "--git-dir", repo, "rev-parse", "--verify", "refs/heads/main")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("the http push never landed (%v): %s", err, out)
	}

	// without a credential the same remote answers 401
	noCred := o
	noCred.Username, noCred.Secret = "", ""
	capsule2 := filepath.Join(base, "capsules", "wi_http2")
	noCred.CapsuleDir = capsule2
	if err := Clone(noCred); err == nil {
		t.Fatal("clone without a credential must fail against the authed server")
	}
}

// startGitHTTP serves git http-backend over plain http:// with basic auth.
func startGitHTTP(t *testing.T, root string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const user, pass = "wi_http", "http-push-secret"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="pf-test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		env := append(os.Environ(),
			"GIT_PROJECT_ROOT="+root,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO="+r.URL.Path,
			"REQUEST_METHOD="+r.Method,
			"QUERY_STRING="+r.URL.RawQuery,
			"REMOTE_ADDR=127.0.0.1",
			"REMOTE_USER="+user,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		)
		if r.ContentLength >= 0 {
			env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
		}
		cmd := exec.Command("git", "http-backend")
		cmd.Env = env
		cmd.Stdin = r.Body
		out, cerr := cmd.Output()
		if cerr != nil {
			http.Error(w, "http-backend failed", http.StatusInternalServerError)
			return
		}
		sep := []byte("\r\n\r\n")
		head, body := out, []byte{}
		if i := bytes.Index(out, sep); i >= 0 {
			head, body = out[:i], out[i+len(sep):]
		} else {
			if i := bytes.Index(out, []byte("\n\n")); i >= 0 {
				head, body = out[:i], out[i+2:]
			}
		}
		status := http.StatusOK
		for _, line := range bytes.Split(head, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			if bytes.HasPrefix(line, []byte("Status:")) {
				if code, perr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(line), "Status:"))); perr == nil {
					status = code
				}
				continue
			}
			if k, v, found := strings.Cut(string(line), ":"); found {
				w.Header().Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// ---------------------------------------------------------------------------
// stale locks

func TestCleanStaleLocksAges(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_locks")
	mkBare(t, barePath, false)
	mkCapsule(t, o)
	lock := filepath.Join(o.GitDir, "index.lock")

	// A YOUNG lock is left alone (git's lock content is a binary
	// in-progress index, not a PID — nothing proves its holder dead) and
	// the push against it fails loudly.
	if err := os.WriteFile(lock, []byte("in-progress index (binary)"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDirty(t, o, "a.txt", "a\n")
	_, _, _, err := CommitPush(o, "one")
	if err == nil {
		t.Fatal("a live young lock must surface as a push failure, not be deleted")
	}
	if _, serr := os.Stat(lock); serr != nil {
		t.Fatal("a young lock must NOT be removed")
	}

	// An OLD lock (outlived the bound) is garbage by any honest reading:
	// removed, and the same CommitPush goes through.
	past := time.Now().Add(-(staleLockAge + time.Minute))
	if err := os.Chtimes(lock, past, past); err != nil {
		t.Fatal(err)
	}
	sha, pushed, _, err := CommitPush(o, "one")
	if err != nil || !pushed {
		t.Fatalf("CommitPush after stale-lock removal = (%q, %v, %v)", sha, pushed, err)
	}
	if _, serr := os.Stat(lock); !os.IsNotExist(serr) {
		t.Fatal("the stale lock must be removed")
	}
}

// ---------------------------------------------------------------------------
// InitLocal / ExternalizeGitDir (the hoisted single implementation)

func TestInitLocalAndExternalize(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_init")

	if err := InitLocal(o); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	if err := ExternalizeGitDir(o); err != nil {
		t.Fatalf("ExternalizeGitDir: %v", err)
	}
	if !isPointerFile(t, o.CapsuleDir) {
		t.Fatal(".git must be a pointer file after externalization")
	}
	if !dirExists(o.GitDir) {
		t.Fatalf("external git-dir missing at %s", o.GitDir)
	}
	// the working branch is main
	out, err := git(o.Env(), o.CapsuleDir, "symbolic-ref", "HEAD")
	if err != nil || strings.TrimSpace(out) != "refs/heads/main" {
		t.Fatalf("working branch = %q (%v), want refs/heads/main", out, err)
	}
	// idempotent: a second externalization is a no-op
	if err := ExternalizeGitDir(o); err != nil {
		t.Fatalf("ExternalizeGitDir (again): %v", err)
	}
}
