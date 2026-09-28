package test

// usage_s12_gate_rewind_test.go: S12 — the business gate FAIL -> bounded
// on_fail rewind lane, end to end against real processes. The fixture
// bundle s12-rewind (test/testdata/bundles/s12-rewind) declares
// build -> polish -> check (gate, on_fail rewind to build) with
// rewind.max_total = 1, and the fakepi drive executor plays the pf-execute
// main loop:
//
//   - (a) the gate returns FAIL once: the rewind to the target step
//     happens (observed DIRECTLY through a hold point right after the
//     rewind: build+polish reset to pending with gate_fail_rewind
//     attempts archived, the FAIL verdict persisted as the target's
//     pending_rewind_input, current_step moved back), the re-dispatch
//     consumes the routed verdict, and the gate PASSING afterward
//     continues the flow to completion (and wraps cleanly);
//   - (b) the bounded rewind-exhausted shape the reference flow defines
//     (01 §4.1 rule V6 defaults, here pinned with the fixture's tiny
//     max_total): the SECOND gate FAIL hits max_total -> the workflow
//     ends failed_rewind_exhausted (WF-101) and the WI lands Core-side
//     blocked (waiting_on=owner) through the agent's own
//     publish_request(block) + report_step_result(block) round trip.
//
// Both variants share one aihub install (each its own machine + WI).

import (
	"strings"
	"testing"
)

func TestUsageS12GateFailRewindRecovers(t *testing.T) {
	inst := newFaultInstall(t)
	release := t.TempDir() + "/release-s12a"
	m := inst.newUsageMachine(t, "mac_s12a")
	m.start(m.faultDriveEnv(
		"FAKEPI_FAULT_STEP=check",
		"FAKEPI_GATE_FAILS=1",
		"FAKEPI_HOLD_AFTER_REWIND=1",
		"FAKEPI_RELEASE_FILE="+release,
	)...)

	wi := inst.wiCreateBundle(t, "S12a: gate FAIL rewinds and the flow recovers", "s12-rewind")
	m.pfOn("submit", wi)

	// The rewound state, observed DIRECTLY at the hold point (the drive
	// loop is parked between the rewind publish and the target re-dispatch;
	// polling the journal's hold record — written only AFTER the
	// interrupting publish answered — also guarantees the published HEAD
	// already carries the rewound state).
	waitUntilUse(t, "the post-rewind hold point", func() bool {
		for _, rec := range journalOfType(t, m, wi, "hold") {
			if rec["point"] == "after_rewind" {
				return true
			}
		}
		return false
	})
	exec := capsuleExecOf(t, m, wi)
	for _, stepID := range []string{"build", "polish"} {
		st := capsuleStep(t, exec, stepID)
		if st["status"] != "pending" {
			t.Fatalf("the rewind must reset %s to pending, got %v", stepID, st["status"])
		}
		attempts, _ := st["attempts"].([]any)
		if len(attempts) != 1 {
			t.Fatalf("%s must carry exactly one archived attempt, got %v", stepID, attempts)
		}
		att, _ := attempts[0].(map[string]any)
		if att["cause"] != "gate_fail_rewind" || att["rewind_gate"] != "check" {
			t.Fatalf("the archived attempt must name the gate cause, got %v", att)
		}
		if st["output"] != nil {
			t.Fatalf("the rewind must clear %s's stale output, got %v", stepID, st["output"])
		}
	}
	if exec["current_step"] != "build" {
		t.Fatalf("the execution position must move back to the target, got %v", exec["current_step"])
	}
	build := capsuleStep(t, exec, "build")
	pri, _ := build["pending_rewind_input"].(map[string]any)
	if pri == nil {
		t.Fatal("the FAIL verdict must be persisted as build's pending_rewind_input")
	}
	if pri["gate_step_id"] != "check" || !strings.Contains(asString(pri["verdict_text"]), "gate FAIL") {
		t.Fatalf("the routed verdict must carry the gate's FAIL judgment, got %v", pri)
	}
	// The gate itself is NOT done and holds its FAIL verdict output.
	check := capsuleStep(t, exec, "check")
	if check["status"] == "done" {
		t.Fatalf("a FAILing gate is never done, got %v", check)
	}
	// The rewound state is also visible through the REAL observe face
	// (the interrupting publish carried it to the published HEAD).
	obs := m.pfOn("observe", "--read-through", wi)
	for _, want := range []string{`"current_step": "build"`, "rewind_stats"} {
		if !strings.Contains(obs, want) {
			t.Fatalf("read-through observe must show the rewound state (%s):\n%s", want, obs)
		}
	}

	// Release the hold: the target re-dispatch consumes the routed verdict,
	// the gate passes on its second judgment, and the flow completes.
	if err := writeReleaseFile(release); err != nil {
		t.Fatal(err)
	}
	waitCapsuleExec(t, m, wi, "the flow to complete after the rewind", func(e map[string]any) bool {
		return e["workflow_status"] == "completed"
	})

	// The assertion ledger: the exact dispatch order of the recovery.
	disp := journalDispatches(t, m, wi)
	order := []struct {
		step    string
		outcome string
		verdict string
	}{
		{"build", "done", ""},
		{"polish", "done", ""},
		{"check", "gate_fail", "FAIL"},
		{"build", "done", ""},
		{"polish", "done", ""},
		{"check", "done", "PASS"},
	}
	if len(disp) != len(order) {
		t.Fatalf("the dispatch ledger must record the full recovery: %v", disp)
	}
	for i, want := range order {
		if disp[i]["step"] != want.step || disp[i]["outcome"] != want.outcome {
			t.Fatalf("dispatch %d: want %s/%s, got %v", i, want.step, want.outcome, disp[i])
		}
		if want.verdict != "" && disp[i]["verdict"] != want.verdict {
			t.Fatalf("dispatch %d: want verdict %s, got %v", i, want.verdict, disp[i])
		}
	}
	// The re-entered target consumed the routed verdict on its first
	// re-dispatch (the include_verdict guarantee, 01 §6.2 step 6).
	if disp[3]["rewind_input_consumed"] != true {
		t.Fatalf("the target re-dispatch must consume the routed FAIL verdict, got %v", disp[3])
	}
	// Exactly one rewind happened; the consumed verdict is cleared.
	exec = capsuleExecOf(t, m, wi)
	if capsuleStep(t, exec, "build")["pending_rewind_input"] != nil {
		t.Fatal("the consumed rewind input must be cleared")
	}
	// A gate FAIL never switches model candidates (INV-1g): no infra
	// failure was archived anywhere.
	for _, rec := range driveJournal(t, m, wi) {
		if rec["type"] == "dispatch" && rec["outcome"] == "provider_error" {
			t.Fatalf("a business gate FAIL must never masquerade as an infra failure: %v", rec)
		}
	}

	// The recovered flow wraps cleanly through the CLI.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", m.daemonLog())
			t.Logf("drive journal:\n%s", capsuleFileOf(t, m, wi, "logs/drive.jsonl"))
		}
	})
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the recovered WI must wrap, got %s", got)
	}
}

func TestUsageS12RewindExhaustedLandsBlocked(t *testing.T) {
	inst := newFaultInstall(t)
	m := inst.newUsageMachine(t, "mac_s12b")
	m.start(m.faultDriveEnv(
		"FAKEPI_FAULT_STEP=check",
		"FAKEPI_GATE_FAILS=3", // the gate keeps failing: the fixture's max_total is 1
	)...)

	wi := inst.wiCreateBundle(t, "S12b: rewind bounds exhausted end blocked", "s12-rewind")
	m.pfOn("submit", wi)

	// The second gate FAIL hits rewind.max_total: the capsule-side terminal
	// is failed_rewind_exhausted (WF-101), never a third rewind.
	exec := waitCapsuleExec(t, m, wi, "the rewind-exhausted terminal", func(e map[string]any) bool {
		return e["workflow_status"] == "failed_rewind_exhausted"
	})
	rs, _ := exec["rewind_stats"].(map[string]any)
	if asInt(rs["total"]) != 1 {
		t.Fatalf("exactly one rewind must have happened before the bound hit, got %v", rs)
	}
	// The exhausted FAIL is NOT a rewind: no second archive round, the
	// verdict stays on the gate step, the position never moved back.
	build := capsuleStep(t, exec, "build")
	if attempts, _ := build["attempts"].([]any); len(attempts) != 1 {
		t.Fatalf("the exhausted FAIL must not archive a second rewind round, got %v", attempts)
	}
	check := capsuleStep(t, exec, "check")
	verdict, _ := check["output"].(map[string]any)
	if verdict == nil || verdict["verdict"] != "FAIL" {
		t.Fatalf("the gate's FAIL verdict must be persisted, got %v", check["output"])
	}

	// Core-side: the workflow terminal lands as blocked + waiting_on=owner
	// (01 §6.5: a Capsule failure terminal is never a Core terminal).
	waitUntilUse(t, "the WI to land blocked", func() bool { return inst.wiStatus(t, wi) == "blocked" })
	if got := inst.wiWaitingOn(t, wi); got != "owner" {
		t.Fatalf("the exhausted WI must wait on the owner, got %q", got)
	}
	// The close went through the agent's own proxy round trip with the
	// daemon-generated flush evidence (never a fabricated one).
	terms := journalOfType(t, m, wi, "terminal")
	if len(terms) != 1 || terms[0]["core_reason"] != "block" || terms[0]["waiting_on"] != "owner" {
		t.Fatalf("the terminal record must name the block close, got %v", terms)
	}
	reps := journalOfType(t, m, wi, "report_step_result")
	if len(reps) != 1 || reps[0]["reason"] != "block" {
		t.Fatalf("one report_step_result(block) must carry the close, got %v", reps)
	}
	// No crash continuation ever touched the job: exactly one agent.
	if m.spawnCount() != 1 {
		t.Fatalf("a business terminal never re-derives the agent, got %d spawns", m.spawnCount())
	}
}
