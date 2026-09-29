package cli

// answerphase_test.go: the polyforge#48 (folded #49) CLI surface. The
// pilot's operator could not tell a delivered answer from a lost one
// because the CLI printed one word for two different facts; these pin
// that they are now distinguishable, and that a pending consumption
// never reads as a failure.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderAnswerPhases(t *testing.T) {
	payload := AnswerPayload{QuestionID: "q_abc123", AnswerID: "a_7k9m2p"}
	cases := []struct {
		name     string
		body     string
		want     []string
		mustNot  []string
		wantNote bool
	}{
		{
			name: "consumption confirmed",
			body: `{"applied":true,"delivered":true,"consumption":"confirmed","answer_id":"a_7k9m2p"}`,
			want: []string{"a_7k9m2p", "q_abc123", "delivered", "consumption confirmed"},
		},
		{
			name: "consumption pending is not an error",
			body: `{"applied":false,"delivered":true,"consumption":"pending","answer_id":"a_7k9m2p",` +
				`"note":"the agent consumes an answer inside a model turn; poll ` + "`pf observe`" + ` for the question to clear"}`,
			want: []string{"delivered", "consumption PENDING", "not an error", "pf observe"},
			// The pilot's exact damage: a successful delivery reported as a
			// failure. No failure vocabulary may appear on this path.
			mustNot: []string{"DAEMON-500", "error:", "failed", "lost"},
		},
		{
			name: "an older daemon answers neither key",
			body: `{"applied":true,"answer_id":"a_7k9m2p"}`,
			want: []string{"a_7k9m2p", "delivered for question q_abc123"},
			// No phase was observed, so none may be claimed.
			mustNot: []string{"confirmed", "PENDING"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAnswerPhases(json.RawMessage(tc.body), payload)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output must mention %q, got:\n%s", w, got)
				}
			}
			for _, n := range tc.mustNot {
				if strings.Contains(strings.ToLower(got), strings.ToLower(n)) {
					t.Errorf("a delivered answer must not read as %q, got:\n%s", n, got)
				}
			}
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("output must be a complete line, got %q", got)
			}
		})
	}
}

func TestAnswerConsumptionLine(t *testing.T) {
	// Waiting for a human: the operator is told what to do.
	waiting := answerConsumptionLine(json.RawMessage(
		`{"current_step":"code","pending_question":{"question_id":"q_1","status":"pending"}}`))
	if !strings.Contains(waiting, "q_1") || !strings.Contains(waiting, "waiting for a human") {
		t.Errorf("an unanswered question must say so, got %q", waiting)
	}

	// Answered and digesting: the #49 item-3 blind spot, now visible.
	digesting := answerConsumptionLine(json.RawMessage(
		`{"pending_question":{"question_id":"q_2","status":"answered","answer_received_at":"2026-09-29T01:11:00Z"}}`))
	for _, want := range []string{"q_2", "DELIVERED", "consuming", "2026-09-29T01:11:00Z"} {
		if !strings.Contains(digesting, want) {
			t.Errorf("a delivered-but-unconsumed answer must be visible as such (%q), got %q", want, digesting)
		}
	}
	if strings.Contains(digesting, "waiting for a human") {
		t.Error("a delivered answer must never still read as waiting for a human")
	}

	// No question at all: say nothing.
	if line := answerConsumptionLine(json.RawMessage(`{"current_step":"code"}`)); line != "" {
		t.Errorf("no open question ⇒ no line, got %q", line)
	}
	if line := answerConsumptionLine(json.RawMessage(`not json`)); line != "" {
		t.Errorf("an unparsable snapshot ⇒ no invented line, got %q", line)
	}
}
