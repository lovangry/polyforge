// Package gitops is the shared capsule-git operations layer used by Runtime
// (clone at Start, dirty-commit at Recover) and Observe (async commit+push
// on execution.json change). It enforces the v1 invariants that need a
// single choke point:
//
//   - per-WI git mutex: Runtime and Observe never run git against the same
//     worktree concurrently (v1-spec-full §六);
//   - seq derivation has ONE authority: CommitPush derives the
//     Polyforge-Seq trailer inside the per-WI lock as
//     rev-list --count HEAD + 1 (first commit = 1). The lock makes the
//     value monotonic, restart-safe and retry-safe (a failed push leaves
//     no commit, so a retry recomputes the same number), which aihub's
//     pre-receive hook requires (strictly increasing per commit);
//   - pushes carry the attempt-scoped credential and go through aihub's
//     git HTTP proxy; the credential travels via a one-shot GIT_ASKPASS
//     helper (never in argv, never in persistent env) and reaches both
//     http:// and https:// remotes;
//   - pushes carry --push-option=attempt-id=<attempt> (generation fencing
//     备层, §六); the server-side pre-receive validation of that option is
//     the aihub lane's obligation;
//   - an index.lock that has outlived the stale bound is removed at
//     acquisition (see cleanStaleLocks for why age, not PID, is the only
//     safe signal).
//
// git-dir is ALWAYS external to the capsule (never inside agent-writable
// space). All git access goes through the real git binary.
package gitops

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
)

var (
	mu    sync.Mutex
	locks = map[string]*sync.Mutex{}
)

// Lock acquires the per-WI git mutex and returns the release func.
func Lock(workItemID string) func() {
	mu.Lock()
	l, ok := locks[workItemID]
	if !ok {
		l = &sync.Mutex{}
		locks[workItemID] = l
	}
	mu.Unlock()
	l.Lock()
	return func() { l.Unlock() }
}

// Opts describe one capsule worktree + its external git-dir.
type Opts struct {
	WorkItemID string // mutex key and push identity
	AttemptID  string // claim attempt id; rides pushes as --push-option=attempt-id=<id> (§六 备层)
	CapsuleDir string // worktree root (agent-writable)
	GitDir     string // external git-dir (e.g. <data>/git/wi_xxx.git)
	RemoteURL  string // aihub git proxy URL, e.g. http://host:8420/git/wi_xxx.git
	Username   string // push credential username (= work item id)
	Secret     string // attempt-scoped push credential secret
}

// Env returns the environment every raw git invocation against this
// capsule must carry (inherited environ + the external git-dir binding).
func (o Opts) Env() []string {
	return append(os.Environ(),
		"GIT_DIR="+o.GitDir,
		"GIT_WORK_TREE="+o.CapsuleDir,
	)
}

// git runs git once with the given environment (nil = inherit) and dir.
// It is a var so the tests can probe the argv the package builds.
var git = func(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = env
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// ErrEmptyHistory is the typed error for the (practically unreachable)
// clean-tree-with-no-standing-HEAD shape: the porcelain is empty but
// rev-parse HEAD fails, so there is nothing to answer and nothing to
// push. Materialization always leaves untracked files, so reaching it
// means the capsule was emptied out from under the daemon. It is NOT
// retry-worthy: a retry cannot conjure a history.
var ErrEmptyHistory = errors.New("gitops: empty capsule history (clean tree with no standing HEAD)")

// CommitPush stages everything, commits with the seq trailer (derived
// here — the single authority) and pushes to the aihub bare repo. It
// takes the per-WI git lock itself; callers must not hold it. A clean
// tree (nothing staged after the porcelain check) PUSHES the standing
// HEAD — an idempotent no-op ("Everything up-to-date") when the server
// already holds it — so the pushed flag is honest on every return shape,
// including the retry-after-failed-push one a wrap/seal re-drive needs:
// a commit that landed locally on a failed push finally reaches the
// server HERE, and a still-dead server answers with its error, not a
// fake success. Returns the local commit SHA and whether the push
// reached the server.
func CommitPush(o Opts, message string) (localSHA string, pushed bool, err error) {
	release := Lock(o.WorkItemID)
	defer release()

	if err := cleanStaleLocks(o.GitDir); err != nil {
		return "", false, err
	}
	env := o.Env()
	// Clean-tree check BEFORE committing: git reports "nothing to commit"
	// on stdout, so the failure cannot be told apart from a real commit
	// error by the error string. The porcelain listing is the honest
	// signal: empty output means the standing HEAD is already the answer.
	status, err := git(env, o.CapsuleDir, "status", "--porcelain")
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(status) == "" {
		// Clean tree: the standing HEAD is the answer — and it is
		// PUSHED, not presumed. Skipping the push here would let a
		// clean-tree retry after a failed push fake success and
		// silence §九 backpressure in the one scenario (persistent
		// server unreachability) it exists for.
		sha, serr := git(env, o.CapsuleDir, "rev-parse", "HEAD")
		if serr != nil {
			return "", false, fmt.Errorf("%w: %v", ErrEmptyHistory, serr)
		}
		localSHA = strings.TrimSpace(sha)
		pushed, perr := pushLocked(o)
		return localSHA, pushed, perr
	}
	seq, err := nextSeq(env, o.CapsuleDir)
	if err != nil {
		return "", false, err
	}
	if _, err := git(env, o.CapsuleDir, "add", "-A"); err != nil {
		return "", false, err
	}
	msg := fmt.Sprintf("%s\n\n%s: %d", message, capsule.SeqTrailer, seq)
	if _, err := git(env, o.CapsuleDir, "commit", "-m", msg); err != nil {
		return "", false, err
	}
	sha, err := git(env, o.CapsuleDir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	localSHA = strings.TrimSpace(sha)
	pushed, perr := pushLocked(o)
	return localSHA, pushed, perr
}

// nextSeq derives the next Polyforge-Seq trailer value from the capsule's
// commit count (monotonic per WI, survives a lost state.json, and safe to
// recompute: only a landed commit moves the count). An unborn branch (no
// commits yet) answers 1.
func nextSeq(env []string, dir string) (int64, error) {
	out, err := git(env, dir, "rev-list", "--count", "HEAD")
	if err != nil {
		if _, verr := git(env, dir, "rev-parse", "--verify", "-q", "HEAD"); verr != nil {
			return 1, nil // unborn branch: the first commit is seq 1
		}
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil || n < 0 {
		return 0, fmt.Errorf("gitops: unparsable rev-list count %q", strings.TrimSpace(out))
	}
	return n + 1, nil
}

// Push sends the capsule's standing HEAD to the aihub bare repo's main
// branch (the retry path for a commit that landed locally but never
// reached the server). Takes the per-WI lock.
func Push(o Opts) (bool, error) {
	release := Lock(o.WorkItemID)
	defer release()
	return pushLocked(o)
}

// pushLocked is push under an already-held per-WI lock.
func pushLocked(o Opts) (bool, error) {
	if o.RemoteURL == "" {
		return false, &api.Error{Code: api.ErrGitUnauthorized, Detail: "no push remote configured"}
	}
	args := []string{"push"}
	if o.AttemptID != "" {
		// Generation fencing 备层 (§六): pre-receive validates the
		// attempt-id push option server-side.
		args = append(args, "--push-option=attempt-id="+o.AttemptID)
	}
	args = append(args, o.RemoteURL, "HEAD:refs/heads/main")
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		return false, err
	}
	defer cleanup()
	env := append(o.Env(), additions...)
	out, err := git(env, o.CapsuleDir, args...)
	if err != nil {
		return false, &api.Error{Code: api.ErrGitInternal, Detail: strings.TrimSpace(out) + ": " + err.Error()}
	}
	return true, nil
}

// Clone materializes a fresh capsule worktree from the aihub bare repo
// (fresh-derivation path of the recovery engine). Takes the per-WI lock.
func Clone(o Opts) error {
	release := Lock(o.WorkItemID)
	defer release()
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		return err
	}
	defer cleanup()
	env := append(os.Environ(), additions...)
	_, err = git(env, "", "clone", o.RemoteURL, o.CapsuleDir)
	return err
}

// InitLocal initializes the capsule worktree without a remote: the
// fresh-derivation fallback when the bare repo is unreachable. Takes the
// per-WI lock.
func InitLocal(o Opts) error {
	release := Lock(o.WorkItemID)
	defer release()
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		return err
	}
	out, err := git(nil, "", "init", o.CapsuleDir)
	if err != nil {
		return fmt.Errorf("git init: %s", strings.TrimSpace(out))
	}
	return nil
}

// ExternalizeGitDir moves the worktree's git-dir out to o.GitDir (the
// agent must never reach hooks/config/index), leaves the one-line .git
// pointer file, and pins the working branch to main. Idempotent: a
// pointer that already names an existing external git-dir is a no-op.
// Takes the per-WI lock.
func ExternalizeGitDir(o Opts) error {
	release := Lock(o.WorkItemID)
	defer release()
	if err := os.MkdirAll(filepath.Dir(o.GitDir), 0o755); err != nil {
		return err
	}
	inner := filepath.Join(o.CapsuleDir, ".git")
	if st, serr := os.Stat(inner); serr == nil && st.Mode().IsRegular() {
		if dirExists(o.GitDir) {
			return nil // already externalized
		}
		return fmt.Errorf("gitops: capsule %s has a pointer but no git-dir at %s", o.CapsuleDir, o.GitDir)
	}
	if err := os.Rename(inner, o.GitDir); err != nil {
		if !os.IsExist(err) {
			return err
		}
		if err := os.RemoveAll(inner); err != nil {
			return err
		}
	}
	if err := os.WriteFile(inner, []byte("gitdir: "+o.GitDir+"\n"), 0o644); err != nil {
		return err
	}
	env := append(os.Environ(),
		"GIT_DIR="+o.GitDir,
		"GIT_WORK_TREE="+o.CapsuleDir,
	)
	if out, err := git(env, o.CapsuleDir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return fmt.Errorf("git symbolic-ref: %s", strings.TrimSpace(out))
	}
	return nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---------------------------------------------------------------------------
// Credential delivery: one-shot GIT_ASKPASS (never argv, never persistent)

// staleLockAge is how old an index.lock must be before cleanStaleLocks
// removes it.
const staleLockAge = 10 * time.Minute

// credentialEnv prepares the one-shot credential delivery for an http(s)
// remote: a GIT_ASKPASS helper script written OUTSIDE the capsule, in a
// 0700 directory sibling of the external git-dir, containing the
// username/password answers. The returned env additions are meant for ONE
// git invocation; cleanup removes the script. The secret never appears in
// argv (visible to the whole agent tree via /proc/*/cmdline) nor in a
// persistent environment — only inside the 0700 script, for the duration
// of the call. Non-http remotes (local bare repos, file://) consult no
// credential: no helper is written.
func credentialEnv(o Opts) (additions []string, cleanup func(), err error) {
	noop := func() {}
	if o.Username == "" || o.Secret == "" {
		return nil, noop, nil
	}
	if !strings.HasPrefix(o.RemoteURL, "http://") && !strings.HasPrefix(o.RemoteURL, "https://") {
		return nil, noop, nil
	}
	base := filepath.Dir(o.GitDir)
	tempBase := false
	if o.GitDir == "" || base == "" || base == "." {
		if base, err = os.MkdirTemp("", "pf-askpass-"); err != nil {
			return nil, noop, err
		}
		// The fallback base is ours alone (nothing else names it);
		// cleaning it up WITH the call, or every invocation leaks a
		// fresh temp base directory.
		tempBase = true
	}
	dir := filepath.Join(base, "askpass")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, noop, err
	}
	// MkdirAll keeps an existing dir's mode: pin 0700 every time.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, noop, err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return nil, noop, err
	}
	script := filepath.Join(dir, "askpass-"+o.WorkItemID+"-"+suffix)
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  Username*) printf '%s\\n' " + shellQuote(o.Username) + " ;;\n" +
		"  *) printf '%s\\n' " + shellQuote(o.Secret) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		return nil, noop, err
	}
	cleanup = func() {
		_ = os.Remove(script)
		_ = os.Remove(dir) // best-effort: another WI's script may keep it alive
		if tempBase {
			_ = os.RemoveAll(base)
		}
	}
	return []string{
		"GIT_ASKPASS=" + script,
		"GIT_TERMINAL_PROMPT=0",
	}, cleanup, nil
}

func randomSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// shellQuote single-quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// SweepAskpass removes the one-shot askpass scripts a killed process
// left behind. credentialEnv's cleanup only runs when the call unwinds:
// a daemon killed mid-push (SIGKILL, power loss) leaves its 0700
// script — which holds the push secret — under <gitRoot>/askpass
// forever. Call it ONCE at daemon startup with the directory that holds
// the external git-dirs (filepath.Dir of any GitDir, i.e.
// <DataDir>/git): nothing can be live that early, so every askpass-*
// entry is crash residue. Best-effort — an unreadable entry is skipped
// (the first removal error is reported, the sweep continues).
func SweepAskpass(gitRoot string) error {
	dir := filepath.Join(gitRoot, "askpass")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "askpass-") {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// Stale lock handling

// cleanStaleLocks removes an index.lock only after it has outlived
// staleLockAge. git's index.lock holds the in-progress index (binary
// content), not a holder PID — nothing in the file can prove the holder
// dead, so a "PID parse" would delete live locks as garbage. Age is the
// only safe signal: our own locks are short-lived by construction (the
// per-WI git mutex serializes every git invocation of this WI, and each
// holds the index lock for one command only), so an index.lock older
// than the bound is stale by any honest reading — and a YOUNG lock,
// foreign or not, is left alone for git to report loudly against (the
// push fails and is retried at the next boundary).
func cleanStaleLocks(gitDir string) error {
	if gitDir == "" {
		return nil
	}
	lockPath := filepath.Join(gitDir, "index.lock")
	st, err := os.Stat(lockPath)
	if err != nil {
		return nil // absent or unreadable: git reports loudly if it matters
	}
	if time.Since(st.ModTime()) < staleLockAge {
		return nil
	}
	return os.Remove(lockPath)
}
