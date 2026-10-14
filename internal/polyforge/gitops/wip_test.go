package gitops

// wip_test.go: the task-repo WIP lane (polyforge#167) against real git —
// PushTaskWIP snapshots uncommitted work to refs/pf-wip/<name> from a
// daemon-owned temp repo without touching the agent's repo, deletes a
// stale WIP when the tree is clean (or the close is wrapped), and never
// runs anything the agent-controlled repo config names.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// wipEnv: a hub bare (no hook) and a capsule with repos/demo-src at one
// work commit. Answers the opts, the repo dir and the work head.
func wipEnv(t *testing.T, wi string) (Opts, string, string) {
	t.Helper()
	setIdentity(t)
	o, base := taskOpts(t, wi)
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	head := mkTaskRepoWithWork(t, o.CapsuleDir, "demo-src", filepath.Join(base, "origin"))
	return o, filepath.Join(o.CapsuleDir, capsule.DirRepos, "demo-src"), head
}

func writeT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// looseObjects counts the repo's loose objects (the WIP lane must write
// none of its blobs, trees or commit into the agent's store).
func looseObjects(t *testing.T, gitDir string) int {
	t.Helper()
	n := 0
	ents, err := os.ReadDir(filepath.Join(gitDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if len(e.Name()) != 2 {
			continue
		}
		sub, _ := os.ReadDir(filepath.Join(gitDir, "objects", e.Name()))
		n += len(sub)
	}
	return n
}

func TestPushTaskWIPSnapshotsUncommittedWork(t *testing.T) {
	o, repo, head := wipEnv(t, "wi_wip1")
	writeT(t, filepath.Join(repo, "work.txt"), "agent work\nmore\n")
	mustRun(t, "git", "-C", repo, "add", "work.txt") // staged in the agent's index
	writeT(t, filepath.Join(repo, "new.txt"), "untracked\n")
	writeT(t, filepath.Join(repo, ".gitignore"), "*.tmp\n")
	writeT(t, filepath.Join(repo, "scratch.tmp"), "ignored\n")
	writeT(t, filepath.Join(repo, ".git", "info", "exclude"), "local-only\n")
	writeT(t, filepath.Join(repo, "local-only"), "excluded\n")
	if err := os.Remove(filepath.Join(repo, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	statusBefore := mustRun(t, "git", "-C", repo, "status", "--porcelain")
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	objsBefore := looseObjects(t, filepath.Join(repo, ".git"))

	PushTaskWIP(o, true, true)

	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName("demo-src")); !ok || got != head {
		t.Fatalf("carrier = %q (%v), want %q (mirrored before the WIP)", got, ok, head)
	}
	wip, ok := bareRefSHA(t, o.RemoteURL, TaskWIPRefName("demo-src"))
	if !ok {
		t.Fatal("no refs/pf-wip/demo-src for a dirty task repo")
	}
	if parents := strings.Fields(mustRun(t, "git", "--git-dir", o.RemoteURL, "rev-list", "--parents", "-n", "1", wip)); len(parents) != 2 || parents[1] != head {
		t.Fatalf("WIP parents = %v, want exactly HEAD %s", parents, head)
	}
	files := strings.Fields(mustRun(t, "git", "--git-dir", o.RemoteURL, "ls-tree", "-r", "--name-only", wip))
	want := ".gitignore new.txt work.txt"
	if strings.Join(files, " ") != want {
		t.Fatalf("WIP tree = %v, want %s (untracked in, ignored/excluded/deleted out)", files, want)
	}
	if got := mustRun(t, "git", "--git-dir", o.RemoteURL, "show", wip+":work.txt"); got != "agent work\nmore\n" {
		t.Fatalf("WIP work.txt = %q", got)
	}
	// the agent's repo is untouched: HEAD, index, status, object store
	if got := strings.TrimSpace(mustRun(t, "git", "-C", repo, "rev-parse", "HEAD")); got != head {
		t.Fatalf("HEAD moved to %s", got)
	}
	if indexAfter, _ := os.ReadFile(filepath.Join(repo, ".git", "index")); string(indexAfter) != string(indexBefore) {
		t.Fatal("the agent's index was rewritten")
	}
	if got := mustRun(t, "git", "-C", repo, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("status changed:\n%s\nwas\n%s", got, statusBefore)
	}
	if got := looseObjects(t, filepath.Join(repo, ".git")); got != objsBefore {
		t.Fatalf("the agent's object store grew %d -> %d", objsBefore, got)
	}
}

// No uncommitted changes → no WIP ref; a later clean close (or a wrapped
// close over a dirty tree) deletes a stale one.
func TestPushTaskWIPCleanTreeAndStaleRef(t *testing.T) {
	o, repo, _ := wipEnv(t, "wi_wip2")
	PushTaskWIP(o, true, true)
	if wip, ok := bareRefSHA(t, o.RemoteURL, TaskWIPRefName("demo-src")); ok {
		t.Fatalf("a clean task repo produced a WIP ref %s", wip)
	}
	writeT(t, filepath.Join(repo, "new.txt"), "x\n")
	PushTaskWIP(o, true, true)
	if _, ok := bareRefSHA(t, o.RemoteURL, TaskWIPRefName("demo-src")); !ok {
		t.Fatal("setup: no WIP ref for a dirty repo")
	}
	if err := os.Remove(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatal(err)
	}
	PushTaskWIP(o, true, true)
	if wip, ok := bareRefSHA(t, o.RemoteURL, TaskWIPRefName("demo-src")); ok {
		t.Fatalf("a clean close left the stale WIP ref %s", wip)
	}
	writeT(t, filepath.Join(repo, "new.txt"), "y\n")
	PushTaskWIP(o, true, true)
	PushTaskWIP(o, false, true) // wrapped
	if wip, ok := bareRefSHA(t, o.RemoteURL, TaskWIPRefName("demo-src")); ok {
		t.Fatalf("a wrapped close left the WIP ref %s", wip)
	}
}

// The WIP is built and pushed from a pf-carrier-* temp repo outside the
// capsule with the carrier-lane flags (argv asserted like the carrier's
// own TestCarrierPushRunsFromATempRepo), and a clean filter / fsmonitor
// planted in the agent's repo config never runs.
func TestPushTaskWIPRunsFromATempRepo(t *testing.T) {
	o, repo, _ := wipEnv(t, "wi_wip3")
	marker := filepath.Join(t.TempDir(), "ran")
	mustRun(t, "git", "-C", repo, "config", "filter.evil.clean", "touch "+marker+"; cat")
	mustRun(t, "git", "-C", repo, "config", "core.fsmonitor", "touch "+marker+"; true")
	writeT(t, filepath.Join(repo, ".gitattributes"), "* filter=evil\n")
	writeT(t, filepath.Join(repo, "new.txt"), "x\n")
	var push, add []string
	gitArgsProbe = func(args []string) {
		for _, a := range args {
			if strings.HasSuffix(a, ":"+TaskWIPRefName("demo-src")) {
				push = append([]string{}, args...)
			}
			if a == "add" {
				add = append([]string{}, args...)
			}
		}
	}
	t.Cleanup(func() { gitArgsProbe = nil })
	PushTaskWIP(o, true, true)
	wip, ok := bareRefSHA(t, o.RemoteURL, TaskWIPRefName("demo-src"))
	if !ok {
		t.Fatal("no WIP ref")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a program named by the agent's repo config ran during the snapshot")
	}
	if len(push) == 0 || !strings.HasPrefix(push[0], "--git-dir=") {
		t.Fatalf("WIP push argv = %q, want a --git-dir temp repo", push)
	}
	tmp := strings.TrimPrefix(push[0], "--git-dir=")
	if !strings.HasPrefix(filepath.Base(tmp), "pf-carrier-") || strings.HasPrefix(tmp, o.CapsuleDir) {
		t.Fatalf("temp repo %q must be a pf-carrier- dir outside the capsule", tmp)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("the temp repo %q must be removed after the push", tmp)
	}
	wantPush := []string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "http.proxy=",
		"-c", "protocol.allow=never", "-c", "protocol.file.allow=always",
		"push", "--no-recurse-submodules", o.RemoteURL, "+" + wip + ":" + TaskWIPRefName("demo-src"),
	}
	if strings.Join(push[1:], " ") != strings.Join(wantPush, " ") {
		t.Fatalf("WIP push argv = %q\nwant --git-dir=<tmp> %q", push, wantPush)
	}
	wantAdd := []string{"--git-dir=" + tmp,
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "http.proxy=",
		"--work-tree=" + repo, "add", "-A"}
	if strings.Join(add, " ") != strings.Join(wantAdd, " ") {
		t.Fatalf("WIP add argv = %q\nwant %q", add, wantAdd)
	}
}
