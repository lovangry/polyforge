package test

// usage_s3_needs_input_test.go: S3 — the needs_input human round trip with
// real processes: the fake WI Agent (a real child of the real daemon)
// parks the step on an open pending_question; `pf observe` presents it in
// the 00 §3.4a shape; `pf answer` relays the five-field message through
// the daemon UDS face to the live agent over stdio; the agent consumes it
// atomically (archive + clear in one write, 03 §4.2 D9) and the run
// continues. Also pins the ledger's idempotency and consumed-question
// refusals through the CLI.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageS3NeedsInputRoundTrip(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s3")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S3: needs_input round trip")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to ask", func() bool {
		return m.pfOn("observe", wi) != "" && strings.Contains(m.pfOn("observe", wi), "q_dg0001")
	})

	// The question is presented in the §3.4a observation shape.
	obs := m.pfOn("observe", wi)
	for _, want := range []string{`"question_id": "q_dg0001"`, `"status": "pending"`, "Which design"} {
		if !strings.Contains(obs, want) {
			t.Fatalf("observe must present the pending question (%s), got:\n%s", want, obs)
		}
	}

	// Answer via the CLI: five-field relay, identity answered_by=usr_root.
	out := m.pfOn("answer", wi, "--question", "q_dg0001",
		"--text", "Use design A; keep it minimal.", "--by", "usr_root", "--answer-id", "a_s3first")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("pf answer must confirm delivery, got:\n%s", out)
	}

	// Consumption: the ring shows the answered event; the snapshot shows
	// the run continuing with NO open question; the capsule-side archive
	// holds the five-field answer (the agent's own atomic write).
	waitUntilUse(t, "the answered ring event", func() bool {
		return strings.Contains(m.pfOn("observe", wi), "answered")
	})
	obs = m.pfOn("observe", wi)
	if strings.Contains(obs, "pending_question") {
		t.Fatalf("the consumed question must be gone from the snapshot:\n%s", obs)
	}
	exec := readCapsuleExecution(t, m, wi)
	step := exec["steps"].(map[string]any)["code"].(map[string]any)
	answers, _ := step["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("the agent must archive exactly one answer, got %v", answers)
	}
	arch := answers[0].(map[string]any)
	if arch["answer_text"] != "Use design A; keep it minimal." || arch["answered_by"] != "usr_root" {
		t.Fatalf("the archived answer must carry the relay verbatim, got %v", arch)
	}
	if step["pending_question"] != nil {
		t.Fatalf("the agent's consumption must clear pending_question atomically, got %v", step["pending_question"])
	}

	// Replay of the SAME answer id: idempotent first outcome (200).
	m.pfOn("answer", wi, "--question", "q_dg0001",
		"--text", "Use design A; keep it minimal.", "--by", "usr_root", "--answer-id", "a_s3first")
	// ...and a NEW answer id against the consumed question: DAEMON-409.
	errs := m.pfOnExit(1, "answer", wi, "--question", "q_dg0001",
		"--text", "again", "--by", "usr_root", "--answer-id", "a_s3other")
	if !strings.Contains(errs, "DAEMON-409") {
		t.Fatalf("a new answer against a consumed question must refuse DAEMON-409, got %q", errs)
	}
	// The archive still holds exactly one consumed answer.
	exec = readCapsuleExecution(t, m, wi)
	step = exec["steps"].(map[string]any)["code"].(map[string]any)
	if answers, _ := step["answers"].([]any); len(answers) != 1 {
		t.Fatalf("replays must not duplicate the archived answer, got %v", answers)
	}

	// The run continued: wrap closes it cleanly.
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("WI must wrap after the round trip, got %s", got)
	}
}

// readCapsuleExecution decodes runtime-state/execution.json of wi on
// machine m (the capsule layout is <home>/workspace/capsules/<wi>).
func readCapsuleExecution(t *testing.T, m *usageMachine, wi string) map[string]any {
	t.Helper()
	raw := readFileOr(t, m.home+"/workspace/capsules/"+wi+"/runtime-state/execution.json")
	if raw == "" {
		t.Fatalf("no execution.json for %s on %s", wi, m.id)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("execution.json undecodable: %v", err)
	}
	return out
}
