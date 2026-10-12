package driver

// supervision.go: the driver half of the driver <-> supervisor contract
// (v2-converge 2.2/2.3, package supervise). The driver
//
//   - announces every dispatch (step_started / step_ended /
//     step_waiting_human / step_answered facts, plus the exec_committed
//     commit boundary) on the per-WI supervise.Link — a never-blocking,
//     never-dropping queue;
//   - consumes the supervisor's verdicts (step_dead, driver_stalled)
//     behind the spawn-ordinal fence: only a verdict for the CURRENT
//     dispatch is accepted, the dispatch then turns "terminating", and
//     everything else about it (its exit, later verdicts) is ignored —
//     at most one verdict and one failure record per dispatch;
//   - classifies every dispatch that ended without a result and applies
//     the death-cause table (switch candidate / same-model retry with a
//     relaxed bound / blocked), keeping the per-step failure counter in
//     execution.json (the single authority).
//
// Nothing here touches the network or git: the verdict path only kills
// the step process and writes execution.json.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// Failure causes recorded on attempts (Attempt.FailureCause): the six
// supervisor verdict causes plus the driver's own exit sub-classes.
const (
	FailHarnessSilent        = string(supervise.CauseHarnessSilent)
	FailModelError           = string(supervise.CauseModelError)
	FailEndedWithoutResult   = string(supervise.CauseEndedWithoutResult)
	FailModelHung            = string(supervise.CauseModelHung)
	FailToolHung             = string(supervise.CauseToolHung)
	FailStepTimeoutLongTask  = string(supervise.CauseStepTimeoutLongTask)
	FailStepTimeoutSlowModel = string(supervise.CauseStepTimeoutSlowModel)
	// FailExitModel: the process exited without a result and the last
	// signal was an error turn, or its exit text names a model problem
	// (unparseable model, 401/auth, quota).
	FailExitModel = "exit_model_class"
	// FailExitHost: the driver's own I/O failed (ENOSPC, EACCES, ...) or
	// the harness reported a host error.
	FailExitHost = "exit_host_class"
	// FailExitOther: any other exit without a result (crash, operator
	// kill, envelope never written).
	FailExitOther = "exit_other"
)

// WallEstimateFactor scales a step's declared budget.wall_minutes into its
// total-time bound T = max(3 x wall_minutes, global --step-timeout).
const WallEstimateFactor = 3

// DefaultMaxStepFailures is the per-step failure counter bound (v2-converge
// 2.3): the third counted failure blocks the step.
const DefaultMaxStepFailures = 3

// ErrDriverStalled is Run's error when the supervisor's driver_stalled
// verdict reached the loop: the recovery engine relaunches the driver
// (counted against the driver-crash bound).
var ErrDriverStalled = errors.New("driver: stalled between steps (supervisor verdict); relaunching")

// RunMeta is the optional description of a spawned step run; production
// runs (stepRun) implement MetaRun, scripted test runs need not.
type RunMeta struct {
	PID            int
	StartedAt      time.Time
	TranscriptPath string
	Harness        string
	Granularity    supervise.Granularity
	Resumed        bool
}

// MetaRun is implemented by StepRuns that can describe themselves.
type MetaRun interface {
	Meta() RunMeta
}

// ExitedError is Wait's error when the step process exited without a
// usable result envelope: the raw material of the driver's own
// exited_without_result classification.
type ExitedError struct {
	Step      string
	SpawnN    int64
	ExitCode  int
	Signal    string
	LastError string // stderr tail / last reported error
	TurnError bool   // the last signal was "turn ended with an error"
	TurnOK    bool   // the last signal was a normally ended turn (no error)
	Detail    string // envelope defect, when one was left behind
}

func (e *ExitedError) Error() string {
	msg := fmt.Sprintf("driver: step %s spawn %d exited without a result (code %d", e.Step, e.SpawnN, e.ExitCode)
	if e.Signal != "" {
		msg += ", " + e.Signal
	}
	msg += ")"
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.LastError != "" {
		msg += ": " + e.LastError
	}
	return msg
}

// StepFailure is one failed dispatch handed to the TransStepFailed
// transition.
type StepFailure struct {
	Cause    string
	Detail   string
	ExitCode int
	// WallMs is the dispatch's wall time, added to the step's cumulative
	// runtime (the T accumulator).
	WallMs int64
}

// ---------------------------------------------------------------------------
// Exit classification (the driver's own judgement, 2.3 table).

var (
	hostPatterns = regexp.MustCompile(`(?i)(no space left|enospc|disk quota|edquot|permission denied|eacces|read-only file system|erofs|too many open files|emfile|cannot allocate memory|out of memory|enomem|input/output error|executable file not found|no such file or directory.*(pi|harness))`)
	// model-class: the model/provider side is broken, a different model
	// may work (F4: a pinned model that cannot resolve must advance the
	// candidate chain).
	modelPatterns = regexp.MustCompile(`(?i)(model\W.{0,60}(not found|unknown|unresolved|cannot be resolved|could not be resolved|not resolve|invalid|unavailable|not available|unsupported|does not exist)|(unknown|invalid|unparse?able|unresolved|unsupported|no such) model|no models? (found|available|match)|\b401\b|\b403\b|unauthori[sz]ed|authentication|invalid api key|incorrect api key|api key|quota|insufficient|billing|credit|rate.?limit|\b429\b|overloaded)`)
)

// classifyWaitFailure maps a Wait error (not a cancellation) onto a
// failure cause.
func classifyWaitFailure(err error) (cause string, exitCode int, detail string) {
	var ee *ExitedError
	if errors.As(err, &ee) {
		text := strings.TrimSpace(ee.LastError + " " + ee.Detail)
		switch {
		case hostPatterns.MatchString(text):
			return FailExitHost, ee.ExitCode, ee.Error()
		case ee.TurnOK && ee.ExitCode == 0 && ee.Signal == "":
			// the turn ended normally and the process then exited CLEANLY
			// with no result: the agent stopped without delivering (a kill
			// or crash after an ok turn is still exit_other). Evaluated
			// before any text match: leftover stderr must never turn a
			// clean stop into a model fault that burns a candidate.
			return FailEndedWithoutResult, ee.ExitCode, ee.Error()
		case ee.TurnError || modelPatterns.MatchString(text):
			return FailExitModel, ee.ExitCode, ee.Error()
		default:
			return FailExitOther, ee.ExitCode, ee.Error()
		}
	}
	// Not a process exit: the driver's own failure (staging an envelope,
	// reading the drop, ...). Host-class when the OS says so.
	if isHostError(err) {
		return FailExitHost, -1, err.Error()
	}
	// A harness that died at startup never read its prompt (EPIPE on the
	// write); the error carries its stderr, and a model-class text there
	// advances the candidate chain (F4).
	if modelPatterns.MatchString(err.Error()) {
		return FailExitModel, -1, err.Error()
	}
	return FailExitOther, -1, err.Error()
}

// isHostError reports whether err is the driver's own host fault.
func isHostError(err error) bool {
	if err == nil {
		return false
	}
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EACCES, syscall.EROFS, syscall.EDQUOT, syscall.EMFILE, syscall.ENOMEM, syscall.EIO} {
		if errors.Is(err, errno) {
			return true
		}
	}
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	return hostPatterns.MatchString(err.Error())
}

// faultClassOf maps a failure cause onto the candidate-chain fault class
// (PolicyFor's input): model-class causes advance the candidate, host
// blocks, everything else is unclassified (never advances).
func faultClassOf(cause string) FaultClass {
	switch cause {
	case FailModelError, FailModelHung, FailStepTimeoutSlowModel, FailExitModel:
		return FaultProvider
	case FailExitHost:
		return FaultHost
	}
	return ""
}

// ---------------------------------------------------------------------------
// Thresholds.

// stepLimits answers the bases of one step: global defaults overridden by
// the workflow's step-level values.
func (d *Driver) stepBases(stepID string) (silence, tool, wall time.Duration) {
	def := d.cfg.Limits.Fill()
	tool, wall = def.Tool, def.StepTimeout
	silence = 0 // resolved per harness below
	if spec := d.workflow.step(stepID); spec != nil {
		if spec.ToolMinutes > 0 {
			tool = time.Duration(spec.ToolMinutes) * time.Minute
		}
		if spec.WallMinutes > 0 {
			// budget.wall_minutes is an ESTIMATE, not a hard limit: the
			// step's total-time bound is generous around it, never below
			// the global default.
			if est := time.Duration(spec.WallMinutes) * time.Minute * WallEstimateFactor; est > wall {
				wall = est
			}
		}
		if spec.SilenceMinutes > 0 {
			silence = time.Duration(spec.SilenceMinutes) * time.Minute
		}
	}
	return silence, tool, wall
}

// effectiveThresholdsLocked answers the thresholds a dispatch of stepID
// runs under: bases (global default, step override) raised to any relaxed
// value persisted in execution.json, plus the step runtime already spent.
func (d *Driver) effectiveThresholdsLocked(stepID string, gran supervise.Granularity) supervise.Thresholds {
	silence, tool, wall := d.stepBases(stepID)
	if silence <= 0 {
		silence = d.cfg.Limits.Fill().SilenceFor(gran)
	}
	th := supervise.Thresholds{Silence: silence, Tool: tool, StepTimeout: wall}
	if st := d.exec.Steps[stepID]; st != nil {
		if sup := st.Supervision; sup != nil {
			if r := time.Duration(sup.RelaxedToolMs) * time.Millisecond; r > th.Tool {
				th.Tool = r
			}
			if r := time.Duration(sup.RelaxedWallMs) * time.Millisecond; r > th.StepTimeout {
				th.StepTimeout = r
			}
		}
		spent := st.BudgetConsumed.WallMs
		if st.Supervision != nil {
			spent -= st.Supervision.RuntimeBaseMs
		}
		if spent < 0 {
			spent = 0
		}
		th.PriorRuntime = time.Duration(spent) * time.Millisecond
	}
	return th
}

func (d *Driver) maxStepFailures() int {
	if d.cfg.MaxStepFailures > 0 {
		return d.cfg.MaxStepFailures
	}
	return DefaultMaxStepFailures
}

// ---------------------------------------------------------------------------
// Facts out.

func (d *Driver) fact(f supervise.Fact) {
	if d.cfg.Link != nil {
		d.cfg.Link.Fact(f)
	}
}

func (d *Driver) harnessName() string {
	if d.cfg.Harness != "" {
		return d.cfg.Harness
	}
	return "pi"
}

// announceStart sends step_started for a freshly spawned dispatch.
func (d *Driver) announceStart(sp *StepSpawn, run StepRun, startedAt time.Time) {
	if d.cfg.Link == nil {
		return
	}
	meta := RunMeta{Harness: d.harnessName(), Granularity: supervise.GranularityToken, StartedAt: startedAt, Resumed: sp.Resume}
	if mr, ok := run.(MetaRun); ok {
		m := mr.Meta()
		if m.Harness != "" {
			meta.Harness = m.Harness
		}
		if m.Granularity != "" {
			meta.Granularity = m.Granularity
		}
		if !m.StartedAt.IsZero() {
			meta.StartedAt = m.StartedAt
		}
		meta.PID, meta.TranscriptPath, meta.Resumed = m.PID, m.TranscriptPath, m.Resumed
	}
	d.mu.Lock()
	th := d.effectiveThresholdsLocked(sp.StepID, meta.Granularity)
	d.mu.Unlock()
	d.cfg.Link.Fact(supervise.StepStarted{
		Step: sp.StepID, SpawnN: sp.SpawnN, Model: sp.Model, Harness: meta.Harness,
		PID: meta.PID, TranscriptPath: meta.TranscriptPath, StartedAt: meta.StartedAt,
		Thresholds: th, Resumed: meta.Resumed,
	})
}

// announceEnd sends step_ended.
func (d *Driver) announceEnd(stepID string, n int64, outcome supervise.Outcome, exitCode int, lastError string) {
	d.fact(supervise.StepEnded{Step: stepID, SpawnN: n, Outcome: outcome, ExitCode: exitCode, LastError: lastError})
}

// ---------------------------------------------------------------------------
// Dispatch wait: result | exit | verdict, first one wins.

type waitResult struct {
	res *StepResult
	err error
}

// awaitDispatch waits for the dispatch's outcome while consuming the
// supervisor's verdicts behind the fence. It returns the result, the
// wait error, and — only when a verdict ended the dispatch — the
// verdict. A result that raced a verdict wins (the dispatch delivered).
func (d *Driver) awaitDispatch(ctx context.Context, run StepRun, stepID string, n int64) (*StepResult, error, *supervise.StepDead) {
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	ch := make(chan waitResult, 1)
	go func() {
		res, err := run.Wait(wctx)
		ch <- waitResult{res, err}
	}()
	var notify <-chan struct{}
	if d.cfg.Link != nil {
		notify = d.cfg.Link.Verdicts.Notify()
	}
	for {
		select {
		case r := <-ch:
			return r.res, r.err, nil
		case <-notify:
			for _, v := range d.cfg.Link.Verdicts.Drain() {
				dead, ok := v.(supervise.StepDead)
				if !ok {
					continue // a driver_stalled verdict for a driver that is demonstrably running: stale
				}
				if dead.SpawnN != n || dead.Step != stepID {
					d.emit("verdict_stale", map[string]any{"step": dead.Step, "spawn_n": dead.SpawnN, "current_spawn_n": n, "cause": dead.Cause})
					continue // the fence: only the current dispatch's verdict counts
				}
				// Terminating: kill the process, then ignore its exit and any
				// further verdict of this dispatch.
				wcancel()
				_ = run.Terminate(teardownBound)
				r := <-ch
				if r.res != nil && r.err == nil {
					return r.res, nil, nil // the envelope landed first: the dispatch delivered
				}
				return nil, r.err, &dead
			}
		}
	}
}

// takeStalled drains the verdict queue between dispatches: a
// driver_stalled verdict that reaches the loop ends it (the recovery
// engine relaunches); stale step verdicts are discarded.
func (d *Driver) takeStalled() *supervise.DriverStalled {
	if d.cfg.Link == nil {
		return nil
	}
	var stalled *supervise.DriverStalled
	for _, v := range d.cfg.Link.Verdicts.Drain() {
		switch vv := v.(type) {
		case supervise.DriverStalled:
			c := vv
			stalled = &c
		case supervise.StepDead:
			d.emit("verdict_stale", map[string]any{"step": vv.Step, "spawn_n": vv.SpawnN, "cause": vv.Cause, "note": "no dispatch in flight"})
		}
	}
	return stalled
}

// ---------------------------------------------------------------------------
// Failure decision (applied atomically inside the TransStepFailed write).

// failureDecision is the outcome of the death-cause table for one failed
// dispatch.
type failureDecision struct {
	Cause       string
	FaultClass  FaultClass
	Switch      bool
	Exhausted   bool // switch wanted, candidate chain spent
	NoChain     bool // switch wanted, no candidate chain at all
	From, To    string
	Block       bool
	BlockReason string
	Report      bool
	Count       int
	RelaxToolMs int64
	RelaxWallMs int64
	Fresh       bool
}

func (dec failureDecision) disposition() string {
	switch {
	case dec.Block:
		return "blocked"
	case dec.Switch:
		return "switch_model"
	default:
		return "retry_same_model"
	}
}

// decideFailureLocked applies the 2.3 table to the step's current state.
func (d *Driver) decideFailureLocked(stepID string, st *capsule.Step, f *StepFailure) failureDecision {
	dec := failureDecision{Cause: f.Cause, FaultClass: faultClassOf(f.Cause)}
	var sup capsule.StepSupervision
	if st.Supervision != nil {
		sup = *st.Supervision
	}
	pol := PolicyFor(dec.FaultClass)
	cands := d.effectiveCandidates(stepID)
	switch {
	case pol.Block:
		dec.Block = true
		dec.Report = pol.Report
		dec.BlockReason = fmt.Sprintf("host-class fault (%s): the machine, not the model, is broken: %s", f.Cause, f.Detail)
	case pol.Advance:
		ptr := CandidatePointer(attemptsToPointer(st), cands)
		switch {
		case len(cands) == 0:
			dec.NoChain = true
		case ptr < len(cands)-1:
			dec.Switch = true
			dec.From, dec.To = cands[ptr], cands[ptr+1]
		default:
			// the pointer is already at the end (CandidatePointer caps
			// there): the failure is still provider-class on the record,
			// but it counts as a same-model retry
			dec.Exhausted = true
		}
	}
	switch f.Cause {
	case FailToolHung:
		_, tool, _ := d.stepBases(stepID)
		cur := time.Duration(sup.RelaxedToolMs) * time.Millisecond
		if cur < tool {
			cur = tool
		}
		dec.RelaxToolMs = (2 * cur).Milliseconds()
	case FailStepTimeoutLongTask:
		if sup.LongTaskTimeouts >= 1 && !dec.Block {
			dec.Block = true
			dec.BlockReason = "second step timeout (step_timeout_long_task) after T was already relaxed: " + f.Detail
		} else {
			_, _, wall := d.stepBases(stepID)
			cur := time.Duration(sup.RelaxedWallMs) * time.Millisecond
			if cur < wall {
				cur = wall
			}
			dec.RelaxWallMs = (2 * cur).Milliseconds()
		}
	}
	if dec.Switch {
		dec.Count = 0 // a new candidate starts a fresh ledger
		dec.Fresh = true
	} else {
		dec.Count = sup.FailureCount + 1
		if dec.Count >= d.maxStepFailures() && !dec.Block {
			dec.Block = true
			dec.BlockReason = fmt.Sprintf("step failed %d times without a result (failure counter bound %d); last cause %s: %s",
				dec.Count, d.maxStepFailures(), f.Cause, f.Detail)
		}
		dec.Fresh = dec.Count >= 2 // 1st retry resumes the transcript, 2nd+ starts fresh
	}
	return dec
}

// applyRejectEscalatedLocked counts one envelope-rejection escalation
// (a driver relaunch) in the step's persisted ledger; the bound blocks.
func (d *Driver) applyRejectEscalatedLocked(req TransitionRequest, info *appliedInfo) error {
	if err := d.requireRunningStep(req.StepID); err != nil {
		return err
	}
	st := d.exec.Steps[req.StepID]
	if st.Supervision == nil {
		st.Supervision = &capsule.StepSupervision{}
	}
	st.Supervision.RejectRelaunches++
	if st.Supervision.RejectRelaunches >= maxRejectRelaunches {
		d.exec.WorkflowStatus = StatusBlocked
		d.exec.BlockedReason = fmt.Sprintf("step %s: the result envelope was rejected through %d driver relaunches (bound %d): %s",
			req.StepID, st.Supervision.RejectRelaunches, maxRejectRelaunches, req.Reason)
	}
	return nil
}

// applyStepFailedLocked records one failed dispatch: the attempt (with
// its cause sub-class), the failure counter, the relaxed thresholds, the
// next dispatch's shape and — when the decision blocks — the blocked
// workflow status.
func (d *Driver) applyStepFailedLocked(req TransitionRequest, info *appliedInfo) error {
	f := req.Failure
	if f == nil {
		return fmt.Errorf("driver: step_failed without a failure record")
	}
	if err := d.requireRunningStep(req.StepID); err != nil {
		return err
	}
	st := d.exec.Steps[req.StepID]
	dec := d.decideFailureLocked(req.StepID, st, f)
	seq := 0
	for _, a := range st.Attempts {
		if a.AttemptSeq > seq {
			seq = a.AttemptSeq
		}
	}
	st.Attempts = append(st.Attempts, capsule.Attempt{
		Cause:         CauseInfraFailure,
		FaultClass:    string(dec.FaultClass),
		FailureCause:  f.Cause,
		AttemptSeq:    seq + 1,
		StartedAt:     st.StepStartedAt,
		InterruptedAt: d.stamp(),
		Detail:        f.Detail,
	})
	sup := st.Supervision
	if sup == nil {
		sup = &capsule.StepSupervision{}
		st.Supervision = sup
	}
	sup.FailureCount = dec.Count
	if f.Cause == FailStepTimeoutLongTask {
		sup.LongTaskTimeouts++
	}
	if dec.RelaxToolMs > sup.RelaxedToolMs {
		sup.RelaxedToolMs = dec.RelaxToolMs
	}
	if dec.RelaxWallMs > sup.RelaxedWallMs {
		sup.RelaxedWallMs = dec.RelaxWallMs
	}
	sup.NextDispatch = ""
	if dec.Fresh {
		sup.NextDispatch = capsule.NextDispatchFresh
	}
	if f.WallMs > 0 {
		st.BudgetConsumed.WallMs += f.WallMs
		d.exec.BudgetConsumed.WallMs += f.WallMs
	}
	if dec.Switch {
		// the new candidate gets a fresh T window (the old one's slow or
		// hung time is not its to repay)
		sup.RuntimeBaseMs = st.BudgetConsumed.WallMs
	}
	if dec.Block {
		d.exec.WorkflowStatus = StatusBlocked
		d.exec.BlockedReason = dec.BlockReason
	}
	info.failure = &dec
	return nil
}

// ---------------------------------------------------------------------------
// Failure handling + wall accounting.

// emit appends one driver event (best-effort news: a failing log is a
// stderr line, never a correctness dependency — execution.json stays the
// authority).
func (d *Driver) emit(kind string, body map[string]any) {
	if d.log == nil {
		return
	}
	if err := d.log.Append(d.wi.ID, kind, body); err != nil {
		fmt.Fprintf(os.Stderr, "driver: %s: event %s append failed (degraded, state unaffected): %v\n", d.wi.ID, kind, err)
	}
}

// addUnbooked adds one finished dispatch's wall time to the step's
// not-yet-persisted runtime.
func (d *Driver) addUnbooked(stepID string, since time.Time) {
	ms := d.now().Sub(since).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	d.mu.Lock()
	d.unbooked[stepID] += ms
	d.mu.Unlock()
}

// takeUnbooked returns and clears the step's pending wall time.
func (d *Driver) takeUnbooked(stepID string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	ms := d.unbooked[stepID]
	delete(d.unbooked, stepID)
	return ms
}

// bookWall persists the dispatch's wall time when it ended by
// cancellation (best-effort: the T accumulator survives restarts).
func (d *Driver) bookWall(stepID string, since time.Time) {
	d.addUnbooked(stepID, since)
	ms := d.takeUnbooked(stepID)
	if ms <= 0 {
		return
	}
	_ = d.transition(TransitionRequest{
		Kind: TransBudgetUpdate, StepID: stepID, SpawnN: d.currentSpawnN(),
		Budget: &capsule.Budget{WallMs: ms},
	})
}

// failDispatch records one failed dispatch (the dispatch's single failure
// record): the step_ended fact, then the TransStepFailed write that
// applies the death-cause table. blocked reports that the decision
// parked the workflow (Run then exits cleanly: the owner holds the next
// move).
func (d *Driver) failDispatch(stepID string, n int64, since time.Time, cause string, exitCode int, detail string) (blocked bool, err error) {
	d.addUnbooked(stepID, since)
	wall := d.takeUnbooked(stepID)
	last := detail
	if len(last) > 500 {
		last = last[:500]
	}
	d.announceEnd(stepID, n, supervise.OutcomeExitedWithoutResult, exitCode, last)
	if err := d.transition(TransitionRequest{
		Kind:    TransStepFailed,
		StepID:  stepID,
		SpawnN:  n,
		Failure: &StepFailure{Cause: cause, Detail: detail, ExitCode: exitCode, WallMs: wall},
	}); err != nil {
		return false, fmt.Errorf("driver: step %s dispatch %d failure record failed: %w", stepID, n, err)
	}
	d.mu.Lock()
	blocked = d.exec.WorkflowStatus == StatusBlocked
	d.mu.Unlock()
	return blocked, nil
}

// announceWaiting sends step_waiting_human for the current step's open
// question.
func (d *Driver) announceWaiting() {
	d.mu.Lock()
	step, n := d.exec.CurrentStep, d.spawnN
	qid, question := "", ""
	if st := d.exec.Steps[step]; st != nil && st.PendingQuestion != nil {
		qid, question = st.PendingQuestion.ID, st.PendingQuestion.Question
	}
	announced := d.waitingAnnounced == qid
	d.waitingAnnounced = qid
	// a driver that finds a question already parked (a restart) tells the
	// log once per life, like a fresh park does at its transition
	reannounce := d.inputEventFor != qid
	d.inputEventFor = qid
	d.mu.Unlock()
	if reannounce {
		d.emit("needs_input", map[string]any{"step": step, "question_id": qid, "question": question, "resumed": true})
	}
	if announced || d.cfg.Link == nil {
		return
	}
	d.cfg.Link.Fact(supervise.StepWaitingHuman{Step: step, SpawnN: n, QuestionID: qid})
}
