package gitops

// V-2 (repo review 2026-10-16): a step that runs `git commit` in the
// capsule root lands an untrailered commit on the chain branch through
// the .git pointer file. CommitPush folds it into the next seq commit
// instead of pushing a chain aihub's REAL pre-receive hook rejects.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aihubgit "github.com/silgrid/polyforge/internal/aihub/git"
)

// realHookBare points o at a bare repo created by aihub's own git
// manager, i.e. with the production pre-receive hook installed.
func realHookBare(t *testing.T, o *Opts) *testBare {
	t.Helper()
	m, err := aihubgit.NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureBare(o.WorkItemID); err != nil {
		t.Fatal(err)
	}
	o.RemoteURL = m.RepoDir(o.WorkItemID)
	return &testBare{dir: o.RemoteURL}
}

// stepCommit is what a step does by mistake: a plain `git commit` from
// the capsule root, resolved through the .git pointer file.
func stepCommit(t *testing.T, o Opts, name, body string) {
	t.Helper()
	writeDirty(t, o, name, body)
	for _, args := range [][]string{
		{"git", "-C", o.CapsuleDir, "add", name},
		{"git", "-C", o.CapsuleDir, "commit", "-q", "-m", "agent: " + name},
	} {
		if out, err := run(args...); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
}

func eventRecorder(o *Opts) *[]map[string]any {
	var got []map[string]any
	o.EventSink = func(kind string, body map[string]any) {
		if kind == EventUntrailedCommitsFolded {
			got = append(got, body)
		}
	}
	return &got
}

func TestCommitPushFoldsUntrailedStepCommit(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_fold")
	bare := realHookBare(t, &o)
	mkCapsule(t, o)
	events := eventRecorder(&o)

	writeDirty(t, o, "base.txt", "base\n")
	if _, pushed, _, err := CommitPush(context.Background(), o, "observe: v1"); err != nil || !pushed {
		t.Fatalf("seed CommitPush: pushed=%v err=%v", pushed, err)
	}

	// Two foreign commits stacked at the tip, plus a dirty file.
	stepCommit(t, o, "agent1.txt", "one\n")
	stepCommit(t, o, "agent2.txt", "two\n")
	writeDirty(t, o, "dirty.txt", "dirty\n")

	if _, pushed, _, err := CommitPush(context.Background(), o, "observe: v2"); err != nil || !pushed {
		t.Fatalf("CommitPush after a step commit: pushed=%v err=%v", pushed, err)
	}
	if got := bare.tipSeq(t); got != 2 {
		t.Fatalf("tip seq = %d, want 2 (the foreign commits must not be counted)", got)
	}
	// The content of every folded commit reached the server.
	for _, f := range []string{"agent1.txt", "agent2.txt", "dirty.txt", "base.txt"} {
		if out, err := run("git", "--git-dir", bare.dir, "cat-file", "-e", "main:"+f); err != nil {
			t.Errorf("server main lacks %s: %v %s", f, err, out)
		}
	}
	if n, _ := run("git", "--git-dir", bare.dir, "rev-list", "--count", "main"); strings.TrimSpace(n) != "2" {
		t.Errorf("server chain length = %s, want 2", strings.TrimSpace(n))
	}
	if len(*events) != 1 || (*events)[0]["count"] != 2 {
		t.Fatalf("fold events = %v, want one with count 2", *events)
	}
}

// The clean-tree path pushes the standing HEAD; a foreign commit at the
// tip with nothing else dirty must still be folded, not pushed as is.
func TestCommitPushCleanTreeFoldsUntrailedStepCommit(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_foldclean")
	bare := realHookBare(t, &o)
	mkCapsule(t, o)
	events := eventRecorder(&o)

	writeDirty(t, o, "base.txt", "base\n")
	if _, pushed, _, err := CommitPush(context.Background(), o, "observe: v1"); err != nil || !pushed {
		t.Fatalf("seed CommitPush: pushed=%v err=%v", pushed, err)
	}
	stepCommit(t, o, "agent.txt", "agent\n")

	sha, pushed, _, err := CommitPush(context.Background(), o, "wrap: wrapped")
	if err != nil || !pushed {
		t.Fatalf("CommitPush on a clean tree over a step commit: pushed=%v err=%v", pushed, err)
	}
	if got := bare.tipSeq(t); got != 2 {
		t.Fatalf("tip seq = %d, want 2", got)
	}
	if out, err := run("git", "--git-dir", bare.dir, "rev-parse", "main"); err != nil || strings.TrimSpace(out) != sha {
		t.Fatalf("server main = %q, want %q (%v)", strings.TrimSpace(out), sha, err)
	}
	if out, err := run("git", "--git-dir", bare.dir, "cat-file", "-e", "main:agent.txt"); err != nil {
		t.Fatalf("folded content lost: %v %s", err, out)
	}
	if len(*events) != 1 || (*events)[0]["count"] != 1 {
		t.Fatalf("fold events = %v, want one with count 1", *events)
	}

	// Steady state: nothing to fold, no further event.
	if _, _, _, err := CommitPush(context.Background(), o, "again"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 {
		t.Fatalf("a trailered tip must not raise a fold event: %v", *events)
	}
	if _, err := os.Stat(filepath.Join(o.CapsuleDir, ".git")); err != nil {
		t.Fatalf("the capsule .git pointer must stay: %v", err)
	}
}

func gitIn(t *testing.T, o Opts, args ...string) {
	t.Helper()
	full := append([]string{"git", "-C", o.CapsuleDir}, args...)
	if out, err := run(full...); err != nil {
		t.Fatalf("%v: %v: %s", full, err, out)
	}
}

// A step that branches and merges at the capsule root: the merge commit
// and the main-side commit (first-parent walk) are folded; both sides'
// content reaches the server under one proper seq.
func TestCommitPushFoldsUntrailedMergeCommit(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_foldmerge")
	bare := realHookBare(t, &o)
	mkCapsule(t, o)
	events := eventRecorder(&o)
	writeDirty(t, o, "base.txt", "base\n")
	if _, pushed, _, err := CommitPush(context.Background(), o, "observe: v1"); err != nil || !pushed {
		t.Fatalf("seed CommitPush: pushed=%v err=%v", pushed, err)
	}
	gitIn(t, o, "checkout", "-q", "-b", "side")
	stepCommit(t, o, "side.txt", "side\n")
	gitIn(t, o, "checkout", "-q", "-")
	stepCommit(t, o, "main.txt", "main\n")
	gitIn(t, o, "merge", "-q", "--no-ff", "-m", "agent merge", "side")

	if _, pushed, _, err := CommitPush(context.Background(), o, "observe: v2"); err != nil || !pushed {
		t.Fatalf("CommitPush over a merge: pushed=%v err=%v", pushed, err)
	}
	if got := bare.tipSeq(t); got != 2 {
		t.Fatalf("tip seq = %d, want 2", got)
	}
	for _, f := range []string{"side.txt", "main.txt", "base.txt"} {
		if out, err := run("git", "--git-dir", bare.dir, "cat-file", "-e", "main:"+f); err != nil {
			t.Errorf("server main lacks %s: %v %s", f, err, out)
		}
	}
	if len(*events) != 1 || (*events)[0]["count"] != 2 {
		t.Fatalf("fold events = %v, want one with count 2 (merge + main-side commit)", *events)
	}
}

// A history with no trailered commit at all has nothing safe to reset
// onto: it is left alone (no fold, no event) and the hook answers loudly.
func TestCommitPushLeavesFullyUntrailedHistoryAlone(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_foldnone")
	realHookBare(t, &o)
	mkCapsule(t, o)
	events := eventRecorder(&o)
	stepCommit(t, o, "agent.txt", "agent\n")
	before, _ := run("git", "-C", o.CapsuleDir, "rev-parse", "HEAD")

	_, pushed, _, err := CommitPush(context.Background(), o, "observe: v1")
	if err == nil || pushed || !strings.Contains(err.Error(), "Polyforge-Seq") {
		t.Fatalf("a fully untrailered chain must be refused by the hook: pushed=%v err=%v", pushed, err)
	}
	if len(*events) != 0 {
		t.Fatalf("nothing may be folded without a trailered commit: %v", *events)
	}
	if out, err := run("git", "-C", o.CapsuleDir, "cat-file", "-e", strings.TrimSpace(before)); err != nil {
		t.Fatalf("the step commit must stay in history: %v %s", err, out)
	}
}
