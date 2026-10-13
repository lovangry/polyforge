package test

// v2_driver_guarantees_test.go: the driver's mechanical guarantees (v2
// §2.3) end to end with the real daemon + fakepi: the human gate on a
// quick-fix copy whose grill declares the flag and the V4 verification-evidence gate. Both
// land on the in-process REJECTED-rewrite lane (the daemon log carries
// the envelope_rejected detail), not the recovery lane.

import (
	"strings"
	"testing"
	"time"
)

func TestV2HumanGateGuaranteeEndToEnd(t *testing.T) {
	inst := newV1Install(t)
	inst.gateQuickFixGrill = true // test-local bundle: quick-fix grill + requires_human_answer (the shipped grills allow zero questions)
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
	// the driver's rejection is an event in the durable per-WI log
	rej := waitForEvent(t, d, wi, "envelope_rejected", 30*time.Second)
	if !strings.Contains(string(rej.Body), "requires a human answer: return needs_input first") {
		t.Fatalf("the completed-without-answer envelope was not rejected in-process: %s", rej.Body)
	}
	answerQuestion(t, d, wi, qid, "Yes, that scope.")
	waitWorkflowStatus(t, d, wi, "completed", 120*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
}
