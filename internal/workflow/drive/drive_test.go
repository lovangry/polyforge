package drive

import (
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

func stepWith(causes ...string) *capsule.Step {
	st := &capsule.Step{}
	for i, c := range causes {
		st.Attempts = append(st.Attempts, capsule.Attempt{Cause: c, AttemptSeq: i + 1})
	}
	return st
}

func TestCandidateOrdinalBoundedWalk(t *testing.T) {
	cases := []struct {
		name    string
		causes  []string
		listLen int
		want    int
	}{
		{"first dispatch, no archive", nil, 3, 1},
		{"one infra failure advances one notch", []string{CauseInfraFailure}, 3, 2},
		{"two infra failures", []string{CauseInfraFailure, CauseInfraFailure}, 3, 3},
		{"bounded by list length (stays at last)", []string{CauseInfraFailure, CauseInfraFailure, CauseInfraFailure}, 2, 2},
		{"rewind archive resets the count", []string{CauseInfraFailure, CauseGateFailRewind, CauseInfraFailure}, 3, 2},
		{"count restarts after last rewind", []string{CauseInfraFailure, CauseInfraFailure, CauseGateFailRewind}, 3, 1},
		{"no-candidate fallback: listLen 0", nil, 0, 0},
		{"no-candidate fallback even with archive", []string{CauseInfraFailure}, 0, 0},
	}
	for _, c := range cases {
		if got := CandidateOrdinal(stepWith(c.causes...), c.listLen); got != c.want {
			t.Fatalf("%s: CandidateOrdinal = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAppendInfraFailureMonotonicAttemptSeq(t *testing.T) {
	st := stepWith(CauseGateFailRewind, CauseInfraFailure)
	AppendInfraFailure(st, "t0", "t1", "probe detail", nil)
	last := st.Attempts[len(st.Attempts)-1]
	if last.Cause != CauseInfraFailure || last.AttemptSeq != 3 || last.Detail != "probe detail" {
		t.Fatalf("appended attempt: %+v", last)
	}
	if last.InterruptedAt != "t1" || last.StartedAt != "t0" {
		t.Fatalf("timestamps: %+v", last)
	}
}

func TestFalsifiedDoneRewrite(t *testing.T) {
	exec := &capsule.Execution{Steps: map[string]*capsule.Step{
		"code": {Status: capsule.StepDone, Output: map[string]any{"summary": "hi"}, StepStartedAt: "t0"},
	}}
	// First falsification: done -> in_progress + one infra_failure archive,
	// ordinal advances to 2 (list of 2 candidates).
	ord, err := FalsifiedDoneRewrite(exec, "code", "publish-time schema violation: /summary minLength", 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ord != 2 {
		t.Fatalf("first rewrite ordinal = %d, want 2", ord)
	}
	st := exec.Steps["code"]
	if st.Status != capsule.StepInProgress {
		t.Fatalf("status after rewrite = %q, want in_progress (never pending)", st.Status)
	}
	if len(st.Attempts) != 1 || st.Attempts[0].Cause != CauseInfraFailure {
		t.Fatalf("attempts after first rewrite: %+v", st.Attempts)
	}
	if st.Output == nil {
		t.Fatal("the falsified output must stay as auditable history until the executor overwrites it")
	}

	// The executor re-runs the step, it completes done again, and a second
	// publish falsifies it again: ordinal is CAPPED at the list length.
	st.Status = capsule.StepDone
	ord, err = FalsifiedDoneRewrite(exec, "code", "second violation", 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ord != 2 {
		t.Fatalf("second rewrite ordinal = %d, want 2 (bounded by list length)", ord)
	}
	if len(st.Attempts) != 2 {
		t.Fatalf("attempts after second rewrite: %+v", st.Attempts)
	}

	// Only a done step may be falsified.
	if _, err := FalsifiedDoneRewrite(exec, "code", "x", 2, time.Now()); err == nil {
		t.Fatal("rewriting an in_progress step must fail")
	}
	if _, err := FalsifiedDoneRewrite(exec, "nope", "x", 2, time.Now()); err == nil {
		t.Fatal("unknown step must fail")
	}
}
