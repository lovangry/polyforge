package driver

// release_verdict_test.go: the release verdict binding — the mechanical
// half of the release bundle's verdict vocabularies. `release` is closed
// [pass, fail]: pass owes a non-empty version plus at least one
// artifacts_published entry, fail owes a non-empty fail_reason, and defer
// is rejected (a release WI without a procedure is an error). `deploy` is
// [pass, defer, fail]: pass owes a non-empty version, defer owes neither
// version nor artifacts_published. A defer verdict on any OTHER step is a
// validation error. The gate binding stays prefix-fail only, so a defer
// verdict clears the deploy gate while fail re-binds to gate_failed.

import (
	"encoding/json"
	"strings"
	"testing"
)

// releaseRes builds a minimal valid envelope for one outcome/output pair,
// bound to the given step.
func releaseRes(step string, outcome Outcome, output string) *StepResult {
	return &StepResult{
		ProtocolVersion: 1,
		ResultID:        "22222222-2222-4222-8222-222222222222",
		WIID:            "wi_run",
		StepID:          step,
		SpawnN:          1,
		Outcome:         outcome,
		Output:          json.RawMessage(output),
	}
}

func TestValidateStepResultReleaseVerdictBinding(t *testing.T) {
	gate := func(step string) ResultSpec {
		return ResultSpec{WIID: "wi_run", StepID: step, SpawnN: 1, Gate: true}
	}
	type tcase struct {
		name        string
		spec        ResultSpec
		res         *StepResult
		wantOutcome Outcome // the outcome the envelope must carry after validation
		wantErr     bool    // envelope rejected
		wantErrSub  string  // substring the rejection must carry
	}
	cases := []tcase{
		{
			name:        "defer on deploy with no version/artifacts validates and does not gate-fail",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"defer"}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on deploy with explicit empty fields validates",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"defer","version":"","artifacts_published":[]}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on deploy carrying a version is rejected",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"defer","version":"v1.2.3"}`),
			wantErr:     true,
			wantErrSub:  "owes neither version nor artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on release is rejected (a release WI without a procedure is an error)",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"defer"}`),
			wantErr:     true,
			wantErrSub:  "defer validates nowhere but the deploy step",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "defer on a non-release step is rejected",
			spec:        gate("verification"),
			res:         releaseRes("verification", OutcomeCompleted, `{"verdict":"defer"}`),
			wantErr:     true,
			wantErrSub:  "defer validates nowhere but the deploy step",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "release fail with fail_reason re-binds to gate_failed",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"fail","fail_reason":"PF-RELEASE-AMBIGUOUS_OWNER: a, b"}`),
			wantOutcome: OutcomeGateFailed,
		},
		{
			name:        "deploy fail with fail_reason re-binds to gate_failed (rollback = step FAIL)",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"fail","fail_reason":"ROLLED BACK to v1.0.0"}`),
			wantOutcome: OutcomeGateFailed,
		},
		{
			name:        "fail without fail_reason is rejected (release)",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"fail"}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty fail_reason",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "fail with a blank fail_reason is rejected (deploy)",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"fail","fail_reason":"   "}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty fail_reason",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "release pass with version and artifacts validates and stays completed",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"pass","version":"v1.6.0","artifacts_published":["tag:v1.6.0","image:ghcr.io/x/y:v1.6.0"]}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "release pass without a version is rejected",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"pass","artifacts_published":["x"]}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty version and at least one artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "release pass without artifacts is rejected",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"pass","version":"v1.6.0"}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty version and at least one artifacts_published",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "deploy pass with a version validates (no artifacts owed)",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"pass","version":"v1.6.0"}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "deploy pass without a version is rejected",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"pass"}`),
			wantErr:     true,
			wantErrSub:  "requires a non-empty version",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "an unknown verdict on release is rejected (closed vocabulary)",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{"verdict":"excellent"}`),
			wantErr:     true,
			wantErrSub:  "outside the closed release vocabulary",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "an unknown verdict on deploy is rejected (closed vocabulary)",
			spec:        gate("deploy"),
			res:         releaseRes("deploy", OutcomeCompleted, `{"verdict":"excellent"}`),
			wantErr:     true,
			wantErrSub:  "outside the closed deploy vocabulary",
			wantOutcome: OutcomeCompleted,
		},
		{
			name:        "a release envelope with no verdict at all is untouched (no output, no binding)",
			spec:        gate("release"),
			res:         releaseRes("release", OutcomeCompleted, `{}`),
			wantOutcome: OutcomeCompleted,
		},
		{
			// A gate_failed envelope riding a FAIL verdict is delivered
			// non-completed — the release binding never judges it.
			name:        "a release gate_failed envelope carrying FAIL is not this binding's business",
			spec:        gate("release"),
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
