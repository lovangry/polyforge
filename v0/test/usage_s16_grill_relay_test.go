package test

// usage_s16_grill_relay_test.go: S16 — the needs_input relay round trip of
// an interactive grill step, end to end against real processes. The
// fixture bundle s16-grill (grill -> spec) drives the interactive step
// through the fakepi drive executor exactly as skills/pf-execute §7
// prescribes, and the scenario pins the FULL chain:
//
//   - the grill step persists a pending_question and the interrupting
//     publish rides the same transition — the question is visible on the
//     observe face (layer 1) AND on the published HEAD (read-through);
//   - the answer goes in through the REAL CLI answer verb;
//   - the five-field relay ({question_id, answer_id, answer_text,
//     answered_at, answered_by}) reaches the waiting agent VERBATIM;
//   - the SAME step continues in place — not a new step, not a re-dispatch
//     (exactly one grill dispatch in the ledger) — and the flow proceeds
//     to the next step;
//   - the question/answer pair lands in the step's answers[] archive AND
//     in the capsule's shared grill-qa log (the artifact the spec step
//     consumes).

import (
	"strings"
	"testing"
)

func TestUsageS16GrillRelayRoundTrip(t *testing.T) {
	inst := newFaultInstall(t)
	m := inst.newUsageMachine(t, "mac_s16")
	m.start(m.faultDriveEnv()...)

	wi := inst.wiCreateBundle(t, "S16: grill needs_input relay round trip", "s16-grill")
	m.pfOn("submit", wi)

	// The question is visible via the observe face (layer 1: the live
	// capsule through the daemon).
	waitUntilUse(t, "the grill question to be asked", func() bool {
		return strings.Contains(m.pfOn("observe", wi), "q_drv001")
	})
	obs := m.pfOn("observe", wi)
	for _, want := range []string{`"question_id": "q_drv001"`, "lean variant", `"status": "pending"`} {
		if !strings.Contains(obs, want) {
			t.Fatalf("observe must present the pending question (%s):\n%s", want, obs)
		}
	}
	// ...and via the read-through face: the interrupting publish carried
	// the question to the published HEAD (01 §4.3: the persisted question
	// is presented through the publish chain to the owner's observer).
	// The layer-1 observe above reads the LIVE capsule and can see the
	// question the moment its write lands — while the owner's read-through
	// face serves it only once the interrupting publish's manifest lands in
	// aihub — so the wait is bounded on the face itself, never asserted
	// against an in-flight publish.
	waitUntilUse(t, "the interrupting publish to carry the question to the published HEAD", func() bool {
		return strings.Contains(m.pfOn("observe", "--read-through", wi), "q_drv001")
	})

	// Answer through the REAL CLI answer verb (the five-field relay).
	out := m.pfOn("answer", wi, "--question", "q_drv001",
		"--text", "Take the lean variant; skip the compatibility layer.",
		"--by", "usr_root", "--answer-id", "a_s16qa")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("pf answer must confirm delivery, got:\n%s", out)
	}

	// The relay reached the waiting agent with all five fields verbatim.
	exec := waitCapsuleExec(t, m, wi, "the answered step to continue and the flow to complete", func(e map[string]any) bool {
		return e["workflow_status"] == "completed"
	})
	relays := journalOfType(t, m, wi, "answer_relay")
	if len(relays) != 1 {
		t.Fatalf("exactly one answer relay must reach the agent, got %v", relays)
	}
	rel := relays[0]
	for key, want := range map[string]any{
		"question_id": "q_drv001", "answer_id": "a_s16qa",
		"answer_text": "Take the lean variant; skip the compatibility layer.",
		"answered_by": "usr_root",
	} {
		if rel[key] != want {
			t.Fatalf("the relay must carry %s verbatim, got %v", key, rel)
		}
	}
	if asString(rel["answered_at"]) == "" {
		t.Fatalf("the relay must carry the answered_at timestamp, got %v", rel)
	}

	// The SAME step continued in place: exactly one grill dispatch in the
	// ledger (the answer resumed the waiting step, it never re-entered as
	// a new dispatch round), then exactly one spec dispatch.
	disp := journalDispatches(t, m, wi)
	if got := len(dispatchesOfStep(disp, "grill")); got != 1 {
		t.Fatalf("the answered step must continue in place — one grill dispatch, got %v", disp)
	}
	if got := len(dispatchesOfStep(disp, "spec")); got != 1 {
		t.Fatalf("the flow must proceed to the next step after the answer, got %v", disp)
	}

	// The question/answer pair landed in the step's answers[] archive
	// (the 03 §4.2 D9 atomic consumption: archive + clear in one write).
	grill := capsuleStep(t, exec, "grill")
	answers, _ := grill["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("the step must archive exactly one consumed answer, got %v", answers)
	}
	arch, _ := answers[0].(map[string]any)
	if arch["question_id"] != "q_drv001" || arch["answer_text"] != "Take the lean variant; skip the compatibility layer." ||
		arch["answered_by"] != "usr_root" {
		t.Fatalf("the archived pair must carry the relay verbatim, got %v", arch)
	}
	if grill["pending_question"] != nil {
		t.Fatalf("the consumption must clear pending_question atomically, got %v", grill["pending_question"])
	}
	if attempts, _ := grill["attempts"].([]any); len(attempts) != 0 {
		t.Fatalf("Q&A history never lands in attempts[] (03 §4.2), got %v", attempts)
	}
	// ...and in the capsule's shared grill-qa log, sectioned by step.
	qa := capsuleFileOf(t, m, wi, "artifacts/grill-qa.md")
	for _, want := range []string{"## grill", "q_drv001", "Take the lean variant; skip the compatibility layer.", "usr_root"} {
		if !strings.Contains(qa, want) {
			t.Fatalf("the grill-qa log must carry the pair (%s):\n%s", want, qa)
		}
	}
	// The grill step's recorded output points at that log (what spec
	// consumed), and the spec step completed on top of it.
	if out, _ := grill["output"].(map[string]any); out == nil || out["qa_log"] != "artifacts/grill-qa.md" {
		t.Fatalf("the grill step's output must reference the qa log, got %v", grill["output"])
	}
	if capsuleStep(t, exec, "spec")["status"] != "done" {
		t.Fatalf("the spec step must complete after the answer, got %v", exec["steps"])
	}

	// The round trip closed the WI cleanly.
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the WI must wrap after the round trip, got %s", got)
	}
}
