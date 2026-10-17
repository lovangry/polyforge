package driver

// d6_budget_test.go: the D6 budget semantics of polyforge#147 — the
// step's total runtime T accumulates over the WHOLE step (no window is
// reopened by a candidate switch or a human continue; only a rewind's
// fresh start zeroes it), and every timeout-class death relaxes the
// step's wall budget x2 at disposal time, persisted in execution.json.
//
// Written RED against the pre-change driver (slow-model timeouts
// relaxed nothing and a switch rebased the T window), GREEN after.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// timeoutOnce drives one timeout-class death of the given cause,
// followed by one healthy dispatch, over a step with the given chain.
func timeoutOnce(t *testing.T, cause supervise.Cause, wall time.Duration, models ...string) *supEnv {
	t.Helper()
	env := newSupEnv(t, func(c *Config) {
		c.Limits = supervise.Defaults{StepTimeout: wall}
	}, chain("s", models...))
	env.sp.script = []fakeEntry{{hold: true}, done(`{"ok":true}`)}
	go func() {
		waitForSpawns(env, 1)
		env.link.Verdict(supervise.StepDead{Step: "s", SpawnN: 1, Cause: cause, Evidence: "T exhausted"})
	}()
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return env
}

// TestD6SlowModelTimeoutRelaxesWallAndSwitches: a slow-model timeout
// switches to the next candidate AND doubles the step's wall budget,
// persisting it in execution.json.
func TestD6SlowModelTimeoutRelaxesWallAndSwitches(t *testing.T) {
	env := timeoutOnce(t, supervise.CauseStepTimeoutSlowModel, 6*time.Second, "a", "b")
	if m := env.models(); len(m) != 2 || m[0] != "a" || m[1] != "b" {
		t.Fatalf("a slow-model timeout switches candidates: %v", m)
	}
	sup := env.exec(t).Steps["s"].Supervision
	if sup == nil || sup.RelaxedWallMs != (12*time.Second).Milliseconds() {
		t.Fatalf("the slow-model disposal must relax T to 12000ms: %+v", sup)
	}
}

// TestD6SlowModelRelaxationDoublesEachTime: every consecutive
// slow-model timeout doubles the previous bound (6s -> 12s -> 24s).
func TestD6SlowModelRelaxationDoublesEachTime(t *testing.T) {
	env := newSupEnv(t, func(c *Config) {
		c.Limits = supervise.Defaults{StepTimeout: 6 * time.Second}
	}, chain("s", "a", "b", "c"))
	env.sp.script = []fakeEntry{{hold: true}, {hold: true}, done(`{"ok":true}`)}
	go func() {
		for n := int64(1); n <= 2; n++ {
			waitForSpawns(env, int(n))
			env.link.Verdict(supervise.StepDead{Step: "s", SpawnN: n, Cause: supervise.CauseStepTimeoutSlowModel, Evidence: "T exhausted"})
		}
	}()
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sup := env.exec(t).Steps["s"].Supervision
	if sup == nil || sup.RelaxedWallMs != (24*time.Second).Milliseconds() {
		t.Fatalf("two relaxations must land 24000ms (6s doubled twice): %+v", sup)
	}
}

// TestD6LongTaskRelaxesSameModelThenBlocks: a long-task timeout relaxes
// T x2 and retries the SAME model; the second one doubles T again and
// blocks the step.
func TestD6LongTaskRelaxesSameModelThenBlocks(t *testing.T) {
	env := newSupEnv(t, func(c *Config) {
		c.Limits = supervise.Defaults{StepTimeout: 3 * time.Second}
	}, chain("s", "a", "b"))
	env.sp.script = []fakeEntry{{hold: true}, {hold: true}}
	go func() {
		for n := int64(1); n <= 2; n++ {
			waitForSpawns(env, int(n))
			env.link.Verdict(supervise.StepDead{Step: "s", SpawnN: n, Cause: supervise.CauseStepTimeoutLongTask, Evidence: "long tool run"})
		}
	}()
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m := env.models(); len(m) != 2 || m[0] != m[1] {
		t.Fatalf("a long-task timeout retries the SAME model: %v", m)
	}
	x := env.exec(t)
	if x.WorkflowStatus != StatusBlocked {
		t.Fatalf("the second long-task timeout blocks: %q", x.WorkflowStatus)
	}
	// the blocking (second) timeout doubles T again before it parks, so
	// a human continue runs under a fresh window: 3s -> 6s -> 12s
	if sup := x.Steps["s"].Supervision; sup == nil || sup.RelaxedWallMs != (12*time.Second).Milliseconds() {
		t.Fatalf("the relaxed bound must persist (3s doubled twice): %+v", x.Steps["s"].Supervision)
	}
}

// TestD6NonTimeoutDeathsNeverRelax: only timeout-class deaths relax the
// wall budget — a model-class or plain death must leave T alone.
func TestD6NonTimeoutDeathsNeverRelax(t *testing.T) {
	for _, cause := range []string{FailExitModel, FailExitOther} {
		env := failOnce(t, cause, "a", "b")
		st := env.exec(t).Steps["s"]
		if st.Supervision != nil && st.Supervision.RelaxedWallMs != 0 {
			t.Fatalf("%s relaxed T to %dms — only timeout-class deaths relax", cause, st.Supervision.RelaxedWallMs)
		}
	}
}

// TestD6SuccessorRunsUnderTheRelaxedBound: after the slow-model
// disposal the successor's thresholds carry the DOUBLED bound with the
// predecessor's runtime carried over — the cumulative T is strictly
// below the relaxed bound, so the newcomer is never judged dead at its
// own startup (the spec's named scenario).
func TestD6SuccessorRunsUnderTheRelaxedBound(t *testing.T) {
	tick := 0
	env := newSupEnv(t, func(c *Config) {
		c.Limits = supervise.Defaults{StepTimeout: 6 * time.Second}
		c.Now = func() time.Time {
			tick++
			return time.Unix(1_000_000, 0).Add(time.Duration(tick) * 5 * time.Second)
		}
	}, chain("s", "a", "b"))
	env.sp.script = []fakeEntry{{hold: true}, done(`{"ok":true}`)}
	go func() {
		waitForSpawns(env, 1)
		env.link.Verdict(supervise.StepDead{Step: "s", SpawnN: 1, Cause: supervise.CauseStepTimeoutSlowModel, Evidence: "T exhausted"})
	}()
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var started []supervise.StepStarted
	for _, f := range env.link.Facts.Drain() {
		if s, ok := f.(supervise.StepStarted); ok {
			started = append(started, s)
		}
	}
	if len(started) != 2 {
		t.Fatalf("step_started facts: %d", len(started))
	}
	th := started[1].Thresholds
	if th.StepTimeout != 12*time.Second {
		t.Fatalf("the successor must run under the DOUBLED bound: %v", th.StepTimeout)
	}
	if th.PriorRuntime <= 0 {
		t.Fatalf("T is cumulative: the successor inherits the spent runtime, got %v", th.PriorRuntime)
	}
	if th.PriorRuntime >= th.StepTimeout {
		t.Fatalf("the successor must have real budget left (%v spent of %v)", th.PriorRuntime, th.StepTimeout)
	}
}

// TestD6RewindZeroesTheStepBudget: the only zeroing of T is a rewind's
// fresh start of the step (the archived attempt keeps the old window's
// accounting).
func TestD6RewindZeroesTheStepBudget(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"), gateSpec("g", "a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransBudgetUpdate, StepID: "a", SpawnN: 1, Budget: &capsule.Budget{WallMs: 5000},
	}); err != nil {
		t.Fatalf("budget: %v", err)
	}
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: completedResult("a", 1, `{}`),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	mustStart(t, d, "g", 2)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "g", SpawnN: 2,
		Result: &StepResult{StepID: "g", SpawnN: 2, Outcome: OutcomeGateFailed},
		Gate:   &GateVerdict{GateStepID: "g", TargetStep: "a", VerdictText: "FAIL: redo it"},
	}); err != nil {
		t.Fatalf("gate fail: %v", err)
	}
	mustStart(t, d, "a", 3)
	st := readExec(t, execPath).Steps["a"]
	if st.BudgetConsumed.WallMs != 0 {
		t.Fatalf("a rewound rerun starts from a zeroed T: %+v", st.BudgetConsumed)
	}
	var archived int64
	for _, a := range st.Attempts {
		if a.ConsumedBudget != nil {
			archived = a.ConsumedBudget.WallMs
		}
	}
	if archived != 5000 {
		t.Fatalf("the archived attempt keeps the old window's accounting: %d", archived)
	}
}

// TestD6GateFailRewindZeroesTheGateBudget: the gate step stays
// in_progress across its own FAIL rewind, so its T must be reset by the
// rewind itself — its re-entry is a fresh window (D6: reset on rewind).
func TestD6GateFailRewindZeroesTheGateBudget(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"), gateSpec("g", "a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: completedResult("a", 1, `{}`),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	mustStart(t, d, "g", 2)
	if err := d.transition(TransitionRequest{
		Kind: TransBudgetUpdate, StepID: "g", SpawnN: 2, Budget: &capsule.Budget{WallMs: 7000},
	}); err != nil {
		t.Fatalf("budget: %v", err)
	}
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "g", SpawnN: 2,
		Result: &StepResult{StepID: "g", SpawnN: 2, Outcome: OutcomeGateFailed},
		Gate:   &GateVerdict{GateStepID: "g", TargetStep: "a", VerdictText: "FAIL: redo it"},
	}); err != nil {
		t.Fatalf("gate fail: %v", err)
	}
	if got := readExec(t, execPath).Steps["g"].BudgetConsumed.WallMs; got != 0 {
		t.Fatalf("a gate-fail rewind must zero the gate's own T, got %dms", got)
	}
}

// repo review 2026-10-16 B-18: once the degraded latch is set, a
// transition is refused BEFORE it touches memory or execution.json.
func TestDegradedLatchBlocksBeforeAnyWrite(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	before := readExec(t, execPath).Version
	d.mu.Lock()
	d.degraded = true
	memBefore := d.exec.Version
	d.mu.Unlock()
	if err := d.transition(TransitionRequest{
		Kind: TransBudgetUpdate, StepID: "a", SpawnN: 1, Budget: &capsule.Budget{WallMs: 5000},
	}); err == nil {
		t.Fatal("a degraded driver must refuse the transition")
	}
	if got := readExec(t, execPath).Version; got != before {
		t.Fatalf("execution.json written while degraded: version %d -> %d", before, got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exec.Version != memBefore {
		t.Fatalf("memory mutated while degraded: version %d -> %d", memBefore, d.exec.Version)
	}
}

// repo review 2026-10-16 B-25: the accepted attempt takes the max-seq
// ordinal (the ledger can hold gaps) and the driver's injected clock.
func TestAcceptedAttemptSeqAndClock(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	d.mu.Lock()
	d.exec.Steps["a"].Attempts = []capsule.Attempt{{Cause: CauseInfraFailure, AttemptSeq: 1}, {Cause: CauseInfraFailure, AttemptSeq: 3}}
	d.mu.Unlock()
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: completedResult("a", 1, `{}`),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	at := readExec(t, execPath).Steps["a"].Attempts
	last := at[len(at)-1]
	if last.Cause != "accepted" || last.AttemptSeq != 4 {
		t.Fatalf("accepted attempt = %+v, want seq 4", last)
	}
	if !strings.HasPrefix(last.ArchivedAt, "2026-09-20T") {
		t.Fatalf("accepted attempt archived at %q, want the driver's (fake) clock", last.ArchivedAt)
	}
}
