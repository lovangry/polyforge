package test

// v1_s22: the V4/V8 gate repairs, end to end with the REAL daemon,
// driver/envelope edition.
//
//   - S22a (V4): the wrap evidence gate on the driver path. The gate's
//     predicate (steps.verification.output.commands readable) judges the
//     DRIVER's execution.json: a verification envelope written WITHOUT
//     command evidence (the no_evidence toggle) makes the gated
//     flush(wrap) seal REFUSED — the typed DAEMON-400
//     VERIFICATION_EVIDENCE_MISSING ack and the wrap_evidence_refused
//     event — with the work item still live and nothing sealed. The
//     repair runs the driver's own bounded-rewind machinery (the
//     gate_failed envelope rewinds [fix, verification)), the re-run
//     verification delivers its evidence ON the envelope, and the
//     second gated flush(wrap) — evidence present — commits, pushes and
//     seals. The flush(wrap) calls are driven through the daemon's UDS
//     flush face with the runtime's announced derivation counter (the
//     B2 fence value of the driver lane), at deterministic parked
//     moments: after envelope 3 the workflow parks on an open question
//     WITH verification done-without-evidence (the refusal window), and
//     after the repair it parks again WITH the evidence (the accept
//     window) — no timing races anywhere.
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

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestV1S22WrapEvidenceGate(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s22: wrap refused without verification evidence, sealed after the repair",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			// The broken shape: verification done with NO command
			// evidence (the no_evidence toggle suppresses the envelope's
			// command_evidence, so execution.json's
			// steps.verification.output.commands stays empty).
			{"do": "envelope", "id": "verification", "outcome": "completed", "no_evidence": true},
			// The deterministic refusal window: release parks the
			// workflow on an open question while verification holds its
			// evidence-less done record.
			{"do": "envelope", "id": "release", "outcome": "needs_input", "question_id": "q_s22_hold",
				"question": "s22a: parked at the refusal window."},
			// The repair: the gate_failed envelope engages the driver's
			// on_fail rewind to fix; the re-run verification delivers
			// its evidence on the envelope.
			{"do": "envelope", "id": "release", "outcome": "gate_failed",
				"output": map[string]any{"verdict": "FAIL", "error": "verification evidence missing; rewinding to fix"}},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			// The accept window: parked again, now with the evidence on
			// the durable face.
			{"do": "envelope", "id": "release", "outcome": "needs_input", "question_id": "q_s22_hold2",
				"question": "s22a: parked at the accept window."},
		}, nil)
	d := newV1Daemon(t, inst, optDriverEngine())
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// The refusal window: verification done WITHOUT evidence, the
	// workflow parked and LIVE.
	waitQuestion(t, d, wi, "q_s22_hold", 30*time.Second)
	waitStepStatus(t, d, wi, "verification", "done", 30*time.Second)
	x := readExecution(t, d.capsuleDir(wi))
	if x.Steps["verification"].Output["commands"] != nil {
		t.Fatalf("the broken shape carries evidence after all: %+v", x.Steps["verification"].Output)
	}

	// The gated seal is REFUSED: the typed DAEMON-400
	// VERIFICATION_EVIDENCE_MISSING ack, the durable event, the work
	// item still live, nothing sealed.
	refusedAck, ferr := d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, SpawnN: 1, Reason: protocol.FlushWrap, FinalStatus: "wrapped",
	})
	if ferr != nil {
		t.Fatalf("the gated seal failed at the transport: %v", ferr)
	}
	// The typed refusal rides the flush ACK (the fence and the gate
	// answer 200 with ok:false + the error text; transport errors are
	// for the route itself).
	var refusedFlush protocol.FlushAck
	if err := json.Unmarshal(refusedAck, &refusedFlush); err != nil {
		t.Fatalf("refusal ack undecodable: %v (%s)", err, refusedAck)
	}
	if refusedFlush.OK {
		t.Fatalf("the evidence-less seal was accepted: %s", refusedAck)
	}
	refused := refusedFlush.Error
	if refused == "" {
		refused = string(refusedAck)
	}
	refusal := waitForEvent(t, d, wi, "wrap_evidence_refused", 30*time.Second)
	var rbody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(refusal.Body, &rbody); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, refusal.Body)
	}
	for _, sub := range []string{"DAEMON-400", "VERIFICATION_EVIDENCE_MISSING", wi, "verification"} {
		if !strings.Contains(rbody.Error, sub) {
			t.Fatalf("refusal event must name %q: %q", sub, rbody.Error)
		}
	}
	if st := inst.wiStatus(t, wi); st != "running" {
		t.Fatalf("a refused seal must leave the work item running, got %q", st)
	}
	// No push bookkeeping fired on the refusal path (no commit was
	// attempted).
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "push_failed" || e.Kind == "wrap_blocked" {
			t.Fatalf("the refused seal must not produce %s: %+v", e.Kind, e.Body)
		}
	}

	// The repair: the owner's answer un-parks release, whose gate_failed
	// envelope engages the bounded rewind to fix; the re-run
	// verification delivers its evidence on the envelope. Exactly one
	// rewind, one recorded verdict.
	answerQuestion(t, d, wi, "q_s22_hold", "Rewind and repair.")
	waitQuestion(t, d, wi, "q_s22_hold2", 120*time.Second)
	x = readExecution(t, d.capsuleDir(wi))
	if n := int(x.RewindStats["count"].(float64)); n != 1 {
		t.Fatalf("rewind count %v, want exactly 1 (the repair rewind)", x.RewindStats["count"])
	}
	if x.ReviewRounds["release"] != 1 {
		t.Fatalf("review rounds %v, want the one FAIL verdict recorded", x.ReviewRounds)
	}
	ver := x.Steps["verification"]
	if ver.Status != "done" || ver.Output["commands"] == nil {
		t.Fatalf("the repaired verification must be done WITH commands evidence: %+v", ver)
	}

	// The gated seal is ACCEPTED: the evidence gate passes, the seal
	// commit+push lands, and the work item wraps through the gated path.
	ack, err := d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, SpawnN: 1, Reason: protocol.FlushWrap, FinalStatus: "wrapped",
	})
	if err != nil {
		t.Fatalf("the repaired seal was refused: %v", err)
	}
	var fa protocol.FlushAck
	if err := json.Unmarshal(ack, &fa); err != nil || !fa.OK || !fa.Pushed {
		t.Fatalf("accept flush ack wrong: %v (%s)", err, ack)
	}
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// The faces: the refusal happened exactly once, the repaired
	// verification's evidence is readable on the sealed chain's HEAD,
	// and both envelopes (broken and repaired) are archived.
	if n := countOf(d.eventKinds(t, wi), "wrap_evidence_refused"); n != 1 {
		t.Fatalf("wrap_evidence_refused fired %d times, want 1", n)
	}
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
	broken := archivedEnvelope(t, d.capsuleDir(wi), "verification", 3)
	if broken["outcome"] != "completed" {
		t.Fatalf("the broken verification envelope wrong: %v", broken["outcome"])
	}
	repaired := archivedEnvelope(t, d.capsuleDir(wi), "verification", 7)
	if repaired["outcome"] != "completed" {
		t.Fatalf("the repaired verification envelope wrong: %v", repaired["outcome"])
	}
}

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
	d := newV1Daemon(t, inst, optDriverEngine())
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
