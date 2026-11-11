package gitops

// w20_carrier_ref_collision_repro_test.go: the polyforge#20 repro,
// corrected to the incident's real production topology after the
// root-cause rewind (the capsule's artifacts/repro.md records the
// gathered evidence; the round-1 test here encoded a shared carrier
// store that production does not have):
//
//   - every work item's capsule pushes its task-repo archive to its OWN
//     carrier repo (the incident's #255/#259/#260 pushed to three
//     distinct http://127.0.0.1:8080/git/wi_<id>.git URLs that the hub
//     maps to three distinct physical bare repos), so a repo-only
//     refs/pf-task/<repo> name never puts two work items on one ref;
//   - what the incident shows is ONE work item's capsule unable to
//     publish at all: its own carrier holds an earlier session's
//     archive whose task-repo line rode the shared branch (its commits
//     reference other tickets' work), the daemon's rewind/restore moves
//     the task repo back to an older head of that line, and every
//     close-point push of the next session's work — built on the older
//     head, so it lacks the archived tip's changes — is rejected
//     non-fast-forward, raising carrier_push_failed +
//     task_ref_diverged: the data-risk signal that pauses the work item.
//
// TestConcurrentCapsulesIsolatedCarriersNeverReject pins the production
// topology (per-WI carrier repos, one shared task-repo origin) and the
// work item's concurrent-capsule acceptance; it passes today.
// TestRewoundCapsuleRepublishesItsCarrierWithoutDivergenceSignal encodes
// the incident's same-WI sequence and FAILS on the current tree.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// sharedTaskOrigin lays the task-repo origin every capsule of the
// project clones (the incident's shared worktree): one init commit.
func sharedTaskOrigin(t *testing.T, origin string) {
	t.Helper()
	if out, err := run("git", "init", "-q", "-b", "main", origin); err != nil {
		t.Fatalf("task origin init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(origin, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", origin, "add", "-A")
	mustRun(t, "git", "-C", origin, "commit", "-q", "-m", "init")
}

// capsuleOpts builds one work item's push opts against its OWN carrier
// store (the production remoteURLFor shape: one bare repo per work
// item), with the capsule materialized for a task-repo clone.
func capsuleOpts(t *testing.T, base, wi string) Opts {
	t.Helper()
	o := Opts{
		WorkItemID: wi,
		CapsuleDir: filepath.Join(base, "capsules", wi),
		GitDir:     filepath.Join(base, "git", wi+".git"),
		RemoteURL:  filepath.Join(base, "hub", wi+".git"),
		Username:   wi,
		Secret:     "push-secret",
	}
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkTaskCapsule(t, o)
	return o
}

// cloneTaskRepo clones the shared task-repo origin into the capsule's
// repos/<name> (the claim's fresh clone) and answers the repo dir.
func cloneTaskRepo(t *testing.T, o Opts, base, name string) string {
	t.Helper()
	repoDir := filepath.Join(o.CapsuleDir, capsule.DirRepos, name)
	mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", name), repoDir)
	return repoDir
}

// commitInTaskRepo records one commit in the capsule's task repo.
func commitInTaskRepo(t *testing.T, repoDir, file, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, file), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", repoDir, "add", "-A")
	mustRun(t, "git", "-C", repoDir, "commit", "-q", "-m", msg)
}

// taskHead answers the task repo's current HEAD SHA.
func taskHead(t *testing.T, repoDir string) string {
	t.Helper()
	return strings.TrimSpace(mustRun(t, "git", "-C", repoDir, "rev-parse", "HEAD"))
}

// closePointPublish runs one capsule's close-point carrier publish
// (the lane pushTaskWIP0(keep=true, chainOK=true) walks — the same
// pushTaskRefsLocked/pushCarrier path the incident's daemon.log shows)
// with the publish's own once-per-ref alert bookkeeping, and answers
// the failed repos and raised events, both tagged with the round.
func closePointPublish(o Opts, round int) (failed, events []string) {
	po := o
	po.EventSink = func(kind string, b map[string]any) {
		events = append(events, fmt.Sprintf("%s(%s round %d)", kind, b["repo"], round))
	}
	po.CarrierAlerts = &CarrierAlerts{}
	_, f := pushTaskWIP0(po, true, true)
	for _, name := range f {
		failed = append(failed, fmt.Sprintf("%s/%s (round %d)", o.WorkItemID, name, round))
	}
	return failed, events
}

// TestConcurrentCapsulesIsolatedCarriersNeverReject pins the incident's
// production topology as the acceptance baseline: two work items'
// capsules cloning the SAME shared task-repo origin, each publishing
// into its OWN carrier repo. Every close-point publish must be clean —
// zero rejected archive pushes, neither carrier_push_failed nor
// task_ref_diverged — and each work item's archive must read its own
// task-repo head back (the claim-side restore contract). Passes today;
// a fix for the incident must keep it green.
func TestConcurrentCapsulesIsolatedCarriersNeverReject(t *testing.T) {
	setIdentity(t)
	base := t.TempDir()
	const repo = "lantay-client"
	sharedTaskOrigin(t, filepath.Join(base, "origin", repo))

	wis := []string{"wi_isolated_a", "wi_isolated_b"}
	// Each work item's own carrier store — the production shape the
	// incident's three aihub bare repos show (one per WI, no aliasing).
	for _, wi := range wis {
		mkBare(t, filepath.Join(base, "hub", wi+".git"), true)
	}

	heads := map[string]string{}
	opts := map[string]Opts{}
	for _, wi := range wis {
		o := capsuleOpts(t, base, wi)
		repoDir := cloneTaskRepo(t, o, base, repo)
		commitInTaskRepo(t, repoDir, wi+".txt", wi+"'s work")
		heads[wi] = taskHead(t, repoDir)
		opts[wi] = o
	}

	var failed, events []string
	for round := 1; round <= 2; round++ {
		for _, wi := range wis {
			f, e := closePointPublish(opts[wi], round)
			failed = append(failed, f...)
			events = append(events, e...)
		}
	}
	if len(failed) != 0 || len(events) != 0 {
		t.Fatalf("concurrent capsules with isolated carriers on the same task repo: rejected archive pushes %v, raised events %v — want zero rejections and no events (each work item's refs/pf-task/%s lives in its own carrier repo, so capsules must never collide)", failed, events, repo)
	}

	// Each work item's archive must round-trip through its own store: a
	// re-claim's fresh clone of the shared origin reads ITS OWN
	// task-repo head back from the carrier, never the other capsule's.
	for _, wi := range wis {
		fresh := filepath.Join(base, "fresh", wi)
		mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", repo), fresh)
		sha, err := FetchTaskRef(context.Background(), opts[wi], fresh, repo)
		if err != nil {
			t.Fatalf("wi %s: FetchTaskRef: %v", wi, err)
		}
		if sha != heads[wi] {
			t.Fatalf("wi %s: the carrier store answered %q, want the capsule's own head %q (each work item's archive must be isolated)", wi, sha, heads[wi])
		}
	}
}

// TestRewoundCapsuleRepublishesItsCarrierWithoutDivergenceSignal is the
// polyforge#20 incident repro (one work item, its own carrier — the
// production topology). The earlier session's task-repo line rides the
// shared branch (its commits are other tickets' work) and is archived as
// the carrier tip; the daemon's rewind/restore moves the task repo back
// to an older head of that line ("restored the previous session's head
// ... from the capsule carrier ref", daemon.log) and the next session
// commits its own work on top. Every close-point publish of that head
// must be clean — the capsule is pushing to its OWN carrier, and the
// tip's content is the shared branch's, not this capsule's work — and a
// re-claim's fresh clone must read the capsule's current head back. On
// the current tree the publish is rejected non-fast-forward and raises
// carrier_push_failed + task_ref_diverged, the incident's pause signal.
func TestRewoundCapsuleRepublishesItsCarrierWithoutDivergenceSignal(t *testing.T) {
	setIdentity(t)
	base := t.TempDir()
	const repo = "lantay-client"
	const wi = "wi_rewind"
	sharedTaskOrigin(t, filepath.Join(base, "origin", repo))
	mkBare(t, filepath.Join(base, "hub", wi+".git"), true)
	o := capsuleOpts(t, base, wi)
	repoDir := cloneTaskRepo(t, o, base, repo)

	// Session 1: the capsule's task repo carries the shared branch's
	// work — the incident's carrier tip (5aa016e8) sits on commits that
	// reference #252/#253/#254, other tickets riding the same branch.
	commitInTaskRepo(t, repoDir, "shared-early.txt", "shared line: earlier ticket's work")
	older := taskHead(t, repoDir)
	commitInTaskRepo(t, repoDir, "shared-late.txt", "shared line: later ticket's work")
	archived := taskHead(t, repoDir)

	if failed, events := closePointPublish(o, 1); len(failed) != 0 || len(events) != 0 {
		t.Fatalf("setup: session 1's first publish of a fresh capsule must be clean: failed %v, events %v", failed, events)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(repo)); !ok || got != archived {
		t.Fatalf("setup: the carrier names %q (%v), want the session-1 head %q", got, ok, archived)
	}

	// The rewind/restore the incident's daemon.log records: the task
	// repo moves back to an earlier head of the same line, an ancestor
	// of the carrier tip, and the next session's work is committed on
	// top of it — so it does not contain the tip's later changes.
	mustRun(t, "git", "-C", repoDir, "reset", "--hard", older)
	commitInTaskRepo(t, repoDir, "rewind-work.txt", "the rewound session's own work")
	current := taskHead(t, repoDir)

	// Two rounds of the rewound session's close-point publish (the
	// daemon.log's repeated same-ref rejections are a burst of
	// publishes, not one).
	var failed, events []string
	for round := 2; round <= 3; round++ {
		f, e := closePointPublish(o, round)
		failed = append(failed, f...)
		events = append(events, e...)
	}
	if len(failed) != 0 || len(events) != 0 {
		t.Fatalf("the rewound capsule publishing to its OWN carrier: rejected archive pushes %v, raised events %v — want zero rejections and no events (the incident: the work item's own later-session heads are rejected non-fast-forward against its own carrier's refs/pf-task/%s tip and raised as a data risk, pausing the work item)", failed, events, repo)
	}

	// A re-claim's fresh clone must read the capsule's current head
	// back from the carrier (the restore contract).
	fresh := filepath.Join(base, "fresh", wi)
	mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", repo), fresh)
	sha, err := FetchTaskRef(context.Background(), o, fresh, repo)
	if err != nil {
		t.Fatalf("FetchTaskRef: %v", err)
	}
	if sha != current {
		t.Fatalf("the carrier answered %q, want the capsule's current head %q (the rewound session's archive must be readable back)", sha, current)
	}
}
