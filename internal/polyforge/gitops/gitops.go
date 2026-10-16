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
	"context"
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
	"unicode/utf8"

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
	// CarrierAlerts is the job's once-per-repo state for
	// carrier_push_failed (owned by the runtime job, so it ends with the
	// job and starts fresh on a re-claim). nil raises every failure.
	CarrierAlerts *CarrierAlerts
	// Deadline, when set, bounds every carrier-lane git call (the task
	// carrier and the WIP lane): each call's timeout is cut to the time
	// left, and on expiry the git process group is killed — the graceful
	// shutdown gives each job's WIP snapshot its own slice of the budget
	// this way, so no push outlives the daemon. Zero = the lane's own
	// timeouts only.
	Deadline time.Time
}

// expired reports whether o.Deadline is set and has passed.
func (o Opts) expired() bool {
	return !o.Deadline.IsZero() && !time.Now().Before(o.Deadline)
}

// bound cuts a carrier-lane timeout to the time left before o.Deadline
// (never below 1ms: a zero timeout would mean unbounded).
func (o Opts) bound(d time.Duration) time.Duration {
	if o.Deadline.IsZero() {
		return d
	}
	left := time.Until(o.Deadline)
	if left < time.Millisecond {
		left = time.Millisecond
	}
	if left < d {
		return left
	}
	return d
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
// the output this package pattern-matches stays stable.
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
	return runGit(0, env, dir, args...)
}

// gitTimed is git bounded by timeout: on expiry the whole git process
// group is killed (git spawns remote helpers and pack-objects), so a
// hung remote cannot hold the per-WI lock forever. A var for the same
// test seam as git.
var gitTimed = func(timeout time.Duration, env []string, dir string, args ...string) (string, error) {
	return runGit(timeout, env, dir, args...)
}

// runGit runs git once; timeout 0 = unbounded.
func runGit(timeout time.Duration, env []string, dir string, args ...string) (string, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = CEnv(env)
	cmd.Dir = dir
	if timeout > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("timed out after %s: %w", timeout, err)
	}
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
	// Auto-heal a foreign commit at the tip BEFORE anything else (both
	// the dirty and the clean-tree path push HEAD, and aihub's hook
	// rejects every pushed commit without an integer seq trailer).
	if err := foldUntrailered(o, env); err != nil {
		return "", false, timings, err
	}
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

// EventUntrailedCommitsFolded is the event kind raised when CommitPush
// folds commits without a seq trailer back into the next chain commit.
const EventUntrailedCommitsFolded = "untrailered_commits_folded"

// foldUntrailered is the V-2 auto-heal (repo review 2026-10-16). The
// capsule root's .git pointer (ExternalizeGitDir) makes a step's own
// `git commit` in the capsule root land on the chain branch with no
// Polyforge-Seq trailer; aihub's pre-receive rejects every pushed commit
// without one, so every later publish of that WI would fail forever, and
// nextSeq would count the foreign commit too. When HEAD is not the newest
// trailered commit (walking first parents), the branch is soft-reset onto
// that commit: the foreign commits' content stays staged and is folded
// into the next chain commit under a proper seq, and an
// EventUntrailedCommitsFolded event records it.
//
// Only commits ABOVE the newest trailered one are ever touched, and none
// of those can be on the server (the hook refuses them), so the reset
// never rewrites published history. An unborn branch, or a history with
// no trailered commit at all, is left alone (nothing safe to reset onto;
// the hook answers loudly). NOT healed: a step's `git commit --amend` (or
// any rewrite) of an already-PUSHED trailered commit — the amended tip
// may still carry a trailer, and the push is then a non-fast-forward the
// hook refuses; resetting onto the server's tip would be a history
// rewrite this function never makes. The pointer file itself stays: without it a
// step's git would walk up into whatever repository encloses the
// workspace, which is worse.
func foldUntrailered(o Opts, env []string) error {
	// The steady state costs one call: HEAD carries its trailer.
	tip, err := git(env, o.CapsuleDir, "log", "-1",
		"--format=%(trailers:key="+capsule.SeqTrailer+",valueonly)", "HEAD")
	if err != nil {
		return nil // unborn branch: nothing committed, nothing to fold
	}
	if integerTrailer(tip) {
		return nil
	}
	out, err := git(env, o.CapsuleDir, "log", "--first-parent",
		"--format=%H%x1f%(trailers:key="+capsule.SeqTrailer+",valueonly)%x1e", "HEAD")
	if err != nil {
		return err
	}
	head, onto := "", ""
	folded := 0
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		sha, trailer, _ := strings.Cut(rec, "\x1f")
		if head == "" {
			head = sha
		}
		if integerTrailer(trailer) {
			onto = sha
			break
		}
		folded++
	}
	if folded == 0 || onto == "" {
		return nil
	}
	if _, err := git(env, o.CapsuleDir, "reset", "-q", "--soft", onto); err != nil {
		return err
	}
	if o.EventSink != nil {
		o.EventSink(EventUntrailedCommitsFolded, map[string]any{
			"count":       folded,
			"folded_head": head,
			"onto":        onto,
		})
	}
	return nil
}

// integerTrailer applies the hook's own reading of a seq trailer value:
// all whitespace removed, the rest must be a non-empty run of digits.
func integerTrailer(v string) bool {
	v = strings.Join(strings.Fields(v), "")
	if v == "" {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
	// own invocations, its own failures — never the chain's result. A
	// carrier failure is raised as a human decision only when the chain
	// push itself landed: a hub outage or a fenced session fails both,
	// and that is not a "cannot be saved" decision (a still-failing
	// carrier is reported on the next successful publish).
	pushTaskRefsLocked(o, perr == nil)
	return pushed, pushMS, perr
}

// pushChainLocked is the chain push itself (HEAD:refs/heads/main) with
// the same-WI push idempotency retry, factored out so pushLocked can
// append the task-repo carrier lane after it without entangling the
// retry semantics.
func pushChainLocked(env []string, capsuleDir string, args []string) (bool, int64, error) {
	for attempt := 1; ; attempt++ {
		pushStart := time.Now()
		out, perr := gitTimed(chainRemoteTimeout, env, capsuleDir, args...)
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
// invariants while keeping deletion, fast-forward and a blob limit (the
// carrier's own, capsule.MaxTaskBlobBytes).
const TaskCarrierNamespace = "refs/pf-task/"

// TaskRefName answers the carrier ref for one task repo.
func TaskRefName(repo string) string { return TaskCarrierNamespace + repo }

// pushTaskRefsLocked mirrors every task-repo worktree's HEAD under the
// capsule into the WI's bare repo (HEAD:refs/pf-task/<name>). Runs under
// the per-WI lock (called from pushLocked), AFTER the chain push, and is
// BEST-EFFORT-LOUD: each push is its own git invocation — a rejected,
// refused or failed carrier never fails the execution-state publish this
// lane rides; it is logged loudly and, when the chain push landed
// (chainOK), raised as a carrier_push_failed event (a human decision,
// once per repo per job until the repo's carrier push succeeds again) so
// the human sees that a re-claim may lose that repo's state. An unborn
// task HEAD is nothing to carry (honest skip, not a failure).
//
// The task repo is agent-controlled (agents are untrusted) and the push
// carries the session credential, so the credentialed push NEVER runs
// inside it (git has no switch to ignore a repo's own config, and an
// agent can rewrite that config at any time): carrierSource only reads
// the repo's HEAD SHA and validates its layout, and pushCarrier pushes
// that SHA from a daemon-owned temporary bare repo that borrows the task
// repo's object store through objects/info/alternates (data only — git
// reads objects from an alternate, it runs nothing from it). It answers
// the symlink-resolved repos dir and the repos that passed carrierSource
// with a born HEAD (the WIP lane, PushTaskWIP, works on exactly those).
func pushTaskRefsLocked(o Opts, chainOK bool) (string, []carrierSrc) {
	reposDir := filepath.Join(o.CapsuleDir, capsule.DirRepos)
	ents, err := os.ReadDir(reposDir)
	if err != nil {
		return "", nil // no repos/ tree: no inventory, nothing to carry
	}
	// ONE symlink-resolved repos dir for every check and push (the
	// GIT_CEILING_DIRECTORIES value and the object paths agree).
	if reposDir, err = filepath.EvalSymlinks(reposDir); err != nil {
		return "", nil
	}
	var srcs []carrierSrc
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if o.expired() {
			fmt.Fprintf(os.Stderr, "gitops: %s: task-ref push for repo %s skipped: deadline exceeded\n", o.WorkItemID, e.Name())
			o.carrierFailed(e.Name(), chainOK, "deadline exceeded")
			continue
		}
		objects, sha, reason := carrierSourceT(reposDir, e.Name(), o.bound(carrierCheckTimeout))
		if reason != "" && o.expired() {
			reason = "deadline exceeded (" + reason + ")"
		}
		if reason != "" {
			fmt.Fprintf(os.Stderr, "gitops: %s: task-ref push for repo %s refused (task-repo state may not survive a re-claim): %s\n", o.WorkItemID, e.Name(), reason)
			o.carrierFailed(e.Name(), chainOK, "carrier push refused: "+reason)
			continue
		}
		if sha == "" {
			continue // unborn HEAD: nothing to carry yet
		}
		srcs = append(srcs, carrierSrc{e.Name(), objects, sha})
	}
	if len(srcs) == 0 {
		return reposDir, nil
	}
	additions, cleanup, cerr := credentialEnv(o)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "gitops: %s: task-ref push skipped (credential prep failed): %v\n", o.WorkItemID, cerr)
		return reposDir, srcs
	}
	defer cleanup()
	for _, s := range srcs {
		out, err := pushCarrier(o, reposDir, s.objects, s.sha, s.name, additions)
		if err == nil {
			o.CarrierAlerts.clear(TaskRefName(s.name))
			continue
		}
		detail := err.Error()
		if o.expired() {
			detail = "deadline exceeded: " + detail
		}
		fmt.Fprintf(os.Stderr, "gitops: %s: task-ref push for repo %s failed (task-repo state may not survive a re-claim): %s: %s\n", o.WorkItemID, s.name, detail, strings.TrimSpace(out))
		o.carrierFailed(s.name, chainOK, detail)
		// Never force: a non-fast-forward rejection means the carrier
		// and this worktree DIVERGED (e.g. a re-claim that started
		// from a clone lacking the carried commits). The carrier is
		// left untouched and the divergence is raised as an error
		// event, not left to stderr.
		if IsNonFastForward(err, out) && o.EventSink != nil {
			o.EventSink(EventTaskRefDiverged, map[string]any{
				"repo":  s.name,
				"ref":   TaskRefName(s.name),
				"error": EventDetail(err.Error()),
			})
		}
	}
	return reposDir, srcs
}

// carrierSrc is one task repo that passed carrierSource with a born HEAD:
// its name, its (validated) object directory and its HEAD commit.
type carrierSrc struct{ name, objects, sha string }

// carrierEnv is the ONE environment of every git call on the carrier
// lane (the task-repo reads and the push alike): the daemon's environment
// minus every GIT_* variable, no system or global config (nothing an
// operator's ~/.gitconfig says can reroute a credentialed push), and
// repository discovery fenced at reposDir so a task repo without its own
// .git can never resolve to the capsule or anything above it.
func carrierEnv(reposDir string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CEILING_DIRECTORIES="+reposDir,
	)
}

// Carrier-lane git timeouts: the read-only checks and the temp-repo
// set-up are local and short; the push talks to the hub.
var (
	carrierCheckTimeout = time.Minute
	carrierPushTimeout  = 10 * time.Minute
)

// chainRemoteTimeout bounds every git call of the chain lane that talks
// to the hub — the chain push, the capsule clone, the carrier ls-remote
// and fetch (repo review 2026-10-16 B-15). Each runs under the per-WI
// lock; unbounded, a hung remote held the lock forever (observe abandons
// a timed-out push goroutine that still holds it, and the shutdown's WIP
// snapshot then blocked on Lock). On expiry the git process group is
// killed, like the carrier lane's calls.
var chainRemoteTimeout = 10 * time.Minute

// carrierFlags lead every git call on the carrier lane: no hooks, no
// fsmonitor (both would run programs), no credential helper (an empty
// value resets the helper list), no http proxy.
var carrierFlags = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "credential.helper=",
	"-c", "http.proxy=",
}

// carrierSource validates the task repo repos/<name> and reads its HEAD.
// reposDir is already symlink-resolved. It answers the repo's object
// directory and the HEAD commit SHA ("" = unborn: nothing to carry), or a
// refusal reason. The repo must own its storage outright
// (carrierLayoutRefusal), and git must resolve the repo to exactly its
// own .git. The calls only READ (rev-parse); none of them pushes or
// holds the credential.
func carrierSource(reposDir, name string) (objects, sha, reason string) {
	return carrierSourceT(reposDir, name, carrierCheckTimeout)
}

// carrierSourceT is carrierSource with an explicit per-call timeout.
func carrierSourceT(reposDir, name string, timeout time.Duration) (objects, sha, reason string) {
	dir := filepath.Join(reposDir, name)
	gitDir := filepath.Join(dir, ".git")
	if reason := carrierLayoutRefusal(gitDir); reason != "" {
		return "", "", "repos/" + name + ": " + reason
	}
	objects = filepath.Join(gitDir, "objects")
	env := carrierEnv(reposDir)
	// safe.directory: the repo may be owned by another uid (#135).
	flags := append(append([]string{}, carrierFlags...), "-c", "safe.directory="+dir)
	out, err := gitTimed(timeout, env, dir, append(flags, "rev-parse", "--absolute-git-dir")...)
	if err != nil || strings.TrimSpace(out) != gitDir {
		return "", "", "git does not resolve repos/" + name + " to its own .git directory"
	}
	head, err := gitTimed(timeout, env, dir, append(flags, "rev-parse", "--verify", "-q", "HEAD^{commit}")...)
	if err != nil {
		return objects, "", "" // unborn HEAD
	}
	return objects, strings.TrimSpace(head), ""
}

// carrierLayoutRefusal answers why the git dir at gitDir does not own its
// storage outright ("" = it does). Every piece git would read refs or
// objects through must be a real file or directory of THIS repo: .git
// itself (not a symlink, not a gitfile), no commondir (a worktree-style
// redirect of refs and objects), no symlink at HEAD, packed-refs,
// anywhere under refs/, at any entry of objects/ (pack, info, the ??
// fan-out dirs) or of objects/pack/, and no objects/info/alternates or
// http-alternates (borrowed object stores).
func carrierLayoutRefusal(gitDir string) string {
	if st, err := os.Lstat(gitDir); err != nil || !st.IsDir() {
		return ".git is not a real directory (missing, a symlink or a gitfile)"
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); err == nil {
		return ".git/commondir redirects to another repository"
	}
	for _, rel := range []string{"HEAD", "packed-refs"} {
		if st, err := os.Lstat(filepath.Join(gitDir, rel)); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return ".git/" + rel + " is a symlink"
		}
	}
	refs := filepath.Join(gitDir, "refs")
	if st, err := os.Lstat(refs); err == nil {
		if !st.IsDir() {
			return ".git/refs is not a real directory"
		}
		bad := ""
		_ = filepath.WalkDir(refs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				bad = "cannot read .git/refs"
				return filepath.SkipAll
			}
			if d.Type()&os.ModeSymlink != 0 {
				rel, _ := filepath.Rel(gitDir, path)
				bad = ".git/" + rel + " is a symlink"
				return filepath.SkipAll
			}
			return nil
		})
		if bad != "" {
			return bad
		}
	}
	objects := filepath.Join(gitDir, "objects")
	if st, err := os.Lstat(objects); err != nil || !st.IsDir() {
		return ".git/objects is not a real directory"
	}
	for _, dir := range []string{objects, filepath.Join(objects, "pack")} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "cannot read " + dir
		}
		for _, e := range ents {
			if e.Type()&os.ModeSymlink != 0 {
				rel, _ := filepath.Rel(gitDir, filepath.Join(dir, e.Name()))
				return ".git/" + rel + " is a symlink"
			}
		}
	}
	for _, alt := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(objects, "info", alt)); err == nil {
			return "borrows objects from another store (objects/info/" + alt + ")"
		}
	}
	return ""
}

// pushCarrier pushes sha to refs/pf-task/<name> on the WI's bare repo
// from a daemon-owned temporary bare repo whose only object source is
// the task repo's object store (objects/info/alternates), with the
// carrier env plus the credential additions, a protocol allow-list of
// the remote's own scheme, and no submodule recursion. The temporary
// repo lives outside the capsule and is removed afterwards (a daemon
// killed mid-push leaves it behind: SweepCarrierTemps). reposDir is the
// symlink-resolved repos dir carrierSource used.
func pushCarrier(o Opts, reposDir, objects, sha, name string, credAdditions []string) (string, error) {
	env := carrierEnv(reposDir)
	tmp, out, err := carrierTempRepo(o, env, objects)
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return out, err
	}
	if out, err := gitTimed(o.bound(carrierCheckTimeout), env, tmp, append(carrierTmpFlags(tmp), "update-ref", "refs/pf-carrier/tmp", sha)...); err != nil {
		return out, err
	}
	return carrierPush(o, env, tmp, sha+":"+TaskRefName(name), credAdditions)
}

// carrierTempRepo creates the daemon-owned temporary bare repo of one
// carrier-lane push (next to the external git-dirs, never inside the
// capsule), borrowing objects (the task repo's object store; "" = none)
// through objects/info/alternates. The caller removes tmp (also on
// error, when it is non-empty).
func carrierTempRepo(o Opts, env []string, objects string) (tmp, out string, err error) {
	base := ""
	if o.GitDir != "" {
		base = filepath.Dir(o.GitDir)
	}
	if tmp, err = os.MkdirTemp(base, "pf-carrier-"); err != nil {
		return "", "", err
	}
	if out, err := gitTimed(o.bound(carrierCheckTimeout), env, tmp, "init", "-q", "--bare", "--template=", tmp); err != nil {
		return tmp, out, err
	}
	if objects == "" {
		return tmp, "", nil
	}
	if err := os.MkdirAll(filepath.Join(tmp, "objects", "info"), 0o700); err != nil {
		return tmp, "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return tmp, "", err
	}
	return tmp, "", nil
}

// carrierTmpFlags leads every git call against the temporary repo: the
// explicit --git-dir plus carrierFlags.
func carrierTmpFlags(tmp string) []string {
	return append([]string{"--git-dir=" + tmp}, carrierFlags...)
}

// carrierProtocolFlags allow exactly the remote's own transport scheme.
func carrierProtocolFlags(o Opts) []string {
	scheme := "file"
	if i := strings.Index(o.RemoteURL, "://"); i > 0 {
		scheme = o.RemoteURL[:i]
	}
	return []string{"-c", "protocol.allow=never", "-c", "protocol." + scheme + ".allow=always"}
}

// carrierPush runs the credentialed push of one refspec from the
// temporary repo tmp: the carrier env plus the credential additions, a
// protocol allow-list of the remote's own scheme, no submodule
// recursion, the session fingerprint push option, the push timeout.
func carrierPush(o Opts, env []string, tmp, refspec string, credAdditions []string) (string, error) {
	args := append(append(carrierTmpFlags(tmp), carrierProtocolFlags(o)...),
		"push", "--no-recurse-submodules")
	if o.Fingerprint != "" {
		args = append(args, "--push-option=session-fingerprint="+o.Fingerprint)
	}
	args = append(args, o.RemoteURL, refspec)
	return gitTimed(o.bound(carrierPushTimeout), append(env, credAdditions...), tmp, args...)
}

// carrierFailed raises carrier_push_failed for repo — only when the
// chain push landed (chainOK), and only the first time since the repo's
// last successful carrier push in this job (CarrierAlerts).
func (o Opts) carrierFailed(repo string, chainOK bool, detail string) {
	o.carrierRefFailed(repo, TaskRefName(repo), chainOK, detail)
}

// carrierRefFailed is carrierFailed for any carrier-lane ref (the task
// carrier or the WIP ref, polyforge#167); the once-per-job alert state is
// kept per ref.
func (o Opts) carrierRefFailed(repo, ref string, chainOK bool, detail string) {
	if !chainOK || o.EventSink == nil || !o.CarrierAlerts.first(ref) {
		return
	}
	o.EventSink(EventCarrierPushFailed, map[string]any{
		"repo":  repo,
		"ref":   ref,
		"error": EventDetail(detail),
	})
}

// eventDetailMax bounds the free-text detail an event body carries.
const eventDetailMax = 2048

// EventDetail truncates an event body's free-text detail (a git error)
// to eventDetailMax bytes, cut on a rune boundary.
func EventDetail(s string) string {
	if len(s) <= eventDetailMax {
		return s
	}
	cut := eventDetailMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
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
	return fetchCarrierRef(o, dir, TaskRefName(name))
}

// fetchCarrierRef is FetchTaskRef for any carrier-lane ref (the task
// carrier or the WIP ref): ("", nil) when the hub holds no such ref.
func fetchCarrierRef(o Opts, dir, ref string) (string, error) {
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
	if _, err := gitTimed(o.bound(chainRemoteTimeout), env, dir, "ls-remote", "--exit-code", o.RemoteURL, ref); err != nil {
		if exitCode(err) == 2 {
			return "", nil // no carrier recorded yet: first claim, nothing to restore
		}
		return "", err
	}
	if _, err := gitTimed(o.bound(chainRemoteTimeout), env, dir, "fetch", "--quiet", "--no-tags", o.RemoteURL, ref); err != nil {
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
	_, err = gitTimed(chainRemoteTimeout, env, "", "clone", o.RemoteURL, o.CapsuleDir)
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

// SweepCarrierTemps removes the pf-carrier-* temporary bare repos a
// killed process left next to the external git-dirs (pushCarrier removes
// its own when the call unwinds). Call it ONCE at daemon startup with
// the same gitRoot as SweepAskpass: nothing can be live that early.
// Best-effort — the first removal error is reported, the sweep
// continues.
func SweepCarrierTemps(gitRoot string) error {
	ents, err := os.ReadDir(gitRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pf-carrier-") {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(gitRoot, e.Name())); rerr != nil && firstErr == nil {
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

// EventCarrierPushFailed is the event kind raised when the carrier push
// of one task repo fails for any reason (blob limit, divergence,
// transport, a refused repo config): a re-claim restores that repo
// without the commits since its last carried head, so the watcher
// classifies it as a human decision (polyforge#158). Raised only after a
// chain push that landed, and once per repo per job until a carrier push
// of that repo succeeds again (CarrierAlerts).
const EventCarrierPushFailed = "carrier_push_failed"

// CarrierAlerts is one job's in-memory record of the carrier-lane refs
// (refs/pf-task/<name>, refs/pf-wip/<name>) whose failure has already
// been raised; a successful push of that ref clears it, so a later
// failure alerts again. The runtime keeps
// one per job: it ends with the job, and a re-claim (a new job) starts
// clean. The zero value is ready; a nil *CarrierAlerts raises every
// failure.
type CarrierAlerts struct {
	mu sync.Mutex
	m  map[string]bool
}

// first records a failure of repo and reports whether it is the first
// since the repo's last success (i.e. whether to alert).
func (a *CarrierAlerts) first(repo string) bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m[repo] {
		return false
	}
	if a.m == nil {
		a.m = map[string]bool{}
	}
	a.m[repo] = true
	return true
}

// clear forgets repo's raised failure after a successful carrier push.
func (a *CarrierAlerts) clear(repo string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.m, repo)
	a.mu.Unlock()
}

// IsPermanentPushRejection reports a push the server answered and refused
// for a reason no retry can change: the pre-receive hook declined it (seq
// trailer, gitlink, blob size limit, session fence) or the ref moved
// non-fast-forward. A transport failure (unreachable, timeout, auth
// against a down proxy) is NOT permanent. Matches git's C-locale output
// as carried in the push error (callers run git under CEnv).
func IsPermanentPushRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "[remote rejected]") ||
		strings.Contains(msg, "pre-receive hook declined") ||
		IsNonFastForward(err, "")
}

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
