package test

// v4_audit_subtraction_acceptance_test.go: the polyforge#147
// (审计减法 A) acceptance suite — authored against the SPEC
// (artifacts/e2e-scenarios.md mirrors spec.md's Acceptance criteria and
// Scenario blocks) BEFORE the deletion lands, step e2e-author. It pins
// the post-subtraction contract through the project's real e2e surfaces
// only: the pf/aihub/fakepi binaries, the `pf` CLI, the daemon's UDS
// face, the capsule's runtime-state files, the events log, the fakepi
// journal and real git — never engine internals.
//
// Suite posture (see artifacts/e2e-scenarios.md for the full mapping):
//   - New-behavior bars (the minimal result contract, blocked with the
//     "result" + "invalid" cause code instead of a relaunch storm, the
//     D6 timeout relaxation of T written into execution.json, the D7
//     same-model backoff for 429-class errors, the release-step
//     deletion, the retired-face grep floor, the D15 replay fixture, the
//     docs bar) are RED before the change — assertion-level reds, never
//     build reds — and must go green after.
//   - Kept-face guards (the long-task relax-then-block semantics, the
//     needs_input wait excluded from T, the rewind zeroing of a rerun's
//     budget, `go build ./...` green) are green BOTH before and after.
//
// Self-containment: this file deliberately re-derives its small event/
// journal parsing helpers (v4-prefixed) instead of borrowing from
// test/v2_supervision_test.go & friends, because those files are on the
// change's rewrite list and this suite must keep compiling across the
// whole flow. Only the v1_harness / inproc-harness helpers (not on the
// rewrite list) are reused.
//
// Self-policing: the retired-face tokens the grep scenario scans for
// are assembled at RUNTIME from fragments (v4Tok) so the scanned
// production surfaces are policed honestly; this file itself is a
// _test.go file and outside the scan's scope (the scan covers
// internal/, cmd/ and test/fakepi production Go files only — the
// rewrite of retired-mechanism unit tests is the change's own plan,
// not this suite's bar).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// ---------------------------------------------------------------------------
// v4 helpers (self-contained; see the file header).
// ---------------------------------------------------------------------------

// v4Tok joins parts into one retired-face token. The fragmentation is
// deliberate (see the file header): the grep-clean scenario below must
// never police a token its own compile unit carries contiguously.
func v4Tok(parts ...string) string {
	return strings.Join(parts, "")
}

// v4FailEvent is the step_failed event body this suite inspects.
type v4FailEvent struct {
	Step        string `json:"step"`
	Cause       string `json:"cause"`
	Disposition string `json:"disposition"`
	Count       int    `json:"failure_count"`
	Next        string `json:"next_dispatch"`
	From        string `json:"from_model"`
	To          string `json:"to_model"`
	RelaxedWall int64  `json:"relaxed_wall_ms"`
}

// v4StepFailures lists the step_failed events of one work item.
func v4StepFailures(t *testing.T, d *v1Daemon, wi string) []v4FailEvent {
	t.Helper()
	var out []v4FailEvent
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind != "step_failed" {
			continue
		}
		var f v4FailEvent
		eventBody(t, e, &f)
		out = append(out, f)
	}
	return out
}

// v4SpawnModels lists the pinned model of every fakepi spawn, in order
// (the fakepi journal's "spawn" records carry the pinned model).
func v4SpawnModels(t *testing.T, capDir string) []string {
	t.Helper()
	var out []string
	for _, rec := range fakeJournal(t, capDir) {
		if rec["type"] == "spawn" {
			m, _ := rec["model"].(string)
			out = append(out, m)
		}
	}
	return out
}

// v4SpawnGap answers the wall time between spawn ordinal n's journal
// entry and spawn ordinal n+1's — the observable lifetime of dispatch n
// plus the driver's disposal (and any backoff) before the next spawn.
func v4SpawnGap(t *testing.T, capDir string, n int) time.Duration {
	t.Helper()
	entries := spawnJournal(t, capDir)
	if len(entries) < n+1 {
		t.Fatalf("spawn journal has %d entries, need %d to measure gap %d", len(entries), n+1, n)
	}
	a, err := time.Parse(time.RFC3339Nano, fmt.Sprint(entries[n-1]["at"]))
	if err != nil {
		t.Fatalf("spawn journal entry %d: %v", n, err)
	}
	b, err := time.Parse(time.RFC3339Nano, fmt.Sprint(entries[n]["at"]))
	if err != nil {
		t.Fatalf("spawn journal entry %d: %v", n+1, err)
	}
	return b.Sub(a)
}

// v4DropPath is the result-envelope drop path of spawn ordinal n.
func v4DropPath(capDir string, n int) string {
	return filepath.Join(capDir, "runtime-state", "pending-results", strconv.Itoa(n)+".json")
}

// v4WriteEnvelope writes one result envelope to the drop path verbatim
// (the scenario is the envelope author: the bytes are exactly what the
// scenario wants the driver to judge, no fakepi involvement).
func v4WriteEnvelope(t *testing.T, capDir string, n int, env map[string]any) {
	t.Helper()
	blob, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(v4DropPath(capDir, n)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v4DropPath(capDir, n), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// v4WriteFile lands one capsule-relative file (the path-convention
// artifact producer: the handoff files the inherit chain reads).
func v4WriteFile(t *testing.T, capDir, rel, content string) {
	t.Helper()
	p := filepath.Join(capDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// v4MinimalEnvelope builds the MINIMAL-contract completed envelope: the
// outcome/output pair plus the dispatch identity (wi/step/spawn — the
// fence the drop path itself encodes), and NONE of the retired protocol
// fields (no version stamp, no envelope id, no artifact manifest, no
// handoff digest reference, no structured evidence list, no timestamp).
func v4MinimalEnvelope(wi, step string, n int, outcome string) map[string]any {
	return map[string]any{
		"wi_id":   wi,
		"step_id": step,
		"spawn_n": n,
		"outcome": outcome,
		"output":  map[string]any{"summary": "minimal contract envelope from the v4 scenario"},
	}
}

// v4HealthyTail is the healthy continuation of the quick-fix lane
// (the release entry only ever fires on the pre-change tree; the
// post-change bundles carry no release step).
func v4HealthyTail() []map[string]any {
	return []map[string]any{
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}
}

func v4Plan(first []map[string]any) []map[string]any {
	return append(first, v4HealthyTail()...)
}

// v4NewWIWithBundle files one execution ticket on an arbitrary bundle
// and scripts its agent with the given plan (self-contained twin of the
// s27 helper; see the file header).
func v4NewWIWithBundle(t *testing.T, inst *v1Install, goal, bundle string, actions []map[string]any) string {
	t.Helper()
	raw, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project:    v1Project,
		Goal:       goal,
		BundleName: strPtr(bundle),
		Attrs:      nil,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &wi); err != nil {
		t.Fatalf("create work item answer: %v (%s)", err, raw)
	}
	inst.setPlanFull(t, wi.ID, actions, nil)
	inst.trackWI(wi.ID)
	return wi.ID
}

// v4ExecRelaxedWall reads the step's persisted relaxed wall bound from
// execution.json (0 when absent).
func v4ExecRelaxedWall(t *testing.T, capDir, step string) float64 {
	t.Helper()
	x := readExecution(t, capDir)
	st, ok := x.Steps[step]
	if !ok || st.Supervision == nil {
		return 0
	}
	v, _ := st.Supervision["relaxed_wall_ms"].(float64)
	return v
}

// ---------------------------------------------------------------------------
// Scenario 1 — the minimal result contract (C2/C3).
// ---------------------------------------------------------------------------

// TestV4MinimalContractEnvelopesAccepted: the result envelope's minimal
// contract is `outcome` + `output` (needs_input adds question_id +
// question), written to the unchanged drop path; an envelope carrying
// NONE of the retired protocol fields must be accepted, the needs_input
// loop must still work end to end through the real CLI answer, and the
// inherit chain must keep reading the handoff files by path convention
// (no digest, no manifest). RED before the change (the current strict
// gate rejects an envelope without the old protocol stamps and sends
// the driver into the retired escalation lane), GREEN after.
func TestV4MinimalContractEnvelopesAccepted(t *testing.T) {
	inst := newV1Install(t)
	// No scripted envelope actions: the scenario itself authors every
	// envelope byte (the agents park alive, which is a legal harness
	// shape — the driver polls the drop path).
	wi := inst.newWI(t, "v4: minimal-contract result envelopes end to end", []map[string]any{}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	if out := d.pf(t, "submit", wi); !strings.Contains(out, "accepted") {
		t.Fatalf("submit answer: %q", out)
	}
	capDir := d.capsuleDir(wi)

	// Spawn 1 parks; write the handoff files the path convention needs
	// BEFORE any downstream dispatch (no digest anywhere — the files are
	// read by convention, never verified against a manifest).
	waitSpawnCount(t, d, wi, 1, 60*time.Second)
	v4WriteFile(t, capDir, "artifacts/handoff-grill.md",
		"# Handoff from grill\n\nminimal-contract handoff written by path convention\n")
	v4WriteFile(t, capDir, "artifacts/handoff-fix.md",
		"# Handoff from fix\n\nminimal-contract handoff written by path convention\n")

	// Ordinal 1: the needs_input minimal envelope — outcome plus the
	// question pair, nothing else on the retired protocol surface.
	v4WriteEnvelope(t, capDir, 1, map[string]any{
		"wi_id": wi, "step_id": "grill", "spawn_n": 1,
		"outcome":     "needs_input",
		"question_id": "q_v4min",
		"question":    "minimal contract: take the lean variant?",
		"output":      map[string]any{},
	})
	waitQuestion(t, d, wi, "q_v4min", 45*time.Second)
	answerQuestion(t, d, wi, "q_v4min", "Yes — the lean variant.")

	// Ordinals 2-4: the completed minimal envelopes for the rest of the
	// lane (grill's continuation, fix, verification).
	waitSpawnCount(t, d, wi, 2, 60*time.Second)
	v4WriteEnvelope(t, capDir, 2, v4MinimalEnvelope(wi, "grill", 2, "completed"))
	waitSpawnCount(t, d, wi, 3, 60*time.Second)
	v4WriteEnvelope(t, capDir, 3, v4MinimalEnvelope(wi, "fix", 3, "completed"))
	waitSpawnCount(t, d, wi, 4, 60*time.Second)
	v4WriteEnvelope(t, capDir, 4, v4MinimalEnvelope(wi, "verification", 4, "completed"))

	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	// The workflow completed with every step done — the minimal contract
	// was sufficient end to end (grill asked, was answered, delivered;
	// fix inherited by path convention; the gate passed).
	x := readExecution(t, capDir)
	if x.WorkflowStatus != "completed" {
		t.Fatalf("workflow status %q, want completed", x.WorkflowStatus)
	}
	for _, step := range []string{"grill", "fix", "verification"} {
		st, ok := x.Steps[step]
		if !ok || st.Status != "done" {
			t.Fatalf("step %s not done: %+v", step, st)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 2 — three in-process rejections block with the
// "result_invalid" cause code, no relaunch, no rewind (C4).
// ---------------------------------------------------------------------------

// TestV4ThreeRejectsBlockResultInvalidNoRelaunch: a step whose every
// envelope is structurally invalid (a needs_input without its question
// — invalid under the old AND the new contract) exhausts the in-process
// retry bound and the step goes BLOCKED with the cause code the spec
// fixed (assumption 1: result + underscore + invalid), with NO driver
// relaunch through the recovery lane (no needs_recovery events) and NO
// on_fail rewind. RED before the change (the escalation relaunches the
// driver through the recovery lane and blocks with the relaunch-count
// reason), GREEN after.
func TestV4ThreeRejectsBlockResultInvalidNoRelaunch(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4: persistent envelope rejection blocks with the result-invalid cause", []map[string]any{}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	// Feed an invalid envelope to EVERY spawn that appears (the driver
	// re-dispatches in-process after each rejection; ordinals keep
	// growing) until the step goes blocked.
	fed := map[int]bool{}
	deadline := time.Now().Add(120 * time.Second)
	blocked := false
	for time.Now().Before(deadline) && !blocked {
		for i := 1; i <= len(spawnJournal(t, capDir)); i++ {
			if fed[i] {
				continue
			}
			v4WriteEnvelope(t, capDir, i, map[string]any{
				"wi_id": wi, "step_id": "grill", "spawn_n": i,
				"outcome": "needs_input", "question_id": "", "question": "",
				"output": map[string]any{},
			})
			fed[i] = true
		}
		if x, err := readExecutionFile(capDir); err == nil {
			if x.WorkflowStatus == "blocked" {
				blocked = true
			} else if x.WorkflowStatus != "running" {
				t.Fatalf("workflow reached %q while feeding rejections", x.WorkflowStatus)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !blocked {
		t.Fatalf("the persistently rejected step never went blocked (fed %d spawns)", len(fed))
	}

	x := readExecution(t, capDir)
	if x.WorkflowStatus != "blocked" {
		t.Fatalf("workflow status %q, want blocked", x.WorkflowStatus)
	}
	// The blocked cause is the spec's assumption-1 code.
	if !strings.Contains(x.BlockedReason, v4Tok("result", "_invalid")) {
		t.Fatalf("SYMPTOM: blocked reason %q does not carry the result-invalid cause code the spec fixed", x.BlockedReason)
	}
	// No relaunch through the recovery lane. The authored bar read
	// "zero needs_recovery events" — corrected by `code` (polyforge#147,
	// recorded in artifacts/code-summary.md) because EVERY durable driver
	// exit, blocked ones included, is journaled by the monitor as one
	// needs_recovery and then translated by the runtime's v2 terminal
	// wiring (runtime/recover.go): the kept-face test
	// test/v1_s17_recovery_budget_test.go pins exactly that one signal for
	// a blocked exit, so "zero" would have demanded breaking a face this
	// subtraction must preserve. The honest no-relaunch observable is the
	// re-derivation count (what the deleted escalation lane actually did),
	// with the recovery signal bounded to that single blocked-exit
	// translation.
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "wi_agent_rederived"); n != 0 {
		t.Fatalf("SYMPTOM: the rejection escalated through the recovery lane: %d driver re-derivations (%v)", n, kinds)
	}
	if n := countOf(kinds, "needs_recovery"); n != 1 {
		t.Fatalf("SYMPTOM: needs_recovery fired %d times, want exactly the one blocked-exit translation (%v)", n, kinds)
	}
	// No rewind fired (result-invalid blocking must not route on_fail).
	if c, _ := x.RewindStats["count"].(float64); c != 0 {
		t.Fatalf("SYMPTOM: a result-invalid block fired a rewind: rewind_stats=%v", x.RewindStats)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3 — D6: the slow-model timeout relaxes T (doubled, persisted)
// and the next candidate runs with the FULL relaxed budget.
// ---------------------------------------------------------------------------

// TestV4SlowModelTimeoutRelaxesTSwitchesCandidate: a trickle-wedge step
// burns its whole T waiting on the model (step_timeout_slow_model). The
// disposal must double the step's wall budget and persist it in
// execution.json, switch to the next candidate, and the new model must
// run under the relaxed bound — NOT be judged timed out at startup on
// the strength of the budget its predecessor spent. RED before the
// change (the switch happens but no relaxation is recorded), GREEN
// after.
func TestV4SlowModelTimeoutRelaxesTSwitchesCandidate(t *testing.T) {
	inst := newV1Install(t)
	inst.stripWallBudgets = true
	wi := inst.newWI(t, "v4: slow-model timeout relaxes T and switches", v4Plan([]map[string]any{
		// Dispatch 1: trickle progress forever — only the cumulative T
		// can end it, and the time went to the model.
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true,
			"open_turn": true, "noise": "progress", "noise_ms": 100, "ms": 600000},
		// Dispatch 2 (the next candidate): healthy but slow enough to
		// prove it was NOT insta-timed-out at its start.
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "ms": 2500},
	}), nil)
	d := newV1Daemon(t, inst, optSupervision(supBounds{
		silence: 30 * time.Second, tool: 30 * time.Second, stepTimeout: 6 * time.Second,
	}))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)

	fails := v4StepFailures(t, d, wi)
	if len(fails) != 1 {
		t.Fatalf("failure records %+v, want exactly one slow-model timeout", fails)
	}
	f := fails[0]
	if f.Cause != "step_timeout_slow_model" || f.Disposition != "switch_model" {
		t.Fatalf("failure %+v, want step_timeout_slow_model/switch_model", f)
	}
	if f.RelaxedWall != 12000 {
		t.Fatalf("SYMPTOM: the slow-model disposal relaxed T to %dms, want 12000 (the 6000ms bound doubled)", f.RelaxedWall)
	}
	if got := v4ExecRelaxedWall(t, capDir, "grill"); got != 12000 {
		t.Fatalf("SYMPTOM: execution.json relaxed wall bound is %v, want 12000", got)
	}
	models := v4SpawnModels(t, capDir)
	if len(models) < 2 || models[0] == models[1] {
		t.Fatalf("the slow-model disposal must switch candidates: %v", models)
	}
	// The mandated pin: the new model ran with the FULL relaxed budget —
	// dispatch 2 lived its scripted ~2.5s+ (an un-relaxed carry-over of
	// the cumulative runtime would have judged it dead at startup, a
	// ~0.4s gap).
	if gap := v4SpawnGap(t, capDir, 2); gap < 1500*time.Millisecond {
		t.Fatalf("SYMPTOM: the successor dispatch lived only %v — it inherited an un-relaxed spent budget", gap)
	}
}

// TestV4SlowModelRelaxDoublingPerTimeout: consecutive slow-model
// timeouts double the bound each time (6s -> 12s -> 24s semantics; here
// T=4s: 8000 then 16000) and the final value persists in
// execution.json. RED before the change, GREEN after.
func TestV4SlowModelRelaxDoublingPerTimeout(t *testing.T) {
	inst := newV1Install(t)
	inst.stripWallBudgets = true
	wi := inst.newWI(t, "v4: each slow-model timeout doubles the relaxed T", v4Plan([]map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true,
			"open_turn": true, "noise": "progress", "noise_ms": 100, "ms": 600000},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true,
			"open_turn": true, "noise": "progress", "noise_ms": 100, "ms": 600000},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "ms": 1500},
	}), nil)
	d := newV1Daemon(t, inst, optSupervision(supBounds{
		silence: 30 * time.Second, tool: 30 * time.Second, stepTimeout: 4 * time.Second,
	}))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)

	fails := v4StepFailures(t, d, wi)
	if len(fails) != 2 {
		t.Fatalf("failure records %+v, want two slow-model timeouts", fails)
	}
	for i, want := range []int64{8000, 16000} {
		f := fails[i]
		if f.Cause != "step_timeout_slow_model" || f.Disposition != "switch_model" {
			t.Fatalf("failure %d %+v, want step_timeout_slow_model/switch_model", i, f)
		}
		if f.RelaxedWall != want {
			t.Fatalf("SYMPTOM: relaxation %d is %dms, want %d (each relaxation doubles the previous)", i, f.RelaxedWall, want)
		}
	}
	if got := v4ExecRelaxedWall(t, capDir, "grill"); got != 16000 {
		t.Fatalf("SYMPTOM: execution.json relaxed wall bound is %v, want the doubled 16000", got)
	}
}

// ---------------------------------------------------------------------------
// Scenario 4 — D6 kept faces: long-task relax-then-block, the
// needs_input wait excluded from T, the rewind zeroing a rerun's budget.
// Green BOTH before and after the change (guards).
// ---------------------------------------------------------------------------

// TestV4LongTaskTimeoutRelaxesThenBlocks: step_timeout_long_task retries
// the SAME model with T relaxed x2; a SECOND long-task timeout blocks
// the step. The persisted relaxed bound and the same-model retry are
// the spec's preserved semantics.
func TestV4LongTaskTimeoutRelaxesThenBlocks(t *testing.T) {
	inst := newV1Install(t)
	inst.stripWallBudgets = true
	wi := inst.newWI(t, "v4: long task relaxes once, the second timeout blocks", []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "tool_ms": 600000, "tool_name": "bash"},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "tool_ms": 600000, "tool_name": "bash"},
	}, nil)
	d := newV1Daemon(t, inst, optSupervision(supBounds{
		silence: 30 * time.Second, tool: 5 * time.Minute, stepTimeout: 3 * time.Second,
	}))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "blocked", 120*time.Second)
	fails := v4StepFailures(t, d, wi)
	if len(fails) != 2 {
		t.Fatalf("failure records %+v, want two step_timeout_long_task", fails)
	}
	if fails[0].Cause != "step_timeout_long_task" || fails[0].Disposition != "retry_same_model" || fails[0].RelaxedWall != 6000 {
		t.Fatalf("first timeout %+v, want retry_same_model with T relaxed to 6000ms", fails[0])
	}
	if fails[1].Cause != "step_timeout_long_task" || fails[1].Disposition != "blocked" {
		t.Fatalf("second timeout %+v, want blocked", fails[1])
	}
	x := readExecution(t, capDir)
	if x.WorkflowStatus != "blocked" || !strings.Contains(x.BlockedReason, "second step timeout") {
		t.Fatalf("execution.json: status %q reason %q", x.WorkflowStatus, x.BlockedReason)
	}
	if got := v4ExecRelaxedWall(t, capDir, "grill"); got != 6000 {
		t.Fatalf("the relaxed T must persist: %v", got)
	}
	models := v4SpawnModels(t, capDir)
	if len(models) < 2 || models[0] != models[1] {
		t.Fatalf("long-task timeouts retry the SAME model: %v", models)
	}
}

// TestV4NeedsInputWaitNotCountedIntoT: the wall time a step spends
// PARKED on an owner question must not enter the step's cumulative
// runtime T (T counts agent running time only).
func TestV4NeedsInputWaitNotCountedIntoT(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4: the needs_input wait is not agent runtime", v4Plan([]map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "needs_input",
			"question_id": "q_v4wait", "question": "should the wait count into T?"},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
	}), nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	waitQuestion(t, d, wi, "q_v4wait", 60*time.Second)
	// The owner takes their time: a wait far longer than either of the
	// step's two agent dispatches.
	time.Sleep(6 * time.Second)
	answerQuestion(t, d, wi, "q_v4wait", "No — only agent runtime counts.")
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	x := readExecution(t, capDir)
	wall, _ := x.Steps["grill"].BudgetConsumed["wall_ms"].(float64)
	if wall >= 5000 {
		t.Fatalf("SYMPTOM: grill booked %vms of wall — the 6s owner wait leaked into T (agent runtime is ~2s)", wall)
	}
}

// TestV4RewindRerunZeroesT: a gate-fail rewind archives the interval and
// the rerun's budget starts from zero (the first, slow run's booked
// wall must not survive the rewind).
func TestV4RewindRerunZeroesT(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4: a rewind rerun starts T from zero", []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 5000},
		// The gate fails on a verdict riding a completed envelope.
		{"do": "envelope", "id": "verification", "outcome": "completed", "output": map[string]any{
			"verdict":     "fail",
			"fail_reason": "the tree is still wrong",
			"findings":    []map[string]any{{"severity": "blocking", "detail": "still broken"}},
		}},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		// Only the pre-change tree dispatches this one.
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)

	x := readExecution(t, capDir)
	if c, _ := x.RewindStats["count"].(float64); c < 1 {
		t.Fatalf("the gate-fail rewind never fired: rewind_stats=%v", x.RewindStats)
	}
	wall, _ := x.Steps["fix"].BudgetConsumed["wall_ms"].(float64)
	if wall >= 4000 {
		t.Fatalf("SYMPTOM: the rerun's fix budget is %vms — the 5s the first (archived) run spent survived the rewind", wall)
	}
}

// ---------------------------------------------------------------------------
// Scenario 5 — D7: 429-class errors back off on the SAME model, bounded
// only by T (no retry-count bound below T), with a floor on the backoff.
// ---------------------------------------------------------------------------

// TestV4RateLimitRetriesSameModelWithBackoff: four consecutive
// 429-rate-limit process deaths stay on the ORIGINAL model, the retry
// count is not capped below T (the step completes after four retries —
// more than the current failure-counter bound of 3), and each re-dispatch
// waits a real backoff gap. RED before the change (the 429 text is
// model-class today: the driver switches candidates, the counter climbs
// and blocks), GREEN after.
func TestV4RateLimitRetriesSameModelWithBackoff(t *testing.T) {
	inst := newV1Install(t)
	crash := map[string]any{
		"do": "envelope", "id": "grill", "crash": "before",
		"crash_stderr": "error: provider request failed: HTTP 429 Too Many Requests (rate limit exceeded)",
	}
	actions := []map[string]any{crash, crash, crash, crash,
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true}}
	wi := inst.newWI(t, "v4: 429 rate limit backs off on the same model", v4Plan(actions), nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	// Fail fast on a premature block (the pre-change counter-bound
	// symptom) instead of burning the whole bound.
	waitUntil(t, 180*time.Second, "the rate-limited step never completed or blocked", func() bool {
		x, err := readExecutionFile(capDir)
		return err == nil && (x.WorkflowStatus == "completed" || x.WorkflowStatus == "blocked")
	})
	x := readExecution(t, capDir)
	if x.WorkflowStatus != "completed" {
		t.Fatalf("SYMPTOM: the rate-limited step ended %q (%q) — the retry count was bounded below T",
			x.WorkflowStatus, x.BlockedReason)
	}

	fails := v4StepFailures(t, d, wi)
	if len(fails) < 4 {
		t.Fatalf("failure records %+v, want at least the four scripted rate-limit deaths", fails)
	}
	for i, f := range fails {
		if f.Disposition != "retry_same_model" {
			t.Fatalf("SYMPTOM: rate-limit failure %d disposed as %q (from %q to %q) — must stay on the original model",
				i, f.Disposition, f.From, f.To)
		}
	}
	models := v4SpawnModels(t, capDir)
	if len(models) < 5 {
		t.Fatalf("want at least 5 spawns, got %v", models)
	}
	for i := 1; i < 5; i++ {
		if models[i] != models[0] {
			t.Fatalf("SYMPTOM: spawn %d ran on %q, spawn 1 on %q — a rate limit switched models", i, models[i], models[0])
		}
	}
	// The backoff floor: every re-dispatch after a rate-limit death waits
	// a real interval (the spec's assumption-2 base is 1s; the measured
	// gap also covers the crashed child's short life).
	for i := 1; i <= 4; i++ {
		if gap := v4SpawnGap(t, capDir, i); gap < 800*time.Millisecond {
			t.Fatalf("SYMPTOM: re-dispatch after rate-limit death %d came after only %v — no backoff", i, gap)
		}
	}
	// A rate limit is not a timeout-class death: it must not relax T.
	if got := v4ExecRelaxedWall(t, capDir, "grill"); got != 0 {
		t.Fatalf("SYMPTOM: a rate-limit disposal relaxed T to %vms — only timeout-class deaths relax", got)
	}
}

// ---------------------------------------------------------------------------
// Scenario 6 — C10 + the all-bundle e2e: no release step anywhere, and
// every bundle runs the new contract end to end.
// ---------------------------------------------------------------------------

// v4BundleAssertsNoRelease: after a bundle lane wraps, execution.json
// must carry NO release step and no spawn must ever have been a release
// dispatch.
func v4BundleAssertsNoRelease(t *testing.T, capDir string) {
	t.Helper()
	x := readExecution(t, capDir)
	if _, ok := x.Steps["release"]; ok {
		t.Fatalf("SYMPTOM: execution.json still carries a release step: %+v", x.Steps["release"])
	}
	for _, e := range spawnJournal(t, capDir) {
		if sid, _ := e["step_id"].(string); sid == "release" {
			t.Fatalf("SYMPTOM: a release dispatch ran: %+v", e)
		}
	}
}

// TestV4BundleQuickFixNewContractNoRelease: quick-fix (grill -> fix ->
// verification) wraps under the new envelope contract with no release
// step. RED before the change (release runs and lands in
// execution.json), GREEN after.
func TestV4BundleQuickFixNewContractNoRelease(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4: quick-fix under the new contract", []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		// Only the pre-change tree dispatches this one.
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)
	v4BundleAssertsNoRelease(t, capDir)
}

// TestV4BundleBugFixNewContractNoRelease: bug-fix (repro -> root-cause
// -> fix -> verification -> review) wraps under the new contract; the
// root-cause lane works from the repro artifact by path convention and
// no release step exists.
func TestV4BundleBugFixNewContractNoRelease(t *testing.T) {
	inst := newV1Install(t)
	wi := v4NewWIWithBundle(t, inst, "v4: bug-fix under the new contract", "bug-fix", []map[string]any{
		{"do": "envelope", "id": "repro", "outcome": "completed", "handoff": true,
			"files": map[string]string{"artifacts/repro.md": "# repro\n\nfake agent reproduction record\n"}},
		{"do": "envelope", "id": "root-cause", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		{"do": "envelope", "id": "review", "outcome": "completed"},
		{"do": "envelope", "id": "release", "outcome": "completed"},
	})
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "wrapped", 150*time.Second)
	v4BundleAssertsNoRelease(t, capDir)
}

// TestV4BundleFeatureFullNewContractNoRelease: feature-full (grill ->
// spec -> grill-review -> plan -> e2e-author -> e2e-baseline -> code ->
// e2e-run -> review -> verification) wraps under the new contract with
// no release step.
func TestV4BundleFeatureFullNewContractNoRelease(t *testing.T) {
	inst := newV1Install(t)
	actions := []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "spec", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "grill-review", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "plan", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-author", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-baseline", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "code", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-run", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "review", "outcome": "completed"},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}
	wi := v4NewWIWithBundle(t, inst, "v4: feature-full under the new contract", "feature-full", actions)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	inst.waitWIStatus(t, wi, "wrapped", 240*time.Second)
	v4BundleAssertsNoRelease(t, capDir)
}

// TestV4ReleaseStepDeletedFromBundles: the three bundles' release step
// templates are gone, their workflow frontmatters declare no release
// step, and the bug-fix frontmatter no longer declares a declared-input
// entry (root-cause reads the repro artifact by path convention).
// RED before the change, GREEN after.
func TestV4ReleaseStepDeletedFromBundles(t *testing.T) {
	root := v1Root(t)
	for _, b := range []string{"feature-full", "bug-fix", "quick-fix"} {
		p := filepath.Join(root, "workflows", b, "steps", "release.md")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("SYMPTOM: %s still ships a release step template (%v)", p, err)
		}
		wm, err := os.ReadFile(filepath.Join(root, "workflows", b, "workflow.md"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(wm, []byte(v4Tok("id: ", "release"))) {
			t.Fatalf("SYMPTOM: %s still declares a release step in its frontmatter", b)
		}
		if bytes.Contains(wm, []byte(v4Tok("inputs", ": ["))) {
			t.Fatalf("SYMPTOM: %s still declares a frontmatter declared-input entry (the parser must not know the key)", b)
		}
	}
	// The root-cause template keeps teaching the path-convention read.
	rc, err := os.ReadFile(filepath.Join(root, "workflows", "bug-fix", "steps", "root-cause.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rc, []byte("artifacts/repro.md")) {
		t.Fatalf("the root-cause template no longer points at the repro artifact by path convention")
	}
}

// ---------------------------------------------------------------------------
// Scenario 7 — D15: the real-record replay fixture exists.
// ---------------------------------------------------------------------------

// TestV4D15ReplayFixturePresent: the D15 verification runs on a REAL
// collected pi session record (never hand-built) checked into the
// supervise package's testdata before any judgment code changes. The
// fixture's presence and parseability are the e2e-visible bar; the
// replay semantics themselves are the change's own red-green tests.
// RED before the change, GREEN after.
func TestV4D15ReplayFixturePresent(t *testing.T) {
	p := filepath.Join(v1Root(t), "internal", "polyforge", "supervise", "testdata", "pi-session-replay.jsonl")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("SYMPTOM: the D15 replay fixture is missing (%v): the spec requires a real collected pi session record replayed through the existing judgment path before any changes", err)
	}
	lines := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		if !json.Valid([]byte(line)) {
			t.Fatalf("replay fixture line %d is not a JSON record", lines)
		}
	}
	if lines < 10 {
		t.Fatalf("replay fixture carries only %d records — too thin to exercise the agent_end / willRetry / compaction order", lines)
	}
}

// ---------------------------------------------------------------------------
// Scenario 8 — the docs bar: context_policy and handoff semantics
// written into the design docs.
// ---------------------------------------------------------------------------

// TestV4DocsStateContextPolicyAndHandoff: 01-workflow-skills and
// v2-converge must state the context_policy (inherit/fresh) and handoff
// (path convention, no digest) semantics. RED before the change (neither
// doc mentions either term today), GREEN after.
func TestV4DocsStateContextPolicyAndHandoff(t *testing.T) {
	root := v1Root(t)
	for _, rel := range []string{
		filepath.Join("docs", "design", "polyforge-vnext", "01-workflow-skills.md"),
		filepath.Join("docs", "design", "polyforge-vnext", "v2-converge.md"),
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, term := range []string{"context_policy", "handoff"} {
			if !bytes.Contains(raw, []byte(term)) {
				t.Fatalf("SYMPTOM: %s does not state the %s semantics (the spec requires both docs to)", rel, term)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 9 — the retired-face grep floor (the deletions really are
// deletions, no compatibility branch anywhere in production).
// ---------------------------------------------------------------------------

// v4RetiredTokens is the retired-face grep list: every token whose
// absence is unambiguous after the subtraction. Assembled at runtime
// from fragments so this file never carries a policed token contiguously.
func v4RetiredTokens() []string {
	return []string{
		// C4: the rejection-relaunch layer.
		v4Tok("Reject", "Relaunches"),
		v4Tok("Trans", "RejectEscalated"),
		// D6: the per-dispatch budget-window rebasing.
		v4Tok("Runtime", "BaseMs"),
		v4Tok("runtime", "_base_ms"),
		// C2/C3: the old envelope protocol + its leniency layer.
		v4Tok("Accepted", "ResultID"),
		v4Tok("Protocol", "Version"),
		v4Tok("result", "_id"),
		v4Tok("protocol", "_version"),
		v4Tok("handoff", "_ref"),
		v4Tok("command", "_evidence"),
		v4Tok("Tolerant", " re-parse"),
		v4Tok("may ", "omit"),
		// C5: the unused frontmatter keys and their mirrors.
		v4Tok("Input", "Ref"),
		v4Tok("ext", "Output"),
		v4Tok("Requires", "CommandEvidence"),
		v4Tok("Require", "CommandEvidence"),
		v4Tok("Include", "Verdict"),
		v4Tok("Silence", "Minutes"),
		v4Tok("Tool", "Minutes"),
		v4Tok(`yaml:"`, `inputs"`),
		v4Tok(`yaml:"`, `outputs"`),
		v4Tok(`yaml:"`, `schema"`),
		v4Tok(`yaml:"`, `requires_command_evidence"`),
		v4Tok(`yaml:"`, `silence_minutes"`),
		v4Tok(`yaml:"`, `tool_minutes"`),
		v4Tok(`yaml:"`, `include_verdict"`),
		// C10: the driver's release hard-binding.
		v4Tok("release", "StepID"),
		v4Tok("release", "VerdictBinding"),
		// Dead code.
		v4Tok("func (d *Driver) ", "Rewind"),
		v4Tok("func (d *Driver) ", "Terminate"),
		v4Tok("std", "Logger"),
		v4Tok(`json:"`, `usd"`),
		v4Tok(`json:"`, `tokens"`),
		v4Tok("Wait", " alive"),
		v4Tok("COLLECT", "ED:"),
		v4Tok("SpawnStep", ": step="),
	}
}

// v4WalkProductionGo walks the production Go surfaces (internal/, cmd/,
// test/fakepi/; _test.go files excluded — the retired-mechanism unit
// tests are the change's own rewrite, verified by its own bar) and
// hands each file's bytes to fn.
func v4WalkProductionGo(root string, fn func(rel string, raw []byte)) error {
	for _, dir := range []string{"internal", "cmd", filepath.Join("test", "fakepi")} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				return nil
			}
			name := de.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, path)
			fn(rel, raw)
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
	}
	return nil
}

// TestV4RetiredFaceGrepClean: zero hits of every retired-face token on
// the production Go surfaces. RED before the change (the faces are
// present by design), GREEN after — and a surviving compatibility
// branch or a leftover mirror fails it just the same.
func TestV4RetiredFaceGrepClean(t *testing.T) {
	root := v1Root(t)
	terms := v4RetiredTokens()
	var hits []string
	err := v4WalkProductionGo(root, func(rel string, raw []byte) {
		for _, term := range terms {
			if bytes.Contains(raw, []byte(term)) {
				hits = append(hits, rel+": "+term)
			}
		}
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("SYMPTOM: retired-face terms still present on production surfaces (%d hits):\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// ---------------------------------------------------------------------------
// Scenario 10 — kept face: the tree stays buildable (the dead-code
// deletion must not break the build). Green before AND after.
// ---------------------------------------------------------------------------

// TestV4GoBuildStaysGreen runs the project's own build component of the
// green bar.
func TestV4GoBuildStaysGreen(t *testing.T) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = v1Root(t)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed: %v: %s", err, out)
	}
}
