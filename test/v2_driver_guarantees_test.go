package test

// v2_driver_guarantees_test.go: the driver's mechanical guarantees (v2
// §2.3) end to end with the real daemon + fakepi: the human gate on the
// SHIPPED quick-fix bundle and the V4 verification-evidence gate. Both
// land on the in-process REJECTED-rewrite lane (the daemon log carries
// the envelope_rejected detail), not the recovery lane.

import (
	"strings"
	"testing"
	"time"
)

func TestV2HumanGateShippedBundle(t *testing.T) {
	inst := newV1Install(t)
	inst.keepHumanGates = true // the shipped quick-fix declares requires_human_answer on grill
	const qid = "q_v2gate"
	wi := inst.newWI(t, "v2: completed grill refused until a human answered",
		[]map[string]any{
			// ordinal 1: tries to skip the human — refused in-process.
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			// ordinal 2: the rewrite asks.
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": "Confirm the scope?"},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)

	waitQuestion(t, d, wi, qid, 60*time.Second)
	if !strings.Contains(d.logTail(), "requires a human answer: return needs_input first") {
		t.Fatalf("the completed-without-answer envelope was not rejected in-process:\n%s", d.logTail())
	}
	answerQuestion(t, d, wi, qid, "Yes, that scope.")
	waitWorkflowStatus(t, d, wi, "completed", 120*time.Second)
}

func TestV2VerificationEvidenceRejectedInProcess(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v2: evidence-less verification refused, rewrite accepted",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed", "no_evidence": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)

	waitWorkflowStatus(t, d, wi, "completed", 120*time.Second)
	if !strings.Contains(d.logTail(), "completed without verification evidence") {
		t.Fatalf("the evidence-less verification was not rejected in-process:\n%s", d.logTail())
	}
	x := readExecution(t, d.capsuleDir(wi))
	if x.Steps["verification"].Output["commands"] == nil {
		t.Fatalf("accepted verification must keep its evidence: %+v", x.Steps["verification"].Output)
	}
}
