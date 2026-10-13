package driver

// result_test.go: the staging gates over the result envelope. The
// pinned vectors double as the contract documentation: fencing, the
// needs_input question pair, the gate verdict binding, and the MINIMAL
// CONTRACT (polyforge#147) — an envelope missing the retired protocol
// fields is never judged invalid for its absence; there is no protocol
// stamp, no envelope id, no artifact manifest and no handoff digest
// anywhere.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fixture is one assembled validation scene: a spec and a matching
// completed result with an output object.
type fixture struct {
	spec ResultSpec
	res  *StepResult
}

func newFixture(t *testing.T, handoff bool) *fixture {
	t.Helper()
	f := &fixture{}
	f.spec = ResultSpec{
		WIID:    "wi_17",
		StepID:  "fix",
		SpawnN:  3,
		Outputs: []OutputSpec{{Path: "artifacts/fix-summary.md", Schema: "object"}},
	}
	f.res = &StepResult{
		WIID:    "wi_17",
		StepID:  "fix",
		SpawnN:  3,
		Outcome: OutcomeCompleted,
		Output:  json.RawMessage(`{"summary":"ok","lines":2}`),
	}
	_ = handoff // the handoff duty is a path-convention read; the envelope carries no digest reference
	return f
}

func TestValidateStepResultCompleted(t *testing.T) {
	f := newFixture(t, true)
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("completed result with declared output must validate: %v", err)
	}
}

func TestValidateStepResultShape(t *testing.T) {
	f := newFixture(t, false)

	if err := ValidateStepResult(nil, f.spec); err == nil {
		t.Fatal("nil result must be rejected")
	}

	// The minimal contract: an envelope with ONLY the outcome + output
	// pair and the identity/fence fields validates — the retired
	// protocol fields are not required, and their absence is never an
	// error (polyforge#147).
	res := *f.res
	if err := ValidateStepResult(&res, f.spec); err != nil {
		t.Fatalf("minimal-contract envelope must validate: %v", err)
	}

	res = *f.res
	res.Outcome = Outcome("vibes")
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("unknown outcome must be rejected")
	}
}

func TestValidateStepResultBindingAndFence(t *testing.T) {
	f := newFixture(t, false)

	res := *f.res
	res.WIID = "wi_999"
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("foreign wi_id must fence with ErrIdentityMismatch, got %v", err)
	}
	res = *f.res
	res.StepID = "grill"
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("foreign step_id must fence with ErrIdentityMismatch, got %v", err)
	}

	// The fence: a result from a superseded spawn (stale spawn_n) is
	// rejected even though everything else about it is perfect.
	res = *f.res
	res.SpawnN = f.spec.SpawnN - 1
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrStaleSpawn) {
		t.Fatalf("stale spawn_n must fence with ErrStaleSpawn, got %v", err)
	}
	res.SpawnN = f.spec.SpawnN + 1
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrStaleSpawn) {
		t.Fatalf("future spawn_n must fence with ErrStaleSpawn, got %v", err)
	}
}

func TestValidateStepResultNonCompletedOutcomes(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeGateFailed, OutcomeNeedsInput, OutcomeIncomplete} {
		f := newFixture(t, false)
		f.res.Outcome = outcome
		// Declared outputs are NOT enforced for non-completed outcomes —
		// and the envelope carries none.
		f.res.Output = nil
		if outcome == OutcomeNeedsInput {
			f.res.QuestionID, f.res.Question = "q1", "which one?" // a needs_input envelope owes its question
		}
		if err := ValidateStepResult(f.res, f.spec); err != nil {
			t.Fatalf("%s result without outputs must validate: %v", outcome, err)
		}
	}
	// A needs_input envelope without its question pair is rejected.
	f := newFixture(t, false)
	f.res.Outcome = OutcomeNeedsInput
	if err := ValidateStepResult(f.res, f.spec); err == nil {
		t.Fatal("needs_input without question_id/question must be rejected")
	}
}

func TestValidateStepResultDeclaredOutputs(t *testing.T) {
	f := newFixture(t, false)

	// The output JSON violates the declared object schema.
	f.res.Output = json.RawMessage(`["not","an","object"]`)
	if err := ValidateStepResult(f.res, f.spec); err == nil {
		t.Fatal("completed result whose output violates the declared schema must be rejected")
	}

	// No declared outputs at all: unconstrained.
	f.spec.Outputs = nil
	f.res.Output = json.RawMessage(`"anything goes"`)
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("step without declared outputs must not constrain output: %v", err)
	}
}

func TestValidateOutputSchema(t *testing.T) {
	cases := []struct {
		name   string
		output string
		schema string
		ok     bool
	}{
		{"object yes", `{"a":1}`, "object", true},
		{"object no-array", `[1]`, "object", false},
		{"object no-string", `"x"`, "object", false},
		{"object no-null", `null`, "object", false},
		{"object no-empty", ``, "object", false},
		{"array yes", `[1,2]`, "array", true},
		{"array no", `{"a":1}`, "array", false},
		{"string yes", `"text"`, "string", true},
		{"string no", `1`, "string", false},
		{"number int yes", `42`, "number", true},
		{"number float yes", `42.5`, "number", true},
		{"number no-quoted", `"42"`, "number", false},
		{"boolean yes", `true`, "boolean", true},
		{"boolean no", `"true"`, "boolean", false},
		{"any json", `[{"x":1}]`, "any", true},
		{"empty unconstrained", ``, "", true},
		{"unconstrained valid json", `{}`, "", true},
		{"unconstrained invalid json", `{[`, "", false},
		{"typo rejected", `{}`, "objects", false},
	}
	for _, tc := range cases {
		err := ValidateOutputSchema(json.RawMessage(tc.output), tc.schema)
		if tc.ok && err != nil {
			t.Errorf("%s: schema %q over %s: unexpected error %v", tc.name, tc.schema, tc.output, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: schema %q over %s: must be rejected", tc.name, tc.schema, tc.output)
		}
	}
}

func TestStepResultJSONRoundTrip(t *testing.T) {
	// Pin the envelope's wire tags: the staging side persists result
	// envelopes verbatim, so a renamed tag is a protocol break. The
	// MINIMAL CONTRACT (polyforge#147): identity/fence + outcome/output
	// (+ the needs_input question pair); no retired protocol fields.
	f := newFixture(t, false)
	f.res.Outcome = OutcomeNeedsInput
	f.res.QuestionID = "q_hand"
	f.res.Question = "which variant?"
	f.res.CommandEvidence = []CommandEvidence{{
		Argv:       []string{"go", "test", "./..."},
		Cwd:        "/w/repo",
		ExitCode:   0,
		StartedAt:  time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
		DurationMs: 4242,
		LogPath:    "logs/fix.log",
	}}
	raw, err := json.Marshal(f.res)
	if err != nil {
		t.Fatal(err)
	}
	var back StepResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.WIID != f.res.WIID || back.StepID != f.res.StepID || back.SpawnN != f.res.SpawnN ||
		back.Outcome != f.res.Outcome {
		t.Fatalf("round trip lost envelope binding: %+v", back)
	}
	if back.QuestionID != "q_hand" || back.Question != "which variant?" {
		t.Fatalf("round trip lost the question pair: %+v", back)
	}
	if len(back.CommandEvidence) != 1 || back.CommandEvidence[0].DurationMs != 4242 ||
		len(back.CommandEvidence[0].Argv) != 3 {
		t.Fatalf("round trip lost command evidence: %+v", back.CommandEvidence)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"wi_id", "step_id", "spawn_n", "outcome", "output",
		"question_id", "question", "command_evidence", "submitted_at"} {
		if _, ok := probe[tag]; !ok {
			t.Fatalf("envelope JSON must carry tag %q", tag)
		}
	}
	// The retired protocol fields must not reappear on the wire.
	for _, tag := range []string{"protocol_version", "result_id", "artifacts", "handoff_ref"} {
		if _, ok := probe[tag]; ok {
			t.Fatalf("minimal-contract envelope must not carry retired tag %q", tag)
		}
	}
}
