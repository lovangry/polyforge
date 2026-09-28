package test

// usage_s14_budget_test.go: S14 — budget exhaustion lands the OVER_BUDGET
// typed interrupt (01 §6.6 / WF-103), end to end against real processes.
// The fixture bundle s14-budget gives the `burn` step a tiny step-level
// ceiling (requests: 2) while the workflow-level baseline stays generous
// (requests: 100); the fake agent over-consumes on burn's first dispatch.
// The scenario pins the typed interrupt shape:
//
//   - the STEP layer reports its own error first (whoever's bound is hit
//     wins; the workflow layer is never consulted past it);
//   - the capsule reflects the interrupt: workflow_status=blocked,
//     current_step UNCHANGED, the stopped step stays in_progress — NOT a
//     FAIL (no failed_gate, no rewind archive, no verdict), NOT a crash
//     continuation (no re-derivation, the agent lives on parked);
//   - the agent's budget authority was the REAL tier-3 proxy read
//     (read_workflow_ref -> GET /budget through the daemon);
//   - the Core close is the agent's own publish_request(block) +
//     report_step_result(block, waiting_on=owner) round trip carrying
//     the daemon-generated flush evidence: the WI lands blocked with
//     waiting_on=owner, and only a human can requeue it.

import (
	"strings"
	"testing"
)

func TestUsageS14BudgetExhaustionInterrupts(t *testing.T) {
	inst := newFaultInstall(t)
	m := inst.newUsageMachine(t, "mac_s14")
	m.start(m.faultDriveEnv(
		"FAKEPI_FAULT_STEP=burn",
		"FAKEPI_BUDGET_HOG=1",
	)...)

	wi := inst.wiCreateBundle(t, "S14: step budget exhaustion lands OVER_BUDGET blocked", "s14-budget")
	m.pfOn("submit", wi)

	// The typed interrupt: the capsule reflects Core's blocked state with
	// the stopped step left exactly where it was.
	exec := waitCapsuleExec(t, m, wi, "the OVER_BUDGET interrupt", func(e map[string]any) bool {
		return e["workflow_status"] == "blocked"
	})
	if exec["current_step"] != "burn" {
		t.Fatalf("over-budget stops the current step WITHOUT moving the position, got %v", exec["current_step"])
	}
	burn := capsuleStep(t, exec, "burn")
	if burn["status"] != "in_progress" {
		t.Fatalf("the stopped step must stay in_progress (neither pending nor done), got %v", burn["status"])
	}
	if attempts, _ := burn["attempts"].([]any); len(attempts) != 0 {
		t.Fatalf("OVER_BUDGET archives no attempt (it is neither a rewind nor an infra failure), got %v", attempts)
	}
	// NOT a FAIL: the workflow never entered a gate terminal, and the
	// completed earlier step is untouched.
	if exec["workflow_status"] == "failed_gate" || exec["workflow_status"] == "failed_rewind_exhausted" {
		t.Fatalf("over-budget must never be recorded as a gate failure, got %v", exec["workflow_status"])
	}
	if warm := capsuleStep(t, exec, "warm"); warm["status"] != "done" {
		t.Fatalf("the completed step must stay done across the interrupt, got %v", warm["status"])
	}
	// The consumed over-budget numbers are persisted with the interrupt
	// (the requests dimension: 1 + the hog blast of 10, limit 2).
	bc, _ := burn["budget_consumed"].(map[string]any)
	if bc == nil || asInt(bc["requests"]) < 11 {
		t.Fatalf("the over-consumption must be persisted on the step, got %v", burn["budget_consumed"])
	}

	// The typed event names WF-103 at the STEP level (the layer whose
	// bound was hit reports its own error).
	events := journalOfType(t, m, wi, "budget_event")
	if len(events) != 1 {
		t.Fatalf("exactly one WF-103 budget event must be journaled, got %v", events)
	}
	ev := events[0]
	for key, want := range map[string]any{"code": "WF-103", "level": "step", "step_id": "burn", "dimension": "requests"} {
		if ev[key] != want {
			t.Fatalf("the budget event must carry %s=%v, got %v", key, want, ev)
		}
	}
	if asInt(ev["consumed"]) < 11 || asInt(ev["limit"]) != 2 {
		t.Fatalf("the step layer must report its own consumed/limit pair, got %v", ev)
	}
	// The agent's budget authority was the real proxy read (tier 3):
	// the workflow-level effective ceiling it read is the fixture's 100.
	reads := journalOfType(t, m, wi, "budget_read")
	if len(reads) != 1 || asInt(reads[0]["effective_requests"]) != 100 {
		t.Fatalf("the tier-3 budget read must surface the workflow baseline, got %v", reads)
	}

	// Core-side: blocked, waiting on the owner — the over-budget closed
	// through the agent's own proxy round trip with daemon evidence.
	waitUntilUse(t, "the WI to land blocked", func() bool { return inst.wiStatus(t, wi) == "blocked" })
	if got := inst.wiWaitingOn(t, wi); got != "owner" {
		t.Fatalf("the over-budget WI must wait on the owner, got %q", got)
	}
	reps := journalOfType(t, m, wi, "report_step_result")
	if len(reps) != 1 || reps[0]["reason"] != "block" || reps[0]["waiting_on"] != "owner" {
		t.Fatalf("one report_step_result(block, owner) must carry the close, got %v", reps)
	}
	// The interrupted state reached the published HEAD (the block publish
	// was real): the read-through face serves it.
	obs := m.pfOn("observe", "--read-through", wi)
	if !strings.Contains(obs, "blocked") {
		t.Fatalf("the read-through observe must serve the interrupted state:\n%s", obs)
	}

	// NOT a crash continuation: no re-derivation, the one agent lives on.
	if m.spawnCount() != 1 {
		t.Fatalf("OVER_BUDGET must never crash-continue the agent, got %d spawns", m.spawnCount())
	}
	if !processAliveUsage(m.agentPIDs()[0]) {
		t.Fatal("the parked agent process must still be alive after the interrupt")
	}
	// The daemon-side bounded-termination (storm guard) never fired: the
	// interrupt is not a hang or a death verdict.
	if strings.Contains(m.daemonLog(), "watchdog_bounded_termination") {
		t.Fatalf("OVER_BUDGET must never be handled as a watchdog verdict:\n%s", m.daemonLog())
	}
}
