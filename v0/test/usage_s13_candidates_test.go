package test

// usage_s13_candidates_test.go: S13 — the infrastructure-failure candidate
// advancement lane (01 §6.3), end to end against real processes. The
// fixture bundle s13-candidates declares one step with exactly two model
// candidates ([fake-alpha, fake-beta], no defaults). The scenario injects
// THREE provider errors at the fake-harness level on the first three
// dispatches — more infra failures than the list has entries — and pins:
//
//   - the infra failure is archived as an infra_failure attempt and the
//     step resumes IN PLACE (status stays in_progress, never reset to
//     pending, no FAIL verdict anywhere);
//   - the 1 -> 2 ordinal advance and the re-dispatch per 01 §6.3 (the
//     effective candidate is recomputed from the attempts[] archive by
//     the REAL drive.CandidateOrdinal — the first re-dispatch runs with
//     the list's SECOND entry);
//   - the bounded stop at the list's end: the ordinal caps at the list
//     length — the third failure re-dispatches with fake-beta AGAIN
//     (never past the list's end), and exhausting candidates is NOT a
//     terminal: the fourth dispatch succeeds and the flow completes.

import (
	"strings"
	"testing"
)

func TestUsageS13CandidateAdvancement(t *testing.T) {
	inst := newFaultInstall(t)
	m := inst.newUsageMachine(t, "mac_s13")
	m.start(m.faultDriveEnv(
		"FAKEPI_FAULT_STEP=probe",
		"FAKEPI_PROVIDER_ERRORS=3", // deliberately one MORE than the list holds
	)...)

	wi := inst.wiCreateBundle(t, "S13: infra failure advances the candidate, bounded by the list", "s13-candidates")
	m.pfOn("submit", wi)

	waitCapsuleExec(t, m, wi, "the flow to complete over the injected failures", func(e map[string]any) bool {
		return e["workflow_status"] == "completed"
	})

	// The dispatch ledger: the exact candidate walk the recompute chose.
	disp := dispatchesOfStep(journalDispatches(t, m, wi), "probe")
	if len(disp) != 4 {
		t.Fatalf("three injected failures + one success = four dispatches, got %v", disp)
	}
	wantCandidates := []string{"fake-alpha", "fake-beta", "fake-beta", "fake-beta"}
	wantOrdinals := []int64{1, 2, 2, 2}
	wantOutcomes := []string{"provider_error", "provider_error", "provider_error", "done"}
	for i, d := range disp {
		if d["candidate"] != wantCandidates[i] {
			t.Fatalf("dispatch %d must run with candidate %s, got %v", i+1, wantCandidates[i], d)
		}
		if asInt(d["ordinal"]) != wantOrdinals[i] {
			t.Fatalf("dispatch %d must run at ordinal %d (1-BASED, capped by the list length), got %v",
				i+1, wantOrdinals[i], d)
		}
		if d["outcome"] != wantOutcomes[i] {
			t.Fatalf("dispatch %d outcome must be %s, got %v", i+1, wantOutcomes[i], d)
		}
	}
	// Same step, resumed in place: every re-dispatch saw the step
	// still in_progress (01 §6.3 — never reset to pending).
	for i, d := range disp {
		if i > 0 && d["status_at_dispatch"] != "in_progress" {
			t.Fatalf("re-dispatch %d must resume the in-place step, got %v", i+1, d)
		}
	}

	// The capsule archive: exactly three infra_failure attempts, no
	// business archive, the step done.
	exec := capsuleExecOf(t, m, wi)
	probe := capsuleStep(t, exec, "probe")
	if probe["status"] != "done" {
		t.Fatalf("the step must reach done after the bounded walk, got %v", probe["status"])
	}
	attempts, _ := probe["attempts"].([]any)
	if len(attempts) != 3 {
		t.Fatalf("each infra failure archives one attempt, got %v", attempts)
	}
	for i, a := range attempts {
		att, _ := a.(map[string]any)
		if att["cause"] != "infra_failure" {
			t.Fatalf("attempt %d must carry cause infra_failure, got %v", i+1, att)
		}
		if !strings.Contains(asString(att["detail"]), "provider_error") {
			t.Fatalf("attempt %d must carry the injected provider_error detail, got %v", i+1, att)
		}
	}

	// The whole walk stayed inside one generation, one agent, one step:
	// an infra failure is never a crash-continuation trigger here (the
	// agent process lived through all of it).
	if m.spawnCount() != 1 {
		t.Fatalf("the in-place resume never re-derives the agent, got %d spawns", m.spawnCount())
	}
	if n := len(journalOfType(t, m, wi, "rewind")); n != 0 {
		t.Fatalf("an infra failure must never trigger a rewind, got %d rewind records", n)
	}

	// The flow completed and wraps through the CLI.
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the recovered WI must wrap, got %s", got)
	}
}
