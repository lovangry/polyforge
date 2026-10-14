package gitops

// wip.go: the uncommitted-work lane of the task-repo carrier
// (polyforge#167). The carrier ref refs/pf-task/<name> records a task
// repo's HEAD only, so a re-claim (blocked → resume) restored the
// committed head and silently lost every uncommitted edit of the dead
// session. At the close points where the step process is already gone
// (seal, pause, bounded-fail, graceful shutdown) PushTaskWIP snapshots a
// dirty task repo as ONE WIP commit whose single parent is HEAD and
// pushes it to refs/pf-wip/<name> on the WI's bare repo; the claim-time
// restore (runtime.restoreTaskRepoState, via FetchTaskWIP) checks that
// commit's tree out into the fresh clone's working tree when its parent is
// the restored head.
//
// Snapshot method: a daemon-owned temporary bare repo (the same
// pf-carrier-* repo the carrier push uses) whose object store borrows the
// task repo's through objects/info/alternates, pointed at the task repo
// as --work-tree, with a temporary index file (GIT_INDEX_FILE inside the
// temp repo): read-tree HEAD, add -A, write-tree, commit-tree -p HEAD.
// Nothing of the agent's repo is written — not its HEAD, branch, index,
// config or object store (the new blobs, trees and the commit land in the
// temp repo) — and nothing of it is READ as configuration: git reads the
// temp repo's config, so no repo-defined clean filter, fsmonitor, hook or
// credential helper can run. .gitignore files in the working tree are
// honoured (data, not config); the repo's .git/info/exclude is copied
// into the temp repo. `git stash create` was rejected: it runs inside the
// agent repo (its config, its object store) and never records untracked
// files.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TaskWIPNamespace is the ref namespace, inside the work item's own bare
// repo, carrying each task repo's uncommitted work across a re-claim
// (polyforge#167). Unlike refs/pf-task/* it is scratch: every close point
// supersedes it (force) or deletes it, and aihub's pre-receive hook
// accepts deletion and non-fast-forward updates for it (blob limit:
// capsule.MaxTaskBlobBytes, as for the carrier).
const TaskWIPNamespace = "refs/pf-wip/"

// TaskWIPRefName answers the WIP ref for one task repo.
func TaskWIPRefName(repo string) string { return TaskWIPNamespace + repo }

// EventTaskWIPNotApplied is the event kind the claim-time restore raises
// when a WIP ref exists but is not applied (its parent is not the
// restored head, or it cannot be read).
const EventTaskWIPNotApplied = "task_wip_not_applied"

// wipIdentity is the author/committer of every WIP commit (the temp repo
// has no config to take one from).
var wipIdentity = []string{
	"GIT_AUTHOR_NAME=polyforge-daemon",
	"GIT_AUTHOR_EMAIL=polyforge-daemon@localhost",
	"GIT_COMMITTER_NAME=polyforge-daemon",
	"GIT_COMMITTER_EMAIL=polyforge-daemon@localhost",
}

// PushTaskWIP runs the WIP lane at a close point where the step process
// has already exited. It first mirrors every task-repo HEAD to its
// carrier ref (pushTaskRefsLocked — so a WIP commit's parent is the head
// the re-claim restores), then, per task repo:
//
//   - keep and uncommitted changes: snapshot them (see the file comment)
//     and force-push the WIP commit to refs/pf-wip/<name>;
//   - otherwise (a clean repo, or keep=false — the wrapped close): delete
//     a WIP ref the hub still holds (a stale snapshot must never be
//     re-applied over newer work).
//
// Best-effort-loud like the carrier lane: a failure is logged and raised
// as carrier_push_failed (when chainOK) and never fails the close. An
// unborn task HEAD has nothing to anchor a WIP commit to and is skipped.
// Takes the per-WI lock.
func PushTaskWIP(o Opts, keep, chainOK bool) {
	if o.RemoteURL == "" {
		return
	}
	release := Lock(o.WorkItemID)
	defer release()
	reposDir, srcs := pushTaskRefsLocked(o, chainOK)
	if len(srcs) == 0 {
		return
	}
	additions, cleanup, err := credentialEnv(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gitops: %s: task WIP lane skipped (credential prep failed): %v\n", o.WorkItemID, err)
		return
	}
	defer cleanup()
	env := carrierEnv(reposDir)
	existing, lerr := listWIPRefs(o, env, additions)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "gitops: %s: listing the hub's WIP refs failed (stale WIP refs are left for the next close): %v\n", o.WorkItemID, lerr)
	}
	for _, s := range srcs {
		ref := TaskWIPRefName(s.name)
		if keep {
			wip, out, err := pushTaskWIP(o, env, reposDir, s, additions)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gitops: %s: WIP push for repo %s failed (its uncommitted work may not survive a re-claim): %v: %s\n", o.WorkItemID, s.name, err, strings.TrimSpace(out))
				o.carrierRefFailed(s.name, ref, chainOK, err.Error())
				continue
			}
			if wip != "" {
				o.CarrierAlerts.clear(ref)
				fmt.Fprintf(os.Stderr, "gitops: %s: repo %s: uncommitted work saved as %s on %s (parent %s)\n", o.WorkItemID, s.name, wip, ref, s.sha)
				continue
			}
		}
		if !existing[ref] {
			continue
		}
		if out, err := deleteWIPRef(o, env, ref, additions); err != nil {
			fmt.Fprintf(os.Stderr, "gitops: %s: deleting the stale %s failed: %v: %s\n", o.WorkItemID, ref, err, strings.TrimSpace(out))
		}
	}
}

// pushTaskWIP snapshots the working tree of one task repo into a WIP
// commit (parent s.sha) inside a temporary repo and force-pushes it to
// the repo's WIP ref. ("", "", nil) = the working tree matches HEAD:
// nothing to save, nothing pushed.
func pushTaskWIP(o Opts, env []string, reposDir string, s carrierSrc, credAdditions []string) (string, string, error) {
	dir := filepath.Join(reposDir, s.name)
	tmp, out, err := carrierTempRepo(o, env, s.objects)
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return "", out, err
	}
	copyInfoExclude(filepath.Join(dir, ".git"), tmp)
	wenv := append(append(append([]string{}, env...), wipIdentity...), "GIT_INDEX_FILE="+filepath.Join(tmp, "pf-wip-index"))
	flags := append(carrierTmpFlags(tmp), "--work-tree="+dir)
	gitw := func(timeout time.Duration, args ...string) (string, error) {
		return gitTimed(timeout, wenv, dir, append(append([]string{}, flags...), args...)...)
	}
	if out, err := gitw(carrierCheckTimeout, "read-tree", s.sha); err != nil {
		return "", out, err
	}
	// add -A hashes every changed or untracked, not-ignored file: it may
	// read a large tree, so it takes the push bound.
	if out, err := gitw(carrierPushTimeout, "add", "-A"); err != nil {
		return "", out, err
	}
	tree, err := gitw(carrierCheckTimeout, "write-tree")
	if err != nil {
		return "", tree, err
	}
	tree = strings.TrimSpace(tree)
	base, err := gitw(carrierCheckTimeout, "rev-parse", s.sha+"^{tree}")
	if err != nil {
		return "", base, err
	}
	if strings.TrimSpace(base) == tree {
		return "", "", nil // clean: the working tree is HEAD's
	}
	wip, err := gitw(carrierCheckTimeout, "commit-tree", tree, "-p", s.sha, "-m",
		"pf-wip: uncommitted work of repos/"+s.name+" on "+s.sha)
	if err != nil {
		return "", wip, err
	}
	wip = strings.TrimSpace(wip)
	if out, err := carrierPush(o, env, tmp, "+"+wip+":"+TaskWIPRefName(s.name), credAdditions); err != nil {
		return "", out, err
	}
	return wip, "", nil
}

// copyInfoExclude copies the task repo's .git/info/exclude (a regular
// file only — never through a symlink) into the temporary repo, so the
// snapshot honours the same ignore rules the agent saw. Best-effort.
func copyInfoExclude(gitDir, tmp string) {
	src := filepath.Join(gitDir, "info", "exclude")
	st, err := os.Lstat(src)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Join(tmp, "info"), 0o700) == nil {
		_ = os.WriteFile(filepath.Join(tmp, "info", "exclude"), b, 0o600)
	}
}

// listWIPRefs answers the WIP refs the hub holds (ls-remote from an empty
// temporary repo, the carrier flags and env).
func listWIPRefs(o Opts, env, credAdditions []string) (map[string]bool, error) {
	tmp, out, err := carrierTempRepo(o, env, "")
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	args := append(carrierTmpFlags(tmp), carrierProtocolFlags(o)...)
	args = append(args, "ls-remote", o.RemoteURL, TaskWIPNamespace+"*")
	out, err = gitTimed(carrierPushTimeout, append(append([]string{}, env...), credAdditions...), tmp, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			refs[ref] = true
		}
	}
	return refs, nil
}

// deleteWIPRef deletes ref on the hub (a push of the empty source from an
// empty temporary repo).
func deleteWIPRef(o Opts, env []string, ref string, credAdditions []string) (string, error) {
	tmp, out, err := carrierTempRepo(o, env, "")
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return out, err
	}
	return carrierPush(o, env, tmp, ":"+ref, credAdditions)
}

// FetchTaskWIP reads the WIP lane from the claim side: it fetches
// refs/pf-wip/<name> into the task worktree at dir (a fresh daemon-made
// clone, like FetchTaskRef) and answers the WIP commit SHA; ("", nil)
// when the hub holds no WIP for this repo.
func FetchTaskWIP(o Opts, dir, name string) (string, error) {
	return fetchCarrierRef(o, dir, TaskWIPRefName(name))
}
