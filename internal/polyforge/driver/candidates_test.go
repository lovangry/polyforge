package driver

// candidates_test.go: the B-chain recomputation and the fault
// classification. The pinned tables are the policy: provider failures
// alone advance the pointer, a rewind resets it, the list end caps it —
// and host/operator/schema failures never move it.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCandidatePointer(t *testing.T) {
	prov := func() StepAttempt {
		return StepAttempt{Cause: CauseInfraFailure, FaultClass: FaultProvider}
	}
	cls := func(c FaultClass) StepAttempt {
		return StepAttempt{Cause: CauseInfraFailure, FaultClass: c}
	}
	rewind := StepAttempt{Cause: CauseGateFailRewind}

	cases := []struct {
		name       string
		attempts   []StepAttempt
		candidates []string
		want       int
	}{
		{"no chain", []StepAttempt{prov(), prov()}, nil, -1},
		{"no attempts", nil, []string{"m1", "m2", "m3"}, 0},
		{"first provider failure advances to 1", []StepAttempt{prov()}, []string{"m1", "m2"}, 1},
		{"two provider failures advance to 2", []StepAttempt{prov(), prov()}, []string{"m1", "m2", "m3"}, 2},
		{"cap at last candidate", []StepAttempt{prov(), prov(), prov(), prov(), prov()},
			[]string{"m1", "m2", "m3"}, 2},
		{"cap with a single candidate", []StepAttempt{prov(), prov()}, []string{"m1"}, 0},
		{"host failure never advances", []StepAttempt{cls(FaultHost)}, []string{"m1", "m2"}, 0},
		{"operator failure never advances", []StepAttempt{cls(FaultOperator)}, []string{"m1", "m2"}, 0},
		{"schema failure never advances", []StepAttempt{cls(FaultSchema)}, []string{"m1", "m2"}, 0},
		{"unclassified failure never advances", []StepAttempt{cls("")}, []string{"m1", "m2"}, 0},
		{"mixed classes count provider only",
			[]StepAttempt{cls(FaultHost), prov(), cls(FaultSchema), prov(), cls(FaultOperator), prov()},
			[]string{"m1", "m2", "m3", "m4"}, 3},
		{"rewind resets the count",
			[]StepAttempt{prov(), prov(), rewind, prov()},
			[]string{"m1", "m2", "m3"}, 1},
		{"rewind to head with no later failures",
			[]StepAttempt{prov(), prov(), prov(), rewind},
			[]string{"m1", "m2"}, 0},
		{"non-advancing failures after rewind keep the head",
			[]StepAttempt{prov(), rewind, cls(FaultHost), cls(FaultOperator)},
			[]string{"m1", "m2"}, 0},
		{"unknown cause ignored",
			[]StepAttempt{{Cause: "something_new"}, prov()},
			[]string{"m1", "m2"}, 1},
		{"rewind without class field still resets",
			[]StepAttempt{prov(), StepAttempt{Cause: CauseGateFailRewind, FaultClass: FaultProvider}, prov()},
			[]string{"m1", "m2"}, 1},
	}
	for _, tc := range cases {
		got := CandidatePointer(tc.attempts, tc.candidates)
		if got != tc.want {
			t.Errorf("%s: CandidatePointer = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCandidatePointerIsPure(t *testing.T) {
	// The recomputation must derive the same ordinal from the same
	// history twice — and must not mutate its input.
	attempts := []StepAttempt{
		{Cause: CauseInfraFailure, FaultClass: FaultProvider},
		{Cause: CauseGateFailRewind},
		{Cause: CauseInfraFailure, FaultClass: FaultProvider},
	}
	candidates := []string{"m1", "m2", "m3"}
	first := CandidatePointer(attempts, candidates)
	second := CandidatePointer(attempts, candidates)
	if first != second {
		t.Fatalf("recomputation drifted: %d then %d", first, second)
	}
	if attempts[1].FaultClass != "" || attempts[0].Cause != CauseInfraFailure {
		t.Fatalf("CandidatePointer must not mutate its input: %+v", attempts)
	}
}

func TestEffectiveModel(t *testing.T) {
	attempts := []StepAttempt{{Cause: CauseInfraFailure, FaultClass: FaultProvider}}
	if got := EffectiveModel(attempts, []string{"m1", "m2"}); got != "m2" {
		t.Fatalf("effective model after one provider failure: got %q want %q", got, "m2")
	}
	if got := EffectiveModel(attempts, nil); got != "" {
		t.Fatalf("no candidate chain must answer the empty default, got %q", got)
	}
	if got := EffectiveModel(nil, []string{"m1"}); got != "m1" {
		t.Fatalf("no attempts must answer the chain head, got %q", got)
	}
}

func TestPolicyFor(t *testing.T) {
	cases := []struct {
		class       FaultClass
		wantAdvance bool
		wantBlock   bool
		wantReport  bool
	}{
		{FaultProvider, true, false, false},
		{FaultHost, false, true, true},
		{FaultOperator, false, false, false},
		{FaultSchema, false, false, true},
		{FaultClass("mystery"), false, false, false},
	}
	for _, tc := range cases {
		p := PolicyFor(tc.class)
		if p.Advance != tc.wantAdvance || p.Block != tc.wantBlock || p.Report != tc.wantReport {
			t.Errorf("PolicyFor(%q) = %+v, want advance=%v block=%v report=%v",
				tc.class, p, tc.wantAdvance, tc.wantBlock, tc.wantReport)
		}
	}
}

func TestClassifyFault(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FaultClass
	}{
		// provider: upstream model-service failures.
		{"rate limited", errors.New("API error: rate limit exceeded (429)"), FaultProvider},
		{"overloaded", errors.New("provider overloaded, try again"), FaultProvider},
		{"http 503", errors.New("upstream returned 503 Service Unavailable"), FaultProvider},
		{"timeout", errors.New("request timed out after 120s"), FaultProvider},
		{"auth", errors.New("invalid api key"), FaultProvider},
		{"quota", errors.New("billing quota exhausted"), FaultProvider},
		{"network", errors.New("connection reset by peer"), FaultProvider},

		// host: the machine is broken.
		{"disk full", errors.New("write failed: disk full"), FaultHost},
		{"no space", errors.New("write /w/x: no space left on device"), FaultHost},
		{"tool missing", errors.New("exec: pi-exec: no such file or directory"), FaultHost},
		{"permission", errors.New("fork: permission denied"), FaultHost},
		{"oom", errors.New("agent killed: out of memory"), FaultHost},
		{"cgroup", errors.New("cgroup write failed"), FaultHost},

		// operator: an interruption is a stop, not a failure.
		{"interrupted", errors.New("step interrupted by operator"), FaultOperator},
		{"cancelled", errors.New("run canceled at operator request"), FaultOperator},
		{"paused", errors.New("work item paused"), FaultOperator},
		{"signal", errors.New("agent terminated by sigterm"), FaultOperator},
		{"stop notice", errors.New("stop notice received"), FaultOperator},

		// schema: the result contract is broken.
		{"schema violation", errors.New("output schema wants object, got JSON string"), FaultSchema},
		{"validation", errors.New("artifact validation failed: sha256 mismatch"), FaultSchema},
		{"missing output", errors.New("declared output artifacts/fix-summary.md not delivered"), FaultSchema},
		{"manifest", errors.New("manifest entry has no artifact"), FaultSchema},

		// sentinels beat substring order.
		{"context canceled", fmt.Errorf("step wait: %w", context.Canceled), FaultOperator},
		{"canceled under noise", fmt.Errorf("rate limit noise around: %w", context.Canceled), FaultOperator},
		{"deadline exceeded", fmt.Errorf("model call: %w", context.DeadlineExceeded), FaultProvider},

		// unmatched is UNKNOWN: it never advances the candidate pointer
		// (the old provider default made every daemon restart switch
		// models — v2-converge 2.3).
		{"unknown", errors.New("something completely different"), ""},
		{"daemon restart cause text", errors.New("agent process exited"), ""},
	}
	for _, tc := range cases {
		if got := ClassifyFault(tc.err); got != tc.want {
			t.Errorf("%s: ClassifyFault(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
	if got := ClassifyFault(nil); got != "" {
		t.Fatalf("nil error must classify to the empty class, got %q", got)
	}
}

func TestClassifyFaultPrecedence(t *testing.T) {
	// Operator wins over every infra reading: an explicit stop is never
	// reinterpreted as a failure of the machine or the provider.
	if got := ClassifyFault(errors.New("run interrupted: disk full")); got != FaultOperator {
		t.Fatalf("operator must outrank host, got %q", got)
	}
	// Schema names its own domain before host/provider readings of the
	// same message.
	if got := ClassifyFault(errors.New("artifact schema check hit rate limit guard")); got != FaultSchema {
		t.Fatalf("schema must outrank provider, got %q", got)
	}
	// Host outranks provider: a disk-full write inside a provider call
	// is still a broken machine.
	if got := ClassifyFault(errors.New("provider stream: write: no space left on device")); got != FaultHost {
		t.Fatalf("host must outrank provider, got %q", got)
	}
}
