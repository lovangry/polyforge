package driver

// transition.go: the SINGLE write entry point for every execution.json
// mutation (the driver spec's R5 single-write rule). The daemon-side
// driver replaces the WI Agent as the one writer of the capsule's
// runtime-state/execution.json (03-execution-capsule.md §4.2 keeps the
// tmp+fsync+rename contract; the v2 driver additionally fsyncs the
// parent directory so the rename itself survives a crash). Every state
// change — init, step start, step result, gate fail, rewind, budget,
// answer, terminal — rides Driver.transition:
//
//	1. serialize (mutex)
//	2. fence (the spawn counter must match; step_start carries the NEW
//	   derivation's ordinal, every other kind the current one)
//	3. validate + apply to the in-memory execution state
//	4. marshal + persist atomically (tmp, fsync, rename, dir fsync)
//	5. emit one event (news, never state — same contract as the observe
//	   lane's own event log)
//
// A failure at any pre-persist step leaves both the in-memory state and
// the file untouched (the working copy is restored from a snapshot); a
// persist failure BLOCKS the dependent next action (R5). Event emission
// is deliberately the one best-effort step: once the rename lands the
// transition is committed, and reverting in-memory state over a logging
// failure would desynchronize the driver from its own durable file —
// the failure is loud on stderr instead, matching how the runtime's
// EventTap and the observe lane treat their event surfaces.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// Workflow status vocabulary (03-execution-capsule.md §4.2, closed set).
// The strings are owned by the SCHEMA package
// (internal/common/capsule) so non-driver lanes can read them without an
// import cycle; these are the driver's write-side aliases.
const (
	StatusRunning               = capsule.WorkflowStatusRunning
	StatusCompleted             = capsule.WorkflowStatusCompleted
	StatusFailedGate            = capsule.WorkflowStatusFailedGate
	StatusFailedRewindExhausted = capsule.WorkflowStatusFailedRewindExhausted
	// StatusBlocked is the NON-terminal waiting-on-owner state (01
	// §6.4): current_step frozen, neither forward nor back; the owner
	// resumes the step, rewinds, or terminates.
	StatusBlocked = capsule.WorkflowStatusBlocked
)

// Attempt causes (01 §6.3b): the closed two-value set of archived
// attempts. Q&A history never lands in attempts[].
const (
	CauseGateFailRewind = "gate_fail_rewind"
	CauseInfraFailure   = "infra_failure"
)

// ErrStaleFence marks a transition refused by the spawn fence: the
// request names a derivation that is no longer current (B2 carried into
// the driver — only the current spawn may mutate execution.json).
var ErrStaleFence = errors.New("driver: stale spawn fence")

// TransitionKind is one execution-state mutation class.
type TransitionKind string

const (
	TransStepStart    TransitionKind = "step_start"
	TransStepResult   TransitionKind = "step_result"
	TransGateFail     TransitionKind = "gate_fail"
	TransRewind       TransitionKind = "rewind"
	TransTerminal     TransitionKind = "terminal"
	TransBudgetUpdate TransitionKind = "budget_update"
	// TransInit and TransAnswer are additions the single-write rule
	// forces: someone must write the first execution.json (that write is
	// a mutation like any other), and the D9 answer-consumption write —
	// archive the pair and clear pending_question in ONE atomic write —
	// needs a kind to ride. Every other kind follows the driver spec.
	TransInit   TransitionKind = "init"
	TransAnswer TransitionKind = "answer"
	// TransInterrupted is the recovery integration's write: ONE
	// infra_failure attempt for the derivation that died without a
	// result, archived by the single execution.json writer when the
	// recovery lane re-derives. The candidate pointer is a pure
	// recomputation over the attempts archive (candidates.go), so this
	// archive is the only state an interruption leaves behind.
	TransInterrupted TransitionKind = "interrupted"
	// TransUnblock is the §6.4 owner resume verdict for a workflow parked
	// blocked on an honest incomplete result (polyforge#116): flip
	// workflow_status back to running and leave the re-dispatch to the
	// loop. It consumes NO derivation ordinal — the fresh ordinal is the
	// loop's own step_start write on the next dispatch, so the spawn
	// ledger never records a phantom derivation between the owner's
	// verdict and the re-dispatch it names.
	TransUnblock TransitionKind = "unblock"
	// TransStepFailed is the supervision write (v2-converge 2.3): ONE
	// failed dispatch — the attempt with its cause sub-class, the per-step
	// failure counter, relaxed thresholds, the next dispatch's shape and,
	// when the death-cause table says so, the blocked status — in one
	// atomic execution.json write.
	TransStepFailed TransitionKind = "step_failed"
)

// TransitionRequest is one submitted execution.json mutation. SpawnN is
// the fencing key: the ordinal of the derivation the request belongs
// to. The driver's own loop fills it from the dispatch it is settling;
// daemon-side callers (Answer/Rewind/Terminate) snapshot the current
// counter — a mismatch is the honest answer "this request targets a
// superseded spawn", never retried blindly.
type TransitionRequest struct {
	Kind       TransitionKind
	StepID     string
	Result     *StepResult     // step_result
	Gate       *GateVerdict    // gate_fail
	Budget     *capsule.Budget // budget_update: the ADDITIVE delta
	Model      string          // step_start: the pinned candidate this dispatch runs under
	Status     string          // terminal: the target workflow_status
	Reason     string          // terminal/rewind reason; answer: the answer text
	QuestionID string          // answer: the question id being answered
	AnsweredBy string          // answer: who answered ("" → "owner")
	// Failure carries a step_failed write's failed-dispatch record.
	Failure *StepFailure
	// WallMs is the finished dispatch's wall time folded into the step's
	// cumulative runtime by a step_result write.
	WallMs int64
	SpawnN int64
}

// GateVerdict is one business gate FAIL judgment (01 §6.2): the policy
// the workflow declared plus the verdict text the gate produced.
type GateVerdict struct {
	GateStepID string
	// TargetStep is the on_fail rewind target: a gate FAIL always
	// rewinds to it. An empty target means the gate declared no rewind
	// edge and the workflow fails on the verdict.
	TargetStep string
	// VerdictText is the FAIL judgment in full.
	VerdictText string
}

// appliedInfo records what one apply actually did, for the event body.
type appliedInfo struct {
	archived []string
	target   string
	resume   bool
	failure  *failureDecision
	extra    []appliedEvent
}

// appliedEvent is a follow-up event of one transition.
type appliedEvent struct {
	kind string
	body map[string]any
}

// transition is the SINGLE entry point for all execution.json mutations.
// See the file header for the contract; the sequencing guarantees are:
//
//   - the fence check runs before anything else, so a stale request can
//     never observe — let alone mutate — current state;
//   - apply works on the live copy with a snapshot taken first, so a
//     validation or persist failure restores the exact prior state;
//   - the spawn counter advances only WITH the durable step_start write
//     (crash between persist and counter bump is harmless: the dead
//     derivation's results can no longer arrive, and the re-dispatch
//     simply takes the next ordinal).
func (d *Driver) transition(req TransitionRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	want := d.spawnN
	if req.Kind == TransStepStart {
		want++ // the start write carries the NEW derivation's ordinal
	}
	if req.SpawnN != want {
		return fmt.Errorf("%w: %s carries spawn %d, current derivation is %d",
			ErrStaleFence, req.Kind, req.SpawnN, d.spawnN)
	}

	snap := cloneExec(d.exec)
	info := &appliedInfo{}
	if err := d.applyLocked(req, info); err != nil {
		d.exec = snap
		return err
	}
	d.exec.Version++
	d.exec.UpdatedAt = d.stamp()

	blob, err := json.MarshalIndent(d.exec, "", "  ")
	if err != nil {
		d.exec = snap
		return fmt.Errorf("driver: execution state unserializable: %w", err)
	}
	if err := atomicfile.Write(d.execPath, blob, 0o644); err != nil {
		if errors.Is(err, atomicfile.ErrDirSync) {
			// Post-rename failure: disk may hold the new state. Latch
			// degraded — do NOT roll back memory (review fix 6).
			d.degraded = true
			return fmt.Errorf("driver: post-rename durability uncertain (degraded, transitions blocked): %w", err)
		}
		d.exec = snap
		return fmt.Errorf("driver: execution.json persist failed (state reverted, dependent action blocked): %w", err)
	}
	if d.degraded {
		return fmt.Errorf("driver: degraded (post-rename fsync previously failed); transitions blocked until restart")
	}
	d.initialized = true
	if req.Kind == TransStepStart {
		d.spawnN = req.SpawnN
	}
	d.emitLocked(req, info)
	d.wakeLoopLocked()
	if d.cfg.Link != nil {
		// the commit boundary: the supervisor pushes the capsule on this
		// notice (it no longer polls execution.json)
		d.cfg.Link.Fact(supervise.ExecCommitted{Version: d.exec.Version, Step: d.exec.CurrentStep})
	}
	return nil
}

func (d *Driver) applyLocked(req TransitionRequest, info *appliedInfo) error {
	switch req.Kind {
	case TransInit:
		return d.applyInitLocked()
	case TransStepStart:
		return d.applyStepStartLocked(req, info)
	case TransUnblock:
		return d.applyUnblockLocked()
	case TransStepResult:
		return d.applyStepResultLocked(req)
	case TransGateFail:
		return d.applyGateFailLocked(req, info)
	case TransRewind:
		return d.applyRewindLocked(req, info)
	case TransTerminal:
		return d.applyTerminalLocked(req)
	case TransBudgetUpdate:
		return d.applyBudgetLocked(req)
	case TransAnswer:
		return d.applyAnswerLocked(req)
	case TransInterrupted:
		return d.applyInterruptedLocked(req)
	case TransStepFailed:
		return d.applyStepFailedLocked(req, info)
	default:
		return fmt.Errorf("driver: unknown transition kind %q", req.Kind)
	}
}

// applyInitLocked lands the first execution.json: identity, the closed
// running status, and the step records ensured at load — all pending,
// position empty (the first dispatch moves it).
func (d *Driver) applyInitLocked() error {
	if d.initialized {
		return fmt.Errorf("driver: execution.json already initialized (version %d)", d.exec.Version)
	}
	d.exec.CapsuleID = capsuleIDFor(d.wi.ID)
	d.exec.WorkItemID = d.wi.ID
	d.exec.BundleDigest = d.wi.BundleDigest
	d.exec.WorkflowStatus = StatusRunning
	d.exec.CurrentStep = ""
	return nil
}

// applyStepStartLocked flips one step to in_progress (01 §6.5 dispatch)
// and moves the execution position onto it. Two entry shapes share the
// kind: a PENDING step is a fresh entry (first dispatch or rewind
// re-entry — the pending rewind verdict is consumed by this very write,
// D8, and the budget window re-opens after the rewind archived it); an
// IN_PROGRESS step is a resume re-dispatch (answer continuation, crash
// recovery, recovery-lane re-derivation — a new measurement window, the
// prior partial output kept). From StatusBlocked only the current step
// may resume: that is the owner's §6.4 unblock action, which also
// rewrites the status back to running.
func (d *Driver) applyStepStartLocked(req TransitionRequest, info *appliedInfo) error {
	if _, ok := d.workflow.indexOf(req.StepID); !ok {
		return fmt.Errorf("driver: step %q is not in the workflow", req.StepID)
	}
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is terminal (%s); step starts are frozen", d.exec.WorkflowStatus)
	}
	if d.exec.WorkflowStatus == StatusBlocked && req.StepID != d.exec.CurrentStep {
		return fmt.Errorf("driver: workflow is blocked; only the current step %q may resume (rewind or terminate otherwise)", d.exec.CurrentStep)
	}
	st := d.exec.Steps[req.StepID]
	if st == nil {
		st = &capsule.Step{}
		d.exec.Steps[req.StepID] = st
	}
	switch {
	case st.Status == capsule.StepPending || st.Status == "":
		st.PendingRewindInput = nil          // consumed by this dispatch (D8)
		st.Output = nil                      // stale products never feed a re-run (01 §6.2 step 4)
		st.BudgetConsumed = capsule.Budget{} // the rewind archived the prior window
		st.Status = capsule.StepInProgress
	case st.Status == capsule.StepInProgress:
		info.resume = true
	default:
		return fmt.Errorf("driver: step %q is %s; only pending or in_progress steps start", req.StepID, st.Status)
	}
	if st.Supervision != nil {
		st.Supervision.NextDispatch = "" // this dispatch consumed the shape the last failure ordered
	}
	st.Model = req.Model
	st.StepStartedAt = d.stamp() // refreshes on every re-entry (03 §4.2 D8)
	d.exec.WorkflowStatus = StatusRunning
	d.exec.CurrentStep = req.StepID
	return nil
}

// applyUnblockLocked lands the §6.4 owner resume verdict (polyforge#116):
// only a blocked workflow accepts it (03 §4.2: the owner's follow-up
// writes running back), the position stays frozen exactly as the park
// left it (the current step remains in_progress — the loop's own
// step_start write on the next dispatch re-dispatches it as the fresh
// derivation), and no derivation ordinal is consumed here: a spawn
// ledger entry must name a derivation that actually ran.
func (d *Driver) applyUnblockLocked() error {
	if d.exec.WorkflowStatus != StatusBlocked {
		return fmt.Errorf("driver: resume is the owner's blocked-step verdict; workflow is %q", d.exec.WorkflowStatus)
	}
	if d.exec.CurrentStep == "" {
		return fmt.Errorf("driver: blocked workflow without a current step")
	}
	d.exec.WorkflowStatus = StatusRunning
	d.exec.BlockedReason = ""
	// the human's "continue" resets the step's failure ledger (the
	// relaxed thresholds stay: they are the step's learned bounds)
	if st := d.exec.Steps[d.exec.CurrentStep]; st != nil && st.Supervision != nil {
		st.Supervision.FailureCount = 0
		st.Supervision.LongTaskTimeouts = 0
		st.Supervision.NextDispatch = ""
	}
	return nil
}

// applyStepResultLocked accepts one R13 result envelope for the current
// step and applies its outcome (01 §6.5 steps 2-5):
//
//   - completed: the step is done, its output (plus the B10 commands
//     evidence) recorded — the only accept path; envelope/artifact
//     validation happened in the Run loop before this write;
//   - gate_failed: the FAIL verdict is recorded on the gate step (never
//     done); the follow-up gate_fail transition applies the rewind or
//     terminal policy;
//   - needs_input: the open question is persisted in the same write
//     that parks the step (D8/D9: no "asked but unrecorded" window);
//     the workflow stays running — pending_question IS the waiting
//     marker, and the observe face derives needs_input from it;
//   - incomplete: workflow_status=blocked, position frozen (01 §6.4);
//     the owner holds every next move.
func (d *Driver) applyStepResultLocked(req TransitionRequest) error {
	res := req.Result
	if res == nil {
		return fmt.Errorf("driver: step_result without a result envelope")
	}
	if req.StepID == "" {
		req.StepID = res.StepID
	}
	if req.StepID == "" {
		return fmt.Errorf("driver: step_result without a step id")
	}
	if res.StepID != "" && res.StepID != req.StepID {
		return fmt.Errorf("driver: result envelope names step %q, request says %q", res.StepID, req.StepID)
	}
	if res.SpawnN != req.SpawnN {
		return fmt.Errorf("%w: result envelope spawn %d, request spawn %d", ErrStaleFence, res.SpawnN, req.SpawnN)
	}
	if res.WIID != "" && res.WIID != d.wi.ID {
		return fmt.Errorf("driver: result envelope names work item %q, this driver drives %q", res.WIID, d.wi.ID)
	}
	switch res.Outcome {
	case OutcomeCompleted, OutcomeGateFailed, OutcomeNeedsInput, OutcomeIncomplete:
	default:
		return fmt.Errorf("driver: unknown outcome %q", res.Outcome)
	}
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is terminal (%s); results are frozen", d.exec.WorkflowStatus)
	}
	if d.exec.WorkflowStatus == StatusBlocked {
		return fmt.Errorf("driver: workflow is blocked; a result now is stale (resume, rewind or terminate first)")
	}
	if req.StepID != d.exec.CurrentStep {
		return fmt.Errorf("driver: result for step %q but the current step is %q", req.StepID, d.exec.CurrentStep)
	}
	st := d.exec.Steps[req.StepID]
	if st == nil {
		return fmt.Errorf("driver: result for unknown step %q", req.StepID)
	}
	if st.Status != capsule.StepInProgress {
		return fmt.Errorf("driver: step %q is %s; only an in_progress step accepts results", req.StepID, st.Status)
	}
	if req.WallMs > 0 {
		// the finished dispatch's wall time joins the step's cumulative
		// runtime (the T accumulator, human-wait time excluded by
		// construction: no process runs while a question is open)
		st.BudgetConsumed.WallMs += req.WallMs
		d.exec.BudgetConsumed.WallMs += req.WallMs
	}

	switch res.Outcome {
	case OutcomeCompleted:
		out, err := parseOutput(res.Output)
		if err != nil {
			return err
		}
		st.Status = capsule.StepDone
		st.Output = out
		// Commit point: archive the accepted attempt (the step's attempt
		// ledger; the candidate pointer recomputes over it).
		st.Attempts = append(st.Attempts, capsule.Attempt{
			Cause:      "accepted",
			AttemptSeq: len(st.Attempts) + 1,
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		})
	case OutcomeGateFailed:
		out, err := parseOutput(res.Output)
		if err != nil {
			return err
		}
		st.Output = out // the FAIL verdict stays on the gate; it is never done
		if d.exec.ReviewRounds == nil {
			d.exec.ReviewRounds = map[string]int64{}
		}
		d.exec.ReviewRounds[req.StepID]++
	case OutcomeNeedsInput:
		// Shape was validated at staging (ValidateStepResult); the same
		// helper re-derives the pair here and stays a backstop.
		qid, qtext, qerr := needsInputQuestion(res)
		if qerr != nil {
			return qerr
		}
		st.PendingQuestion = &capsule.PendingQuestion{
			ID:       qid,
			Step:     req.StepID,
			Question: qtext,
			Context:  res.Output,
			AskedAt:  d.stamp(),
		}
	case OutcomeIncomplete:
		out, err := parseOutput(res.Output)
		if err != nil {
			return err
		}
		d.exec.WorkflowStatus = StatusBlocked
		st.Output = out
	}
	return nil
}

// applyGateFailLocked consumes one gate FAIL policy (01 §6.2): terminate
// (or a gate without on_fail) ends the workflow failed_gate (WF-102);
// rewind first checks BOTH bounds — a hit ends failed_rewind_exhausted
// (WF-101) before any mutation — then archives every step in
// [target, gate) as a gate_fail_rewind attempt, resets each to pending
// with output cleared, moves the position back to target and (with
// include_verdict, the default) persists the FAIL verdict as the
// target's pending_rewind_input. The gate itself is neither archived
// nor reset: it stays in_progress holding its verdict output (only its
// AnswerBase advances, so a human gate needs a fresh answer on re-entry),
// and its candidate pointer is untouched (INV-1g — rewind never switches
// candidates).
func (d *Driver) applyGateFailLocked(req TransitionRequest, info *appliedInfo) error {
	v := req.Gate
	if v == nil {
		return fmt.Errorf("driver: gate_fail without a verdict")
	}
	gate := req.StepID
	if gate == "" {
		gate = v.GateStepID
	}
	if gate == "" {
		return fmt.Errorf("driver: gate_fail without a gate step id")
	}
	if err := d.requireRunningStep(gate); err != nil {
		return err
	}
	target := v.TargetStep
	if target == "" { // no rewind edge declared: the gate verdict is terminal
		d.exec.WorkflowStatus = StatusFailedGate
		return nil
	}
	tIdx, ok := d.workflow.indexOf(target)
	if !ok {
		return fmt.Errorf("driver: rewind target %q is not in the workflow", target)
	}
	gIdx, _ := d.workflow.indexOf(gate)
	if tIdx >= gIdx {
		return fmt.Errorf("driver: rewind target %q must be an earlier step than the gate %q", target, gate)
	}
	// Bounds BEFORE any mutation (01 §6.2 step 2: a hit must not rewind).
	if d.exec.RewindStats.Count >= int64(d.workflow.MaxTotal) ||
		countToStep(d.exec.RewindStats.ToStep, target) >= int64(d.workflow.MaxPerStep) {
		d.exec.WorkflowStatus = StatusFailedRewindExhausted
		return nil
	}
	d.applyRewindRangeLocked(tIdx, gIdx, target, gate, "", info)
	// The gate itself stays in_progress, but its verdict closes the human
	// round it judged: answers recorded so far must not satisfy a
	// requires_human_answer gate on re-entry (human-gate bypass).
	if gst := d.exec.Steps[gate]; gst != nil {
		gst.AnswerBase = len(gst.Answers)
		// Its conversation judged the version now being redone: the
		// re-entry must start a NEW conversation (prior transcript as
		// read-only reference at most), never resume that judgment.
		if gst.Supervision == nil {
			gst.Supervision = &capsule.StepSupervision{}
		}
		gst.Supervision.NextDispatch = capsule.NextDispatchFresh
	}
	// The FAIL verdict text rides to the target's pending_rewind_input
	// (D8) so the rewound step's next dispatch is guaranteed to see it.
	d.exec.Steps[target].PendingRewindInput = &capsule.RewindInput{
		ToStep: target,
		Reason: v.VerdictText,
	}
	return nil
}

// applyRewindLocked is the OPERATOR rewind (the owner's §6.4 verdict
// after blocked, or an in-flight correction while running): the same
// archive-and-reset mechanics over [target, current), no gate verdict
// to route. The §6.2 bounds do not apply — they bound automated gate
// loops, not owner decisions — but the rewind statistics count it.
// While running, a rewind is refused if the current step holds an open
// question: the awaiting face must stay total (answer it or wait). From
// blocked, the rewind is ALSO the owner's unblock: the verdict rewrites
// the workflow running again (03 §4.2: the owner's follow-up writes
// running or a terminal back).
func (d *Driver) applyRewindLocked(req TransitionRequest, info *appliedInfo) error {
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is terminal (%s); rewind is frozen", d.exec.WorkflowStatus)
	}
	status := d.exec.WorkflowStatus
	if status != StatusRunning && status != StatusBlocked {
		return fmt.Errorf("driver: rewind from %q is illegal", status)
	}
	cur := d.exec.CurrentStep
	cIdx, ok := d.workflow.indexOf(cur)
	if !ok {
		return fmt.Errorf("driver: current step %q is not in the workflow", cur)
	}
	target := req.StepID
	tIdx, ok := d.workflow.indexOf(target)
	if !ok {
		return fmt.Errorf("driver: rewind target %q is not in the workflow", target)
	}
	if tIdx >= cIdx {
		return fmt.Errorf("driver: rewind target %q must be an earlier step than the current %q", target, cur)
	}
	if status == StatusRunning {
		if cs := d.exec.Steps[cur]; cs != nil && cs.PendingQuestion != nil {
			return fmt.Errorf("driver: rewind refused while step %q holds an open question (answer it or wait)", cur)
		}
	}
	d.applyRewindRangeLocked(tIdx, cIdx, target, "", req.Reason, info)
	d.exec.WorkflowStatus = StatusRunning
	return nil
}

// applyRewindRangeLocked archives every step in [from, to) as a
// gate_fail_rewind attempt and resets it to pending with output
// cleared, then moves the position to the target. rewindGate names the
// triggering gate ("" for an operator rewind, whose reason rides the
// archived Detail instead).
func (d *Driver) applyRewindRangeLocked(from, to int, target, rewindGate, detail string, info *appliedInfo) {
	for i := from; i < to; i++ {
		sid := d.workflow.Steps[i].ID
		st := d.exec.Steps[sid]
		if st == nil {
			d.exec.Steps[sid] = &capsule.Step{Status: capsule.StepPending}
			continue
		}
		seq := 0
		for _, a := range st.Attempts {
			if a.AttemptSeq > seq {
				seq = a.AttemptSeq
			}
		}
		raw, _ := json.Marshal(st.Output)
		consumed := st.BudgetConsumed
		st.Attempts = append(st.Attempts, capsule.Attempt{
			Cause:          CauseGateFailRewind,
			AttemptSeq:     seq + 1,
			StartedAt:      st.StepStartedAt,
			ArchivedAt:     d.stamp(),
			RewindGate:     rewindGate,
			Result:         raw,
			Detail:         detail,
			ConsumedBudget: &consumed,
		})
		st.Status = capsule.StepPending
		st.Output = nil
		st.PendingQuestion = nil
		st.PendingRewindInput = nil
		st.Supervision = nil            // a gate rewind into the step resets its failure ledger and relaxed bounds
		st.AnswerBase = len(st.Answers) // a rewound step starts a new attempt chain: earlier answers no longer count toward the human gate
		info.archived = append(info.archived, sid)
	}
	d.exec.RewindStats.Count++
	d.exec.RewindStats.ToStep = append(d.exec.RewindStats.ToStep, target)
	d.exec.CurrentStep = target
	info.target = target
}

// applyTerminalLocked lands a terminal workflow_status. completed is
// reserved for the all-done path (01 §6.5 step 6: the loop must prove
// every step done before writing it — an empty next_step_by_order alone
// is NOT completion); the failure statuses carry the owner's or the
// gate policy's verdict.
func (d *Driver) applyTerminalLocked(req TransitionRequest) error {
	switch req.Status {
	case StatusCompleted, StatusFailedGate, StatusFailedRewindExhausted:
	default:
		return fmt.Errorf("driver: %q is not a terminal workflow_status", req.Status)
	}
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is already terminal (%s)", d.exec.WorkflowStatus)
	}
	if req.Status == StatusCompleted {
		for _, spec := range d.workflow.Steps {
			st := d.exec.Steps[spec.ID]
			if st == nil || st.Status != capsule.StepDone {
				return fmt.Errorf("driver: completed requires every step done; %q is %s", spec.ID, stepStatusOf(st))
			}
		}
	}
	d.exec.WorkflowStatus = req.Status
	return nil
}

// applyBudgetLocked adds one delta to both budget layers (03 §4.2:
// step-level authority, workflow-level aggregation — the same atomic
// write).
func (d *Driver) applyBudgetLocked(req TransitionRequest) error {
	if req.Budget == nil {
		return fmt.Errorf("driver: budget_update without a delta")
	}
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is terminal (%s); budget is frozen", d.exec.WorkflowStatus)
	}
	stepID := req.StepID
	if stepID == "" {
		stepID = d.exec.CurrentStep
	}
	if stepID == "" {
		return fmt.Errorf("driver: budget_update without a step and no current step")
	}
	st := d.exec.Steps[stepID]
	if st == nil {
		return fmt.Errorf("driver: budget_update for unknown step %q", stepID)
	}
	st.BudgetConsumed.WallMs += req.Budget.WallMs
	d.exec.BudgetConsumed.WallMs += req.Budget.WallMs
	return nil
}

// applyAnswerLocked is the D9 answer-consumption write: archive the
// pair and clear pending_question in ONE atomic write. The binding is
// by question id against the CURRENT step's open question — a mismatch
// is refused (the DAEMON-409 semantics the observe answer face
// enforces on its side of the relay).
func (d *Driver) applyAnswerLocked(req TransitionRequest) error {
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is terminal (%s); answers are moot", d.exec.WorkflowStatus)
	}
	if d.exec.WorkflowStatus == StatusBlocked {
		return fmt.Errorf("driver: workflow is blocked; there is no open question to answer")
	}
	cur := d.exec.CurrentStep
	if cur == "" {
		return fmt.Errorf("driver: answer with no current step")
	}
	if req.StepID != "" && req.StepID != cur {
		return fmt.Errorf("driver: answer for step %q but the current step is %q", req.StepID, cur)
	}
	st := d.exec.Steps[cur]
	if st == nil || st.PendingQuestion == nil {
		return fmt.Errorf("driver: no open question on step %q", cur)
	}
	q := st.PendingQuestion
	if q.ID != req.QuestionID {
		return fmt.Errorf("driver: answer for question %q but the open question is %q", req.QuestionID, q.ID)
	}
	by := req.AnsweredBy
	if by == "" {
		by = "owner"
	}
	st.Answers = append(st.Answers, capsule.ArchivedAnswer{
		QuestionID: q.ID,
		Answer:     req.Reason,
		AnsweredBy: by,
		AnsweredAt: d.stamp(),
	})
	st.PendingQuestion = nil
	return nil
}

// applyInterruptedLocked archives the interrupted derivation as one
// infra_failure attempt on the current in_progress step (the recovery
// lane's re-derivation input; the recomputation advances on
// provider-class entries alone). A dispatch that ended legitimately
// archives nothing: a parked question means the dispatch DELIVERED, and
// a step that is not in flight means the recovery cause predates the
// step's own resolution. The no-archive shapes are silent, not errors —
// the transition still lands (one version bump, no state change).
func (d *Driver) applyInterruptedLocked(req TransitionRequest) error {
	if d.exec.WorkflowStatus != StatusRunning {
		return nil
	}
	cur := d.exec.CurrentStep
	if cur == "" {
		return nil
	}
	st := d.exec.Steps[cur]
	if st == nil || st.Status != capsule.StepInProgress || st.PendingQuestion != nil {
		return nil
	}
	seq := 0
	for _, a := range st.Attempts {
		if a.AttemptSeq > seq {
			seq = a.AttemptSeq
		}
	}
	// The interruption is audit only: it is NOT a counted failure (the
	// per-step failure counter is untouched — a daemon restart or driver
	// relaunch must not push a step towards blocked) and its cause is not
	// model-class, so it never advances the candidate pointer.
	cause := "driver_relaunch"
	if req.Reason == "daemon_restart" {
		cause = "daemon_restart"
	}
	st.Attempts = append(st.Attempts, capsule.Attempt{
		Cause:         CauseInfraFailure,
		FailureCause:  cause,
		AttemptSeq:    seq + 1,
		StartedAt:     st.StepStartedAt,
		InterruptedAt: d.stamp(),
		Detail:        req.Reason,
	})
	return nil
}

// requireRunningStep is the shared gate_fail precondition: a known,
// current, in_progress step under a running (not terminal, not blocked)
// workflow.
func (d *Driver) requireRunningStep(stepID string) error {
	if _, ok := d.workflow.indexOf(stepID); !ok {
		return fmt.Errorf("driver: step %q is not in the workflow", stepID)
	}
	if isTerminalStatus(d.exec.WorkflowStatus) {
		return fmt.Errorf("driver: workflow is terminal (%s)", d.exec.WorkflowStatus)
	}
	if d.exec.WorkflowStatus == StatusBlocked {
		return fmt.Errorf("driver: workflow is blocked; gate verdicts need a running workflow")
	}
	if stepID != d.exec.CurrentStep {
		return fmt.Errorf("driver: gate %q is not the current step (%q)", stepID, d.exec.CurrentStep)
	}
	st := d.exec.Steps[stepID]
	if st == nil || st.Status != capsule.StepInProgress {
		return fmt.Errorf("driver: gate %q is not in_progress", stepID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Atomic persistence: tmp + fsync + rename + parent-dir fsync.

// emitLocked emits one progress event for a committed transition. The
// bodies carry the fence identity (spawn_n) so events line up with the
// spawn that produced them. Names follow the observe lane's closed
// vocabulary where one exists (needs_input, answered).
func (d *Driver) emitLocked(req TransitionRequest, info *appliedInfo) {
	if d.log == nil {
		return
	}
	var kind string
	body := map[string]any{"spawn_n": d.spawnN}
	switch req.Kind {
	case TransInit:
		kind = "exec_init"
		body["steps"] = len(d.workflow.Steps)
	case TransUnblock:
		kind = "resumed"
		body["step"] = d.exec.CurrentStep
		body["resume"] = true
	case TransStepStart:
		kind = "step_started"
		body["step"] = req.StepID
		body["model"] = req.Model
		body["resume"] = info.resume
	case TransStepResult:
		if req.Result == nil {
			return
		}
		body["step"] = req.StepID
		switch req.Result.Outcome {
		case OutcomeCompleted:
			kind = "step_done"
		case OutcomeGateFailed:
			kind = "gate_verdict"
		case OutcomeNeedsInput:
			kind = "needs_input"
			d.inputEventFor = req.Result.QuestionID
			body["question_id"] = req.Result.QuestionID
			if _, q, qerr := needsInputQuestion(req.Result); qerr == nil {
				body["question"] = q
			}
		case OutcomeIncomplete:
			kind = "incomplete"
		}
	case TransGateFail:
		if isTerminalStatus(d.exec.WorkflowStatus) {
			kind = "terminal"
			body["status"] = d.exec.WorkflowStatus
			body["gate"] = req.StepID
		} else {
			kind = "rewind"
			body["gate"] = req.StepID
			body["target"] = info.target
			body["archived"] = info.archived
		}
	case TransRewind:
		kind = "rewind"
		body["target"] = req.StepID
		body["archived"] = info.archived
		body["reason"] = req.Reason
	case TransTerminal:
		kind = "terminal"
		body["status"] = req.Status
		body["reason"] = req.Reason
	case TransBudgetUpdate:
		kind = "budget_update"
		body["step"] = req.StepID
		body["wall_ms"] = d.exec.BudgetConsumed.WallMs
	case TransAnswer:
		kind = "answered"
		body["step"] = d.exec.CurrentStep
		body["question_id"] = req.QuestionID
		body["answered_by"] = req.AnsweredBy
	case TransInterrupted:
		kind = "interrupted"
		body["step"] = d.exec.CurrentStep
		body["detail"] = req.Reason
	case TransStepFailed:
		kind = "step_failed"
		d.failureEventsLocked(req, info, body)
	}
	if kind == "" {
		return
	}
	d.emit(kind, body)
	for _, x := range info.extra {
		d.emit(x.kind, x.body)
	}
}

// failureEventsLocked fills the step_failed body and queues the
// follow-up events (blocked, candidate_switched, candidates_exhausted)
// the owner-facing log carries.
func (d *Driver) failureEventsLocked(req TransitionRequest, info *appliedInfo, body map[string]any) {
	dec := info.failure
	if dec == nil || req.Failure == nil {
		return
	}
	body["step"] = req.StepID
	body["cause"] = dec.Cause
	body["detail"] = req.Failure.Detail
	body["disposition"] = dec.disposition()
	body["failure_count"] = dec.Count
	body["next_dispatch"] = "resume"
	if dec.Fresh {
		body["next_dispatch"] = "fresh"
	}
	if dec.RelaxToolMs > 0 {
		body["relaxed_tool_ms"] = dec.RelaxToolMs
	}
	if dec.RelaxWallMs > 0 {
		body["relaxed_wall_ms"] = dec.RelaxWallMs
	}
	if dec.Backoff > 0 {
		body["backoff_ms"] = dec.Backoff.Milliseconds()
	}
	if dec.Switch {
		body["from_model"], body["to_model"] = dec.From, dec.To
		info.extra = append(info.extra, appliedEvent{"candidate_switched", map[string]any{
			"step": req.StepID, "from": dec.From, "to": dec.To, "cause": dec.Cause,
		}})
	}
	if dec.Exhausted {
		body["candidates_exhausted"] = true
		info.extra = append(info.extra, appliedEvent{"candidates_exhausted", map[string]any{
			"step": req.StepID, "cause": dec.Cause,
			"note": "the candidate chain is spent: the pointer stays at the end and this counts as a same-model retry",
		}})
	}
	if dec.NoChain {
		body["no_candidate_chain"] = true
	}
	if dec.Block {
		info.extra = append(info.extra, appliedEvent{"blocked", map[string]any{
			"step": req.StepID, "cause": dec.Cause, "reason": dec.BlockReason, "report": dec.Report,
		}})
	}
}

// cloneExec deep-copies an execution state (JSON round-trip — every
// field is JSON-serializable, and RawMessage payloads survive verbatim).
func cloneExec(e *capsule.Execution) *capsule.Execution {
	blob, err := json.Marshal(e)
	if err != nil {
		return &capsule.Execution{Steps: map[string]*capsule.Step{}} // unreachable: the struct is plain data
	}
	var out capsule.Execution
	if err := json.Unmarshal(blob, &out); err != nil {
		return &capsule.Execution{Steps: map[string]*capsule.Step{}}
	}
	if out.Steps == nil {
		out.Steps = map[string]*capsule.Step{}
	}
	return &out
}

// countToStep counts how many times target appears in the rewind stats'
// to_step log — the max_per_step bound's input.
func countToStep(toStep []string, target string) int64 {
	var n int64
	for _, s := range toStep {
		if s == target {
			n++
		}
	}
	return n
}

// isTerminalStatus reports whether a workflow_status is terminal
// (current_step frozen; every further transition refused).
func isTerminalStatus(s string) bool {
	switch s {
	case StatusCompleted, StatusFailedGate, StatusFailedRewindExhausted:
		return true
	}
	return false
}

// stepStatusOf answers a step's status ("absent" for a nil record —
// used in error messages only).
func stepStatusOf(st *capsule.Step) string {
	if st == nil {
		return "absent"
	}
	return st.Status
}
