package driver

// guarantees_run_test.go: Run-loop tests — the guarantees land on the
// in-process REJECTED-rewrite lane, never the recovery lane.

import (
	"context"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

func TestRunRejectsQuestionlessNeedsInputInProcess(t *testing.T) {
	d, sp, execPath := runEnv(t, StepSpec{ID: "grill"})
	bad := fakeEntry{res: &StepResult{ProtocolVersion: 1, Outcome: OutcomeNeedsInput, Question: "lean or compatible?"}}
	sp.script = []fakeEntry{bad, asks("q_1", "lean or compatible?"), done(`{}`)}
	doneCh := make(chan error, 1)
	go func() { doneCh <- d.Run(context.Background()) }()
	waitUntil(t, "the rewritten question on disk", func() bool {
		exec := tryReadExec(execPath)
		return exec != nil && exec.Steps["grill"].PendingQuestion != nil
	})
	second := sp.spawnAt(1)
	for _, want := range []string{"question_id", "needs_input (question) report", "lean or compatible?"} {
		if !strings.Contains(second.PriorRejection, want) {
			t.Fatalf("feedback must contain %q, got %q", want, second.PriorRejection)
		}
	}
	if err := d.Answer("q_1", "lean", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := <-doneCh; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunHumanGateRejectsCompletedUntilAnswered(t *testing.T) {
	d, sp, execPath := runEnv(t, StepSpec{ID: "grill", RequiresHumanAnswer: true})
	sp.script = []fakeEntry{done(`{}`), asks("q_1", "ok?"), done(`{}`)}
	doneCh := make(chan error, 1)
	go func() { doneCh <- d.Run(context.Background()) }()
	waitUntil(t, "the question after the rejected completion", func() bool {
		exec := tryReadExec(execPath)
		return exec != nil && exec.Steps["grill"].PendingQuestion != nil
	})
	if fb := sp.spawnAt(1).PriorRejection; !strings.Contains(fb, "requires a human answer: return needs_input first") {
		t.Fatalf("feedback: %q", fb)
	}
	if err := d.Answer("q_1", "yes", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := <-doneCh; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec := readExec(t, execPath); exec.Steps["grill"].Status != capsule.StepDone {
		t.Fatalf("step must complete after the human answer: %v", exec.Steps["grill"].Status)
	}
}

func TestRunVerificationRequiresEvidence(t *testing.T) {
	d, sp, execPath := runEnv(t, StepSpec{ID: "verification"})
	withEv := done(`{}`)
	withEv.res.CommandEvidence = goodEvidence()
	sp.script = []fakeEntry{done(`{}`), withEv}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(sp.spawns); n != 2 {
		t.Fatalf("evidence-less completion must be re-dispatched once, got %d spawns", n)
	}
	if !strings.Contains(sp.spawnAt(1).PriorRejection, "verification evidence") {
		t.Fatalf("feedback: %q", sp.spawnAt(1).PriorRejection)
	}
	st := readExec(t, execPath).Steps["verification"]
	if st.Status != capsule.StepDone || st.Output["commands"] == nil {
		t.Fatalf("accepted evidence must stay in the stored output: %+v", st)
	}
}
