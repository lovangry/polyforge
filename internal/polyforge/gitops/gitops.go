// Package gitops is the shared capsule-git operations layer used by Runtime
// (clone at Start, dirty-commit at Recover) and Observe (async commit+push
// on execution.json change). It enforces the v1 invariants that need a
// single choke point:
//
//   - per-WI git mutex: Runtime and Observe never run git against the same
//     worktree concurrently (v1-spec-full §六);
//   - every commit carries the Polyforge-Seq trailer so aihub's pre-receive
//     hook can enforce monotonic sequence;
//   - pushes carry the attempt-scoped credential and go through aihub's
//     git HTTP proxy;
//   - stale index.lock files whose holding PID is dead are removed at
//     acquisition (restart safety, v1-spec-full §六).
//
// git-dir is ALWAYS external to the capsule (never inside agent-writable
// space). All git access goes through the real git binary.
package gitops

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

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
	CapsuleDir string // worktree root (agent-writable)
	GitDir     string // external git-dir (e.g. <data>/git/wi_xxx.git)
	RemoteURL  string // aihub git proxy URL, e.g. https://host/git/wi_xxx.git
	Username   string // push credential username (= work item id)
	Secret     string // attempt-scoped push credential secret
}

func (o Opts) env() []string {
	return append(os.Environ(),
		"GIT_DIR="+o.GitDir,
		"GIT_WORK_TREE="+o.CapsuleDir,
	)
}

// git runs git once with the given environment (nil = inherit) and dir.
func git(env []string, dir string, args ...string) (string, error) {
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

// CommitPush stages everything, commits with the seq trailer and pushes to
// the aihub bare repo. It takes the per-WI git lock itself; callers must
// not hold it. Returns the local commit SHA and whether the push reached
// the server (the flush ack distinguishes the two, per the review ruling).
func CommitPush(o Opts, seq int64, message string) (localSHA string, pushed bool, err error) {
	release := Lock(o.WorkItemID)
	defer release()

	if err := cleanStaleLocks(o.GitDir); err != nil {
		return "", false, err
	}
	env := o.env()
	if _, err := git(env, o.CapsuleDir, "add", "-A"); err != nil {
		return "", false, err
	}
	msg := fmt.Sprintf("%s\n\n%s: %d", message, capsule.SeqTrailer, seq)
	if _, err := git(env, o.CapsuleDir, "commit", "-m", msg); err != nil {
		if strings.Contains(err.Error(), "nothing to commit") ||
			strings.Contains(err.Error(), "no changes added") {
			sha, _ := git(env, o.CapsuleDir, "rev-parse", "HEAD")
			return strings.TrimSpace(sha), false, nil
		}
		return "", false, err
	}
	sha, err := git(env, o.CapsuleDir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	localSHA = strings.TrimSpace(sha)
	pushed, perr := push(o)
	return localSHA, pushed, perr
}

// push sends HEAD to the aihub bare repo's main branch with the
// attempt-scoped credential embedded in the remote URL.
func push(o Opts) (bool, error) {
	if o.RemoteURL == "" || o.Username == "" || o.Secret == "" {
		return false, &api.Error{Code: api.ErrGitUnauthorized, Detail: "no push credential configured"}
	}
	cred := o.Username + ":" + o.Secret
	remote := strings.Replace(o.RemoteURL, "https://", "https://"+cred+"@", 1)
	out, err := git(o.env(), o.CapsuleDir, "push", remote, "HEAD:refs/heads/main")
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
	cred := o.Username + ":" + o.Secret
	remote := strings.Replace(o.RemoteURL, "https://", "https://"+cred+"@", 1)
	_, err := git(nil, "", "clone", remote, o.CapsuleDir)
	return err
}

// Init creates the capsule-side worktree against an existing (possibly
// empty) external git-dir: init + first commit + branch main. Used by
// Runtime.Start when aihub reports an empty bare repo.
func Init(o Opts) error {
	release := Lock(o.WorkItemID)
	defer release()
	if _, err := git(nil, o.CapsuleDir, "init", o.CapsuleDir); err != nil {
		return err
	}
	// Point the worktree's git-dir at the external location.
	gitDir := filepath.Join(o.CapsuleDir, ".git")
	if err := os.MkdirAll(filepath.Dir(o.GitDir), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: "+o.GitDir+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := git(nil, o.CapsuleDir, "checkout", "-b", "main"); err != nil {
		return err
	}
	return nil
}

// cleanStaleLocks removes index.lock whose holding PID is dead (restart
// safety). A live holder is left alone — the per-WI mutex serializes us
// against ourselves; a live foreign holder surfaces as a push failure and
// is handled by the credential fencing at the server.
func cleanStaleLocks(gitDir string) error {
	lockPath := filepath.Join(gitDir, "index.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return nil // absent or unreadable: git reports loudly if it matters
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return os.Remove(lockPath) // garbage content: stale by definition
	}
	if processAlive(pid) {
		return nil
	}
	return os.Remove(lockPath)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
