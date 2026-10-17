package driver

// guarantees_test.go: the three mechanical driver guarantees (v2 §2.3) —
// needs_input envelope validation, the human-gate guarantee (#138) and
// the V4 verification-evidence gate — plus the retry-feedback and
// evidence-decoding plumbing they ride on.

import (
	"context"
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

// rawEvidence encodes evidence entries as the envelope carries them.
func rawEvidence(ev []CommandEvidence) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range ev {
		b, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		out = append(out, b)
	}
	return out
}

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

// D11: the agent-chosen question id must have the accepted shape; a
// malformed id makes the envelope invalid (the in-process retry lane).
func TestNeedsInputQuestionIDShape(t *testing.T) {
	for _, ok := range []string{"q1", "q_7f3a1c", "3f2b8c1e-5d4a-4f6b-9a7c-2e1d0b9a8f7e", "grill-review-wi_0123456789abcdef-d5-confirm", "a.b-c_D", strings.Repeat("x", 64)} {
		f := newFixture(t, false)
		f.res.Outcome, f.res.Output = OutcomeNeedsInput, nil
		f.res.QuestionID, f.res.Question = ok, "why?"
		if err := ValidateStepResult(f.res, f.spec); err != nil {
			t.Fatalf("question id %q must validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"q 1", "q/../x", "q\nfake line", "[polyforge]", "q;rm", "é", strings.Repeat("x", 65)} {
		f := newFixture(t, false)
		f.res.Outcome, f.res.Output = OutcomeNeedsInput, nil
		f.res.QuestionID, f.res.Question = bad, "why?"
		if err := ValidateStepResult(f.res, f.spec); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("question id %q must be rejected as malformed, got %v", bad, err)
		}
		// embedded in the output object: same rule
		g := newFixture(t, false)
		g.res.Outcome = OutcomeNeedsInput
		blob, _ := json.Marshal(map[string]string{"question_id": bad, "question": "why?"})
		g.res.Output = blob
		if err := ValidateStepResult(g.res, g.spec); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("embedded question id %q must be rejected as malformed, got %v", bad, err)
		}
	}
}

func TestHumanGateGuarantee(t *testing.T) {
	f := newFixture(t, false)
	f.spec.RequiresHumanAnswer = true
	err := ValidateStepResult(f.res, f.spec)
	if err == nil || !strings.Contains(err.Error(), "requires a human answer: return needs_input first") {
		t.Fatalf("completed without a human answer must be rejected, got %v", err)
	}
	// V-8 (owner ruling): a native gate_failed cannot dodge the human
	// either — no rewind or terminal fail without an answer.
	gf := *f.res
	gf.Outcome = OutcomeGateFailed
	if err := ValidateStepResult(&gf, f.spec); err == nil || !strings.Contains(err.Error(), "requires a human answer: return needs_input first") {
		t.Fatalf("gate_failed without a human answer must be rejected, got %v", err)
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
		f.res.CommandEvidence = rawEvidence(ev)
		err := ValidateStepResult(f.res, f.spec)
		if err == nil || !strings.Contains(err.Error(), "verification evidence") {
			t.Fatalf("%s: want evidence rejection, got %v", name, err)
		}
	}
	f.res.CommandEvidence = rawEvidence(goodEvidence())
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("complete evidence must validate: %v", err)
	}
	// a step without the flag owes nothing.
	g := newFixture(t, false)
	if err := ValidateStepResult(g.res, g.spec); err != nil {
		t.Fatal(err)
	}
}

// The command-evidence duty comes from the declaration alone (decision
// 2026-10-16 G10): the step id "verification" implies nothing.
func TestCommandEvidenceFromDeclarationOnly(t *testing.T) {
	d := &Driver{wi: WIInfo{ID: "wi_1"}, workflow: &Workflow{Steps: []StepSpec{{ID: "verification"}, {ID: "check", RequiresCommandEvidence: true}}}, exec: &capsule.Execution{Steps: map[string]*capsule.Step{}}}
	if d.buildResultSpec("verification", 1).RequireCommandEvidence {
		t.Fatal("an undeclared verification step must not owe evidence by its id")
	}
	if !d.buildResultSpec("check", 1).RequireCommandEvidence {
		t.Fatal("a declared step must owe evidence")
	}
	wf, err := parseWorkflow([]byte("steps:\n  - {id: a, requires_human_answer: true}\n  - {id: b, requires_command_evidence: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !wf.Steps[0].RequiresHumanAnswer || wf.Steps[0].RequiresCommandEvidence || !wf.Steps[1].RequiresCommandEvidence {
		t.Fatalf("frontmatter fields not parsed: %+v", wf.Steps)
	}
}

// requires_human_answer takes true/false or "if_asked" (polyforge#164
// H11); any other value is a parse error, never a silent false.
func TestRequiresHumanAnswerIfAskedParse(t *testing.T) {
	wf, err := parseWorkflow([]byte("steps:\n  - {id: a, requires_human_answer: if_asked}\n  - {id: b, requires_human_answer: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !wf.Steps[0].HumanAnswerIfAsked || wf.Steps[0].RequiresHumanAnswer || wf.Steps[1].HumanAnswerIfAsked || wf.Steps[1].RequiresHumanAnswer {
		t.Fatalf("if_asked not parsed: %+v", wf.Steps)
	}
	if _, err := parseWorkflow([]byte("steps:\n  - {id: a, requires_human_answer: sometimes}\n")); err == nil {
		t.Fatal("an unknown requires_human_answer value must be refused")
	}
}

func TestShippedBundlesDeclareHumanGates(t *testing.T) {
	// Only steps whose text REQUIRES a human confirmation carry the flag:
	// the grills allow zero questions, so they must not. autopilot grill
	// (the lane's only checkpoint) declares if_asked (polyforge#164 H11).
	want := map[string][]string{
		"feature-full": {"grill-review"},
	}
	qf, err := LoadWorkflow(filepath.Join("..", "..", "..", "workflows", "autopilot"))
	if err != nil {
		t.Fatal(err)
	}
	if s := qf.step("grill"); s == nil || !s.HumanAnswerIfAsked || s.RequiresHumanAnswer {
		t.Fatalf("autopilot/grill must declare requires_human_answer: if_asked: %+v", s)
	}
	for bundle, ids := range map[string][]string{"feature-full": {"grill"}, "autopilot": {"grill"}} {
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

// A command_evidence list holding a non-object item still collects (it
// is valid JSON, so never a dispatch defect) and is then rejected by the
// envelope gate, on any step, naming the item — the rejection feeds the
// in-process retry lane.
func TestNonObjectEvidenceCollectsThenIsRejected(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "1.json")
	raw := `{"outcome":"completed","output":{},
	 "command_evidence":["ran go test", {"argv":["go","test"],"exit_code":0,"duration_ms":42}]}`
	if err := os.WriteFile(drop, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &stepRun{dropPath: drop, sp: StepSpawn{StepID: "verification", SpawnN: 1}, wiID: "wi"}
	res, err := r.collect()
	if err != nil || res == nil {
		t.Fatalf("collect: %v %v", res, err)
	}
	if len(res.CommandEvidence) != 2 {
		t.Fatalf("every item is kept verbatim: %s", res.CommandEvidence)
	}
	for _, owes := range []bool{true, false} {
		spec := ResultSpec{WIID: "wi", StepID: "verification", SpawnN: 1, RequireCommandEvidence: owes}
		if err := ValidateStepResult(res, spec); err == nil || !strings.Contains(err.Error(), "command_evidence[0] is not an OBJECT") {
			t.Fatalf("owes=%v: a string item must be rejected by name, got %v", owes, err)
		}
	}
}

// The destination of string evidence: the step is re-dispatched with the
// rejection as feedback and a well-formed rewrite is accepted; an author
// that keeps writing strings blocks the step result_invalid.
func TestRunStringEvidenceRejectedThenRewritten(t *testing.T) {
	stringEv := func() fakeEntry {
		e := done(`{}`)
		e.res.CommandEvidence = []json.RawMessage{json.RawMessage(`"ran go test"`)}
		return e
	}
	good := done(`{}`)
	good.res.CommandEvidence = rawEvidence(goodEvidence())
	d, sp, _ := runEnv(t, StepSpec{ID: "verification", ContextPolicy: "fresh", RequiresCommandEvidence: true})
	sp.script = []fakeEntry{stringEv(), good}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fb := sp.spawnAt(1).PriorRejection; !strings.Contains(fb, "is not an OBJECT") {
		t.Fatalf("the retry must carry the rejection: %q", fb)
	}
	if x := d.Snapshot(); x.WorkflowStatus != StatusCompleted {
		t.Fatalf("the well-formed rewrite must be accepted: %s", x.WorkflowStatus)
	}

	d, sp, _ = runEnv(t, StepSpec{ID: "verification", ContextPolicy: "fresh", RequiresCommandEvidence: true})
	for i := 0; i <= maxEnvelopeRejects; i++ {
		sp.script = append(sp.script, stringEv())
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if x := d.Snapshot(); x.WorkflowStatus != StatusBlocked || !strings.Contains(x.BlockedReason, "result_invalid") {
		t.Fatalf("status %q reason %q, want blocked with result_invalid", x.WorkflowStatus, x.BlockedReason)
	}
}

func TestDeliveryContractStatesGuaranteesOnlyWhenTheyApply(t *testing.T) {
	const evLine = "Command evidence: this step requires `command_evidence`"
	const humanLine = "this step requires a human answer before completion: return `needs_input` first; a `completed` or `gate_failed` envelope without an answer in this round is rejected"
	plain := deliveryContract(promptParts{WIID: "wi", StepID: "a", SpawnN: 1})
	for _, l := range []string{evLine, humanLine, `"command_evidence"`} {
		if strings.Contains(plain, l) {
			t.Fatalf("plain step must not carry %q", l)
		}
	}
	ev := deliveryContract(promptParts{WIID: "wi", StepID: "verification", SpawnN: 1, RequireCommandEvidence: true})
	if !strings.Contains(ev, evLine) || !strings.Contains(ev, `"duration_ms": 1200`) || !strings.Contains(ev, "(> 0)") {
		t.Fatalf("evidence step must state the object shape and rule:\n%s", ev)
	}
	if strings.Contains(ev, humanLine) {
		t.Fatal("evidence step must not carry the human-answer line")
	}
	hu := deliveryContract(promptParts{WIID: "wi", StepID: "g", SpawnN: 1, RequiresHumanAnswer: true})
	if !strings.Contains(hu, humanLine) || strings.Contains(hu, evLine) {
		t.Fatalf("human-gate step must state only the human-answer rule:\n%s", hu)
	}
}

func TestEvidenceRejectionNamesBadStartedAt(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "1.json")
	raw := `{"outcome":"completed","output":{},
	 "command_evidence":[{"argv":["go","test"],"exit_code":0,"duration_ms":42,"started_at":"yesterday"}]}`
	if err := os.WriteFile(drop, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &stepRun{dropPath: drop, sp: StepSpawn{StepID: "verification", SpawnN: 1}, wiID: "wi"}
	res, err := r.collect()
	if err != nil || res == nil {
		t.Fatalf("collect: %v %v", res, err)
	}
	spec := ResultSpec{WIID: "wi", StepID: "verification", SpawnN: 1, RequireCommandEvidence: true}
	err = ValidateStepResult(res, spec)
	if err == nil || !strings.Contains(err.Error(), "started_at must be RFC3339") || !strings.Contains(err.Error(), "command_evidence[0] could not be decoded") {
		t.Fatalf("rejection must name the real reason, got %v", err)
	}
}
