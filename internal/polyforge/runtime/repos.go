package runtime

// repos.go: the task-repo half of the v0 claim contract, restored for v1
// (真机 pilot gap DAEMON-507 lane). Claim answers the project's task-repo
// inventory (api.Claim.Repos, served by aihub from the project row);
// materialize turns it into real git clones under the capsule's
// repos/<name> BEFORE the agent spawns, and capsule.json records each
// clone's claim-time HEAD SHA as the baseline (name → SHA). The agent
// works inside repos/<name>; the daemon's ship bridge later pushes those
// worktrees back to their own origins — this file is the "incoming"
// half only.
//
// Credentials: task repos are ordinary remotes (GitHub, an internal
// git host, a local file:// path). Cloning uses the machine's default git
// credential context — the daemon process env, nothing injected — and
// happens strictly before the agent starts, so the agent's environment
// never carries any task-repo credential (only the attempt-scoped capsule
// push credential does, via the gitops choke point).
//
// Recovery contract: a repos/<name> that already exists at materialize
// time (a same-generation husk from a crashed Start, or an in-place
// materialize rerun) is NEVER fetched, reset or re-cloned — the agent's
// uncommitted edits in that worktree are the live scene a later
// commit-dirty/ship path needs. Existence is verified, the recorded
// baseline is carried forward, nothing else is touched.
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
	"strings"

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
	if len(j.repos) == 0 {
		return out, nil
	}
	// The prior capsule.json (same-generation husk) is read BEFORE this
	// materialize overwrites it: recorded baselines are claim-time facts
	// and must survive a re-materialize even if the agent has since
	// committed (moved HEAD).
	prior := readCapsuleRepos(j.capsuleDir)
	for _, pr := range j.repos {
		if !safeRepoName(pr.Name) {
			return nil, repoCloneErr(j.wi, pr, "repo name %q is not a single safe path element", pr.Name)
		}
		dir := filepath.Join(j.capsuleDir, capsule.DirRepos, pr.Name)
		if dirExists(dir) {
			// Resume/recovery shape: presence is the check; the baseline
			// is the recorded one, falling back to the worktree's HEAD
			// only when no prior record exists (a torn first Start).
			if sha, ok := prior[pr.Name]; ok && sha != "" {
				out[pr.Name] = sha
				continue
			}
			sha, err := gitHeadSHA(ctx, dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: repo %s: HEAD unreadable in existing worktree: %v\n", j.wi, pr.Name, err)
			}
			out[pr.Name] = sha
			continue
		}
		if err := gitClone(ctx, pr.URL, dir); err != nil {
			return nil, repoCloneErr(j.wi, pr, "clone failed: %v", err)
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
		// clone is moved there — the recorded baseline then names the
		// scene the workflow actually left behind, not the origin's
		// default branch. ("", nil) = no carrier (first claim). A carrier
		// that exists but cannot be restored FAILS the claim (typed
		// DAEMON-507, the WI closes blocked, the carrier stays safe on
		// aihub) and the half-restored clone is removed so a retry
		// cannot mistake it for a recovery husk.
		restored, rerr := r.restoreTaskRepoState(ctx, j, pr.Name, dir)
		if rerr != nil {
			_ = os.RemoveAll(dir)
			return nil, repoCloneErr(j.wi, pr, "task_repo_restore_failed: %v (the WI's commits are safe on %s at aihub; fix the cause and resume)", rerr, gitops.TaskRefName(pr.Name))
		}
		if restored != "" {
			sha = restored
		}
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
// clone sourced from a detached-HEAD checkout (locally sourced clones,
// w079) has none, so the carrier tip lands on the deterministic branch
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

// carrierFallbackBranch names the local branch a carrier is restored onto
// when the fresh clone has no checked-out branch (detached-HEAD source).
const carrierFallbackBranch = "pf-carrier"

// gitTask runs one git command inside a task-repo worktree: the
// worktree's OWN git-dir (never the capsule's external GIT_DIR binding —
// the capsule history holds none of these objects), the machine's
// default credential context, ctx-bounded like the other task-repo git
// calls of this file.
func gitTask(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = os.Environ()
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
// the machine's default git credential context, nothing injected.
func gitClone(ctx context.Context, url, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", url, dir)
	cmd.Env = os.Environ()
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
