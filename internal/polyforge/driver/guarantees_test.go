package driver

// guarantees_test.go: the mechanical driver guarantees (v2 §2.3) —
// needs_input envelope validation and the human-gate guarantee (#138) —
// plus the retry-feedback plumbing they ride on.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

func TestNeedsInputEnvelopeValidation(t *testing.T) {
	mk := func() *fixture {
		f := newFixture(t, false)
		f.res.Outcome = OutcomeNeedsInput
		f.res.Output = nil
		return f
	}
	f := mk()
	err := ValidateStepResult(f.res, f.spec)
	if err == nil || !strings.Contains(err.Error(), "question_id and question") {
		t.Fatalf("needs_input without question must be rejected naming both fields, got %v", err)
	}
	f = mk()
	f.res.QuestionID = "q1"
	if err := ValidateStepResult(f.res, f.spec); err == nil || !strings.Contains(err.Error(), "missing question") {
		t.Fatalf("missing question text must be rejected, got %v", err)
	}
	f = mk()
	f.res.Question = "why?"
	if err := ValidateStepResult(f.res, f.spec); err == nil || !strings.Contains(err.Error(), "question_id") {
		t.Fatalf("missing question_id must be rejected, got %v", err)
	}
	// positive: envelope level, and embedded in the output object.
	f = mk()
	f.res.QuestionID, f.res.Question = "q1", "why?"
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("envelope-level question must validate: %v", err)
	}
	f = mk()
	f.res.Output = json.RawMessage(`{"question_id":"q2","question":"which?"}`)
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("embedded question must validate: %v", err)
	}
}

func TestHumanGateGuarantee(t *testing.T) {
	f := newFixture(t, false)
	f.spec.RequiresHumanAnswer = true
	err := ValidateStepResult(f.res, f.spec)
	if err == nil || !strings.Contains(err.Error(), "requires a human answer: return needs_input first") {
		t.Fatalf("completed without a human answer must be rejected, got %v", err)
	}
	// needs_input is still allowed on a human-gate step.
	q := *f.res
	q.Outcome, q.QuestionID, q.Question, q.Output = OutcomeNeedsInput, "q1", "ok?", nil
	if err := ValidateStepResult(&q, f.spec); err != nil {
		t.Fatalf("needs_input must stay legal on a human-gate step: %v", err)
	}
	f.spec.HumanAnswers = 1
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("completed after a human answer must validate: %v", err)
	}
	// undeclared steps are untouched.
	g := newFixture(t, false)
	if err := ValidateStepResult(g.res, g.spec); err != nil {
		t.Fatalf("undeclared step must not owe a human answer: %v", err)
	}
}

func TestCurrentChainAnswersResetByRewind(t *testing.T) {
	st := &capsule.Step{Answers: []capsule.ArchivedAnswer{{QuestionID: "a"}, {QuestionID: "b"}}}
	if n := currentChainAnswers(st); n != 2 {
		t.Fatalf("never-rewound step counts all answers, got %d", n)
	}
	st.AnswerBase = 2 // set by applyRewindRangeLocked
	if n := currentChainAnswers(st); n != 0 {
		t.Fatalf("answers before the rewind must not count, got %d", n)
	}
	st.Answers = append(st.Answers, capsule.ArchivedAnswer{QuestionID: "c"})
	if n := currentChainAnswers(st); n != 1 {
		t.Fatalf("a post-rewind answer counts, got %d", n)
	}
}

func TestShippedBundlesDeclareHumanGates(t *testing.T) {
	// Only steps whose text REQUIRES a human confirmation carry the flag:
	// the grills allow zero questions, so they must not.
	want := map[string][]string{
		"feature-full": {"grill-review"},
	}
	for bundle, ids := range map[string][]string{"feature-full": {"grill"}, "quick-fix": {"grill"}} {
		wf, err := LoadWorkflow(filepath.Join("..", "..", "..", "workflows", bundle))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if s := wf.step(id); s == nil || s.RequiresHumanAnswer {
				t.Fatalf("%s/%s allows zero questions and must not declare requires_human_answer", bundle, id)
			}
		}
	}
	for bundle, ids := range want {
		wf, err := LoadWorkflow(filepath.Join("..", "..", "..", "workflows", bundle))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if s := wf.step(id); s == nil || !s.RequiresHumanAnswer {
				t.Fatalf("%s/%s must declare requires_human_answer", bundle, id)
			}
		}
	}
}

func TestDeliveryContractStatesGuaranteesOnlyWhenTheyApply(t *testing.T) {
	const humanLine = "this step requires a human answer before completion: return `needs_input` first; a `completed` envelope without an answer in this round is rejected"
	plain := deliveryContract(promptParts{WIID: "wi", StepID: "a", SpawnN: 1})
	if strings.Contains(plain, humanLine) {
		t.Fatalf("plain step must not carry %q", humanLine)
	}
	hu := deliveryContract(promptParts{WIID: "wi", StepID: "g", SpawnN: 1, RequiresHumanAnswer: true})
	if !strings.Contains(hu, humanLine) {
		t.Fatalf("human-gate step must state the human-answer rule:\n%s", hu)
	}
}
