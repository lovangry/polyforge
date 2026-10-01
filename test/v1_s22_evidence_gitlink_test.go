package test

// v1_s22: the V4/V8 gate repairs, end to end with the REAL daemon.
//
//   - S22a (V4): a verification step whose done-write carries no
//     structured command evidence must make flush(wrap) REFUSED — the
//     typed DAEMON-400 VERIFICATION_EVIDENCE_MISSING ack, the work item
//     still running, no blocked transition — and the repaired re-flush
//     (evidence appended) seals normally. The sealed chain's HEAD must
//     carry the readable commands evidence.
//   - S22b (V8): an embedded git repository laid in the capsule (the
//     #48 poison shape) is refused by the LOCAL precheck — classified
//     push_poisoned_local, never push_failed — and the same-error retry
//     cap bounds the round at exactly 5 attempts before the halt; the
//     documented cleanup (delete the nested .git) plus a fresh commit
//     boundary recovers the chain, which seals with NO gitlink anywhere
//     in the published history.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestV1S22WrapEvidenceGate(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s22: wrap refused without verification evidence, sealed after the repair",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "step", "id": "verification", "no_evidence": true},
			// The broken seal: refused, plan continues to the repair.
			{"do": "wrap_unless_refused"},
			// The repair: re-run the verification step WITH the command
			// evidence, then wrap again.
			{"do": "step", "id": "verification"},
			{"do": "wrap"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// The refusal: the typed event lands, the work item is NOT
	// terminal, and the agent (which continued the plan) is alive.
	refusal := waitForEvent(t, d, wi, "wrap_evidence_refused", 60*time.Second)
	var rbody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(refusal.Body, &rbody); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, refusal.Body)
	}
	for _, sub := range []string{"DAEMON-400", "VERIFICATION_EVIDENCE_MISSING", wi, "verification"} {
		if !strings.Contains(rbody.Error, sub) {
			t.Fatalf("refusal error must name %q: %q", sub, rbody.Error)
		}
	}
	if st := inst.wiStatus(t, wi); st != "running" {
		t.Fatalf("a refused wrap must leave the work item running, got %q", st)
	}
	if d.agentPIDOf(t, wi) == 0 {
		t.Fatal("the refused wrap must not kill the agent")
	}

	// The agent's own journal saw both faces: the refused wrap (ok=false)
	// and, after the repair, the accepted one (ok=true).
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
	var refused, accepted bool
	for _, e := range fakeJournal(t, d.capsuleDir(wi)) {
		if e["type"] != "flush" || e["reason"] != "wrap" {
			continue
		}
		if ok, _ := e["ok"].(bool); ok {
			accepted = true
		} else {
			refused = true
		}
	}
	if !refused || !accepted {
		t.Fatalf("journal must show the refused wrap then the accepted one (refused=%v accepted=%v)", refused, accepted)
	}

	// The sealed chain's HEAD carries the readable evidence: the M4
	// read-through shape lands in the published history.
	execRaw := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	var exec struct {
		Steps map[string]struct {
			Status string `json:"status"`
			Output struct {
				Commands []map[string]any `json:"commands"`
			} `json:"output"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(execRaw), &exec); err != nil {
		t.Fatalf("sealed execution.json undecodable: %v", err)
	}
	v := exec.Steps["verification"]
	if v.Status != "done" || len(v.Output.Commands) == 0 {
		t.Fatalf("sealed verification step must be done with commands evidence: %+v", v)
	}
	c := v.Output.Commands[0]
	if c["exit_code"] == nil || c["duration_ms"] == nil {
		t.Fatalf("the evidence entry must be a readable {argv, exit_code, duration_ms}: %+v", c)
	}

	// No push bookkeeping fired on the refusal path.
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "push_failed" || e.Kind == "wrap_blocked" {
			t.Fatalf("the refused wrap must not produce %s: %+v", e.Kind, e.Body)
		}
	}
}

func TestV1S22LocalGitlinkPrecheckAndRetryCap(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s22: local gitlink precheck, poisoned-local classification, same-error cap",
		[]map[string]any{
			{"do": "step", "id": "code"},
			// The #48 poison shape: an embedded repository OUTSIDE
			// repos/ (which is gitignored). The version bump arms the
			// async push, whose CommitPush must refuse locally.
			{"do": "poison_gitlink"},
			// Production timing: PushRetryBackoff is 10s and the cap is
			// 5 identical failures — the halt lands at ~40s.
			{"do": "sleep", "ms": 55000},
			// The documented cleanup, plus a fresh commit boundary that
			// re-arms the halted round.
			{"do": "unpoison_gitlink"},
			{"do": "step", "id": "review"},
			{"do": "wrap"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// The first local refusal lands within one poll+push round.
	waitForEvent(t, d, wi, "push_poisoned_local", 30*time.Second)

	// The same-error cap: after 5 consecutive identical refusals the
	// round halts with the reason=same_error degraded event (the steps
	// ladder's own event may follow on the same failure count — wait for
	// the cap's specifically).
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
	// only, never push_failed; and the cap means EXACTLY 5 attempts
	// before the cleanup (no 185-push storm).
	time.Sleep(2 * time.Second)
	var poisoned []protocol.EventEntry
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "push_poisoned_local" {
			poisoned = append(poisoned, e)
		}
	}
	if len(poisoned) != 5 {
		t.Fatalf("the capped round must attempt exactly 5 poisoned pushes, got %d", len(poisoned))
	}
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "push_failed" {
			t.Fatalf("a local precheck refusal must never classify as push_failed: %+v", e.Body)
		}
	}

	// The cleanup recovers the chain: the halted round re-arms at the
	// next commit boundary, the push succeeds, and the WI seals.
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

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
}
