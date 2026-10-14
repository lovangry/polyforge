package driver

// d8_upstream_transient_test.go: the upstream-transient class of
// wi_seWTgHeKtaa8 (audit 2026-10-13, the #147 incident's follow-up).
// An upstream's OCCASIONAL return corruption — truncated tool-call
// arguments, an empty response, a dropped connection — is NOT a
// model-class death: the same model is retried under the D7 backoff,
// bounded at UpstreamTransientRetryBound; past the bound the attempt
// archives as a model-class death and switches candidates. Allowance /
// auth / model-name texts keep the immediate switch (the existing D7
// tests pin those); pure throttle texts keep their own class.

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// TestD8TransientTextsClassifyUpstreamTransient: the transient
// signatures (also on an error turn) classify as the upstream-transient
// cause, which never advances the candidate chain; the controls keep
// their classes.
func TestD8TransientTextsClassifyUpstreamTransient(t *testing.T) {
	for _, text := range []string{
		"Upstream tool call has incomplete or invalid JSON arguments",
		"tool call arguments truncated mid-stream",
		"empty response from the provider",
		"connection reset by peer while streaming",
		"upstream dropped the connection",
		"unexpected EOF",
	} {
		for _, turnErr := range []bool{false, true} {
			got, _, _ := classifyWaitFailure(&ExitedError{ExitCode: 1, LastError: text, TurnError: turnErr})
			if got != FailUpstreamTransient {
				t.Errorf("%q (turn error %v) classified %q, want %q", text, turnErr, got, FailUpstreamTransient)
			}
		}
		if got, _, _ := classifyWaitFailure(errWithText("read transcript: " + text)); got != FailUpstreamTransient {
			t.Errorf("startup %q classified %q, want %q", text, got, FailUpstreamTransient)
		}
	}
	// controls: the other classes keep their texts.
	if got, _, _ := classifyWaitFailure(&ExitedError{ExitCode: 1, LastError: "429 Too Many Requests"}); got != FailRateLimited {
		t.Errorf("a throttle must stay a throttle: %q", got)
	}
	if got, _, _ := classifyWaitFailure(&ExitedError{ExitCode: 1, LastError: "insufficient quota"}); got != FailExitModel {
		t.Errorf("an allowance problem must stay model-class: %q", got)
	}
	if got, _, _ := classifyWaitFailure(&ExitedError{ExitCode: 1, LastError: `model "x/y" not found`}); got != FailExitModel {
		t.Errorf("a model-name problem must stay model-class: %q", got)
	}
	if IsModelClass(FailUpstreamTransient) {
		t.Fatalf("an upstream transient must never advance the candidate pointer")
	}
}

// errWithText builds a plain error carrying the text (the startup-path
// classification input).
func errWithText(text string) error {
	return &textError{text}
}

type textError struct{ s string }

func (e *textError) Error() string { return e.s }

// TestD8TransientStreakResetsOnAnotherCause: the bound's ledger counts
// only the TRAILING consecutive transients.
func TestD8TransientStreakResetsOnAnotherCause(t *testing.T) {
	att := func(cause string) capsule.Attempt {
		return capsule.Attempt{Cause: CauseInfraFailure, FailureCause: cause}
	}
	cases := []struct {
		name string
		in   []capsule.Attempt
		want int
	}{
		{"none", nil, 0},
		{"one", []capsule.Attempt{att(FailUpstreamTransient)}, 1},
		{"two", []capsule.Attempt{att(FailUpstreamTransient), att(FailUpstreamTransient)}, 2},
		{"interrupted", []capsule.Attempt{att(FailUpstreamTransient), att(FailExitOther), att(FailUpstreamTransient)}, 1},
		{"trailing other", []capsule.Attempt{att(FailUpstreamTransient), att(FailExitModel)}, 0},
	}
	for _, tc := range cases {
		if got := upstreamTransientStreak(tc.in); got != tc.want {
			t.Errorf("%s: streak %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestD8TransientArchiveLedgerAndSwitch: three consecutive transients —
// the first two archive upstream_transient and retry the same model
// (counter untouched, backoff imposed, booked into T), the third
// archives exit_model_class and switches. The archive alone answers the
// remaining bound.
func TestD8TransientArchiveLedgerAndSwitch(t *testing.T) {
	var slept []time.Duration
	env := newSupEnv(t, func(c *Config) {
		c.Sleep = func(_ context.Context, d time.Duration) { slept = append(slept, d) }
	}, chain("s", "a", "b"))
	text := "Upstream tool call has incomplete or invalid JSON arguments"
	env.sp.script = []fakeEntry{
		exitErr(1, text, true),
		exitErr(1, text, true),
		exitErr(1, text, true),
		done(`{"ok":true}`),
	}
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	x := env.exec(t)
	if x.WorkflowStatus != StatusCompleted {
		t.Fatalf("the transient-bounded step must deliver: status %q (%q)", x.WorkflowStatus, x.BlockedReason)
	}
	st := x.Steps["s"]
	var causes []string
	for _, a := range st.Attempts {
		if a.Cause == CauseInfraFailure {
			causes = append(causes, a.FailureCause)
		}
	}
	want := []string{FailUpstreamTransient, FailUpstreamTransient, FailExitModel}
	if len(causes) != len(want) {
		t.Fatalf("archived failure causes %v, want %v", causes, want)
	}
	for i := range want {
		if causes[i] != want[i] {
			t.Fatalf("archived failure causes %v, want %v", causes, want)
		}
	}
	// the bound-crossing attempt is a model-class SWITCH: the blocking
	// counter starts the new candidate's fresh ledger (Count=0, the
	// existing switch rule), and the two in-bound retries never moved it.
	if st.Supervision != nil && st.Supervision.FailureCount != 0 {
		t.Fatalf("the in-bound retries must not move the blocking counter: %+v", st.Supervision)
	}
	// the two in-bound retries waited the D7 ladder; the switch did not.
	if len(slept) != 2 || slept[0] != RateLimitBackoffBase || slept[1] != 2*RateLimitBackoffBase {
		t.Fatalf("backoff waits %v, want the D7 ladder [1s 2s] for the two retries only", slept)
	}
}
