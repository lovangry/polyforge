package gitops

// w20_carrier_ref_collision_repro_test.go: the polyforge#20 repro —
// two work items' capsules clone the SAME task-repo origin (the shared
// worktree setup) and push their task-repo archives into the carrier
// store at their close points. The archive ref name carries no
// work-item identity, so both capsules write ONE ref for the repo; the
// second capsule's head (an independent sibling line from the shared
// origin) is rejected non-fast-forward, and the lane raises
// carrier_push_failed + task_ref_diverged — the data-risk signal that
// conservatively pauses the work item. The work item's acceptance:
// concurrent dual capsule, ZERO rejections, each capsule's archive
// readable back (its own ref). This test encodes that acceptance; on
// the current tree it fails with the incident's exact signal.

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

// capsuleWithTaskWork builds one work item's capsule against the shared
// carrier store and clones the shared origin into its repos/<name>,
// committing one independent line of work (the sibling head the incident
// produced: each capsule works from the shared origin's base). Answers
// the capsule opts and the local task-repo head SHA.
func capsuleWithTaskWork(t *testing.T, base, hub, wi, name, work string) (Opts, string) {
	t.Helper()
	o := Opts{
		WorkItemID: wi,
		CapsuleDir: filepath.Join(base, "capsules", wi),
		GitDir:     filepath.Join(base, "git", wi+".git"),
		RemoteURL:  hub,
		Username:   wi,
		Secret:     "push-secret",
	}
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkTaskCapsule(t, o)
	repoDir := filepath.Join(o.CapsuleDir, capsule.DirRepos, name)
	mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", name), repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, work), []byte(wi+"'s work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", repoDir, "add", "-A")
	mustRun(t, "git", "-C", repoDir, "commit", "-q", "-m", wi+" work")
	return o, strings.TrimSpace(mustRun(t, "git", "-C", repoDir, "rev-parse", "HEAD"))
}

// TestConcurrentCapsulesSameTaskRepoNeverRejectCarrier is the polyforge#20
// repro. Two capsules of two different work items, one shared task-repo
// origin, one carrier store: every close-point publish of the second
// capsule must not be rejected on the first's archive, must raise neither
// carrier_push_failed nor task_ref_diverged, and each capsule's archive
// must read its own task-repo head back (the claim-side restore contract).
func TestConcurrentCapsulesSameTaskRepoNeverRejectCarrier(t *testing.T) {
	setIdentity(t)
	base := t.TempDir()
	const repo = "lantay-client"

	sharedTaskOrigin(t, filepath.Join(base, "origin", repo))

	// The carrier store both work items' task-repo archives land in: the
	// hub the incident's same-named refs/pf-task/<repo> ref lived in.
	// A hub with a pre-receive hook models the production side (which
	// accepts non-fast-forward carrier updates — the rejection is the
	// pushing client's, exactly as the daemon.log shows).
	hub := filepath.Join(base, "hub", "wi-store.git")
	mkBare(t, hub, true)

	oA, headA := capsuleWithTaskWork(t, base, hub, "wi_collide_a", repo, "a-work.txt")
	oB, headB := capsuleWithTaskWork(t, base, hub, "wi_collide_b", repo, "b-work.txt")

	// Two rounds of both capsules' close-point carrier publish (the
	// daemon.log's repeated same-ref rejections are a burst of publishes,
	// not one): each capsule is its own job, with its own once-per-ref
	// alert bookkeeping.
	var events []string
	var failed []string
	publish := func(o Opts, round int) {
		t.Helper()
		o.EventSink = func(kind string, b map[string]any) {
			events = append(events, fmt.Sprintf("%s(%s round %d)", kind, b["repo"], round))
		}
		o.CarrierAlerts = &CarrierAlerts{}
		_, f := pushTaskWIP0(o, true, true)
		for _, name := range f {
			failed = append(failed, fmt.Sprintf("%s/%s (round %d)", o.WorkItemID, name, round))
		}
	}
	for round := 1; round <= 2; round++ {
		publish(oA, round)
		publish(oB, round)
	}
	if len(failed) != 0 || len(events) != 0 {
		t.Fatalf("concurrent capsules on the same task repo: rejected archive pushes %v, raised events %v — want zero rejections and no events (the incident's signal: a capsule's archive push is rejected non-fast-forward on the shared refs/pf-task/%s name and judged a data risk, pausing the work item)", failed, events, repo)
	}

	// Each capsule's archive must round-trip through the store: a
	// re-claim's fresh clone of the shared origin reads ITS OWN
	// task-repo head back from the carrier (never the other capsule's).
	for _, tc := range []struct {
		o    Opts
		want string
	}{
		{oA, headA},
		{oB, headB},
	} {
		fresh := filepath.Join(base, "fresh", tc.o.WorkItemID)
		mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", repo), fresh)
		sha, err := FetchTaskRef(context.Background(), tc.o, fresh, repo)
		if err != nil {
			t.Fatalf("wi %s: FetchTaskRef: %v", tc.o.WorkItemID, err)
		}
		if sha != tc.want {
			t.Fatalf("wi %s: the carrier store answered %q, want the capsule's own head %q (each work item's archive must be isolated)", tc.o.WorkItemID, sha, tc.want)
		}
	}
}
