// drive.go: FAKEPI_MODE=drive — the dogfood executor double. Where the
// idle/ask modes only stage a frozen capsule state, the drive mode runs a
// MECHANICAL stand-in for the pf-execute main loop (01-workflow-skills.md
// §6.5) against the capsule the real daemon materialized:
//
//   - the workflow graph, rewind bounds, budgets and model candidates are
//     read from the MATERIALIZED workflow.md through the REAL
//     internal/workflow/schema parser (03 §4.5 read-only snapshot);
//   - every runtime-state write goes through the REAL internal/capsule
//     write face (MakeWritable + SaveExecution: tmp+fsync+rename, the
//     03 §4.2 commit contract, INV-6 permission flips included);
//   - the infra-failure lane's attempt archive and candidate-ordinal
//     recomputation are the REAL internal/workflow/drive functions
//     (AppendInfraFailure / CandidateOrdinal — 01 §6.3);
//   - every sync trigger is a REAL publish through the daemon's local
//     proxy (04 §5.3 proxy_call), and every agent-driven Core close
//     (OVER_BUDGET, failed_gate, failed_rewind_exhausted) obtains the
//     daemon-generated flush_evidence from its own publish_request and
//     hands it to report_step_result exactly as skills/pf-execute §9/§11
//     prescribe — the agent itself holds no credentials.
//
// Fault injection (the scenario's knobs, all FAKEPI_* environment):
//
//	FAKEPI_FAULT_STEP        the step id the fault knobs apply to
//	FAKEPI_GATE_FAILS        N: the gate step returns a FAIL verdict its
//	                         first N dispatches (business judgment)
//	FAKEPI_PROVIDER_ERRORS   N: the step's first N dispatches die with an
//	                         injected provider_error (infrastructure
//	                         failure, never a verdict)
//	FAKEPI_BUDGET_HOG        the step's every dispatch over-consumes
//	                         requests (drives WF-103 OVER_BUDGET)
//	FAKEPI_HOLD_BEFORE       park right before this step's dispatch
//	                         (AFTER its start write: the held step is in
//	                         flight — in_progress, position moved, and the
//	                         published HEAD carries that state)
//	FAKEPI_HOLD_AFTER_REWIND park right after a rewind lands, before the
//	                         target re-dispatch (lets the scenario observe
//	                         the rewound state directly)
//	FAKEPI_RELEASE_FILE      a hold point parks until this file exists
//	FAKEPI_PROXY_SOCKET      the daemon UDS face for the proxy calls
//
// Every dispatch / rewind / answer / budget event is journaled to
// logs/drive.jsonl (a sync surface, so it publishes and recovers with the
// capsule) — the scenario's assertion ledger.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/workflow/drive"
	"github.com/silgrid/polyforge/internal/workflow/schema"
	"gopkg.in/yaml.v3"
)

// driveQuestionID is the persisted needs_input question the interactive
// step asks (deterministic so the scenario can answer by id).
const driveQuestionID = "q_drv001"

// driveHogRequests is the extra per-dispatch request consumption the
// budget hog injects (large enough to blow a tiny step ceiling on the
// first dispatch).
const driveHogRequests = 10

// driveEnv is the shared state of the drive executor: the RPC control
// loop (main goroutine) and the drive loop (background goroutine) meet
// here. The write gate is the pf/quiesce discipline (SKILL §12): between
// a quiesce ack and its pf/resume the control loop HOLDS the gate token,
// so the drive loop cannot start a new sync-surface write inside the
// daemon's freeze window.
type driveEnv struct {
	cfg driveConfig

	cap *capsule.Capsule
	idn *capsule.Identity
	fm  *schema.Frontmatter

	// gate carries one token; "holding the gate" = owning the token.
	gate chan struct{}

	// quiesced marks the OPEN pf/quiesce window (SKILL §12). The window
	// is a STATE, not a single token hand-off: the daemon's one close
	// path (04 §6.2) sends pf/stop_notice, then an EXTRA pf/quiesce whose
	// ack it discards, and the final flush's freeze window then sends a
	// SECOND pf/quiesce with no pf/resume between the two — a real agent
	// already inside the window simply re-confirms. The gate token is
	// held for exactly as long as the window is open.
	quiesced bool

	mu   sync.Mutex // guards exec, wfBudget, journals and the fault counters
	exec *capsule.Execution

	wfBudget *workflowBudget // effective workflow-level ceiling (proxy read)

	// per-step fault counters (consumed by dispatch).
	providerErrs map[string]int
	gateFails    map[string]int
}

// driveConfig is the parsed FAKEPI_* knob set.
type driveConfig struct {
	faultStep       string
	gateFails       int
	providerErrors  int
	budgetHog       bool
	holdBefore      string
	holdAfterRewind bool
	releaseFile     string
	proxySocket     string
	stopAdvancing   bool // set by pf/stop_notice (guarded by mu)
}

// workflowBudget is the effective workflow-level ceiling read through
// the real proxy (read_workflow_ref -> GET /budget, 02 §5.1 row 12).
type workflowBudget struct {
	requests *int64
}

// newDriveEnv assembles the executor state from the capsule the real
// daemon created and materialized. A missing/broken capsule parks the
// agent with a loud stderr line (the scenario then fails on its own
// bounded wait — never a silent no-op).
func newDriveEnv() *driveEnv {
	cfg := driveConfig{
		faultStep:       os.Getenv("FAKEPI_FAULT_STEP"),
		gateFails:       envInt("FAKEPI_GATE_FAILS"),
		providerErrors:  envInt("FAKEPI_PROVIDER_ERRORS"),
		budgetHog:       os.Getenv("FAKEPI_BUDGET_HOG") != "",
		holdBefore:      os.Getenv("FAKEPI_HOLD_BEFORE"),
		holdAfterRewind: os.Getenv("FAKEPI_HOLD_AFTER_REWIND") != "",
		releaseFile:     os.Getenv("FAKEPI_RELEASE_FILE"),
		proxySocket:     os.Getenv("FAKEPI_PROXY_SOCKET"),
	}
	wi := wiID()
	if wi == "" {
		fmt.Fprintln(os.Stderr, "fakepi drive: PF_WORK_ITEM_ID not set; parking")
		return nil
	}
	wsRoot := filepath.Dir(filepath.Dir(mustWD()))
	capc, idn, err := capsule.Open(wsRoot, wi)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: capsule open failed (cwd is not a live capsule?): %v; parking\n", err)
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(capc.Dir, "workflow.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: materialized workflow.md unreadable: %v; parking\n", err)
		return nil
	}
	front, _, ok := schema.SplitFrontmatter(raw)
	if !ok {
		fmt.Fprintln(os.Stderr, "fakepi drive: materialized workflow.md carries no frontmatter; parking")
		return nil
	}
	var fm schema.Frontmatter
	if err := yaml.Unmarshal(front, &fm); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: workflow frontmatter unparsable: %v; parking\n", err)
		return nil
	}
	env := &driveEnv{
		cfg: cfg, cap: capc, idn: idn, fm: &fm,
		gate:         make(chan struct{}, 1),
		providerErrs: map[string]int{},
		gateFails:    map[string]int{},
	}
	env.gate <- struct{}{} // the single write-gate token
	return env
}

// ---------------------------------------------------------------------------
// The executor main loop (01 §6.5).

// runDriveLoop repeatedly resolves next_step_by_order, dispatches, and
// acts on the outcome. It runs on its own goroutine; the RPC control loop
// keeps answering control methods on the main goroutine, so a publish's
// pf/quiesce is always ackable while a proxy call is in flight.
func runDriveLoop(env *driveEnv) {
	if env == nil {
		parkForever()
	}
	if err := env.initExecution(); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: execution init failed: %v; parking\n", err)
		parkForever()
	}
	env.readBudgetRef()
	for {
		if env.stopAdvancing() {
			// pf/stop_notice (graceful shutdown): stop advancing at the
			// next safe boundary (SKILL §12) — the daemon's close owns
			// the flush and the pause from here.
			journalDrive(env, map[string]any{"type": "hold", "point": "stop_notice"})
			parkForever()
		}
		stepID, idx := env.nextStepByOrder()
		if stepID == "" {
			// INV-1e: "no next step" is EITHER all-done or a recorded
			// failure terminal; the terminal variants close and park
			// inside gateFail, so reaching here with a running status
			// means every step reached done — mark completed and fire
			// the final-outcome trigger.
			if err := env.commit(func(x *capsule.Execution) {
				x.WorkflowStatus = capsule.StatusCompleted
			}); err != nil {
				fmt.Fprintf(os.Stderr, "fakepi drive: completed write failed: %v; parking\n", err)
			}
			env.publishProxy("wi_outcome")
			journalDrive(env, map[string]any{"type": "completed"})
			parkForever()
		}
		env.runStep(stepID, idx)
	}
}

// initExecution implements the recovery-first rule (SKILL §1): the ONLY
// source of "what has already happened" is runtime-state/execution.json —
// never process memory. A fresh capsule starts with every step pending.
func (e *driveEnv) initExecution() error {
	ex, err := e.cap.LoadExecution()
	if err != nil {
		if err != capsule.ErrNoExecutionState {
			return err
		}
		ex = &capsule.Execution{
			CapsuleID: e.cap.ID(), WorkItemID: wiID(), Generation: e.idn.Generation,
			BundleDigest: e.idn.BundleDigest, WorkflowStatus: capsule.StatusRunning,
			Steps: map[string]*capsule.Step{}, ReviewRounds: map[string]int64{},
			Version: 0, // commit() bumps to 1
		}
		for _, st := range e.fm.Steps {
			ex.Steps[st.ID] = &capsule.Step{Status: capsule.StepPending}
		}
	}
	e.mu.Lock()
	e.exec = ex
	e.mu.Unlock()
	return e.commit(func(x *capsule.Execution) {}) // persist the base state
}

// nextStepByOrder scans the frontmatter steps array in declared order and
// selects the earliest step whose status is not terminal (01 §6.5; V3:
// the array order is the only order).
func (e *driveEnv) nextStepByOrder() (string, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, st := range e.fm.Steps {
		if x := e.exec.Steps[st.ID]; x != nil && x.Status != capsule.StepDone {
			return st.ID, i
		}
	}
	return "", -1
}

// stepDecl returns the frontmatter declaration of one step id.
func (e *driveEnv) stepDecl(stepID string) *schema.Step {
	for i := range e.fm.Steps {
		if e.fm.Steps[i].ID == stepID {
			return &e.fm.Steps[i]
		}
	}
	return nil
}

// stepIndex returns the declared position of one step id (-1 unknown).
func (e *driveEnv) stepIndex(stepID string) int {
	for i := range e.fm.Steps {
		if e.fm.Steps[i].ID == stepID {
			return i
		}
	}
	return -1
}

// isGate / isInteractive decode the optional frontmatter booleans.
func isGate(st *schema.Step) bool { return st.Gate != nil && *st.Gate }
func isInteractive(st *schema.Step) bool {
	return st.Interactive != nil && *st.Interactive
}

// stopAdvancing reports the pf/stop_notice flag.
func (e *driveEnv) stopAdvancing() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.stopAdvancing
}

// setStopAdvancing is the pf/stop_notice landing (control goroutine).
func (e *driveEnv) setStopAdvancing() {
	e.mu.Lock()
	e.cfg.stopAdvancing = true
	e.mu.Unlock()
}

// enterQuiesce opens the pf/quiesce window (or re-confirms an already
// open one): take the write-gate token — waiting for any in-flight
// sync-surface write to land first — and hold it until pf/resume.
// Idempotent by design: a second quiesce inside an open window must ack
// immediately, or the daemon's close path (which quiesces once for the
// boundary and once more from the final flush's freeze) deadlocks on
// the writer that never confirms.
func (e *driveEnv) enterQuiesce() {
	e.mu.Lock()
	if e.quiesced {
		e.mu.Unlock()
		return // already inside the window: nothing new to confirm
	}
	e.mu.Unlock()
	<-e.gate // let the in-flight write finish, then hold the token
	e.mu.Lock()
	e.quiesced = true
	e.mu.Unlock()
}

// leaveResume closes the pf/quiesce window (pf/resume): sync-surface
// writes may start again. A resume with no open window is a no-op — the
// token is returned exactly once per window.
func (e *driveEnv) leaveResume() {
	e.mu.Lock()
	open := e.quiesced
	e.quiesced = false
	e.mu.Unlock()
	if open {
		e.gate <- struct{}{}
	}
}

// commit is the ONE write path: quiesce gate + mutex + the REAL capsule
// write face, with the version bump the capsule writer requires.
func (e *driveEnv) commit(mut func(*capsule.Execution)) error {
	<-e.gate
	defer func() { e.gate <- struct{}{} }()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exec.Version++
	mut(e.exec)
	if err := e.cap.MakeWritable(); err != nil {
		return err
	}
	return e.cap.SaveExecution(e.exec)
}

// ---------------------------------------------------------------------------
// One step: dispatch loop (infra failures re-dispatch in place), gate
// semantics, the interactive needs_input wait, budget accounting.

// dispatchOutcome is one fake step-agent dispatch's result.
type dispatchOutcome struct {
	providerError bool
	verdict       string // PASS | FAIL for gates, "" otherwise
	detail        string
}

// runStep drives one step to a business outcome or a typed terminal.
func (e *driveEnv) runStep(stepID string, declIdx int) {
	decl := e.stepDecl(stepID)
	// Start the step: consume any pending rewind input in the SAME write
	// that flips it to in_progress (SKILL §4 step 2).
	rewindConsumed := false
	if err := e.commit(func(x *capsule.Execution) {
		st := x.Steps[stepID]
		if st.PendingRewindInput != nil {
			rewindConsumed = true
			st.PendingRewindInput = nil
		}
		st.Status = capsule.StepInProgress
		st.StepStartedAt = nowStamp()
		st.Output = nil
		st.BudgetConsumed = capsule.Budget{}
		x.CurrentStep = stepID
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: step start write failed: %v; parking\n", err)
		parkForever()
	}
	// The FAKEPI_HOLD_BEFORE park point: the step is STARTED (its start
	// write landed — in_progress, position moved, recoverable from the
	// published HEAD) and the loop parks right before its first dispatch:
	// the "step in flight when the operator shuts the machine down" shape.
	if e.cfg.holdBefore == stepID {
		journalDrive(e, map[string]any{"type": "hold", "point": "before:" + stepID})
		holdPoint(e)
	}

	// The interactive needs_input wait (SKILL §7) sits inside the step:
	// the step is started, parks on its open question, and the SAME step
	// continues in place once the relayed answer is consumed.
	if isInteractive(decl) {
		e.askAndWait(stepID)
	}

	for {
		out := e.fakeDispatch(stepID, decl, rewindConsumed)
		rewindConsumed = false // only the first dispatch of a re-entry consumes it
		if out.providerError {
			// 01 §6.3: infrastructure failure — same step, IN PLACE.
			// Archive the attempt, reset the per-attempt budget window,
			// recompute the ordinal from the archive, re-dispatch.
			detail := out.detail
			if err := e.commit(func(x *capsule.Execution) {
				st := x.Steps[stepID]
				consumed := st.BudgetConsumed
				drive.AppendInfraFailure(st, st.StepStartedAt, nowStamp(), detail, &consumed)
				st.BudgetConsumed = capsule.Budget{}
			}); err != nil {
				fmt.Fprintf(os.Stderr, "fakepi drive: infra archive write failed: %v; parking\n", err)
				parkForever()
			}
			e.publishProxy("step_interrupted")
			continue
		}
		// Budget was consumed inside the dispatch; judge both layers now
		// (01 §6.6: whichever bound is hit first wins and reports its own
		// error).
		if ev := e.checkBudget(stepID, decl); ev != nil {
			e.overBudgetTerminal(stepID, ev)
			parkForever()
		}
		if isGate(decl) && out.verdict == "FAIL" {
			e.gateFail(stepID, declIdx, decl, out)
			return // rewound (the main loop resumes at target) or terminal+parked
		}
		// Business success: mark done with a schema-conforming output and
		// fire the step-completed trigger.
		candidate, _, _ := e.candidateUsed(stepID, decl)
		output := e.buildOutput(stepID, out.verdict, candidate)
		if err := e.commit(func(x *capsule.Execution) {
			st := x.Steps[stepID]
			st.Status = capsule.StepDone
			st.Output = output
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fakepi drive: step done write failed: %v; parking\n", err)
			parkForever()
		}
		e.writeStepArtifact(stepID, output)
		e.publishProxy("step_completed")
		return
	}
}

// fakeDispatch runs one scripted step-agent dispatch and consumes budget.
// The knobs are per step and strictly ordered: provider errors first (an
// infrastructure failure produces NO business judgment), then the budget
// consumption, then the gate verdict.
func (e *driveEnv) fakeDispatch(stepID string, decl *schema.Step, rewindConsumed bool) dispatchOutcome {
	candidate, ordinal, source := e.candidateUsed(stepID, decl)
	e.mu.Lock()
	inProgress := e.exec.Steps[stepID].Status == capsule.StepInProgress
	e.mu.Unlock()

	out := dispatchOutcome{}
	if e.cfg.faultStep == stepID && e.cfg.providerErrors > 0 {
		e.mu.Lock()
		if e.providerErrs[stepID] < e.cfg.providerErrors {
			e.providerErrs[stepID]++
			e.mu.Unlock()
			out.providerError = true
			out.detail = fmt.Sprintf("provider_error: candidate %s upstream connection reset (fake-harness injected)", candidate)
			e.consumeBudget(stepID, 1)
			e.journalDispatch(stepID, candidate, ordinal, source, "provider_error", inProgress, rewindConsumed, out)
			return out
		}
		e.mu.Unlock()
	}
	// Consume: one request per dispatch, plus the hog blast.
	consumed := int64(1)
	if e.cfg.faultStep == stepID && e.cfg.budgetHog {
		consumed += driveHogRequests
	}
	e.consumeBudget(stepID, consumed)

	if isGate(decl) && e.cfg.faultStep == stepID && e.cfg.gateFails > 0 {
		e.mu.Lock()
		if e.gateFails[stepID] < e.cfg.gateFails {
			e.gateFails[stepID]++
			e.mu.Unlock()
			out.verdict = "FAIL"
			out.detail = "the polished build does not satisfy the acceptance criteria (fake verdict)"
			e.journalDispatch(stepID, candidate, ordinal, source, "gate_fail", inProgress, rewindConsumed, out)
			return out
		}
		e.mu.Unlock()
	}
	if isGate(decl) {
		out.verdict = "PASS"
	}
	e.journalDispatch(stepID, candidate, ordinal, source, "done", inProgress, rewindConsumed, out)
	return out
}

// consumeBudget adds one attempt's consumption to BOTH layers (01 §6.6:
// the per-step attempt window and the workflow-wide cumulative). The
// numbers persist with the NEXT status write — never in memory across a
// sync trigger.
func (e *driveEnv) consumeBudget(stepID string, requests int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exec.Steps[stepID].BudgetConsumed.Requests += requests
	e.exec.Steps[stepID].BudgetConsumed.Tokens += 100
	e.exec.BudgetConsumed.Requests += requests
	e.exec.BudgetConsumed.Tokens += 100
}

// checkBudget judges both layers after one dispatch's consumption
// (01 §6.6: the step level first — each layer reports its own error).
func (e *driveEnv) checkBudget(stepID string, decl *schema.Step) map[string]any {
	e.mu.Lock()
	stepConsumed := e.exec.Steps[stepID].BudgetConsumed.Requests
	total := e.exec.BudgetConsumed.Requests
	wf := e.wfBudget
	e.mu.Unlock()
	if decl.Budget != nil && decl.Budget.Requests != nil && stepConsumed > *decl.Budget.Requests {
		return map[string]any{
			"code": "WF-103", "level": "step", "step_id": stepID,
			"dimension": "requests", "consumed": stepConsumed, "limit": *decl.Budget.Requests,
		}
	}
	if wf != nil && wf.requests != nil && total > *wf.requests {
		return map[string]any{
			"code": "WF-103", "level": "workflow", "step_id": stepID,
			"dimension": "requests", "consumed": total, "limit": *wf.requests,
		}
	}
	return nil
}

// overBudgetTerminal lands the OVER_BUDGET typed interrupt (SKILL §9):
// stop the step, reflect Core's blocked state in the capsule, obtain the
// daemon-generated flush_evidence from the agent's OWN publish, and end
// the generation through report_step_result — the agent holds no
// credentials of its own. NOT a FAIL (no verdict, no rewind archive) and
// never a crash continuation.
func (e *driveEnv) overBudgetTerminal(stepID string, ev map[string]any) {
	ev["type"] = "budget_event"
	journalDrive(e, ev)
	if err := e.commit(func(x *capsule.Execution) {
		// The typed interrupt shape: blocked + current_step UNCHANGED +
		// the step still in_progress (03 §4.2's blocked reflection).
		x.WorkflowStatus = capsule.StatusBlocked
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: over-budget write failed: %v; parking\n", err)
	}
	fe := e.publishProxy("block")
	e.reportStepResult("block", "owner", fe)
	journalDrive(e, map[string]any{"type": "terminal", "workflow_status": capsule.StatusBlocked,
		"core_reason": "block", "waiting_on": "owner", "event": ev["code"]})
}

// gateFail implements 01 §6.2 (the bounded on_fail rewind) and its two
// terminal variants (WF-102 failed_gate, WF-101 failed_rewind_exhausted).
func (e *driveEnv) gateFail(gateID string, gateIdx int, decl *schema.Step, out dispatchOutcome) {
	// Persist the FAIL verdict on the gate step first (01 §6.2: the FAIL
	// 判定全文 rides the next publish; the gate step is NOT done).
	candidate, _, _ := e.candidateUsed(gateID, decl)
	verdictOutput := e.buildOutput(gateID, "FAIL", candidate)
	verdictText := verdictText(out, verdictOutput)
	if err := e.commit(func(x *capsule.Execution) {
		x.Steps[gateID].Output = verdictOutput
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: gate verdict write failed: %v; parking\n", err)
		parkForever()
	}
	onFail := decl.OnFail
	if onFail == nil || onFail.Action == "" || onFail.Action == "terminate" {
		e.failTerminal(gateID, capsule.StatusFailedGate, verdictText)
		parkForever()
	}
	target := onFail.Target
	maxTotal, maxPerStep := e.rewindBounds()
	e.mu.Lock()
	total := e.exec.RewindStats.Total
	perStep := e.exec.RewindStats.ByStep[target]
	e.mu.Unlock()
	if total >= maxTotal || perStep >= maxPerStep {
		e.failTerminal(gateID, capsule.StatusFailedRewindExhausted, verdictText)
		parkForever()
	}
	// The rewind itself (01 §6.2 steps 3-6): counters ++, archive every
	// step in [target, gate) as a gate_fail_rewind attempt, reset each to
	// pending with output cleared, position back to target, and route the
	// verdict to the target as pending_rewind_input — ONE atomic write.
	targetIdx := e.stepIndex(target)
	archived := []string{}
	if err := e.commit(func(x *capsule.Execution) {
		x.RewindStats.Total++
		if x.RewindStats.ByStep == nil {
			x.RewindStats.ByStep = map[string]int64{}
		}
		x.RewindStats.ByStep[target]++
		for i := targetIdx; i < gateIdx; i++ {
			sid := e.fm.Steps[i].ID
			st := x.Steps[sid]
			seq := 0
			for _, a := range st.Attempts {
				if a.AttemptSeq > seq {
					seq = a.AttemptSeq
				}
			}
			raw, _ := json.Marshal(st.Output)
			consumed := st.BudgetConsumed
			st.Attempts = append(st.Attempts, capsule.Attempt{
				Cause: drive.CauseGateFailRewind, AttemptSeq: seq + 1,
				StartedAt: st.StepStartedAt, ArchivedAt: nowStamp(),
				RewindGate: gateID, Result: json.RawMessage(raw),
				ConsumedBudget: &consumed,
			})
			st.Status = capsule.StepPending
			st.Output = nil
			st.PendingQuestion = nil
			st.PendingRewindInput = nil
			archived = append(archived, sid)
		}
		x.CurrentStep = target
		include := onFail.IncludeVerdict == nil || *onFail.IncludeVerdict
		if include {
			x.Steps[target].PendingRewindInput = &capsule.RewindInput{
				GateStepID: gateID, VerdictText: verdictText, RewoundAt: nowStamp(),
			}
		}
		// A rewind costs one request unit of workflow budget (01 §6.6).
		x.BudgetConsumed.Requests++
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: rewind write failed: %v; parking\n", err)
		parkForever()
	}
	journalDrive(e, map[string]any{"type": "rewind", "gate": gateID, "target": target,
		"archived": archived, "verdict_text": verdictText})
	e.publishProxy("step_interrupted")
	if e.cfg.holdAfterRewind {
		journalDrive(e, map[string]any{"type": "hold", "point": "after_rewind"})
		holdPoint(e)
	}
	// The main loop's next next_step_by_order lands on target (INV-1d):
	// the reset-to-pending steps are re-run by selection, never by a
	// second trigger.
}

// failTerminal lands failed_gate / failed_rewind_exhausted: a capsule-side
// terminal plus the SAME Core close shape as OVER_BUDGET (01 §6.5: Core
// blocked, waiting_on=owner — never a Core terminal, never a wrap).
func (e *driveEnv) failTerminal(gateID, status, verdictText string) {
	if err := e.commit(func(x *capsule.Execution) {
		x.WorkflowStatus = status
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: terminal write failed: %v; parking\n", err)
	}
	fe := e.publishProxy("block")
	e.reportStepResult("block", "owner", fe)
	journalDrive(e, map[string]any{"type": "terminal", "workflow_status": status,
		"core_reason": "block", "waiting_on": "owner", "gate": gateID, "verdict_text": verdictText})
}

// rewindBounds resolves rewind.max_total / max_per_step with the schema
// defaults (4 / 2, 01 §4.1 rule V6).
func (e *driveEnv) rewindBounds() (int64, int64) {
	maxTotal, maxPerStep := int64(4), int64(2)
	if e.fm.Rewind != nil {
		if e.fm.Rewind.MaxTotal != nil {
			maxTotal = *e.fm.Rewind.MaxTotal
		}
		if e.fm.Rewind.MaxPerStep != nil {
			maxPerStep = *e.fm.Rewind.MaxPerStep
		}
	}
	return maxTotal, maxPerStep
}

// candidateUsed resolves the effective model candidate for the CURRENT
// dispatch: the REAL drive.CandidateOrdinal recomputation over the
// attempts[] archive (01 §6.3 step 4 — a 1-BASED ordinal, capped by the
// list length, recomputed from persisted state only), then the list's
// ordinal-TH entry. The no-candidate fallback keeps the adapter default.
func (e *driveEnv) candidateUsed(stepID string, decl *schema.Step) (candidate string, ordinal int, source string) {
	list := decl.ModelCandidates
	source = "step"
	if list == nil && e.fm.Defaults != nil {
		list = e.fm.Defaults.ModelCandidates
		source = "defaults"
	}
	if len(list) == 0 {
		return "", 0, "adapter-default"
	}
	e.mu.Lock()
	ord := drive.CandidateOrdinal(e.exec.Steps[stepID], len(list))
	e.mu.Unlock()
	return list[ord-1], ord, source
}

// ---------------------------------------------------------------------------
// The interactive needs_input wait (SKILL §7).

// askAndWait persists the open pending_question in the same write that
// parks the step, fires the INTERRUPTING publish (the step-interrupted
// trigger rides the same transition, 01 §4.3), then waits until the
// answer consumption (driveConsumeAnswer, on the control goroutine)
// clears it — the SAME step continues in place, never a new dispatch.
func (e *driveEnv) askAndWait(stepID string) {
	content := "Which shape should step " + stepID + " take: the lean variant or the compatible one?"
	if err := e.commit(func(x *capsule.Execution) {
		x.Steps[stepID].PendingQuestion = &capsule.PendingQuestion{
			QuestionID: driveQuestionID, AskedAt: nowStamp(), Content: content,
		}
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: ask write failed: %v; parking\n", err)
		parkForever()
	}
	journalDrive(e, map[string]any{"type": "ask", "step": stepID, "question_id": driveQuestionID, "content": content})
	e.publishProxy("step_interrupted")
	for {
		e.mu.Lock()
		open := e.exec.Steps[stepID].PendingQuestion != nil
		e.mu.Unlock()
		if !open {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// driveConsumeAnswer is the control-goroutine half of the relay round
// trip (pf/answer, 00 §3.4a): journal the FIVE-FIELD relay verbatim, then
// archive the answer and clear pending_question in the SAME atomic write
// (03 §4.2 D9), append the pair to the shared grill-qa log, and only then
// let the caller ack — the ack is the durability signal.
func driveConsumeAnswer(env *driveEnv, params []byte) {
	var msg struct {
		Params struct {
			QuestionID string `json:"question_id"`
			AnswerID   string `json:"answer_id"`
			AnswerText string `json:"answer_text"`
			AnsweredAt string `json:"answered_at"`
			AnsweredBy string `json:"answered_by"`
		} `json:"params"`
	}
	if json.Unmarshal(params, &msg) != nil {
		return
	}
	journalDrive(env, map[string]any{"type": "answer_relay", "question_id": msg.Params.QuestionID,
		"answer_id": msg.Params.AnswerID, "answer_text": msg.Params.AnswerText,
		"answered_at": msg.Params.AnsweredAt, "answered_by": msg.Params.AnsweredBy})
	<-env.gate
	err := func() error {
		env.mu.Lock()
		defer env.mu.Unlock()
		if env.exec.CurrentStep == "" || env.exec.Steps[env.exec.CurrentStep] == nil {
			return nil
		}
		step := env.exec.Steps[env.exec.CurrentStep]
		if step.PendingQuestion == nil || step.PendingQuestion.QuestionID != msg.Params.QuestionID {
			return nil // not the waiting question: nothing to consume
		}
		content := step.PendingQuestion.Content
		step.Answers = append(step.Answers, capsule.ArchivedAnswer{
			QuestionID: msg.Params.QuestionID, AnswerText: msg.Params.AnswerText,
			AnsweredAt: msg.Params.AnsweredAt, AnsweredBy: msg.Params.AnsweredBy,
		})
		step.PendingQuestion = nil
		env.exec.Version++
		if err := env.cap.MakeWritable(); err != nil {
			return err
		}
		if err := env.cap.SaveExecution(env.exec); err != nil {
			return err
		}
		appendGrillQA(env, env.exec.CurrentStep, msg.Params.QuestionID, content,
			msg.Params.AnswerText, msg.Params.AnsweredBy, msg.Params.AnsweredAt)
		return nil
	}()
	env.gate <- struct{}{}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: answer consumption write failed: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// Outputs, artifacts, journals.

// buildOutput synthesizes a step output conforming to the step skill's
// pinned output schema (the materialized .pi/agents/<id>.md, parsed with
// the REAL schema.ParseStepSkill): every required property gets a typed
// value; the verdict/candidate/qa_log conventions carry the dispatch's
// own facts.
func (e *driveEnv) buildOutput(stepID, verdict, candidate string) map[string]any {
	out := map[string]any{"summary": stepID + " fakepi-drive output"}
	raw, err := os.ReadFile(filepath.Join(e.cap.Dir, ".pi", "agents", stepID+".md"))
	if err != nil {
		return out
	}
	skill, err := schema.ParseStepSkill(stepID, string(raw))
	if err != nil {
		return out
	}
	if ap, ok := skill.Output["additionalProperties"].(bool); ok && !ap {
		out = map[string]any{}
	}
	if req, ok := skill.Output["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			switch name {
			case "verdict":
				v := verdict
				if v == "" {
					v = "PASS"
				}
				out["verdict"] = v
			case "candidate":
				out["candidate"] = candidate
			case "qa_log":
				out["qa_log"] = "artifacts/grill-qa.md"
			case "summary":
				out["summary"] = stepID + " fakepi-drive output"
			default:
				out[name] = stepID + " fakepi-drive output"
			}
		}
	}
	return out
}

// writeStepArtifact lands the step's primary artifact under artifacts/.
func (e *driveEnv) writeStepArtifact(stepID string, output map[string]any) {
	body := fmt.Sprintf("# %s\n\nfakepi-drive output of step %s.\n\n", stepID, stepID)
	for k, v := range output {
		body += fmt.Sprintf("- %s: %v\n", k, v)
	}
	p := filepath.Join(e.cap.Dir, "artifacts", stepID+".md")
	<-e.gate
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		_ = os.WriteFile(p, []byte(body), 0o644)
	}
	e.gate <- struct{}{}
}

// appendGrillQA appends one question/answer pair to the shared grill-qa
// log (01 §4.3: the artifact the spec step consumes, sectioned by step).
// Called with env.mu held.
func appendGrillQA(env *driveEnv, stepID, questionID, question, answerText, answeredBy, answeredAt string) {
	p := filepath.Join(env.cap.Dir, "artifacts", "grill-qa.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## %s\n\nQ [%s]: %s\nA [%s, %s]: %s\n\n",
		stepID, questionID, question, answeredBy, answeredAt, answerText)
}

// journalDispatch records one dispatch in logs/drive.jsonl.
func (e *driveEnv) journalDispatch(stepID, candidate string, ordinal int, source, outcome string, inProgress, rewindConsumed bool, out dispatchOutcome) {
	status := "other"
	if inProgress {
		status = "in_progress"
	}
	rec := map[string]any{
		"type": "dispatch", "step": stepID, "candidate": candidate,
		"ordinal": ordinal, "candidate_source": source, "outcome": outcome,
		"status_at_dispatch": status, "rewind_input_consumed": rewindConsumed,
	}
	if out.verdict != "" {
		rec["verdict"] = out.verdict
	}
	journalDrive(e, rec)
}

// journalDrive appends one JSONL record to logs/drive.jsonl (a sync
// surface: the journal publishes and recovers with the capsule).
func journalDrive(e *driveEnv, rec map[string]any) {
	if e == nil || e.cap == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p := filepath.Join(e.cap.Dir, "logs", "drive.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f.Write(append(raw, '\n'))
}

// verdictText assembles the FAIL verdict text routed to the rewind target.
func verdictText(out dispatchOutcome, verdictOutput map[string]any) string {
	raw, _ := json.Marshal(verdictOutput)
	return "gate FAIL: " + out.detail + " (verdict output: " + string(raw) + ")"
}

// ---------------------------------------------------------------------------
// The daemon local proxy client (04 §5.3): SO_PEERCRED-bound, whitelist
// operations only — publish_request, report_step_result, read_workflow_ref.

// proxyCall posts one proxy_call on the daemon's UDS face. The caller pid
// IS the derived WI Agent, so the SO_PEERCRED peer check passes. A
// bounded retry covers the spawn-window race (the agent process boots
// faster than the daemon's post-spawn bookkeeping registers the jobs[]
// row the peer check reads) — the real agent's loop is equally free to
// retry its own proxy calls.
func (e *driveEnv) proxyCall(operation string, payload map[string]any) (map[string]any, error) {
	if e.cfg.proxySocket == "" {
		return nil, fmt.Errorf("FAKEPI_PROXY_SOCKET not configured")
	}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		out, err := e.proxyCallOnce(operation, payload)
		if err == nil {
			return out, nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return nil, lastErr
}

// proxyCallOnce is one proxy_call round trip.
func (e *driveEnv) proxyCallOnce(operation string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{
		"work_item_id": wiID(), "operation": operation, "payload": payload,
	})
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", e.cfg.proxySocket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req, err := http.NewRequest("POST", "http://daemon/proxy_call", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil },
	}}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("proxy %s: HTTP %d: %s", operation, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("proxy %s: undecodable answer %s", operation, raw)
	}
	return out, nil
}

// publishProxy fires one sync trigger through the proxy and returns the
// flush evidence JSON when the end-reason publish produced one.
func (e *driveEnv) publishProxy(reason string) json.RawMessage {
	out, err := e.proxyCall("publish_request", map[string]any{"reason": reason})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: publish_request(%s) failed: %v\n", reason, err)
		journalDrive(e, map[string]any{"type": "publish", "reason": reason, "error": err.Error()})
		return nil
	}
	rec := map[string]any{"type": "publish", "reason": reason}
	if hs, ok := out["head_seq"].(float64); ok {
		rec["head_seq"] = int64(hs)
	}
	journalDrive(e, rec)
	fe, _ := json.Marshal(out["flush_evidence"])
	if string(fe) == "null" {
		return nil
	}
	return fe
}

// reportStepResult ends the generation through the proxy (the agent's
// ONLY Core lifecycle write; the daemon signs with its held token).
func (e *driveEnv) reportStepResult(reason, waitingOn string, fe json.RawMessage) {
	payload := map[string]any{"reason": reason, "waiting_on": waitingOn}
	if fe != nil {
		payload["flush_evidence"] = json.RawMessage(fe)
	}
	if _, err := e.proxyCall("report_step_result", payload); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: report_step_result(%s) failed: %v\n", reason, err)
		journalDrive(e, map[string]any{"type": "report_step_result", "reason": reason, "error": err.Error()})
		return
	}
	journalDrive(e, map[string]any{"type": "report_step_result", "reason": reason, "waiting_on": waitingOn})
}

// readBudgetRef performs the REAL tier-3 budget read through the proxy
// (read_workflow_ref, 04 §5.3): the WI Agent's only budget authority.
func (e *driveEnv) readBudgetRef() {
	out, err := e.proxyCall("read_workflow_ref", map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi drive: read_workflow_ref failed: %v\n", err)
		return
	}
	rec := map[string]any{"type": "budget_read"}
	if b, ok := out["budget"].(map[string]any); ok {
		if eff, ok := b["effective"].(map[string]any); ok {
			if v, ok := eff["requests"].(float64); ok {
				rec["effective_requests"] = int64(v)
				n := int64(v)
				e.mu.Lock()
				e.wfBudget = &workflowBudget{requests: &n}
				e.mu.Unlock()
			}
		}
	}
	journalDrive(e, rec)
}

// ---------------------------------------------------------------------------
// Small helpers.

// holdPoint parks the drive loop until the release file exists (or
// forever when no release file is configured — the shutdown scenario
// parks mid-flight with none).
func holdPoint(e *driveEnv) {
	if e.cfg.releaseFile == "" {
		parkForever()
	}
	for {
		if _, err := os.Stat(e.cfg.releaseFile); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// parkForever sleeps without end (a bounded timer per tick so the Go
// runtime's all-asleep deadlock detector never fires).
func parkForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// envInt parses one FAKEPI_* integer knob (0 on garbage).
func envInt(name string) int {
	n, _ := strconv.Atoi(os.Getenv(name))
	return n
}

// mustWD returns the process working directory.
func mustWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
