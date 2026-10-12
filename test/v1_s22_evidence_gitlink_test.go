package test

// v1_s22: the V8 gate repair, end to end with the REAL daemon,
// driver/envelope edition. (The S22a V4 wrap-evidence-gate scenario
// pinned the retired flush(wrap) seal path and was deleted with it; the
// driver track re-adds the evidence gate.)
//
//   - S22b (V8): an embedded git repository laid in the capsule (the
//     #48 poison shape, the grill envelope action) is refused by the
//     LOCAL precheck — classified push_poisoned_local, never
//     push_failed — and the same-error retry cap bounds the round at
//     exactly 5 attempts before the halt; the documented cleanup (the
//     unpoison envelope action) plus a fresh commit boundary recovers
//     the chain, which seals with NO gitlink anywhere in the published
//     history.
import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestV1S22LocalGitlinkPrecheckAndRetryCap(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s22: local gitlink precheck, poisoned-local classification, same-error cap",
		[]map[string]any{
			// The #48 poison shape: an embedded repository OUTSIDE
			// repos/ (which is gitignored), laid by the grill envelope
			// action; the needs_input transition bumps the Version and
			// parks the workflow, so the push round runs against the
			// poisoned worktree while the WI stays live.
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": "q_s22b",
				"question":       "s22b: parked while the poisoned push round runs.",
				"poison_gitlink": true},
			// The documented cleanup, scripted as the continuation: the
			// unpoison envelope action deletes the nested .git and bumps
			// the Version — the halted round re-arms at that boundary.
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true,
				"unpoison_gitlink": true},
			// The fix step burns 15s before its envelope: the re-armed
			// push round's second attempt (the 10s backoff) lands on the
			// CLEAN tree and recovers the chain while the workflow is
			// still live — the wrap must never outrun the recovery push.
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 15000},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitQuestion(t, d, wi, "q_s22b", 30*time.Second)

	// The first local refusal lands within one poll+push round.
	waitForEvent(t, d, wi, "push_poisoned_local", 30*time.Second)

	// The same-error cap: after 5 consecutive identical refusals the
	// round halts with the reason=same_error degraded event (the steps
	// ladder's own event may follow on the same failure count — wait for
	// the cap's specifically). The parked question keeps the WI live and
	// version-stable while the cap burns.
	deadline := time.Now().Add(120 * time.Second)
	var capBody struct {
		Reason string `json:"reason"`
		Streak int    `json:"streak"`
	}
	for {
		for _, e := range d.eventsOf(t, wi) {
			if e.Kind != "push_backpressure" {
				continue
			}
			_ = json.Unmarshal(e.Body, &capBody)
			if capBody.Reason == "same_error" {
				break
			}
		}
		if capBody.Reason == "same_error" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if capBody.Reason != "same_error" || capBody.Streak != 5 {
		t.Fatalf("want the same-error cap event (reason=same_error, streak=5), got %+v", capBody)
	}

	// The classification held the whole poisoned episode: local refusals
	// only, never push_failed; and the cap means the round HALTED at
	// exactly 5 attempts (no 185-push storm). The re-armed round's first
	// attempt (the answer's boundary, before the scripted cleanup lands)
	// meets the still-poisoned index once more and is refused — the 6th
	// and last local refusal — and its second attempt (the 10s backoff,
	// the tree now clean) recovers the chain.
	time.Sleep(2 * time.Second)
	preCleanup := 0
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "push_poisoned_local" {
			preCleanup++
		}
	}
	if preCleanup != 5 {
		t.Fatalf("the capped round must attempt exactly 5 poisoned pushes, got %d", preCleanup)
	}
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "push_failed" {
			t.Fatalf("a local precheck refusal must never classify as push_failed: %+v", e.Body)
		}
	}

	// The cleanup recovers the chain: the owner's answer re-dispatches
	// grill, whose envelope action unpoisons and bumps the Version — the
	// halted round re-arms, the push succeeds, and the workflow runs to
	// its terminal wrap.
	answerQuestion(t, d, wi, "q_s22b", "Cleaned up; resume.")
	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)

	// The recovery push: the re-armed round's first attempt lands on the
	// CLEAN tree (the scripted cleanup runs inside the 10s backoff
	// window) — the refusal count stays at the capped 5, the push
	// succeeds, and the recovered chain lands while fix holds its 15s
	// window, BEFORE the wrap stops the monitor.
	waitUntil(t, 60*time.Second, "the re-armed round never recovered the chain", func() bool {
		kinds := d.eventKinds(t, wi)
		return countOf(kinds, "push_poisoned_local") == 5 && countOf(kinds, "published") >= 1
	})
	inst.waitBareCommits(t, wi, 1, 60*time.Second)

	// THE load-bearing assertion of the precheck: the published history
	// contains NO gitlink anywhere — the poison never entered it.
	out := gitRun(t, inst.bareRepo(wi), "ls-tree", "-r", "main")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "160000") {
			t.Fatalf("the sealed chain must not contain a gitlink:\n%s", out)
		}
	}
	// And the formerly embedded files landed as plain capsule files (the
	// cleanup's promise: nothing of the work is lost).
	if !inst.bareHasPath(t, wi, "poison/inner.txt") {
		t.Fatalf("the cleaned-up embedded file must ride the chain as a plain file")
	}
	// The driver faces: the whole chain done, the poisoned ordinal's
	// envelope archived.
	x := readExecution(t, d.capsuleDir(wi))
	for _, step := range []string{"grill", "fix", "verification", "release"} {
		if st := x.Steps[step]; st == nil || st.Status != "done" {
			t.Fatalf("step %s: %+v, want done", step, st)
		}
	}
	env2 := archivedEnvelope(t, d.capsuleDir(wi), "grill", 2)
	if env2["outcome"] != "completed" {
		t.Fatalf("the cleanup continuation envelope wrong: %v", env2["outcome"])
	}
}
