package driver

// guarantees_test.go: the three mechanical driver guarantees (v2 §2.3) —
// needs_input envelope validation, the human-gate guarantee (#138) and
// the V4 verification-evidence gate — plus the retry-feedback and
// tolerant-re-parse plumbing they ride on.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

func goodEvidence() []CommandEvidence {
	return []CommandEvidence{{Argv: []string{"go", "test", "./..."}, ExitCode: 0, DurationMs: 1200}}
}

func TestNeedsInputEnvelopeValidation(t *testing.T) {
	mk := func() *fixture {
		f := newFixture(t, false)
		f.res.Outcome = OutcomeNeedsInput
		f.res.Artifacts, f.res.Output = nil, nil
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
	q.Outcome, q.QuestionID, q.Question, q.Artifacts, q.Output = OutcomeNeedsInput, "q1", "ok?", nil, nil
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

func TestVerificationEvidenceGate(t *testing.T) {
	f := newFixture(t, false)
	f.spec.RequireCommandEvidence = true
	bad := map[string][]CommandEvidence{
		"none":         nil,
		"empty argv":   {{ExitCode: 0, DurationMs: 5}},
		"blank argv":   {{Argv: []string{" "}, DurationMs: 5}},
		"no duration":  {{Argv: []string{"go", "test"}}},
		"one bad of 2": append(goodEvidence(), CommandEvidence{Argv: []string{"x"}}),
	}
	for name, ev := range bad {
		f.res.CommandEvidence = ev
		err := ValidateStepResult(f.res, f.spec)
		if err == nil || !strings.Contains(err.Error(), "verification evidence") {
			t.Fatalf("%s: want evidence rejection, got %v", name, err)
		}
	}
	f.res.CommandEvidence = goodEvidence()
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("complete evidence must validate: %v", err)
	}
	// a step without the flag owes nothing.
	g := newFixture(t, false)
	if err := ValidateStepResult(g.res, g.spec); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationEvidenceFromStepIDAndField(t *testing.T) {
	d := &Driver{wi: WIInfo{ID: "wi_1"}, exec: &capsule.Execution{Steps: map[string]*capsule.Step{}}}
	if !d.buildResultSpec("verification", 1).RequireCommandEvidence {
		t.Fatal("step id verification must require evidence")
	}
	if d.buildResultSpec("fix", 1).RequireCommandEvidence {
		t.Fatal("other steps must not")
	}
	wf, err := ParseDriverWorkflow([]byte("steps:\n  - {id: a, requires_human_answer: true}\n  - {id: b, requires_command_evidence: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !wf.Steps[0].RequiresHumanAnswer || wf.Steps[0].RequiresCommandEvidence || !wf.Steps[1].RequiresCommandEvidence {
		t.Fatalf("frontmatter fields not parsed: %+v", wf.Steps)
	}
}

func TestShippedBundlesDeclareHumanGates(t *testing.T) {
	want := map[string][]string{
		"feature-full": {"grill", "grill-review"},
		"quick-fix":    {"grill"},
	}
	for bundle, ids := range want {
		wf, err := LoadDriverWorkflow(filepath.Join("..", "..", "..", "workflows", bundle))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if s := wf.StepByID(id); s == nil || !s.RequiresHumanAnswer {
				t.Fatalf("%s/%s must declare requires_human_answer", bundle, id)
			}
		}
	}
}

func TestTolerantReparseKeepsObjectEvidence(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "1.json")
	raw := `{"protocol_version":1,"result_id":"r1","outcome":"completed","output":{},
	 "command_evidence":["ran go test", {"argv":["go","test"],"exit_code":0,"duration_ms":42}]}`
	if err := os.WriteFile(drop, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &stepRun{dropPath: drop, sp: StepSpawn{StepID: "verification", SpawnN: 1}, wiID: "wi"}
	res, err := r.collect()
	if err != nil || res == nil {
		t.Fatalf("collect: %v %v", res, err)
	}
	if len(res.CommandEvidence) != 1 || res.CommandEvidence[0].DurationMs != 42 {
		t.Fatalf("object evidence entry must survive the tolerant re-parse: %+v", res.CommandEvidence)
	}
	if err := checkCommandEvidence(res.CommandEvidence); err != nil {
		t.Fatal(err)
	}
	// all-strings: dropped, and the gate then rejects with the object hint.
	if err := os.WriteFile(drop, []byte(`{"protocol_version":1,"result_id":"r1","outcome":"completed","output":{},"command_evidence":["x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = r.collect()
	if err != nil || len(res.CommandEvidence) != 0 {
		t.Fatalf("string-only evidence must be dropped: %+v %v", res, err)
	}
	if err := checkCommandEvidence(res.CommandEvidence); err == nil || !strings.Contains(err.Error(), "OBJECT") {
		t.Fatalf("got %v", err)
	}
}
