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
//   - pushes carry the session-scoped credential and go through aihub's
//     git HTTP proxy; the credential travels via a one-shot GIT_ASKPASS
//     helper (never in argv, never in persistent env) and reaches both
//     http:// and https:// remotes;
//   - pushes carry --push-option=session-fingerprint=<fp> (v1.5 §1.6
//     fencing-on-use backup layer: Opts.Fingerprint, the PUBLIC
//     session handle). The runtime always sets it; the aihub hook
//     REQUIRES the option on every push that travels through its git
//     proxy;
//   - an index.lock that has outlived the stale bound is removed at
//     acquisition (see cleanStaleLocks for why age, not PID, is the only
//     safe signal);
//   - NO gitlink ever enters local history: `git add -A` followed by a
//     `git ls-files --stage` scan refuses mode-160000 entries with the
//     typed GIT-412 LOCAL_GITLINK_DETECTED error BEFORE the commit is
//     created (the pilot's #48 poison chain made every later push of the
//     standing HEAD unfetchable; the precheck keeps the poison out of
//     history entirely).
//   - task-repo durability lane (polyforge#108): every publish also
//     mirrors each repos/<name> worktree's HEAD into the WI's own bare
//     repo as refs/pf-task/<name> (pushTaskRefsLocked) — the capsule
//     chain itself cannot carry task-repo state (ruling C1: /repos/ is
//     gitignored), and without this lane the only copy of a session's
//     code commits is the capsule directory a re-claim renames away.
//     Best-effort-loud: a carrier failure never fails the
//     execution-state publish; the claim-time clone lane reads the
//     carrier back via FetchTaskRef. aihub's pre-receive hook exempts
//     the namespace from the chain-only seq/gitlink invariants.
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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// Fingerprint is the v1.5 PUBLIC session fingerprint
	// (hex(sha256(session_id))[:16]); rides pushes as
	// --push-option=session-fingerprint=<fp> (§1.6 fencing-on-use
	// backup layer). The runtime sets it from the claim answer; empty
	// sends no push option at all.
	Fingerprint string
	CapsuleDir  string // worktree root (agent-writable)
	GitDir      string // external git-dir (e.g. <data>/git/wi_xxx.git)
	RemoteURL   string // aihub git proxy URL, e.g. http://host:8420/git/wi_xxx.git
	Username    string // push credential username (= work item id)
	Secret      string // push credential secret (v1.5: the session id; askpass only — never argv, never env)
	// EventSink receives the durable error events the gitops lanes raise
	// (e.g. EventTaskRefDiverged), already bound to this work item by the
	// runtime. nil drops them.
	EventSink func(kind string, body map[string]any)
}

// Env returns the environment every raw git invocation against this
// capsule must carry (inherited environ + the external git-dir binding).
func (o Opts) Env() []string {
	return append(os.Environ(),
		"GIT_DIR="+o.GitDir,
		"GIT_WORK_TREE="+o.CapsuleDir,
	)
}

// CEnv returns env (nil = inherited environ) pinned to the C locale:
// LC_ALL=C and no LANGUAGE, so git's messages are never translated and
// the output this package (and ship) pattern-matches stays stable.
func CEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "LANGUAGE=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_ALL=C")
}

// exitCoder is satisfied by *exec.ExitError.
type exitCoder interface{ ExitCode() int }

// gitError is the failure of one git invocation: the formatted message
// plus the underlying exec error (for exit-code checks).
type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string { return e.msg }
func (e *gitError) Unwrap() error { return e.err }

// exitCode answers err's git exit code (-1 when it is not an exit error).
func exitCode(err error) int {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// git runs git once with the given environment (nil = inherit) and dir.
// It is a var so the tests can probe the argv the package builds.
var git = func(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = CEnv(env)
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
		return stdout.String(), &gitError{msg: fmt.Sprintf("git %s: %s", strings.Join(args, " "), msg), err: err}
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

// localGitlinkPrefix marks the LOCAL precheck's typed refusal (the
// sentinel observe classifies push_poisoned_local; a server-side
// pre-receive gitlink rejection never carries it). The refusal is
// returned as an api.Error carrying code GIT-412 with this sentinel at
// the head of its Detail; see checkLocalGitlink for the full shape.
//
// Why the precheck exists: the pilot's poison-chain incident (#48) was
// this shape discovered too late — the gitlink had already been
// committed, and every push of the standing HEAD was then rejected by
// aihub's pre-receive hook (185 consecutive push_failed events, ~1h
// storm). The precheck moves the refusal BEFORE the commit: the poison
// never enters local history, so no retry loop can ever re-offer it to
// the server. NOT retry-worthy: the identical error would answer every
// retry until the tree is cleaned.
const localGitlinkPrefix = "LOCAL_GITLINK_DETECTED"

// IsLocalGitlink reports whether err is the local precheck's typed
// gitlink refusal (as opposed to a server-side hook rejection of an
// already-committed gitlink, which stays an ordinary push error).
func IsLocalGitlink(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) &&
		ae.Code == api.ErrGitGitlink &&
		strings.HasPrefix(ae.Detail, localGitlinkPrefix)
}

// checkLocalGitlink scans the staged index for gitlink (mode 160000)
// entries and, when any exist, unstages everything (best-effort `git
// reset -q` — the worktree is untouched, only the index is dropped, and
// the next healthy CommitPush re-stages it) and answers the typed
// GIT-412 LOCAL_GITLINK_DETECTED refusal (an *api.Error, so observe's
// IsLocalGitlink classification sees the code + sentinel pair): the
// error names the polluted paths and the cleanup recipe (delete the
// nested .git, git rm --cached) so the operator can act on it. Repos
// under the capsule's ignored trees (repos/, artifacts/release-*/)
// never reach the index, so ANY staged gitlink is poison by definition.
func checkLocalGitlink(env []string, dir string) error {
	out, err := git(env, dir, "ls-files", "--stage")
	if err != nil {
		return err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) > 0 && fields[0] == "160000" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	// Drop the index so the poison does not linger staged for any other
	// commit path; a failure here is ignored — the refusal below is the
	// load-bearing half either way.
	_, _ = git(env, dir, "reset", "-q")
	detail := localGitlinkPrefix + ": staged gitlink (mode 160000) at " +
		strings.Join(paths, ", ") +
		" — an embedded git repository would poison the capsule chain (aihub's pre-receive rejects mode 160000 on every push, forever). " +
		"No commit was created. Clean it: delete the embedded repository's .git directory (" +
		filepath.Join(paths[0], ".git") + "), then `git rm --cached " + paths[0] + "`, then retry; " +
		"verify with `git ls-files --stage | grep ^160000` (must be empty)."
	return &api.Error{Code: api.ErrGitGitlink, Detail: detail}
}

// PushTimings carries the wall-clock durations of the underlying git
// invocations a publish attempt is made of (integer milliseconds, as
// seen by this process around the raw git calls): CommitMS spans the
// git commit call, PushMS spans the git push call. Zero is the honest
// reading for a call that did not happen on this path (a clean-tree
// publish performs no commit). Observed-only bookkeeping (M8 latency
// samples in the published event bodies); no judgment consumes it here.
type PushTimings struct {
	CommitMS int64 `json:"commit_ms"`
	PushMS   int64 `json:"push_ms"`
}

// CommitPush stages everything, commits with the seq trailer (derived
// here — the single authority) and pushes to the aihub bare repo. It
// takes the per-WI git lock itself; callers must not hold it. A clean
// tree (nothing staged after the porcelain check) PUSHES the standing
// HEAD — an idempotent no-op ("Everything up-to-date") when the server
// already holds it — so the pushed flag is honest on every return shape,
// including the retry-after-failed-push one a wrap/seal re-drive needs:
// a commit that landed locally on a failed push finally reaches the
// server HERE, and a still-dead server answers with its error, not a
// fake success. Returns the local commit SHA, whether the push reached
// the server, and the commit/push wall-clock timings.
func CommitPush(o Opts, message string) (string, bool, PushTimings, error) {
	release := Lock(o.WorkItemID)
	defer release()

	var timings PushTimings

	if err := cleanStaleLocks(o.GitDir); err != nil {
		return "", false, timings, err
	}
	env := o.Env()
	// Clean-tree check BEFORE committing: git reports "nothing to commit"
	// on stdout, so the failure cannot be told apart from a real commit
	// error by the error string. The porcelain listing is the honest
	// signal: empty output means the standing HEAD is already the answer.
	status, err := git(env, o.CapsuleDir, "status", "--porcelain")
	if err != nil {
		return "", false, timings, err
	}
	if strings.TrimSpace(status) == "" {
		// Clean tree: the standing HEAD is the answer — and it is
		// PUSHED, not presumed. Skipping the push here would let a
		// clean-tree retry after a failed push fake success and
		// silence §九 backpressure in the one scenario (persistent
		// server unreachability) it exists for.
		sha, serr := git(env, o.CapsuleDir, "rev-parse", "HEAD")
		if serr != nil {
			return "", false, timings, fmt.Errorf("%w: %v", ErrEmptyHistory, serr)
		}
		localSHA := strings.TrimSpace(sha)
		pushed, pushMS, perr := pushLocked(o)
		timings.PushMS = pushMS
		return localSHA, pushed, timings, perr
	}
	seq, err := nextSeq(env, o.CapsuleDir)
	if err != nil {
		return "", false, timings, err
	}
	if _, err := git(env, o.CapsuleDir, "add", "-A"); err != nil {
		// An embedded repository with no commit checked out fails the add
		// itself ("'x/' does not have a commit checked out") — the same
		// poison shape one step earlier. Type it identically so the
		// operator sees the actionable refusal, not a bare git error.
		if strings.Contains(err.Error(), "does not have a commit checked out") {
			_, _ = git(env, o.CapsuleDir, "reset", "-q")
			return "", false, timings, &api.Error{
				Code:   api.ErrGitGitlink,
				Detail: localGitlinkPrefix + ": embedded git repository without a commit checked out — " + err.Error() + ". No commit was created. Clean it: delete the embedded repository's .git directory, then retry.",
			}
		}
		return "", false, timings, err
	}
	// LOCAL gitlink precheck (BEFORE the commit is created): an embedded
	// git repository staged by the add above must never reach local
	// history — see ErrLocalGitlink. Runs after `add -A` because
	// ls-files reads the index: this placement catches both a standing
	// poisoned index and the fresh nested .git the add just staged.
	if err := checkLocalGitlink(env, o.CapsuleDir); err != nil {
		return "", false, timings, err
	}
	msg := fmt.Sprintf("%s\n\n%s: %d", message, capsule.SeqTrailer, seq)
	commitStart := time.Now()
	_, cerr := git(env, o.CapsuleDir, "commit", "-m", msg)
	timings.CommitMS = time.Since(commitStart).Milliseconds()
	if cerr != nil {
		return "", false, timings, cerr
	}
	sha, err := git(env, o.CapsuleDir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, timings, err
	}
	localSHA := strings.TrimSpace(sha)
	pushed, pushMS, perr := pushLocked(o)
	timings.PushMS = pushMS
	return localSHA, pushed, timings, perr
}

// Head answers the capsule's current HEAD commit SHA ("" with an error
// for an unborn branch / unreadable repo). Read-only.
func Head(o Opts) (string, error) {
	release := Lock(o.WorkItemID)
	defer release()
	out, err := git(o.Env(), o.CapsuleDir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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
// reached the server). Takes the per-WI lock. The push timing is
// measured but not answered: this entry point serves the runtime seal
// retry, which emits no published event and has no consumer for it.
func Push(o Opts) (bool, error) {
	release := Lock(o.WorkItemID)
	defer release()
	pushed, _, err := pushLocked(o)
	return pushed, err
}

// pushLocked is push under an already-held per-WI lock. The returned
// pushMS is the wall-clock duration of the git push invocation,
// measured honestly on failure too (the observed-only latency sample).
// Besides the capsule chain (HEAD:refs/heads/main) it drives the
// task-repo durability lane (pushTaskRefsLocked): best-effort, after
// the chain push either way, never blocking it.
//
// Same-WI push idempotency (the shutdown-window push race): the receive
// protocol compare-and-swaps every push against the ref value its
// client saw at advertisement time, so a duplicate push of an
// already-published commit — e.g. a recovery push of one work item
// racing an overlapping push of the same WI across a stop+restart
// boundary — is rejected with "cannot lock ref" even though the server
// already holds the exact state being pushed. That rejection shape is
// retried a bounded number of times: each retry re-advertises, and a
// server already holding the standing HEAD answers "Everything
// up-to-date" (success). Anything still failing — or failing for a
// different reason — surfaces as the honest error.
func pushLocked(o Opts) (bool, int64, error) {
	if o.RemoteURL == "" {
		return false, 0, &api.Error{Code: api.ErrGitUnauthorized, Detail: "no push remote configured"}
	}
	args := []string{"push"}
	if o.Fingerprint != "" {
		// v1.5 §1.6 fencing-on-use backup layer: the push option carries
		// the PUBLIC session fingerprint; the pre-receive hook rejects a
		// push whose option differs from the credential's fingerprint,
		// naming the CURRENT holder so a superseded daemon self-diagnoses.
		args = append(args, "--push-option=session-fingerprint="+o.Fingerprint)
	}
	args = append(args, o.RemoteURL, "HEAD:refs/heads/main")
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		return false, 0, err
	}
	defer cleanup()
	env := append(o.Env(), additions...)
	pushed, pushMS, perr := pushChainLocked(env, o.CapsuleDir, args)
	// Task-repo durability lane: after the chain push either way, its
	// own invocations, its own failures — never the chain's result.
	pushTaskRefsLocked(o)
	return pushed, pushMS, perr
}

// pushChainLocked is the chain push itself (HEAD:refs/heads/main) with
// the same-WI push idempotency retry, factored out so pushLocked can
// append the task-repo carrier lane after it without entangling the
// retry semantics.
func pushChainLocked(env []string, capsuleDir string, args []string) (bool, int64, error) {
	for attempt := 1; ; attempt++ {
		pushStart := time.Now()
		out, perr := git(env, capsuleDir, args...)
		pushMS := time.Since(pushStart).Milliseconds()
		if perr == nil {
			return true, pushMS, nil
		}
		detail := strings.TrimSpace(out) + ": " + perr.Error()
		if !refCasRejected(detail) || attempt >= pushRaceAttempts {
			return false, pushMS, &api.Error{Code: api.ErrGitInternal, Detail: detail}
		}
		// The ref moved (or appeared) between this push's advertisement
		// and its ref transaction: re-advertise and try again — the
		// winner of the overlap usually delivered this very commit, so
		// the retry answers up-to-date.
		time.Sleep(pushRaceBackoff)
	}
}

// TaskCarrierNamespace is the ref namespace, inside the work item's own
// bare repo, that carries each task repo's local head across session
// boundaries (polyforge#108): pushTaskRefsLocked mirrors every
// repos/<name> HEAD to refs/pf-task/<name> on each publish, and the
// claim-time clone lane (runtime.cloneTaskRepos, via FetchTaskRef)
// restores it when a bounded-fail re-claim must rebuild repos/<name>
// from its bare origin. The capsule chain cannot carry this state
// (ruling C1: /repos/ is gitignored), so without the lane the only copy
// of a session's code commits is the capsule directory — exactly what a
// re-claim renames away. The namespace lives OUTSIDE refs/heads/* so a
// plain clone of the bare repo never materializes task branches, and
// aihub's pre-receive hook exempts it from the chain-only seq/gitlink
// invariants while keeping deletion, fast-forward and the blob limit.
const TaskCarrierNamespace = "refs/pf-task/"

// TaskRefName answers the carrier ref for one task repo.
func TaskRefName(repo string) string { return TaskCarrierNamespace + repo }

// pushTaskRefsLocked mirrors every task-repo worktree's HEAD under the
// capsule into the WI's bare repo (HEAD:refs/pf-task/<name>). Runs under
// the per-WI lock (called from pushLocked), AFTER the chain push, and is
// BEST-EFFORT-LOUD: each push is its own git invocation — a rejected or
// failed carrier never fails the execution-state publish this lane
// rides; it is logged loudly so the operator can see a re-claim would
// lose that repo's state. An unborn or unreadable task HEAD is nothing
// to carry (honest skip, not a failure). The invocations run inside each
// task worktree with its OWN git-dir — never the capsule's external
// GIT_DIR/GIT_WORK_TREE binding, whose history holds none of these
// objects.
func pushTaskRefsLocked(o Opts) {
	reposDir := filepath.Join(o.CapsuleDir, capsule.DirRepos)
	ents, err := os.ReadDir(reposDir)
	if err != nil {
		return // no repos/ tree: no inventory, nothing to carry
	}
	additions, cleanup, cerr := credentialEnv(o)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "gitops: %s: task-ref push skipped (credential prep failed): %v\n", o.WorkItemID, cerr)
		return
	}
	defer cleanup()
	env := append(os.Environ(), additions...)
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(reposDir, e.Name())
		if out, err := git(nil, dir, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(out) == "" {
			continue // unborn/absent HEAD: nothing to carry yet
		}
		// The task repo is agent-controlled (agents are untrusted): its
		// hooks (pre-push would run as the daemon WITH the credential
		// askpass) and fsmonitor are disabled for this invocation.
		args := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "push"}
		if o.Fingerprint != "" {
			args = append(args, "--push-option=session-fingerprint="+o.Fingerprint)
		}
		args = append(args, o.RemoteURL, "HEAD:"+TaskRefName(e.Name()))
		if out, err := git(env, dir, args...); err != nil {
			fmt.Fprintf(os.Stderr, "gitops: %s: task-ref push for repo %s failed (task-repo state may not survive a re-claim): %v: %s\n", o.WorkItemID, e.Name(), err, strings.TrimSpace(out))
			// Never force: a non-fast-forward rejection means the carrier
			// and this worktree DIVERGED (e.g. a re-claim that started
			// from a clone lacking the carried commits). The carrier is
			// left untouched and the divergence is raised as an error
			// event, not left to stderr.
			if IsNonFastForward(err, out) && o.EventSink != nil {
				o.EventSink(EventTaskRefDiverged, map[string]any{
					"repo":  e.Name(),
					"ref":   TaskRefName(e.Name()),
					"error": err.Error(),
				})
			}
		}
	}
}

// FetchTaskRef reads the carrier lane from the claim side: it fetches
// refs/pf-task/<name> from the WI's bare repo into the task worktree at
// dir and answers the SHA the carrier names. "" with a nil error means
// the carrier holds nothing for this repo yet — the NORMAL first-claim
// shape (no previous session has published a head), answered quietly;
// any other failure comes back loud to the caller. The fetch carries the
// session credential (one-shot askpass) and never touches the capsule
// worktree: dir is the task repo's own clone with its own git-dir. Takes
// the per-WI lock, mirroring Clone — the claim lane is single-threaded
// per WI, but the invariant (one git face per WI at a time) stays
// mechanical, not remembered.
func FetchTaskRef(o Opts, dir, name string) (string, error) {
	if o.RemoteURL == "" {
		return "", nil // no capsule remote: nowhere a carrier could exist
	}
	release := Lock(o.WorkItemID)
	defer release()
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := append(os.Environ(), additions...)
	// ls-remote --exit-code answers 2 when no ref matches: the
	// language-independent "no carrier yet" signal (never git's message).
	if _, err := git(env, dir, "ls-remote", "--exit-code", o.RemoteURL, TaskRefName(name)); err != nil {
		if exitCode(err) == 2 {
			return "", nil // no carrier recorded yet: first claim, nothing to restore
		}
		return "", err
	}
	if _, err := git(env, dir, "fetch", "--quiet", "--no-tags", o.RemoteURL, TaskRefName(name)); err != nil {
		return "", err
	}
	out, err := git(env, dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// pushRaceAttempts bounds how many times a ref-transaction CAS rejection
// is re-advertised before the push surfaces its error, and
// pushRaceBackoff spaces the retries (the overlapping winner's
// transaction needs a moment to land).
const (
	pushRaceAttempts = 3
	pushRaceBackoff  = 250 * time.Millisecond
)

// refCasRejected reports whether a failed push's combined output is the
// ref-transaction compare-and-swap rejection: git-receive-pack refuses
// the whole push when the ref moved ("cannot lock ref ... is at X but
// expected Y") or came into existence ("reference already exists")
// after the client fixed its expected-old value at advertisement time.
// Those shapes mean an overlapping push of the same ref won the race —
// a duplicate-publish condition, not a wrong push.
func refCasRejected(out string) bool {
	return strings.Contains(out, "cannot lock ref") && strings.Contains(out, "failed to update ref")
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

// crossDeviceMove moves src to dst, falling back to copy+remove when
// rename(2) refuses to cross a filesystem boundary (EXDEV) — the
// workspace-capsule era puts capsules under -workspace while the git
// store stays under the data-dir, and the two may live on different
// mounts (#103 cutover: the first cross-device claim failed exactly
// here). A fresh clone's git-dir is small; the copy is a one-time
// per-claim cost.
func crossDeviceMove(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if le, ok := err.(*os.LinkError); ok && le.Err == syscall.EXDEV {
		if cerr := copyDirTree(src, dst); cerr != nil {
			return cerr
		}
		return os.RemoveAll(src)
	}
	return err
}

// copyDirTree copies one directory tree verbatim (files, symlinks,
// modes; the git-dir's objects are regular files — packed or loose —
// and its few symlinks never leave the store it names).
func copyDirTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		switch {
		case e.Type()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(s)
			if lerr != nil {
				return lerr
			}
			if lerr := os.Symlink(target, d); lerr != nil && !os.IsExist(lerr) {
				return lerr
			}
		case e.IsDir():
			if cerr := copyDirTree(s, d); cerr != nil {
				return cerr
			}
		default:
			info, serr := e.Info()
			if serr != nil {
				return serr
			}
			in, perr := os.Open(s)
			if perr != nil {
				return perr
			}
			out, perr := os.OpenFile(d, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
			if perr != nil {
				in.Close()
				return perr
			}
			_, cerr := io.Copy(out, in)
			in.Close()
			if cerr != nil {
				out.Close()
				return cerr
			}
			if cerr := out.Close(); cerr != nil {
				return cerr
			}
		}
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
	// An existing target here is a STALE git-dir (a previous session's
	// HEAD/index): silently adopting it (or merging the fresh clone over
	// it) leaves a worktree at the remote tip under a diverged history.
	// Fail loudly; the caller quarantines the unit before cloning.
	if _, serr := os.Lstat(o.GitDir); serr == nil {
		return fmt.Errorf("gitops: refusing to externalize %s: git-dir target %s already exists (stale git-dir from a previous session; quarantine it first)", inner, o.GitDir)
	} else if !os.IsNotExist(serr) {
		return serr
	}
	if err := crossDeviceMove(inner, o.GitDir); err != nil {
		return err
	}
	if err := os.WriteFile(inner, []byte("gitdir: "+o.GitDir+"\n"), 0o644); err != nil {
		return err
	}
	env := append(os.Environ(),
		"GIT_DIR="+o.GitDir,
		"GIT_WORK_TREE="+o.CapsuleDir,
	)
	if _, err := git(env, o.CapsuleDir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		// The real diagnosis lives in the error (stderr); stdout is empty
		// on failure — wrapping `out` swallowed every real cause.
		return fmt.Errorf("git symbolic-ref: %s", err.Error())
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

// EventTaskRefDiverged is the error event kind raised when the carrier
// push of one task repo is rejected non-fast-forward.
const EventTaskRefDiverged = "task_ref_diverged"

// IsNonFastForward reports a push refused because the remote ref is not an
// ancestor of the pushed commit. It matches git's C-locale output
// ("! [rejected] HEAD -> main (non-fast-forward)" / "(fetch first)"), so
// callers must run git under CEnv. err may be nil; out is the push output.
func IsNonFastForward(err error, out string) bool {
	msg := out
	if err != nil {
		msg += " " + err.Error()
	}
	return strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "(fetch first)")
}
