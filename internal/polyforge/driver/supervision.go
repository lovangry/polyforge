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
	// FailRateLimited: the provider throttled the dispatch (429, rate
	// limit, overloaded, quota, billing credit). Not model-class
	// (polyforge#147 D7): the SAME model is retried after an exponential
	// backoff, with no retry-count bound of its own — only the step's
	// cumulative runtime T limits the retries.
	FailRateLimited = string(supervise.CauseRateLimited)
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
	// FailResultInvalid: the step's result envelope stayed structurally
	// invalid through the in-process retry bound (maxEnvelopeRejects).
	// A step-content failure, not infrastructure: it blocks the step —
	// no relaunch, no on_fail routing.
	FailResultInvalid = "result_invalid"
)

// WallEstimateFactor scales a step's declared budget.wall_minutes into its
// total-time bound T = max(3 x wall_minutes, global --step-timeout).
const WallEstimateFactor = 3

// DefaultMaxStepFailures is the per-step failure counter bound (v2-converge
// 2.3): the third counted failure blocks the step.
const DefaultMaxStepFailures = 3

// The D7 rate-limit backoff (polyforge#147): the first throttled
// dispatch is retried after RateLimitBackoffBase, each consecutive one
// after twice the previous wait, never longer than RateLimitBackoffMax.
// There is no retry-count bound: the step's cumulative runtime T is the
// only limit, and a T exhaustion takes the ordinary timeout path.
const (
	RateLimitBackoffBase = 1 * time.Second
	RateLimitBackoffMax  = 300 * time.Second
)

// rateLimitBackoff answers the wait before the (streak+1)-th throttled
// dispatch of a step: the base doubled once per already-recorded
// consecutive rate-limit death, capped.
func rateLimitBackoff(streak int) time.Duration {
	d := RateLimitBackoffBase
	for i := 0; i < streak; i++ {
		if d >= RateLimitBackoffMax {
			break
		}
		d *= 2
	}
	if d > RateLimitBackoffMax {
		d = RateLimitBackoffMax
	}
	return d
}

// rateLimitStreak counts the step's trailing consecutive rate-limit
// infra failures (the archived attempts are the authority, like the
// candidate pointer): a dispatch that died of anything else — or a
// delivered result — ends the streak and the backoff starts over.
func rateLimitStreak(attempts []capsule.Attempt) int {
	n := 0
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].FailureCause != FailRateLimited {
			break
		}
		n++
	}
	return n
}

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
	modelPatterns = regexp.MustCompile(`(?i)(model\W.{0,60}(not found|unknown|unresolved|cannot be resolved|could not be resolved|not resolve|invalid|unavailable|not available|unsupported|does not exist)|(unknown|invalid|unparse?able|unresolved|unsupported|no such) model|no models? (found|available|match)|\b401\b|\b403\b|unauthori[sz]ed|authentication|invalid api key|incorrect api key|api key)`)
	// rate-limit class (polyforge#147 D7): the provider is throttling or
	// out of allowance. Another model would be throttled just the same
	// (and a switch would burn the chain), so these never advance the
	// candidate pointer: the driver waits out a backoff and retries the
	// SAME model.
	rateLimitPatterns = regexp.MustCompile(`(?i)(\b429\b|rate.?limit|too many requests|overloaded|quota|insufficient|billing|credit)`)
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
		case rateLimitPatterns.MatchString(text):
			// evaluated before the model class: a throttle is the
			// provider being busy, not the model being broken
			return FailRateLimited, ee.ExitCode, ee.Error()
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
	// advances the candidate chain (F4) — a throttle text does not.
	if rateLimitPatterns.MatchString(err.Error()) {
		return FailRateLimited, -1, err.Error()
	}
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

// ---------------------------------------------------------------------------
// Thresholds.

// stepLimits answers the bases of one step: the global defaults, with
// the wall base raised around the step's budget estimate.
func (d *Driver) stepBases(stepID string) (silence, tool, wall time.Duration) {
	def := d.cfg.Limits.Fill()
	tool, wall = def.Tool, def.StepTimeout
	silence = 0 // resolved per harness below
	if spec := d.workflow.step(stepID); spec != nil {
		if spec.WallMinutes > 0 {
			// budget.wall_minutes is an ESTIMATE, not a hard limit: the
			// step's total-time bound is generous around it, never below
			// the global default.
			if est := time.Duration(spec.WallMinutes) * time.Minute * WallEstimateFactor; est > wall {
				wall = est
			}
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
		// T is CUMULATIVE over the whole step (polyforge#147 D6): a
		// switch to another candidate or a human continue does not
		// reopen the window — only a rewind's fresh start zeroes it.
		spent := st.BudgetConsumed.WallMs
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
	// Backoff is the wait the driver owes before the next dispatch of
	// this step (D7: a rate-limit disposal only ever backs off).
	Backoff time.Duration
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
	dec := failureDecision{Cause: f.Cause}
	var sup capsule.StepSupervision
	if st.Supervision != nil {
		sup = *st.Supervision
	}
	cands := d.effectiveCandidates(stepID)
	switch {
	case f.Cause == FailResultInvalid:
		dec.Block = true
		dec.Report = true
		dec.BlockReason = fmt.Sprintf("step %s: result_invalid: the result envelope stayed invalid through the in-process retry bound (%d): %s",
			stepID, maxEnvelopeRejects, f.Detail)
	case f.Cause == FailExitHost:
		dec.Block = true
		dec.Report = true
		dec.BlockReason = fmt.Sprintf("host-class fault (%s): the machine, not the model, is broken: %s", f.Cause, f.Detail)
	case f.Cause == FailRateLimited:
		// D7: the provider throttled us. No switch (the chain is not
		// spent by someone else's quota) and no block — the backoff
		// before the next dispatch is the whole disposal.
		dec.Backoff = rateLimitBackoff(rateLimitStreak(st.Attempts))
	case IsModelClass(f.Cause):
		ptr := CandidatePointer(st.Attempts, cands)
		switch {
		case len(cands) == 0:
			dec.NoChain = true
		case ptr < len(cands)-1:
			dec.Switch = true
			dec.From, dec.To = cands[ptr], cands[ptr+1]
		default:
			// the pointer is already at the end (CandidatePointer caps
			// there): the failure is still model-class on the record,
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
	case FailStepTimeoutSlowModel:
		// A timeout-class death relaxes the step's wall budget x2 and
		// persists it (polyforge#147 D6): the next candidate runs under
		// the DOUBLED bound, with the cumulative runtime carried over —
		// never judged timed out at its own startup.
		dec.RelaxWallMs = d.doubledWallLocked(stepID, sup)
	case FailStepTimeoutLongTask:
		if sup.LongTaskTimeouts >= 1 && !dec.Block {
			dec.Block = true
			dec.BlockReason = "second step timeout (step_timeout_long_task) after T was already relaxed: " + f.Detail
		} else {
			dec.RelaxWallMs = d.doubledWallLocked(stepID, sup)
		}
	}
	if dec.Switch {
		dec.Count = 0 // a new candidate starts a fresh ledger
		dec.Fresh = true
	} else if f.Cause == FailRateLimited {
		// D7: a throttle is not the step failing. The counter that
		// blocks never moves (no retry bound below T) and the retry
		// resumes the transcript it already has.
		dec.Count = sup.FailureCount
		dec.Fresh = false
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

// doubledWallLocked answers the step's relaxed wall budget after one
// more relaxation: the current bound (the persisted relaxed value, or
// the step's base when it was never relaxed) doubled. Only timeout-class
// deaths call it — every other disposal leaves T alone.
func (d *Driver) doubledWallLocked(stepID string, sup capsule.StepSupervision) int64 {
	_, _, wall := d.stepBases(stepID)
	cur := time.Duration(sup.RelaxedWallMs) * time.Millisecond
	if cur < wall {
		cur = wall
	}
	return (2 * cur).Milliseconds()
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
func (d *Driver) failDispatch(ctx context.Context, stepID string, n int64, since time.Time, cause string, exitCode int, detail string) (blocked bool, err error) {
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
	if !blocked && cause == FailRateLimited {
		d.rateLimitWait(ctx, stepID)
	}
	return blocked, nil
}

// rateLimitWait waits out the provider's throttle before the step is
// dispatched again on the SAME model (D7): the exponential backoff
// grows with the step's trailing streak of rate-limit deaths (the
// attempts just archived are the authority) and is capped at
// RateLimitBackoffMax. The wait is not agent runtime, so it never
// enters the step's cumulative T.
func (d *Driver) rateLimitWait(ctx context.Context, stepID string) {
	d.mu.Lock()
	streak := 0
	if st := d.exec.Steps[stepID]; st != nil {
		streak = rateLimitStreak(st.Attempts)
	}
	d.mu.Unlock()
	if streak <= 0 {
		streak = 1
	}
	wait := rateLimitBackoff(streak - 1) // the archived death is this one
	d.emit("rate_limited", map[string]any{
		"step": stepID, "consecutive": streak, "backoff_ms": wait.Milliseconds(),
		"note": "provider throttle: the same model is retried after the backoff; only the step's cumulative runtime T bounds the retries",
	})
	d.sleep(ctx, wait)
}

// sleep waits for d (tests inject Config.Sleep), cut short by ctx.
func (d *Driver) sleep(ctx context.Context, wait time.Duration) {
	if wait <= 0 {
		return
	}
	if d.cfg.Sleep != nil {
		d.cfg.Sleep(ctx, wait)
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
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
