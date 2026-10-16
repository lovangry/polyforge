package runtime

// repos.go: the task-repo half of the v0 claim contract, restored for v1
// (真机 pilot gap DAEMON-507 lane). Claim answers the task-repo inventory
// (api.Claim.Repos, served by aihub: the WI's declared repo_inventory,
// else every repo of its project — 02 §4.6);
// materialize turns it into real git clones under the capsule's
// repos/<name> BEFORE the agent spawns, and capsule.json records each
// clone's claim-time HEAD SHA as the baseline (name → SHA). The agent
// works inside repos/<name>; the workflows' deliver step later pushes the
// feature branch back to its origin (polyforge#166) — this file is the
// "incoming" half only.
//
// Credentials: task repos are ordinary remotes (GitHub, an internal
// git host, a local file:// path). Cloning uses the machine's default git
// credential context — the daemon process env, nothing injected — and
// happens strictly before the agent starts, so the agent's environment
// never carries any task-repo credential (only the attempt-scoped capsule
// push credential does, via the gitops choke point).
//
// Recovery contract: a repos/<name> that already exists at materialize
// time AND has a baseline recorded in the prior capsule.json (a
// same-generation husk from a crashed Start, or an in-place materialize
// rerun) is NEVER fetched, reset or re-cloned — the agent's uncommitted
// edits in that worktree are the live scene a later commit-dirty/deliver
// path needs. A directory with NO recorded baseline is the crash window
// between the clone and the capsule.json write: an untouched fresh
// clone, which takes the carrier restore like any new clone.
//
// Durability contract (polyforge#108): a repos/<name> that does NOT
// exist at materialize time — the bounded-fail re-claim shape, where
// quarantine moved the whole scene away — is cloned from the bare
// origin, which knows nothing of the previous session's commits. When
// the previous session published a head to the WI's carrier ref
// (refs/pf-task/<name>, written by every capsule publish via the gitops
// lane), the fresh clone is MOVED there before the baseline is
// recorded: the workflow resumes on the tree it actually left behind,
// and capsule.json names the true scene, not the origin's default
// branch. The move is non-destructive by construction — it only ever
// runs on a just-created clone with no local state.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// cloneTaskRepos materializes the claim's task-repo inventory into
// repos/<name> and answers the capsule.json Repos map (repo name →
// claim-time baseline SHA). Fresh names are cloned from their URL; names
// whose directory already exists take the non-destructive path (see the
// recovery contract above). Any clone failure is the typed DAEMON-507
// error — the caller closes the attempt as blocked, never a daemon crash.
func (r *Runtime) cloneTaskRepos(ctx context.Context, j *job) (map[string]string, error) {
	out := map[string]string{}
	// The prior capsule.json (same-generation husk) is read BEFORE this
	// materialize overwrites it: recorded baselines are claim-time facts
	// and must survive a re-materialize even if the agent has since
	// committed (moved HEAD).
	prior := readCapsuleRepos(j.capsuleDir)
	// The required set is the claim inventory UNION the repos the prior
	// capsule.json recorded: an inventory that went empty (or lost a
	// repo) must not silently drop a task repo the WI already works in.
	repos := append([]api.ProjectRepo(nil), j.repos...)
	inInventory := map[string]bool{}
	for _, pr := range repos {
		inInventory[pr.Name] = true
	}
	var recorded []string
	for name := range prior {
		if !inInventory[name] {
			recorded = append(recorded, name)
		}
	}
	sort.Strings(recorded)
	for _, name := range recorded {
		repos = append(repos, api.ProjectRepo{Name: name})
	}
	for _, pr := range repos {
		if !safeRepoName(pr.Name) {
			return nil, repoCloneErr(j.wi, pr, "repo name %q is not a single safe path element", pr.Name)
		}
		dir := filepath.Join(j.capsuleDir, capsule.DirRepos, pr.Name)
		exists := dirExists(dir)
		if exists {
			// Resume/recovery shape: presence is the check; a recorded
			// baseline is carried forward untouched.
			// "Recorded" means the key is present, even with an empty value:
			// a repo that was empty at claim time baselines "" and its
			// directory may hold agent edits that must never be restored over
			// or removed.
			if sha, ok := prior[pr.Name]; ok {
				out[pr.Name] = sha
				continue
			}
			// No recorded key: the previous Start died between the
			// clone and the capsule.json write, so the directory is an
			// untouched fresh clone (the agent never ran). It takes the
			// same carrier restore as a new clone, instead of adopting
			// the origin's HEAD as the baseline.
		} else {
			if pr.URL == "" {
				return nil, repoCloneErr(j.wi, pr, "recorded in the prior capsule.json but absent from the claim inventory and missing on disk: no URL to clone from")
			}
			if err := gitClone(ctx, pr.URL, dir); err != nil {
				return nil, repoCloneErr(j.wi, pr, "clone failed: %v", err)
			}
		}
		// A clone of a history-less remote succeeds with no HEAD: the
		// baseline is then empty, which is honest — there is no commit
		// to stand on.
		sha, err := gitHeadSHA(ctx, dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: repo %s: fresh clone has no HEAD (empty remote?): %v\n", j.wi, pr.Name, err)
		}
		// Durability lane (polyforge#108): when a previous session
		// published its head to the WI's carrier ref, the fresh origin
		// clone is moved there. ("", nil) = no carrier (first claim). A
		// carrier that exists but cannot be restored FAILS the claim
		// (typed DAEMON-507, the WI closes blocked, the carrier stays
		// safe on aihub) and the half-restored clone is removed so a
		// retry re-clones it cleanly.
		restored, rerr := r.restoreTaskRepoState(ctx, j, pr.Name, dir)
		if rerr != nil {
			_ = os.RemoveAll(dir)
			return nil, repoCloneErr(j.wi, pr, "task_repo_restore_failed: %v (the WI's commits are safe on %s at aihub; fix the cause and resume)", rerr, gitops.TaskRefName(pr.Name))
		}
		if restored != "" {
			sha = restored
		}
		// The previous session's uncommitted work (polyforge#167):
		// checked out on top of the restored head, never committed.
		r.restoreTaskWIP(ctx, j, pr.Name, dir, sha)
		out[pr.Name] = sha
	}
	return out, nil
}

// restoreTaskRepoState moves a FRESH task-repo clone to the head the
// previous session left in the WI's carrier ref (gitops.TaskRefName):
// fetch the ref, point a local branch at it, sync index and worktree.
// ("", nil) means "nothing to restore" — only the first-claim shape, no
// carrier recorded. Every other outcome is either the restored SHA or an
// error: a repo that HAS a carrier must never proceed on a clone lacking
// the WI's commits (principle: never silently lose or rebuild work), and
// a carrier that cannot be read is not evidence there is none.
//
// The branch is the clone's checked-out branch when HEAD is symbolic; a
// clone sourced from a detached-HEAD checkout (a remote whose HEAD is detached)
// has none, so the carrier tip lands on the deterministic branch
// carrierFallbackBranch. Non-destructive by construction: the worktree
// is a just-created clone with no local state.
func (r *Runtime) restoreTaskRepoState(ctx context.Context, j *job, name, dir string) (string, error) {
	ref := gitops.TaskRefName(name)
	sha, err := gitops.FetchTaskRef(j.git, dir, name)
	if err != nil {
		return "", fmt.Errorf("carrier %s fetch failed: %w", ref, err)
	}
	if sha == "" {
		return "", nil // no carrier recorded: first claim, nothing to restore
	}
	if cur, err := gitHeadSHA(ctx, dir); err == nil && cur == sha {
		return sha, nil // the origin clone already stands on the carried head
	}
	if out, err := gitTask(ctx, dir, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return "", fmt.Errorf("carrier %s names %s, which is not a readable commit: %v: %s", ref, sha, err, strings.TrimSpace(out))
	}
	branch := carrierFallbackBranch
	if b, err := gitTask(ctx, dir, "symbolic-ref", "--short", "HEAD"); err == nil && strings.TrimSpace(b) != "" {
		branch = strings.TrimSpace(b)
	}
	// checkout --force -B: attaches HEAD to the branch (a detached or
	// unborn HEAD included) and syncs index+worktree to it.
	if out, err := gitTask(ctx, dir, "checkout", "--force", "--quiet", "-B", branch, sha); err != nil {
		return "", fmt.Errorf("carrier %s restore failed (checkout %s): %v: %s", ref, branch, err, strings.TrimSpace(out))
	}
	if cur, err := gitHeadSHA(ctx, dir); err != nil || cur != sha {
		return "", fmt.Errorf("carrier %s restore failed: HEAD is %q after restore, want %s (%v)", ref, cur, sha, err)
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: repo %s: restored the previous session's head %s from the capsule carrier ref (branch %s)\n", j.wi, name, sha, branch)
	return sha, nil
}

// restoreTaskWIP applies the previous session's uncommitted work
// (gitops.TaskWIPRefName, polyforge#167) to a FRESH task-repo clone whose
// HEAD is head: when the WIP commit's single parent is head, git applies
// the head→wip change to the working tree through a temporary copy of the
// index (checkoutWIPTree: read-tree -m -u) — so HEAD, the branch and the
// repo's own index stay on head and the work shows up as uncommitted
// changes, as the step left it (staged and unstaged edits both come back
// unstaged). A WIP on
// another parent (the carrier moved past it, or the restore fell back)
// is NOT applied: task_wip_not_applied is raised and the ref is left for
// the next close to supersede. Best-effort by design — the committed head
// is already restored and a WIP that cannot be read must not block the
// claim; every failure is loud (stderr + the event). The ref also stays
// after an apply: the next close supersedes or deletes it, and
// re-applying it after a crash before that close is the same restore.
func (r *Runtime) restoreTaskWIP(ctx context.Context, j *job, name, dir, head string) {
	ref := gitops.TaskWIPRefName(name)
	notApplied := func(reason string, extra map[string]any) {
		fmt.Fprintf(os.Stderr, "runtime: %s: repo %s: %s not applied: %s\n", j.wi, name, ref, reason)
		if j.git.EventSink == nil {
			return
		}
		body := map[string]any{"repo": name, "ref": ref, "head": head, "reason": gitops.EventDetail(reason)}
		for k, v := range extra {
			body[k] = v
		}
		j.git.EventSink(gitops.EventTaskWIPNotApplied, body)
	}
	wip, err := gitops.FetchTaskWIP(j.git, dir, name)
	if err != nil {
		notApplied("fetch failed: "+err.Error(), nil)
		return
	}
	if wip == "" {
		return // no uncommitted work was saved
	}
	out, err := gitTask(ctx, dir, "rev-list", "--parents", "-n", "1", wip)
	if err != nil {
		notApplied("unreadable WIP commit: "+err.Error(), map[string]any{"wip": wip})
		return
	}
	fields := strings.Fields(out)
	parent := ""
	if len(fields) > 1 {
		parent = strings.Join(fields[1:], " ")
	}
	if len(fields) != 2 || head == "" || parent != head {
		notApplied(fmt.Sprintf("its parent %q is not the restored head %q", parent, head), map[string]any{"wip": wip, "wip_parent": parent})
		return
	}
	if err := checkoutWIPTree(ctx, dir, head, wip); err != nil {
		notApplied("checkout failed: "+err.Error(), map[string]any{"wip": wip})
		return
	}
	fmt.Fprintf(os.Stderr, "runtime: %s: repo %s: restored the previous session's uncommitted work from %s (%s) onto %s\n", j.wi, name, ref, wip, head)
}

// checkoutWIPTree applies wip on top of head to dir's working tree
// without touching HEAD or the repo's index: a COPY of the fresh clone's
// index (which stands on head) becomes a temporary GIT_INDEX_FILE and git
// itself carries the change over — `read-tree -m -u <head> <wip>` (a
// two-tree fast-forward of the work tree). Git applies deletions, modes
// and a directory turned symlink without ever following a symlink out of
// the work tree; no file is removed by hand (a hand-written removal loop
// once followed a dir→symlink change out of the repo).
func checkoutWIPTree(ctx context.Context, dir, head, wip string) error {
	gitDir, err := gitTask(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "pf-wip-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	index := filepath.Join(tmp, "index")
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(gitDir), "index"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(index, b, 0o600); err != nil {
		return err
	}
	env := append(taskGitEnv(), "GIT_INDEX_FILE="+index)
	_, err = gitTaskEnv(ctx, env, dir, "read-tree", "-m", "-u", head, wip)
	return err
}

// carrierFallbackBranch names the local branch a carrier is restored onto
// when the fresh clone has no checked-out branch (detached-HEAD source).
const carrierFallbackBranch = "pf-carrier"

// gitTask runs one git command inside a task-repo worktree: the
// worktree's OWN git-dir (never the capsule's external GIT_DIR binding —
// the capsule history holds none of these objects), the machine's
// default credential context, ctx-bounded like the other task-repo git
// calls of this file.
func gitTask(ctx context.Context, dir string, args ...string) (string, error) {
	return gitTaskEnv(ctx, taskGitEnv(), dir, args...)
}

// taskGitEnv is the environment of every task-repo git call: the daemon's
// own environ (the machine's default credential context) made
// non-interactive (repo review 2026-10-16 B-8). The claim lane runs
// them outside the runtime's opMu, but a credential prompt on the
// daemon's tmux tty, or a stalled transfer, would still pin the claim
// (and its slot) until a stop or a shutdown cancels it. So: git's
// own credential prompt is off (fail instead), and an HTTP transfer below
// 1 KB/s for 60 s aborts. An operator's explicit setting of either
// variable wins.
func taskGitEnv() []string {
	env := os.Environ()
	for _, kv := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_HTTP_LOW_SPEED_LIMIT=1000",
		"GIT_HTTP_LOW_SPEED_TIME=60",
	} {
		key := kv[:strings.IndexByte(kv, '=')+1]
		set := false
		for _, e := range env {
			if strings.HasPrefix(e, key) {
				set = true
				break
			}
		}
		if !set {
			env = append(env, kv)
		}
	}
	return env
}

// gitTaskEnv is gitTask with an explicit environment.
func gitTaskEnv(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
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

// safeRepoName keeps the inventory-served name inside repos/: one path
// element, no separators, no dot-escape, no control bytes.
func safeRepoName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// repoCloneErr builds the typed DAEMON-507 claim-lane failure.
func repoCloneErr(wi string, pr api.ProjectRepo, format string, args ...any) *api.Error {
	return &api.Error{
		Code:   api.ErrDaemonSpawnFail,
		Detail: fmt.Sprintf("work item %s: task repo %s (%s): "+format, append([]any{wi, pr.Name, pr.URL}, args...)...),
	}
}

// gitClone runs a plain full clone with the daemon's own environment:
// the machine's default git credential context, made non-interactive
// (taskGitEnv).
//
// A cancelled ctx (a stop or a shutdown during the claim, repo review
// 2026-10-16 B-8) kills the whole process group — git hands the transfer
// to a remote helper that would otherwise keep running and keep the
// output pipe open.
func gitClone(ctx context.Context, url, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--", url, dir)
	cmd.Env = taskGitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// gitHeadSHA answers the worktree's HEAD SHA ("" when it has none).
func gitHeadSHA(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readCapsuleRepos loads the Repos map of the capsule.json currently on
// disk (nil when absent) — the prior baselines of a re-materialized
// capsule.
func readCapsuleRepos(capsuleDir string) map[string]string {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.CapsuleJSON))
	if err != nil {
		return nil
	}
	var idn capsule.Identity
	if json.Unmarshal(raw, &idn) != nil {
		return nil
	}
	return idn.Repos
}

// gitEventSink binds the durable EventTap to one WI for the gitops lanes
// (task_ref_diverged, carrier_push_failed); nil when no tap is
// configured.
func (r *Runtime) gitEventSink(wi string) func(kind string, body map[string]any) {
	tap := r.cfg.EventTap
	if tap == nil {
		return nil
	}
	return func(kind string, body map[string]any) { tap(wi, kind, body) }
}
