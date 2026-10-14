package test

// v4_audit_subtraction_acceptance_test.go: the polyforge#147 e2e
// acceptance checks that no unit or other e2e test already covers,
// driven through the real pf/aihub/fakepi binaries. The rest of the
// #147 contract (minimal envelope, result_invalid blocking, D6
// relaxation and rewind zeroing, D7 backoff, release-step absence, the
// D15 replay) is pinned by internal/polyforge/driver, supervise and the
// v1/v2 e2e suites.

import (
	"testing"
	"time"
)

// TestV4NeedsInputWaitNotCountedIntoT: the wall time a step spends
// PARKED on an owner question must not enter the step's cumulative
// runtime T (T counts agent running time only).
func TestV4NeedsInputWaitNotCountedIntoT(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4: the needs_input wait is not agent runtime", plan([]map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "needs_input",
			"question_id": "q_v4wait", "question": "should the wait count into T?"},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
	}), nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	waitQuestion(t, d, wi, "q_v4wait", 60*time.Second)
	// The owner takes their time: a wait far longer than either of the
	// step's two agent dispatches.
	time.Sleep(6 * time.Second)
	answerQuestion(t, d, wi, "q_v4wait", "No — only agent runtime counts.")
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	x := readExecution(t, capDir)
	wall, _ := x.Steps["grill"].BudgetConsumed["wall_ms"].(float64)
	if wall >= 5000 {
		t.Fatalf("SYMPTOM: grill booked %vms of wall — the 6s owner wait leaked into T (agent runtime is ~2s)", wall)
	}
}

// TestV4BundleFeatureFullLaneWraps: the shipped feature-full bundle
// (grill -> spec -> grill-review -> plan -> e2e-author -> e2e-baseline
// -> code -> e2e-run -> review -> verification) runs end to end through
// the real daemon and wraps — the only e2e run of that lane.
func TestV4BundleFeatureFullLaneWraps(t *testing.T) {
	inst := newV1Install(t)
	actions := []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "spec", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "grill-review", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "plan", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-author", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-baseline", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "code", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-run", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "review", "outcome": "completed"},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	}
	wi := newWIWithBundle(t, inst, "v4: feature-full lane end to end", "feature-full", actions)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	inst.waitWIStatus(t, wi, "wrapped", 240*time.Second)
	x := readExecution(t, d.capsuleDir(wi))
	if x.WorkflowStatus != "completed" {
		t.Fatalf("workflow status %q, want completed", x.WorkflowStatus)
	}
	for _, step := range []string{"grill", "spec", "grill-review", "plan", "e2e-author", "e2e-baseline", "code", "e2e-run", "review", "verification"} {
		if st, ok := x.Steps[step]; !ok || st.Status != "done" {
			t.Fatalf("step %s not done: %+v", step, st)
		}
	}
}
