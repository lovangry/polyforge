package driver

// transition_test.go: the state-machine and atomic-write tests for the
// single write entry point — fencing, per-kind validation, snapshot
// revert on persist failure, and the durable execution.json shape the
// observe lane keeps consuming.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// eventLog is the counting EventLogger fake.
type eventLog struct {
	entries []loggedEvent
}

type loggedEvent struct {
	wi   string
	kind string
	body map[string]any
}

func (l *eventLog) Append(wi, kind string, body any) error {
	b, _ := body.(map[string]any)
	l.entries = append(l.entries, loggedEvent{wi: wi, kind: kind, body: b})
	return nil
}

func (l *eventLog) kinds() []string {
	out := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e.kind)
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Second); return c.t }

// testEnv builds a Driver over a temp capsule with a programmatic
// workflow and no spawner calls expected.
func testEnv(t *testing.T, steps ...StepSpec) (*Driver, *eventLog, *clock, string) {
	t.Helper()
	dir := t.TempDir()
	execPath := filepath.Join(dir, capsule.ExecutionJSON)
	log := &eventLog{}
	clk := &clock{t: time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)}
	d, err := New(Config{
		WI:       WIInfo{ID: "wi_t1", CapsuleDir: dir, BundleName: "b"},
		ExecPath: execPath,
		Workflow: &Workflow{Steps: steps, MaxTotal: 4, MaxPerStep: 2},
		Spawner:  &fakeSpawner{},
		Log:      log,
		Now:      clk.now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, log, clk, execPath
}

func spec(id string) StepSpec { return StepSpec{ID: id, ContextPolicy: "fresh"} }

func gateSpec(id, target string) StepSpec {
	return StepSpec{
		ID:            id,
		Gate:          true,
		ContextPolicy: "fresh",
		OnFail:        &OnFail{Action: "rewind", Target: target, IncludeVerdict: true},
	}
}

// mustInit persists the skeleton and returns the post-init version.
func mustInit(t *testing.T, d *Driver) {
	t.Helper()
	if err := d.transition(TransitionRequest{Kind: TransInit, SpawnN: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
}

// mustStart starts a step as derivation n.
func mustStart(t *testing.T, d *Driver, stepID string, n int64) {
	t.Helper()
	if err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: stepID, Model: "prov/m1", SpawnN: n}); err != nil {
		t.Fatalf("start %s: %v", stepID, err)
	}
}

func completedResult(stepID string, n int64, output string) *StepResult {
	return &StepResult{
		ProtocolVersion: 1,
		ResultID:        "319584c8-10e7-5703-8b0f-4af0a03f8657" + stepID + "-" + string(rune('0'+n)),
		WIID:            "wi_t1",
		StepID:          stepID,
		SpawnN:          n,
		Outcome:         OutcomeCompleted,
		Output:          json.RawMessage(output),
	}
}

// readExec parses the durable file (never the driver's memory — the
// tests assert what actually landed on disk).
func readExec(t *testing.T, path string) *capsule.Execution {
	t.Helper()
	exec := tryReadExec(path)
	if exec == nil {
		t.Fatalf("read execution.json: %s", path)
	}
	return exec
}

// tryReadExec is the nil-tolerant variant for polling conditions (the
// file legitimately does not exist before the init write).
func tryReadExec(path string) *capsule.Execution {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var exec capsule.Execution
	if err := json.Unmarshal(raw, &exec); err != nil {
		return nil
	}
	return &exec
}

// tmpLeftovers lists the atomic-write temp files next to path.
func tmpLeftovers(path string) []string {
	m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp"))
	return m
}

// ---------------------------------------------------------------------------
// init + atomic persistence

func TestTransitionInitWritesSkeleton(t *testing.T) {
	d, log, _, execPath := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	exec := readExec(t, execPath)
	if exec.WorkflowStatus != StatusRunning || exec.CurrentStep != "" {
		t.Fatalf("bad skeleton: status=%q current=%q", exec.WorkflowStatus, exec.CurrentStep)
	}
	if exec.Version != 1 {
		t.Fatalf("init must write version 1, got %d", exec.Version)
	}
	if exec.CapsuleID != "cap_wi_t1" || exec.WorkItemID != "wi_t1" {
		t.Fatalf("bad identity: %+v", exec)
	}
	for _, id := range []string{"a", "b"} {
		if s := exec.Steps[id]; s == nil || s.Status != capsule.StepPending {
			t.Fatalf("step %s must be pending in the skeleton, got %+v", id, s)
		}
	}
	if m := tmpLeftovers(execPath); len(m) > 0 {
		t.Fatalf("tmp file must not survive the rename: %v", m)
	}
	// Double init is refused — there is exactly one first write.
	if err := d.transition(TransitionRequest{Kind: TransInit, SpawnN: 0}); err == nil {
		t.Fatal("double init must be refused")
	}
	if got := log.kinds(); len(got) != 1 || got[0] != "exec_init" {
		t.Fatalf("init must emit exactly one exec_init event, got %v", got)
	}
}

func TestTransitionVersionBumpsPerWrite(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1,
		Result: completedResult("a", 1, `{"summary":"A"}`),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	mustStart(t, d, "b", 2)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "b", SpawnN: 2,
		Result: completedResult("b", 2, `{"summary":"B"}`),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := d.transition(TransitionRequest{Kind: TransTerminal, Status: StatusCompleted, SpawnN: 2}); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	exec := readExec(t, execPath)
	if exec.Version != 6 { // init + start/result a + start/result b + terminal
		t.Fatalf("version must bump once per write: want 6, got %d", exec.Version)
	}
	if exec.WorkflowStatus != StatusCompleted || exec.Steps["a"].Status != capsule.StepDone || exec.Steps["b"].Status != capsule.StepDone {
		t.Fatalf("final state wrong: %+v", exec)
	}
	if exec.UpdatedAt == "" {
		t.Fatal("every write must stamp updated_at")
	}
}

// ---------------------------------------------------------------------------
// fencing

func TestTransitionFence(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"))
	mustInit(t, d)

	// step_start must carry EXACTLY current+1 — neither the current
	// ordinal nor one further ahead.
	for _, bad := range []int64{0, 2, 3} {
		err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: "a", SpawnN: bad})
		if !errors.Is(err, ErrStaleFence) {
			t.Fatalf("step_start with spawn %d must be fenced, got %v", bad, err)
		}
	}
	// Every other kind must carry the CURRENT ordinal.
	if err := d.transition(TransitionRequest{Kind: TransTerminal, Status: StatusFailedGate, SpawnN: 1}); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("non-start with the next ordinal must be fenced, got %v", err)
	}
	// A fenced request changed nothing on disk.
	if exec := readExec(t, execPath); exec.Version != 1 || exec.CurrentStep != "" {
		t.Fatalf("fenced request mutated the file: %+v", exec)
	}
	// The correct ordinal passes and advances the counter.
	mustStart(t, d, "a", 1)
	// ... and afterwards the OLD ordinal is stale for result delivery.
	err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 0,
		Result: completedResult("a", 0, `{}`),
	})
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("result with a stale spawn must be fenced, got %v", err)
	}
	// The result envelope's own spawn_n must agree with the request.
	err = d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "162bc0dd-9635-5323-b170-99044b484a69", StepID: "a", SpawnN: 99, Outcome: OutcomeCompleted},
	})
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("envelope/request spawn disagreement must be fenced, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// step_start

func TestTransitionStepStartFreshConsumesRewindInput(t *testing.T) {
	d, _, clk, _ := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	// Simulate a rewind having parked its verdict on step a.
	d.mu.Lock()
	d.exec.Steps["a"].PendingRewindInput = &capsule.RewindInput{ToStep: "a", Reason: "fix it harder"}
	d.exec.Steps["a"].Output = map[string]any{"stale": true}
	d.exec.Steps["a"].BudgetConsumed = capsule.Budget{Tokens: 42}
	d.mu.Unlock()

	before := clk.t
	mustStart(t, d, "a", 1)
	snap := d.Snapshot()
	st := snap.Steps["a"]
	if st.Status != capsule.StepInProgress || st.PendingRewindInput != nil {
		t.Fatalf("start must flip to in_progress and consume the rewind verdict: %+v", st)
	}
	if st.Output != nil || st.BudgetConsumed.Tokens != 0 {
		t.Fatalf("a fresh start reopens a clean window: output=%v budget=%+v", st.Output, st.BudgetConsumed)
	}
	if st.Model != "prov/m1" || st.StepStartedAt == "" || st.StepStartedAt <= before.Format(time.RFC3339Nano) {
		t.Fatalf("start must record model + a fresh measurement window: %+v", st)
	}
	if snap.CurrentStep != "a" || snap.WorkflowStatus != StatusRunning {
		t.Fatalf("start must move the position: %+v", snap)
	}
}

func TestTransitionStepStartResumeRefreshesWindow(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	first := d.Snapshot().Steps["a"].StepStartedAt
	// needs_input parks the step; the answer resume re-dispatches it
	// in place — the SAME step_start kind, resume shape.
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "6b42e63c-86e4-56ae-a240-d08ecb1ec59f", StepID: "a", SpawnN: 1,
			Outcome: OutcomeNeedsInput, QuestionID: "q_1", Question: "which shape?"},
	}); err != nil {
		t.Fatalf("needs_input: %v", err)
	}
	if err := d.Answer("q_1", "the lean one", "alice"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	mustStart(t, d, "a", 2) // resume re-dispatch
	st := d.Snapshot().Steps["a"]
	if st.Status != capsule.StepInProgress || st.StepStartedAt == first {
		t.Fatalf("resume must refresh the window, got started_at=%q (first %q)", st.StepStartedAt, first)
	}
}

func TestTransitionStepStartIllegal(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	if err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: "nope", SpawnN: 1}); err == nil {
		t.Fatal("unknown step must be refused")
	}
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1,
		Result: completedResult("a", 1, `{}`),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	// A done step never restarts.
	if err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: "a", SpawnN: 2}); err == nil {
		t.Fatal("a done step must not start")
	}
	// A blocked workflow only resumes its CURRENT step.
	mustStart(t, d, "b", 2)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "b", SpawnN: 2,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "1856f743-9ae1-5efa-871b-3e60732c504d", StepID: "b", SpawnN: 2, Outcome: OutcomeIncomplete},
	}); err != nil {
		t.Fatalf("incomplete: %v", err)
	}
	if err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: "b", SpawnN: 3}); err != nil {
		t.Fatalf("resuming the blocked current step must be legal: %v", err)
	}
	if s := d.Snapshot().WorkflowStatus; s != StatusRunning {
		t.Fatalf("the blocked resume must rewrite running, got %q", s)
	}
}

// ---------------------------------------------------------------------------
// step_result

func TestTransitionStepResultCompleted(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	res := completedResult("a", 1, `{"summary":"A"}`)
	res.CommandEvidence = []CommandEvidence{{
		Argv: []string{"go", "test", "./..."}, Cwd: "/w", ExitCode: 0,
		StartedAt: time.Now(), DurationMs: 1200, LogPath: "logs/a.log",
	}}
	if err := d.transition(TransitionRequest{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: res}); err != nil {
		t.Fatalf("result: %v", err)
	}
	st := d.Snapshot().Steps["a"]
	if st.Status != capsule.StepDone || st.Output["summary"] != "A" {
		t.Fatalf("completed must record done + output: %+v", st)
	}
	// Snapshot deep-copies via JSON, so the evidence comes back as the
	// generic decode — assert on the re-marshaled shape (B10 keys).
	cmds, ok := st.Output["commands"].([]any)
	if !ok || len(cmds) != 1 {
		t.Fatalf("B10 command evidence must land in output.commands: %+v", st.Output["commands"])
	}
	cmd, _ := cmds[0].(map[string]any)
	if cmd["log_path"] != "logs/a.log" || cmd["duration_ms"] != float64(1200) {
		t.Fatalf("bad evidence entry: %+v", cmd)
	}
}

func TestTransitionStepResultRefusals(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	// No envelope, no idempotency key, wrong protocol, unknown outcome,
	// foreign work item — all refused before any mutation.
	mustStart(t, d, "a", 1)
	cases := []TransitionRequest{
		{Kind: TransStepResult, StepID: "a", SpawnN: 1},
		{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: &StepResult{ProtocolVersion: 1, StepID: "a", SpawnN: 1, Outcome: OutcomeCompleted}},
		{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: &StepResult{ProtocolVersion: 2, ResultID: "6cd3ee96-87f8-5308-afb8-15adc04deb32", StepID: "a", SpawnN: 1, Outcome: OutcomeCompleted}},
		{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: &StepResult{ProtocolVersion: 1, ResultID: "34db40ce-3b19-5b67-89e5-dc3027d765bb", StepID: "a", SpawnN: 1, Outcome: OutcomeWeird}},
		{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: &StepResult{ProtocolVersion: 1, ResultID: "545a1611-82ea-553a-b526-5b0f7f3c2772", WIID: "wi_OTHER", StepID: "a", SpawnN: 1, Outcome: OutcomeCompleted}},
		// Result for a step that is not current.
		{Kind: TransStepResult, StepID: "b", SpawnN: 1, Result: completedResult("b", 1, `{}`)},
	}
	for i, req := range cases {
		if err := d.transition(req); err == nil {
			t.Fatalf("case %d must be refused", i)
		}
	}
	if st := d.Snapshot().Steps["a"]; st.Status != capsule.StepInProgress {
		t.Fatalf("refused results must not mutate: %+v", st)
	}
	// needs_input without a question payload is refused too.
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "92309c71-09d2-5605-b425-e09f2ae05034", StepID: "a", SpawnN: 1, Outcome: OutcomeNeedsInput},
	}); err == nil {
		t.Fatal("needs_input without question_id must be refused")
	}
}

const OutcomeWeird Outcome = "weird"

func TestTransitionStepResultGateFailedAndIncomplete(t *testing.T) {
	d, _, _, _ := testEnv(t, gateSpec("check", "a"), spec("a"))
	mustInit(t, d)
	mustStart(t, d, "check", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "63f44695-aa75-5030-a746-03ded8957265", StepID: "check", SpawnN: 1,
			Outcome: OutcomeGateFailed, Output: json.RawMessage(`{"verdict":"FAIL"}`)},
	}); err != nil {
		t.Fatalf("gate_failed: %v", err)
	}
	snap := d.Snapshot()
	st := snap.Steps["check"]
	if st.Status != capsule.StepInProgress || st.Output["verdict"] != "FAIL" {
		t.Fatalf("a FAILing gate is never done and holds its verdict: %+v", st)
	}
	if snap.ReviewRounds["check"] != 1 {
		t.Fatalf("review_rounds must count the evaluation: %+v", snap.ReviewRounds)
	}
	// incomplete → blocked, position frozen, partial output recorded.
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "6cfbbada-9905-5d57-90f4-25866ff90918", StepID: "check", SpawnN: 1,
			Outcome: OutcomeIncomplete, Output: json.RawMessage(`{"partial":true}`)},
	}); err != nil {
		t.Fatalf("incomplete: %v", err)
	}
	snap = d.Snapshot()
	if snap.WorkflowStatus != StatusBlocked || snap.CurrentStep != "check" || snap.Steps["check"].Status != capsule.StepInProgress {
		t.Fatalf("incomplete must block with the position frozen: %+v", snap)
	}
	if snap.Steps["check"].Output["partial"] != true {
		t.Fatalf("the honest partial output must be recorded: %+v", snap.Steps["check"])
	}
}

func TestTransitionNeedsInputAndAnswer(t *testing.T) {
	d, log, _, _ := testEnv(t, spec("a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "a", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "f830c814-4a8d-5f53-b83a-820807c56981", StepID: "a", SpawnN: 1,
			Outcome: OutcomeNeedsInput, QuestionID: "q_abc123", Question: "lean or compatible?",
			Output: json.RawMessage(`{"ctx":1}`)},
	}); err != nil {
		t.Fatalf("needs_input: %v", err)
	}
	snap := d.Snapshot()
	q := snap.Steps["a"].PendingQuestion
	if q == nil || q.ID != "q_abc123" || q.Question != "lean or compatible?" || q.AskedAt == "" {
		t.Fatalf("the open question must persist atomically: %+v", q)
	}
	if string(q.Context) != `{"ctx":1}` {
		t.Fatalf("the ask context rides the question: %s", q.Context)
	}
	if snap.WorkflowStatus != StatusRunning {
		t.Fatalf("a driven ask is waiting, not blocked: %q", snap.WorkflowStatus)
	}
	// Wrong question id is refused (the D9 binding).
	if err := d.Answer("q_other", "nope", "bob"); err == nil {
		t.Fatal("a mismatched question id must be refused")
	}
	if err := d.Answer("q_abc123", "the lean one", "alice"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	st := d.Snapshot().Steps["a"]
	if st.PendingQuestion != nil || len(st.Answers) != 1 {
		t.Fatalf("the answer must archive + clear in one write: %+v", st)
	}
	a := st.Answers[0]
	if a.QuestionID != "q_abc123" || a.Answer != "the lean one" || a.AnsweredBy != "alice" || a.AnsweredAt == "" {
		t.Fatalf("bad archived answer: %+v", a)
	}
	// A second answer for the consumed question has nothing to bind to.
	if err := d.Answer("q_abc123", "again", "alice"); err == nil {
		t.Fatal("an answer with no open question must be refused")
	}
	got := log.kinds()
	want := []string{"exec_init", "step_started", "needs_input", "answered"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event kinds: want %v, got %v", want, got)
	}
}

// ---------------------------------------------------------------------------
// gate_fail / rewind

func TestTransitionGateFailRewind(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("build"), spec("polish"), gateSpec("check", "build"))
	mustInit(t, d)
	for _, s := range []struct {
		id string
		n  int64
	}{
		{"build", 1}, {"polish", 2},
	} {
		mustStart(t, d, s.id, s.n)
		if err := d.transition(TransitionRequest{
			Kind: TransStepResult, StepID: s.id, SpawnN: s.n,
			Result: completedResult(s.id, s.n, `{"ok":true}`),
		}); err != nil {
			t.Fatalf("result %s: %v", s.id, err)
		}
	}
	mustStart(t, d, "check", 3)
	// The gate FAILs.
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 3,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "15ea00ac-1695-5056-bcc3-726228268ff9", StepID: "check", SpawnN: 3,
			Outcome: OutcomeGateFailed, Output: json.RawMessage(`{"verdict":"FAIL: tests red"}`)},
	}); err != nil {
		t.Fatalf("gate result: %v", err)
	}
	if err := d.transition(TransitionRequest{
		Kind: TransGateFail, StepID: "check", SpawnN: 3,
		Gate: &GateVerdict{GateStepID: "check", Action: "rewind", TargetStep: "build",
			IncludeVerdict: true, VerdictText: "FAIL: tests red"},
	}); err != nil {
		t.Fatalf("gate_fail: %v", err)
	}
	snap := d.Snapshot()
	// [build, polish] archived + reset to pending with output cleared.
	for _, id := range []string{"build", "polish"} {
		st := snap.Steps[id]
		if st.Status != capsule.StepPending || st.Output != nil {
			t.Fatalf("%s must be reset pending with cleared output: %+v", id, st)
		}
		// Attempts now include "accepted" records (R14); find the rewind entry.
		var att capsule.Attempt
		for _, a := range st.Attempts {
			if a.Cause == CauseGateFailRewind {
				att = a
				break
			}
		}
		if att.Cause != CauseGateFailRewind || att.RewindGate != "check" || att.ArchivedAt == "" {
			t.Fatalf("bad rewind archive on %s: %+v", id, att)
		}
		if string(att.Result) != `{"ok":true}` {
			t.Fatalf("the archived attempt carries the prior result: %s", att.Result)
		}
	}
	// The gate itself: untouched, still in_progress holding the verdict.
	if st := snap.Steps["check"]; st.Status != capsule.StepInProgress || len(st.Attempts) != 0 || st.Output["verdict"] == nil {
		t.Fatalf("the gate is neither archived nor reset: %+v", st)
	}
	// Position, stats, routed verdict.
	if snap.CurrentStep != "build" {
		t.Fatalf("position must move back to the target: %q", snap.CurrentStep)
	}
	if snap.RewindStats.Count != 1 || len(snap.RewindStats.ToStep) != 1 || snap.RewindStats.ToStep[0] != "build" {
		t.Fatalf("rewind stats: %+v", snap.RewindStats)
	}
	if pri := snap.Steps["build"].PendingRewindInput; pri == nil || pri.Reason != "FAIL: tests red" || pri.ToStep != "build" {
		t.Fatalf("the verdict must route to the target (D8): %+v", pri)
	}
}

func TestTransitionGateFailBounds(t *testing.T) {
	// max_total = 1: the SECOND rewind hits the bound and must terminate
	// WITHOUT mutating anything (bounds run before the archive).
	d, _, _, _ := testEnv(t, spec("a"), gateSpec("check", "a"))
	d.workflow.MaxTotal = 1
	mustInit(t, d)
	for round := 0; round < 2; round++ {
		n := int64(round*2 + 1)
		mustStart(t, d, "a", n)
		if err := d.transition(TransitionRequest{
			Kind: TransStepResult, StepID: "a", SpawnN: n, Result: completedResult("a", n, `{}`),
		}); err != nil {
			t.Fatalf("result a (round %d): %v", round, err)
		}
		mustStart(t, d, "check", n+1)
		if err := d.transition(TransitionRequest{
			Kind: TransStepResult, StepID: "check", SpawnN: n + 1,
			Result: &StepResult{ProtocolVersion: 1, ResultID: "28d435b1-398e-51c3-b1a6-1da4bc80cbb3", StepID: "check", SpawnN: n + 1, Outcome: OutcomeGateFailed},
		}); err != nil {
			t.Fatalf("gate result (round %d): %v", round, err)
		}
		if err := d.transition(TransitionRequest{
			Kind: TransGateFail, StepID: "check", SpawnN: n + 1,
			Gate: &GateVerdict{GateStepID: "check", Action: "rewind", TargetStep: "a", IncludeVerdict: true, VerdictText: "v"},
		}); err != nil {
			t.Fatalf("gate_fail (round %d): %v", round, err)
		}
	}
	snap := d.Snapshot()
	if snap.WorkflowStatus != StatusFailedRewindExhausted {
		t.Fatalf("the second rewind must hit max_total: %q", snap.WorkflowStatus)
	}
	// Round 2's step a was NOT re-archived: the bound fired first.
	// ("accepted" attempts from R14 are extra; count only rewind archives.)
	rewindArchives := 0
	for _, att := range snap.Steps["a"].Attempts {
		if att.Cause == CauseGateFailRewind {
			rewindArchives++
		}
	}
	if rewindArchives != 1 {
		t.Fatalf("the exhausted rewind must not re-archive: %d rewind archives (attempts=%d)", rewindArchives, len(snap.Steps["a"].Attempts))
	}
	if snap.RewindStats.Count != 1 {
		t.Fatalf("the exhausted rewind must not count: %+v", snap.RewindStats)
	}
	// max_per_step behaves the same way per target.
	d2, _, _, _ := testEnv(t, spec("a"), gateSpec("check", "a"))
	d2.workflow.MaxPerStep = 1
	mustInit(t, d2)
	mustStart(t, d2, "a", 1)
	if err := d2.transition(TransitionRequest{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: completedResult("a", 1, `{}`)}); err != nil {
		t.Fatalf("result: %v", err)
	}
	mustStart(t, d2, "check", 2)
	if err := d2.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 2,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "d9d2310b-7808-5936-8004-ed81be4792d5", StepID: "check", SpawnN: 2, Outcome: OutcomeGateFailed},
	}); err != nil {
		t.Fatalf("gate result: %v", err)
	}
	// First rewind to a passes (per-step count 0 → 1)...
	if err := d2.transition(TransitionRequest{
		Kind: TransGateFail, StepID: "check", SpawnN: 2,
		Gate: &GateVerdict{GateStepID: "check", Action: "rewind", TargetStep: "a", VerdictText: "v"},
	}); err != nil {
		t.Fatalf("first rewind: %v", err)
	}
	// ...re-run and FAIL again: per-step bound hits.
	mustStart(t, d2, "a", 3)
	if err := d2.transition(TransitionRequest{Kind: TransStepResult, StepID: "a", SpawnN: 3, Result: completedResult("a", 3, `{}`)}); err != nil {
		t.Fatalf("result: %v", err)
	}
	mustStart(t, d2, "check", 4)
	if err := d2.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 4,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "b81d5f9d-5f09-5149-b445-0792f6dd4081", StepID: "check", SpawnN: 4, Outcome: OutcomeGateFailed},
	}); err != nil {
		t.Fatalf("gate result: %v", err)
	}
	if err := d2.transition(TransitionRequest{
		Kind: TransGateFail, StepID: "check", SpawnN: 4,
		Gate: &GateVerdict{GateStepID: "check", Action: "rewind", TargetStep: "a", VerdictText: "v"},
	}); err != nil {
		t.Fatalf("second rewind: %v", err)
	}
	if s := d2.Snapshot().WorkflowStatus; s != StatusFailedRewindExhausted {
		t.Fatalf("max_per_step must terminate: %q", s)
	}
}

func TestTransitionGateFailTerminateAndIllegalTarget(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"), gateSpec("check", "a"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{Kind: TransStepResult, StepID: "a", SpawnN: 1, Result: completedResult("a", 1, `{}`)}); err != nil {
		t.Fatalf("result: %v", err)
	}
	mustStart(t, d, "check", 2)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 2,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "7de51012-d10c-5845-a159-e33267353d9c", StepID: "check", SpawnN: 2, Outcome: OutcomeGateFailed},
	}); err != nil {
		t.Fatalf("gate result: %v", err)
	}
	// terminate (the default policy too) → failed_gate.
	if err := d.transition(TransitionRequest{
		Kind: TransGateFail, StepID: "check", SpawnN: 2,
		Gate: &GateVerdict{GateStepID: "check", Action: "", VerdictText: "v"},
	}); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if s := d.Snapshot().WorkflowStatus; s != StatusFailedGate {
		t.Fatalf("terminate must land failed_gate, got %q", s)
	}
	// Terminal freeze: nothing transitions anymore.
	if err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: "a", SpawnN: 3}); err == nil {
		t.Fatal("a terminal workflow must refuse further transitions")
	}

	d2, _, _, _ := testEnv(t, spec("a"), spec("b"), gateSpec("check", "a"))
	mustInit(t, d2)
	mustStart(t, d2, "check", 1)
	if err := d2.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "check", SpawnN: 1,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "79f56efb-3d66-54ce-9c06-51abd76a0495", StepID: "check", SpawnN: 1, Outcome: OutcomeGateFailed},
	}); err != nil {
		t.Fatalf("gate result: %v", err)
	}
	// A rewind target at or after the gate is illegal ("b" in
	// [a, b, check] IS legal — the target only needs to be earlier than
	// the gate).
	for _, bad := range []string{"check", "nope"} {
		err := d2.transition(TransitionRequest{
			Kind: TransGateFail, StepID: "check", SpawnN: 1,
			Gate: &GateVerdict{GateStepID: "check", Action: "rewind", TargetStep: bad, VerdictText: "v"},
		})
		if err == nil {
			t.Fatalf("rewind target %q must be refused", bad)
		}
	}
}

func TestTransitionOperatorRewindFromBlocked(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"), spec("b"), spec("c"))
	mustInit(t, d)
	for _, s := range []struct {
		id string
		n  int64
	}{
		{"a", 1}, {"b", 2},
	} {
		mustStart(t, d, s.id, s.n)
		if err := d.transition(TransitionRequest{
			Kind: TransStepResult, StepID: s.id, SpawnN: s.n, Result: completedResult(s.id, s.n, `{"x":1}`),
		}); err != nil {
			t.Fatalf("result %s: %v", s.id, err)
		}
	}
	mustStart(t, d, "c", 3)
	if err := d.transition(TransitionRequest{
		Kind: TransStepResult, StepID: "c", SpawnN: 3,
		Result: &StepResult{ProtocolVersion: 1, ResultID: "d1814589-7aa0-56a8-9722-54c20a0ab3b5", StepID: "c", SpawnN: 3, Outcome: OutcomeIncomplete},
	}); err != nil {
		t.Fatalf("incomplete: %v", err)
	}
	// The owner's verdict: rewind to a.
	if err := d.Rewind("a", "owner: wrong premise"); err != nil {
		t.Fatalf("operator rewind: %v", err)
	}
	snap := d.Snapshot()
	for _, id := range []string{"a", "b"} {
		st := snap.Steps[id]
		if st.Status != capsule.StepPending {
			t.Fatalf("%s archived+reset: %+v", id, st)
		}
		// Find the operator-rewind archive ("accepted" attempts are extra).
		var att capsule.Attempt
		for _, a := range st.Attempts {
			if a.Detail == "owner: wrong premise" {
				att = a
				break
			}
		}
		if att.Cause == "" {
			t.Fatalf("%s missing operator rewind archive: %+v", id, st.Attempts)
		}
		if att.RewindGate != "" {
			t.Fatalf("an operator rewind archives without a gate: %+v", att)
		}
	}
	if snap.Steps["c"].Status != capsule.StepInProgress || len(snap.Steps["c"].Attempts) != 0 {
		t.Fatalf("the current step is not in the rewind range: %+v", snap.Steps["c"])
	}
	if snap.CurrentStep != "a" || snap.RewindStats.Count != 1 {
		t.Fatalf("position/stats: current=%q stats=%+v", snap.CurrentStep, snap.RewindStats)
	}
	// While running with an open question the rewind is refused.
	if err := d.Rewind("a", "too late"); err == nil {
		t.Fatal("a rewind past a non-current target must be refused (c is current)")
	}
}

// ---------------------------------------------------------------------------
// terminal + budget

func TestTransitionTerminalValidation(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	if err := d.transition(TransitionRequest{Kind: TransTerminal, Status: "wrapped", SpawnN: 0}); err == nil {
		t.Fatal("a status outside the closed set must be refused")
	}
	if err := d.transition(TransitionRequest{Kind: TransTerminal, Status: StatusCompleted, SpawnN: 0}); err == nil {
		t.Fatal("completed with steps pending must be refused")
	}
	if err := d.transition(TransitionRequest{Kind: TransTerminal, Status: StatusFailedGate, Reason: "owner kill", SpawnN: 0}); err != nil {
		t.Fatalf("failure terminal from running must be legal: %v", err)
	}
	if err := d.transition(TransitionRequest{Kind: TransTerminal, Status: StatusFailedGate, SpawnN: 0}); err == nil {
		t.Fatal("terminal-on-terminal must be refused")
	}
}

func TestTransitionBudgetUpdate(t *testing.T) {
	d, _, _, _ := testEnv(t, spec("a"), spec("b"))
	mustInit(t, d)
	mustStart(t, d, "a", 1)
	if err := d.transition(TransitionRequest{
		Kind: TransBudgetUpdate, StepID: "a", SpawnN: 1,
		Budget: &capsule.Budget{USD: 0.25, WallMs: 1000, Tokens: 500},
	}); err != nil {
		t.Fatalf("budget: %v", err)
	}
	// StepID empty credits the CURRENT step.
	if err := d.transition(TransitionRequest{
		Kind: TransBudgetUpdate, SpawnN: 1,
		Budget: &capsule.Budget{USD: 0.10, Tokens: 100},
	}); err != nil {
		t.Fatalf("budget (current step): %v", err)
	}
	snap := d.Snapshot()
	st := snap.Steps["a"]
	if st.BudgetConsumed.USD != 0.35 || st.BudgetConsumed.WallMs != 1000 || st.BudgetConsumed.Tokens != 600 {
		t.Fatalf("step budget must accumulate: %+v", st.BudgetConsumed)
	}
	if snap.BudgetConsumed.USD != 0.35 || snap.BudgetConsumed.Tokens != 600 {
		t.Fatalf("workflow budget aggregates the same deltas: %+v", snap.BudgetConsumed)
	}
}

// ---------------------------------------------------------------------------
// persist failure → revert

func TestTransitionPersistFailureRevertsInMemory(t *testing.T) {
	d, _, _, execPath := testEnv(t, spec("a"))
	mustInit(t, d)
	// Occupy the target path with a directory: the tmp write succeeds,
	// the rename cannot replace a directory with a file — the persist
	// fails after apply, proving the snapshot revert.
	if err := os.Remove(execPath); err != nil {
		t.Fatalf("remove exec file: %v", err)
	}
	if err := os.Mkdir(execPath, 0o755); err != nil {
		t.Fatalf("occupy path: %v", err)
	}
	before := d.Snapshot()
	err := d.transition(TransitionRequest{Kind: TransStepStart, StepID: "a", Model: "m", SpawnN: 1})
	if err == nil {
		t.Fatal("the persist must fail over an occupied path")
	}
	if after := d.Snapshot(); after.CurrentStep != before.CurrentStep || after.Steps["a"].Status != before.Steps["a"].Status {
		t.Fatalf("in-memory state must revert on persist failure: %+v", after)
	}
	if d.SpawnN() != 0 {
		t.Fatalf("the spawn counter must not advance over a failed start: %d", d.SpawnN())
	}
	if m := tmpLeftovers(execPath); len(m) > 0 {
		t.Fatalf("the tmp file must be cleaned on failure: %v", m)
	}
}
