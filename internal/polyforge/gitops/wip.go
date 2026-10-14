package gitops

// wip.go: the uncommitted-work lane of the task-repo carrier
// (polyforge#167). The carrier ref refs/pf-task/<name> records a task
// repo's HEAD only, so a re-claim (blocked → resume) restored the
// committed head and silently lost every uncommitted edit of the dead
// session. At the close points where the step process is already gone
// (stopOp's seal, pause, bounded-fail, graceful shutdown) PushTaskWIP
// snapshots a dirty task repo as ONE WIP commit whose single parent is
// HEAD and pushes it to refs/pf-wip/<name> on the WI's bare repo; the
// claim-time restore (runtime.restoreTaskWIP, after
// runtime.restoreTaskRepoState, via FetchTaskWIP) applies that commit to
// the fresh clone's working tree when its parent is the restored head.
//
// Snapshot method: a daemon-owned temporary bare repo (the same
// pf-carrier-* repo the carrier push uses) whose object store borrows the
// task repo's through objects/info/alternates, pointed at the task repo
// as --work-tree, with a temporary index file (GIT_INDEX_FILE inside the
// temp repo): read-tree HEAD, pre-scan (prescanWIP), add -A
// --ignore-errors, prune (pruneWIPIndex), write-tree, commit-tree -p
// HEAD. Nothing of the agent's repo is written — not its HEAD, branch,
// index, config or object store (the new blobs, trees and the commit land
// in the temp repo) — and its config is never loaded as configuration:
// git reads the temp repo's config, so no repo-defined clean filter,
// fsmonitor, hook or credential helper can run. The flip side: no
// clean/smudge filter or attribute-driven conversion applies, the
// snapshot stores RAW working-tree bytes — so files whose attributes name
// the lfs or git-crypt filter are left out (their raw content is the
// smudged object/plaintext).
//
// Ignore and attribute sources the carrier env drops are copied in as
// data (bounded regular files, opened non-blocking): the effective
// core.excludesFile / core.attributesFile (the repo's, else the
// operator's global, else $XDG_CONFIG_HOME/git/ignore|attributes) and the
// repo's .git/info/exclude / .git/info/attributes; .gitignore and
// .gitattributes in the work tree apply as usual.
//
// Left out and reported (commit message + task_wip_skipped), so one bad
// path never loses the whole snapshot: embedded repositories, untracked
// files over the blob limit or above the total cap (excluded BEFORE add —
// never hashed), tracked files over the blob limit (kept at HEAD via
// skip-worktree, never hashed), files git cannot read (add
// --ignore-errors), lfs/git-crypt files. A left-out tracked path keeps
// its HEAD entry, never becomes a deletion.
//
// `git stash create` was rejected: it runs inside the agent repo (its
// config, its object store) and never records untracked files.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
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

// EventTaskWIPSkipped is the event kind the snapshot raises when it left
// paths of a task repo's uncommitted work out of the WIP commit (an
// embedded repository, a filtered LFS/git-crypt file, a blob over the
// carrier limit, the total-size cap): those paths come back at their HEAD
// state (or absent) after a re-claim.
const EventTaskWIPSkipped = "task_wip_skipped"

// wipTotalCap bounds the bytes of changed content one WIP snapshot may
// carry (the largest changed files are left out first above it). A var
// for the tests.
var wipTotalCap int64 = capsule.MaxTaskBlobBytes

// wipBlobCap is the per-blob limit of a snapshot: the hub's carrier blob
// limit (a larger blob would make the whole push refused). A var for the
// tests.
var wipBlobCap int64 = capsule.MaxTaskBlobBytes

// wipSkippedFilters are the clean/smudge filters whose files the snapshot
// leaves out: it stores RAW working-tree content (no filter runs), and
// raw content of these files is the smudged plaintext/object, not what
// the repo commits.
var wipSkippedFilters = map[string]bool{"lfs": true, "git-crypt": true}

// PushTaskWIP runs the WIP lane at a close point where the step process
// has already exited. It first mirrors every task-repo HEAD to its
// carrier ref (pushTaskRefsLocked — so a WIP commit's parent is the head
// the re-claim restores), then, per task repo:
//
//   - keep and uncommitted changes: snapshot them (see the file comment)
//     and force-push the WIP commit to refs/pf-wip/<name>;
//   - otherwise (a clean repo, or keep=false — the wrapped close):
//     delete the hub's WIP ref (a stale snapshot must never be re-applied
//     over newer work; deleting a ref the hub does not hold is a quiet
//     no-op).
//
// A keep snapshot that FAILS leaves the hub's older WIP ref alone: it is
// only ever applied when its parent is the restored head — exactly the
// state that older snapshot was taken on — so applying it is strictly
// better than nothing; carrier_push_failed says the hub's WIP predates
// this close.
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
	for _, s := range srcs {
		ref := TaskWIPRefName(s.name)
		if o.expired() {
			fmt.Fprintf(os.Stderr, "gitops: %s: repo %s: WIP lane skipped: deadline exceeded\n", o.WorkItemID, s.name)
			if keep {
				o.carrierRefFailed(s.name, ref, chainOK, "deadline exceeded before the WIP snapshot (the hub's WIP ref, if any, predates this close)")
			}
			continue
		}
		if keep {
			wip, skipped, out, err := pushTaskWIP(o, env, reposDir, s, additions)
			if len(skipped) > 0 {
				fmt.Fprintf(os.Stderr, "gitops: %s: repo %s: WIP snapshot left out %d path(s): %s\n", o.WorkItemID, s.name, len(skipped), strings.Join(skipped, "; "))
				if o.EventSink != nil {
					o.EventSink(EventTaskWIPSkipped, map[string]any{
						"repo": s.name, "ref": ref, "skipped": EventDetail(strings.Join(skipped, "\n")),
					})
				}
			}
			if err != nil {
				detail := err.Error()
				if o.expired() {
					detail = "deadline exceeded: " + detail
				}
				fmt.Fprintf(os.Stderr, "gitops: %s: WIP push for repo %s failed (its uncommitted work may not survive a re-claim; the hub's WIP ref, if any, predates this close): %s: %s\n", o.WorkItemID, s.name, detail, strings.TrimSpace(out))
				o.carrierRefFailed(s.name, ref, chainOK, detail+" (the hub's WIP ref, if any, predates this close)")
				continue
			}
			if wip != "" {
				o.CarrierAlerts.clear(ref)
				fmt.Fprintf(os.Stderr, "gitops: %s: repo %s: uncommitted work saved as %s on %s (parent %s)\n", o.WorkItemID, s.name, wip, ref, s.sha)
				continue
			}
		}
		dropWIPRef(o, env, ref, additions)
	}
}

// pushTaskWIP snapshots the working tree of one task repo into a WIP
// commit (parent s.sha) inside a temporary repo and force-pushes it to
// the repo's WIP ref. wip "" with a nil error = the (kept part of the)
// working tree matches HEAD: nothing to save, nothing pushed. skipped
// names the paths left out ("path (reason)"), on every return.
func pushTaskWIP(o Opts, env []string, reposDir string, s carrierSrc, credAdditions []string) (wip string, skipped []string, out string, err error) {
	dir := filepath.Join(reposDir, s.name)
	tmp, out, err := carrierTempRepo(o, env, s.objects)
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return "", nil, out, err
	}
	wenv := append(append(append([]string{}, env...), wipIdentity...), "GIT_INDEX_FILE="+filepath.Join(tmp, "pf-wip-index"))
	flags := append(carrierTmpFlags(tmp), "--work-tree="+dir)
	gitw := func(timeout time.Duration, args ...string) (string, error) {
		return gitTimed(o.bound(timeout), wenv, dir, append(append([]string{}, flags...), args...)...)
	}
	if out, err := gitw(carrierCheckTimeout, "read-tree", s.sha); err != nil {
		return "", nil, out, err
	}
	head, out, err := wipIndex(gitw)
	if err != nil {
		return "", nil, out, err
	}
	// The ignore and attribute sources the carrier env drops, as data.
	if err := writeWIPInfo(tmp, "attributes", wipAttributes(o, dir, tmp)); err != nil {
		return "", nil, "", err
	}
	ignore := wipIgnores(o, dir, tmp)
	if err := writeWIPInfo(tmp, "exclude", ignore); err != nil {
		return "", nil, "", err
	}
	// Pre-scan (before anything is hashed): embedded repositories and
	// over-limit untracked files go to the exclude, over-limit tracked
	// files keep their HEAD entry (skip-worktree) — `add -A` never reads
	// a multi-GB file.
	extra, skipped, out, err := prescanWIP(gitw, dir, head)
	if err != nil {
		return "", skipped, out, err
	}
	if len(extra) > 0 {
		if err := writeWIPInfo(tmp, "exclude", ignore+"\n"+strings.Join(extra, "\n")+"\n"); err != nil {
			return "", skipped, "", err
		}
	}
	// add -A hashes every changed or untracked, not-ignored file: it may
	// read a large tree, so it takes the push bound. --ignore-errors: a
	// file git cannot read (root-owned build output, mode 000) is left
	// out — exit 1 is a partial snapshot, not a lost one.
	if out, err := gitw(carrierPushTimeout, "add", "-A", "--ignore-errors"); err != nil {
		if exitCode(err) != 1 {
			return "", skipped, out, err
		}
		skipped = append(skipped, unindexedPaths(err.Error())...)
	}
	more, out, err := pruneWIPIndex(gitw, dir, head)
	skipped = append(skipped, more...)
	if err != nil {
		return "", skipped, out, err
	}
	tree, err := gitw(carrierCheckTimeout, "write-tree")
	if err != nil {
		return "", skipped, tree, err
	}
	tree = strings.TrimSpace(tree)
	base, err := gitw(carrierCheckTimeout, "rev-parse", s.sha+"^{tree}")
	if err != nil {
		return "", skipped, base, err
	}
	if strings.TrimSpace(base) == tree {
		return "", skipped, "", nil // clean: the working tree is HEAD's
	}
	msg := "pf-wip: uncommitted work of repos/" + s.name + " on " + s.sha
	if len(skipped) > 0 {
		list := skipped
		if len(list) > 50 {
			list = append(append([]string{}, list[:50]...), fmt.Sprintf("... and %d more", len(skipped)-50))
		}
		msg += "\n\nLeft out (tracked paths kept at HEAD, untracked absent):\n" + strings.Join(list, "\n")
	}
	wip, err = gitw(carrierCheckTimeout, "commit-tree", tree, "-p", s.sha, "-m", msg)
	if err != nil {
		return "", skipped, wip, err
	}
	wip = strings.TrimSpace(wip)
	if out, err := carrierPush(o, env, tmp, "+"+wip+":"+TaskWIPRefName(s.name), credAdditions); err != nil {
		return "", skipped, out, err
	}
	return wip, skipped, "", nil
}

// indexEntry is one stage-0 index entry.
type indexEntry struct{ mode, sha string }

// wipIndex reads the temporary index (ls-files -s -z): path → entry.
func wipIndex(gitw func(time.Duration, ...string) (string, error)) (map[string]indexEntry, string, error) {
	out, err := gitw(carrierCheckTimeout, "ls-files", "-s", "-z")
	if err != nil {
		return nil, out, err
	}
	m := map[string]indexEntry{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode sha stage
		if len(f) == 3 {
			m[path] = indexEntry{f[0], f[1]}
		}
	}
	return m, "", nil
}

// prescanWIP runs before `add -A` on the temporary index (HEAD's
// entries): `ls-files -o --exclude-standard` (the temp exclude in effect)
// lists the untracked, not-ignored paths — git shows an embedded
// repository as one "dir/" entry and never descends into ignored
// directories. It answers the extra exclude lines (embedded repositories;
// untracked files over the blob cap; above the total cap the largest
// untracked files first) and the left-out paths, and marks every tracked
// regular file whose working copy is over the blob cap skip-worktree so
// `add -A` keeps its HEAD entry without hashing it (recorded when its
// size differs from HEAD's blob).
func prescanWIP(gitw func(time.Duration, ...string) (string, error), dir string, head map[string]indexEntry) (extra, skipped []string, out string, err error) {
	others, err := gitw(carrierPushTimeout, "ls-files", "-o", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, others, err
	}
	type file struct {
		path string
		size int64
	}
	var untracked []file
	var total int64
	for _, p := range strings.Split(others, "\x00") {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			rel := strings.TrimSuffix(p, "/")
			extra = append(extra, "/"+escapeIgnore(rel)+"/")
			skipped = append(skipped, rel+" (embedded git repository)")
			continue
		}
		st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if st.Size() > wipBlobCap {
			extra = append(extra, "/"+escapeIgnore(p))
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes, over the %d-byte blob limit; not read)", p, st.Size(), wipBlobCap))
			continue
		}
		untracked = append(untracked, file{p, st.Size()})
		total += st.Size()
	}
	if total > wipTotalCap {
		sort.SliceStable(untracked, func(a, b int) bool { return untracked[a].size > untracked[b].size })
		for _, f := range untracked {
			if total <= wipTotalCap {
				break
			}
			extra = append(extra, "/"+escapeIgnore(f.path))
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes, left out to keep the snapshot under %d bytes; not read)", f.path, f.size, wipTotalCap))
			total -= f.size
		}
	}
	var pin []string
	paths := make([]string, 0, len(head))
	for p := range head {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		e := head[p]
		if e.mode != "100644" && e.mode != "100755" {
			continue
		}
		st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || !st.Mode().IsRegular() || st.Size() <= wipBlobCap {
			continue
		}
		pin = append(pin, p)
		if hs, err := gitw(carrierCheckTimeout, "cat-file", "-s", e.sha); err != nil || strings.TrimSpace(hs) != strconv.FormatInt(st.Size(), 10) {
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes, over the %d-byte blob limit; kept at HEAD, not read)", p, st.Size(), wipBlobCap))
		}
	}
	for i := 0; i < len(pin); i += 200 {
		if out, err := gitw(carrierCheckTimeout, append([]string{"update-index", "--skip-worktree", "--"}, pin[i:min(i+200, len(pin))]...)...); err != nil {
			return extra, skipped, out, err
		}
	}
	return extra, skipped, "", nil
}

// unindexedPaths answers the paths a `git add --ignore-errors` reported
// it could not index (C-locale "unable to index file '<path>'" lines),
// each marked as left out; one generic entry when none parse.
func unindexedPaths(msg string) []string {
	var out []string
	for _, line := range strings.Split(msg, "\n") {
		i := strings.Index(line, "unable to index file '")
		if i < 0 {
			continue
		}
		p := strings.TrimSuffix(strings.TrimSpace(line[i+len("unable to index file '"):]), "'")
		out = append(out, p+" (unreadable: git could not index it)")
	}
	if len(out) == 0 {
		first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
		out = append(out, "(some paths: git add reported errors: "+first+")")
	}
	return out
}

// pruneWIPIndex leaves out of the staged snapshot the changed entries
// that must not ride it: files whose attributes name a clean/smudge
// filter the snapshot cannot apply (wipSkippedFilters), blobs over the
// carrier blob limit, and — above the total cap — the largest remaining
// changed files first. A left-out path is reset to its HEAD entry (or
// dropped when HEAD has none), so the restore brings it back at HEAD's
// state and never deletes it. Answers the left-out paths.
func pruneWIPIndex(gitw func(time.Duration, ...string) (string, error), dir string, head map[string]indexEntry) ([]string, string, error) {
	idx, out, err := wipIndex(gitw)
	if err != nil {
		return nil, out, err
	}
	type changed struct {
		path string
		size int64
	}
	var paths []string
	for p, e := range idx {
		if h, ok := head[p]; !ok || h != e {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, "", nil
	}
	filters := map[string]string{}
	for i := 0; i < len(paths); i += 200 {
		chunk := paths[i:min(i+200, len(paths))]
		out, err := gitw(carrierCheckTimeout, append([]string{"check-attr", "-z", "filter", "--"}, chunk...)...)
		if err != nil {
			return nil, out, err
		}
		f := strings.Split(out, "\x00")
		for j := 0; j+2 < len(f); j += 3 {
			filters[f[j]] = f[j+2]
		}
	}
	reasons := map[string]string{}
	var kept []changed
	var total int64
	for _, p := range paths {
		if v := filters[p]; wipSkippedFilters[v] {
			reasons[p] = "filter=" + v + " not applied by the snapshot"
			continue
		}
		var size int64
		if st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p))); err == nil && st.Mode().IsRegular() {
			size = st.Size()
		}
		if size > wipBlobCap {
			reasons[p] = fmt.Sprintf("%d bytes, over the %d-byte blob limit", size, wipBlobCap)
			continue
		}
		kept = append(kept, changed{p, size})
		total += size
	}
	if total > wipTotalCap {
		sort.SliceStable(kept, func(a, b int) bool { return kept[a].size > kept[b].size })
		for _, c := range kept {
			if total <= wipTotalCap {
				break
			}
			reasons[c.path] = fmt.Sprintf("%d bytes, left out to keep the snapshot under %d bytes", c.size, wipTotalCap)
			total -= c.size
		}
	}
	if len(reasons) == 0 {
		return nil, "", nil
	}
	var skipped, revert, remove []string
	for _, p := range paths {
		r, ok := reasons[p]
		if !ok {
			continue
		}
		skipped = append(skipped, p+" ("+r+")")
		if h, ok := head[p]; ok {
			revert = append(revert, "--cacheinfo", h.mode+","+h.sha+","+p)
		} else {
			remove = append(remove, p)
		}
	}
	for i := 0; i < len(revert); i += 400 {
		if out, err := gitw(carrierCheckTimeout, append([]string{"update-index"}, revert[i:min(i+400, len(revert))]...)...); err != nil {
			return skipped, out, err
		}
	}
	for i := 0; i < len(remove); i += 200 {
		if out, err := gitw(carrierCheckTimeout, append([]string{"update-index", "--force-remove", "--"}, remove[i:min(i+200, len(remove))]...)...); err != nil {
			return skipped, out, err
		}
	}
	return skipped, "", nil
}

// wipExcludeMax bounds each ignore/attribute source the snapshot copies.
const wipExcludeMax = 1 << 20

// wipIgnores answers the ignore sources the carrier env would otherwise
// drop, as one exclude file: the excludes file git would use (the task
// repo's own core.excludesFile, else the operator's global one, else
// $XDG_CONFIG_HOME/git/ignore or ~/.config/git/ignore), then the repo's
// .git/info/exclude.
func wipIgnores(o Opts, dir, tmp string) string {
	return readIgnoreSource(effectiveInfoFile(o, dir, tmp, "core.excludesFile", "ignore"), true) + "\n" +
		readIgnoreSource(filepath.Join(dir, ".git", "info", "exclude"), false) + "\n"
}

// wipAttributes is wipIgnores for attributes (core.attributesFile, else
// $XDG_CONFIG_HOME/git/attributes, then .git/info/attributes) — the
// snapshot reads them only to leave filtered (lfs, git-crypt) files out.
func wipAttributes(o Opts, dir, tmp string) string {
	return readIgnoreSource(effectiveInfoFile(o, dir, tmp, "core.attributesFile", "attributes"), true) + "\n" +
		readIgnoreSource(filepath.Join(dir, ".git", "info", "attributes"), false) + "\n"
}

// writeWIPInfo writes the temporary repo's info/<name>.
func writeWIPInfo(tmp, name, content string) error {
	if err := os.MkdirAll(filepath.Join(tmp, "info"), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(tmp, "info", name), []byte(content), 0o600)
}

// effectiveInfoFile answers the file git would use for key
// (core.excludesFile / core.attributesFile) in the task repo: the repo
// config's value (read as a file — no includes), else the operator's
// global value, else $XDG_CONFIG_HOME/git/<xdgName>. Relative values are
// taken against the work tree.
func effectiveInfoFile(o Opts, dir, tmp, key, xdgName string) string {
	// the daemon's own environment minus GIT_* — except the operator's
	// GIT_CONFIG_GLOBAL, which names the global config to read
	plain := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL=") {
			plain = append(plain, kv)
		}
	}
	get := func(args ...string) string {
		out, err := gitTimed(o.bound(carrierCheckTimeout), plain, tmp, append([]string{"config", "--type=path", "--get"}, args...)...)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	v := ""
	if repoCfg := filepath.Join(dir, ".git", "config"); regularFile(repoCfg) {
		v = get("--file", repoCfg, key)
	}
	if v == "" {
		v = get("--global", key)
	}
	if v == "" {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			xdg = filepath.Join(home, ".config")
		}
		return filepath.Join(xdg, "git", xdgName)
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(dir, v)
	}
	return v
}

// regularFile reports whether p is a regular file (no symlink).
func regularFile(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular()
}

// readIgnoreSource reads one ignore/attribute file as data: a regular
// file (when follow, the path may be a symlink to one — an operator's
// dotfiles often are; otherwise never through a symlink), opened
// non-blocking (a FIFO swapped in cannot hang the close) and read through
// a limit of wipExcludeMax bytes; "" otherwise (or when larger).
func readIgnoreSource(p string, follow bool) string {
	if p == "" {
		return ""
	}
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !follow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(p, flags, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(f, wipExcludeMax+1))
	if err != nil || len(b) > wipExcludeMax {
		return ""
	}
	return string(b)
}

// escapeIgnore escapes a literal path for a gitignore pattern.
func escapeIgnore(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\*?[ !#`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dropWIPRef deletes ref on the hub (a push of the empty source from an
// empty temporary repo). A ref the hub does not hold is the normal case
// and stays quiet; any other failure is logged.
func dropWIPRef(o Opts, env []string, ref string, credAdditions []string) {
	tmp, out, err := carrierTempRepo(o, env, "")
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err == nil {
		out, err = carrierPush(o, env, tmp, ":"+ref, credAdditions)
	}
	if err == nil {
		return
	}
	if strings.Contains(out+" "+err.Error(), "remote ref does not exist") {
		return
	}
	fmt.Fprintf(os.Stderr, "gitops: %s: deleting %s failed (a stale WIP may remain): %v: %s\n", o.WorkItemID, ref, err, strings.TrimSpace(out))
}

// FetchTaskWIP reads the WIP lane from the claim side: it fetches
// refs/pf-wip/<name> into the task worktree at dir (a fresh daemon-made
// clone, like FetchTaskRef) and answers the WIP commit SHA; ("", nil)
// when the hub holds no WIP for this repo.
func FetchTaskWIP(o Opts, dir, name string) (string, error) {
	return fetchCarrierRef(o, dir, TaskWIPRefName(name))
}
