package driver

// release_verdict_test.go: the release verdict binding (polyforge#123) —
// the mechanical half of the extended release verdict vocabulary
// [pass, defer, fail]. A completed envelope for the release step is judged
// against the closed vocabulary: pass owes a non-empty version plus at
// least one artifacts_published entry, fail owes a non-empty fail_reason,
// defer owes neither version nor artifacts_published. A defer verdict on
// any OTHER step is a validation error: defer validates nowhere but the
// release step. The gate binding stays prefix-fail only, so a defer
// verdict clears the release gate (no rewind) while fail still re-binds to
// gate_failed and fires the on_fail rewind.

import (
	"encoding/json"
	"strings"
	"testing"
)

// releaseRes builds a minimal valid envelope for one outcome/output pair,
// bound to the given step.
func releaseRes(step string, outcome Outcome, output string) *StepResult {
	return &StepResult{
		WIID:    "wi_run",
		StepID:  step,
		SpawnN:  1,
		Outcome: outcome,
		Output:  json.RawMessage(output),
	}
}

func TestValidateStepResultReleaseVerdictBinding(t *testing.T) {
	cases := []struct {
		name        string
		spec        ResultSpec
		res         *StepResult
		wantOutcome Outcome // the outcome the envelope must carry after validation
		wantErr     bool    // envelope rejected
		wantErrSub  string  // substring the rejection must carry
	}{
		{
			name:        "defer on release with no version/artifacts validates and does not gate-fail",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"defer"}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on release with explicit empty fields validates",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"defer","version":"","artifacts_published":[]}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on release carrying a version is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"defer","version":"v1.2.3"}`),
			wantErr:     true,
			wantErrSub:  "owes neither version nor artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on release carrying artifacts is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"defer","artifacts_published":["releases/v1.2.3/pf"]}`),
			wantErr:     true,
			wantErrSub:  "owes neither version nor artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on a non-release step is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "verification", SpawnN: 1, Gate: true},
			res:         releaseRes("verification", OutcomeCompleted, `{"verdict":"defer"}`),
			wantErr:     true,
			wantErrSub:  "defer validates nowhere but the release step",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "fail with fail_reason re-binds to gate_failed (the rewind contract)",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"fail","fail_reason":"the publish claim hit a taken version"}`),
			wantOutcome: OutcomeGateFailed,
		},
		{
			name:        "fail without fail_reason is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"fail"}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty fail_reason",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "fail with a blank fail_reason is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"fail","fail_reason":"   "}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty fail_reason",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "pass with version and artifacts validates and stays completed",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"pass","version":"v1.6.0","artifacts_published":["releases/v1.6.0/aihub","releases/v1.6.0/pf"]}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "pass without a version is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"pass","artifacts_published":["x"]}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty version and at least one artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "pass without artifacts is rejected",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"pass","version":"v1.6.0"}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty version and at least one artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "an unknown verdict on release is rejected (closed vocabulary)",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"excellent"}`),
			wantErr:     true,
			wantErrSub:  "outside the closed release vocabulary",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "a release envelope with no verdict at all is untouched (no output, no binding)",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeCompleted, `{}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			// The s22/w079 shape: a gate_failed envelope riding a FAIL verdict
			// is delivered non-completed — the release binding never judges it.
			name:        "a release gate_failed envelope carrying FAIL is not this binding's business",
			spec:        ResultSpec{WIID: "wi_run", StepID: "release", SpawnN: 1, Gate: true},
			res:         releaseRes("release", OutcomeGateFailed, `{"verdict":"FAIL","error":"rewinding"}`),
			wantOutcome: OutcomeGateFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStepResult(tc.res, tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("envelope must be rejected")
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("rejection must name the reason %q: %v", tc.wantErrSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("envelope must validate: %v", err)
			}
			if tc.res.Outcome != tc.wantOutcome {
				t.Fatalf("outcome after validation = %q, want %q", tc.res.Outcome, tc.wantOutcome)
			}
		})
	}
}
