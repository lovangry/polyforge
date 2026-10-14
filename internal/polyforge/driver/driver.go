// Package driver implements the polyforge v2 execution driver: the Go
// replacement for the LLM-driven WI Agent inside the daemon. The driver
// reads runtime-state/execution.json to determine the current state,
// spawns step processes (harness sessions) directly, validates step
// results against the R13 envelope (artifacts verified by hash), writes
// execution.json exclusively through the single transition entry point
// (transition.go), and advances the workflow on validated results.
//
// Execution semantics are the frozen v1 workflow contract
// (docs/design/polyforge-vnext/01-workflow-skills.md §6): the
// sequential success path, bounded on_fail rewind on business gate FAIL
// (the gate itself never archived nor reset — INV-1d/INV-1g), in-place
// resume on infrastructure failure (the attempt archive and candidate
// advance belong to the daemon's recovery integration; the core loop
// re-dispatches from the recomputed candidate), incomplete → blocked
// with the owner holding every next move, and §6.5's explicit
// all-done-versus-failed-terminal distinction. execution.json stays the
// single execution authority (INV-1) and the observe lane keeps
// consuming it unchanged: every transition bumps Version, which is the
// observe C3 commit-boundary engine's signal.
package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// WIInfo identifies the work item one Driver drives.
type WIInfo struct {
	ID         string
	CapsuleDir string
	BundleName string
}

// EventLogger is the driver's event emission face — the shape of
// observe.EventLog.Append, so the daemon binds its durable events log
// directly. Events are news, never state (the observe contract): a
// failing logger is reported on stderr and never fails a committed
// transition.
type EventLogger interface {
	Append(wi, kind string, body any) error
}

// Config is the daemon-supplied driver configuration.
type Config struct {
	WI WIInfo
	// BundleDir is the materialized workflow bundle root (workflow.md
	// lives there). Default: <CapsuleDir>/workflows/<BundleName>.
	BundleDir string
	// ExecPath is the execution.json path. Default:
	// <CapsuleDir>/runtime-state/execution.json (capsule.ExecutionJSON).
	ExecPath string
	// Workflow supplies the execution graph directly (tests, or a
	// daemon that already parsed it); nil → loaded from BundleDir.
	Workflow *Workflow
	// StepCandidates is the polyforge#79 effective per-step candidate
	// map (post-precedence, computed at claim time; rehydrated or
	// re-derived at recovery). When non-nil it SUBSTITUTES the parsed
	// step-level lists for every step it keys — a present-empty entry
	// governs and means no switchable chain. A step absent from the map
	// (or a nil map) keeps the bundle's own resolution.
	StepCandidates map[string][]string
	// Interrupted, when set, archives ONE infra_failure attempt on the
	// current in_progress step before the loop re-dispatches (the
	// recovery integration's write, performed by the single
	// execution.json writer). Nil on a fresh start — a claim never
	// archives.
	Interrupted *InterruptedDispatch
	// Spawner derives the step processes. Required.
	Spawner StepSpawner
	// SpawnN seeds the per-WI monotonic derivation counter (B2): the
	// same sources the runtime uses — state.json's spawn_n, or the
	// capsule spawn-journal max across a fresh clone. Each step process
	// the driver derives is one derivation; the counter advances with
	// every durable step_start and is the transition fence key.
	SpawnN int64
	// Log receives progress events; nil disables emission. Production
	// binds the daemon's per-WI event log (durable, monotonic ids).
	Log EventLogger
	// Link is the per-WI driver <-> supervisor channel (supervise
	// package); nil = unsupervised (no facts out, no verdicts in).
	Link *supervise.Link
	// Limits are the global supervision defaults (X, Y, T); step
	// overrides in workflow.md apply on top. Zero fields take the
	// documented defaults.
	Limits supervise.Defaults
	// Harness names the step harness for step_started ("" = pi).
	Harness string
	// MaxStepFailures is the per-step failure counter bound (default 3).
	MaxStepFailures int
	// Now overrides the clock (tests).
	Now func() time.Time
	// Sleep overrides the driver's waits (the D7 rate-limit backoff);
	// nil = a real timer cut short by the context. Tests inject a
	// recorder so the backoff curve is asserted without the wall time.
	Sleep func(ctx context.Context, d time.Duration)
}

// InterruptedDispatch records one derivation that ended without a
// result: the recovery cause. The driver archives it as an audit-only
// infra_failure attempt (never model-class, so never a candidate
// advance) when the recovery lane re-derives.
type InterruptedDispatch struct {
	Detail string
}

// Driver is the per-WI execution driver. One Driver per work item; Run
// is the only loop and is not re-entrant — the daemon serializes
// starts, and the spawn fence plus the transition mutex keep any stray
// concurrent writer honest anyway.
type Driver struct {
	wi       WIInfo
	workflow *Workflow
	exec     *capsule.Execution
	execPath string

	mu     sync.Mutex // serializes every transition (the single write entry)
	spawnN int64
	// initialized records that execution.json exists on disk (loaded,
	// or written by the init transition).
	initialized bool

	cfg Config
	log EventLogger
	// degraded latches when atomicfile.Write succeeds at rename but fails
	// at the post-rename directory fsync: disk may hold the new state
	// while memory rolled back. All further transitions are refused
	// until reconciliation (review issue 6).
	degraded bool

	// wake carries the "state changed, re-plan" signal to a Run loop
	// parked on an open question; one buffered slot, coalescing.
	wake chan struct{}

	// rejects/priorRejection track per-step envelope-rejection state for
	// the in-process re-dispatch lane (the #97/#107 lessons): the count
	// bounds the retries, the string rides the next dispatch's prompt as
	// visible feedback so the author can fix the actual defect.
	rejects        map[string]int
	priorRejection map[string]string
	// unbooked is the wall time of finished dispatches not yet written
	// into the step's cumulative runtime (the T accumulator): folded into
	// the next result/failure write.
	unbooked map[string]int64
	// waitingAnnounced is the question id step_waiting_human was last
	// sent for (one fact per question, however often the loop re-plans).
	waitingAnnounced string
	// inputEventFor is the question id a needs_input event was last
	// emitted for in this driver life (the park transition emits it; a
	// restarted driver re-announces a question it finds parked).
	inputEventFor string
}

// New builds a Driver: it resolves paths, loads the workflow graph from
// the materialized bundle (or takes the supplied one), and loads
// execution.json — a missing file is a fresh capsule (the skeleton is
// built in memory; Run's init transition persists it), a torn one is a
// hard error (never guess execution state). Step records are ensured
// for every workflow step, and any recorded step outside the workflow
// is refused: the bundle digest pins the graph, so a divergence means
// the wrong bundle.
func New(cfg Config) (*Driver, error) {
	if cfg.WI.ID == "" {
		return nil, fmt.Errorf("driver: config without a work item id")
	}
	if cfg.WI.CapsuleDir == "" {
		return nil, fmt.Errorf("driver: config without a capsule dir")
	}
	if cfg.Spawner == nil {
		return nil, fmt.Errorf("driver: config without a spawner")
	}
	execPath := cfg.ExecPath
	if execPath == "" {
		execPath = filepath.Join(cfg.WI.CapsuleDir, capsule.ExecutionJSON)
	}
	wf := cfg.Workflow
	bundleDir := cfg.BundleDir
	if bundleDir == "" && cfg.WI.BundleName != "" {
		bundleDir = filepath.Join(cfg.WI.CapsuleDir, capsule.DirWorkflows, cfg.WI.BundleName)
	}
	if wf == nil {
		if bundleDir == "" {
			return nil, fmt.Errorf("driver: config without a bundle name or dir")
		}
		loaded, err := LoadWorkflow(bundleDir)
		if err != nil {
			return nil, err
		}
		wf = loaded
	}
	if len(wf.Steps) == 0 {
		return nil, fmt.Errorf("driver: workflow has no steps")
	}
	if wf.MaxTotal <= 0 {
		wf.MaxTotal = defaultMaxRewindTotal
	}
	if wf.MaxPerStep <= 0 {
		wf.MaxPerStep = defaultMaxRewindPerStep
	}

	exec, existed, err := loadExecution(execPath, cfg.WI, wf)
	if err != nil {
		return nil, err
	}
	return &Driver{
		wi:             cfg.WI,
		workflow:       wf,
		exec:           exec,
		execPath:       execPath,
		spawnN:         cfg.SpawnN,
		initialized:    existed,
		cfg:            cfg,
		log:            cfg.Log,
		wake:           make(chan struct{}, 1),
		rejects:        make(map[string]int),
		priorRejection: make(map[string]string),
		unbooked:       make(map[string]int64),
	}, nil
}

// Run is the main loop (01 §6.5): determine the current step, start it
// if it is not already in progress, spawn its process, wait for the
// result, validate, transition, and repeat — until the workflow reaches
// a terminal state (nil return), parks blocked on an incomplete result
// (nil return: the owner holds the next move; the durable file says
// why), or an infrastructure failure surfaces (non-nil error: spawn
// failure, persist failure, rejected result, context cancellation — the
// daemon's recovery lane owns re-derivation).
//
// Crash safety: the step_start write lands BEFORE the process exists,
// so a crash between them leaves an in_progress step that this loop's
// resume path re-dispatches on the next Run. A crash while a question
// is open is equally recoverable: the pending question is durable, the
// re-invoked loop parks on it again, and Answer() delivers across the
// restart.
func (d *Driver) Run(ctx context.Context) error {
	if !d.isInitialized() {
		if err := d.transition(TransitionRequest{Kind: TransInit, SpawnN: d.currentSpawnN()}); err != nil {
			return fmt.Errorf("driver: execution init failed: %w", err)
		}
	}
	// polyforge#79 era: the recovery integration's interrupted-dispatch
	// archive (the recovery lane triggers it; the single execution.json
	// writer performs it). ONE audit-only infra_failure attempt for the
	// derivation that died without a result, BEFORE the re-derivation
	// dispatches. It is not model-class, so it never moves the candidate
	// pointer; an operator shutdown never reaches this hook (the bridge
	// filters it).
	if d.cfg.Interrupted != nil {
		if err := d.transition(TransitionRequest{
			Kind:   TransInterrupted,
			Reason: d.cfg.Interrupted.Detail,
			SpawnN: d.currentSpawnN(),
		}); err != nil {
			return fmt.Errorf("driver: interrupted-dispatch archive failed: %w", err)
		}
	}
	for {
		if v := d.takeStalled(); v != nil {
			d.emit("driver_stalled", map[string]any{"duration_ms": v.Duration.Milliseconds(), "phase": v.Phase})
			return ErrDriverStalled
		}
		stop, stepID, err := d.plan(ctx)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}

		spawn, n, err := d.prepareDispatch(stepID)
		if err != nil {
			return err
		}
		if spawn == nil {
			continue // an open question is parked; plan again once answered
		}

		if err := d.transition(TransitionRequest{
			Kind:   TransStepStart,
			StepID: stepID,
			Model:  spawn.Model,
			SpawnN: n,
		}); err != nil {
			return fmt.Errorf("driver: step %s start failed: %w", stepID, err)
		}

		dispatchedAt := d.now()
		run, err := d.cfg.Spawner.SpawnStep(ctx, *spawn)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("driver: step %s spawn %d failed: %w", stepID, n, err)
			}
			// The spawn itself failed (harness binary missing, journal or
			// prompt I/O, ...): a dispatch that never produced a process is
			// a failed dispatch like any other — classified, counted,
			// retried or blocked.
			cause, code, detail := classifyWaitFailure(fmt.Errorf("driver: step %s spawn %d failed: %w", stepID, n, err))
			if blocked, ferr := d.failDispatch(ctx, stepID, n, dispatchedAt, cause, code, detail); ferr != nil {
				return ferr
			} else if blocked {
				return nil
			}
			continue
		}
		d.announceStart(spawn, run, dispatchedAt)
		res, werr, verdict := d.awaitDispatch(ctx, run, stepID, n)
		if verdict != nil {
			// The supervisor judged the dispatch dead. The process is
			// already torn down; this is the dispatch's one failure record.
			d.emit("step_dead", map[string]any{
				"step": stepID, "spawn_n": n, "cause": verdict.Cause, "evidence": verdict.Evidence,
			})
			if blocked, ferr := d.failDispatch(ctx, stepID, n, dispatchedAt, string(verdict.Cause), -1, verdict.Evidence); ferr != nil {
				return ferr
			} else if blocked {
				return nil
			}
			continue
		}
		if werr != nil {
			_ = run.Terminate(5 * time.Second)
			if ctx.Err() != nil {
				// Cancellation (shutdown, recovery relaunch): not a failure of
				// the step — nothing is counted and the candidate pointer
				// stays; the cumulative runtime is still booked.
				d.announceEnd(stepID, n, supervise.OutcomeExitedWithoutResult, -1, "cancelled")
				d.bookWall(stepID, dispatchedAt)
				return fmt.Errorf("driver: step %s dispatch %d ended without a result: %w", stepID, n, werr)
			}
			cause, code, detail := classifyWaitFailure(werr)
			if blocked, ferr := d.failDispatch(ctx, stepID, n, dispatchedAt, cause, code, detail); ferr != nil {
				return ferr
			} else if blocked {
				return nil
			}
			continue
		}
		d.announceEnd(stepID, n, supervise.OutcomeResult, 0, "")
		d.addUnbooked(stepID, dispatchedAt)
		delivered := res.Outcome
		if verr := d.validateResultStrict(res, stepID, n); verr != nil {
			// Envelope-authoring errors are STEP-CONTENT failures, not
			// infrastructure (the #97/#107 lessons: a hand-transcribed digest
			// typo, a missing handoff digest — each used to cost a whole driver
			// restart through the recovery lane, and two rejections inside
			// the storm window bounded-failed the WI outright). The driver
			// re-dispatches the step in-process with the rejection as visible
			// feedback, bounded by maxEnvelopeRejects; only a persistently
			// broken author escalates to the recovery lane.
			_ = run.Terminate(5 * time.Second)
			if d.log != nil {
				_ = d.log.Append(d.wi.ID, EvEnvelopeRejected, map[string]any{
					"step": stepID, "spawn_n": n, "detail": verr.Error(),
				})
			}
			d.mu.Lock()
			d.rejects[stepID]++
			count := d.rejects[stepID]
			feedback := fmt.Sprintf("dispatch %d: %v", n, verr)
			if delivered == OutcomeNeedsInput {
				// Keep the pending-question context across the retry: the
				// rejected envelope was a question, and the rewrite must
				// still be one.
				q := strings.TrimSpace(res.Question)
				if q == "" {
					_, q, _ = needsInputQuestion(res)
				}
				feedback += "\nThe rejected envelope was a needs_input (question) report"
				if q != "" {
					feedback += fmt.Sprintf(" asking: %q", q)
				}
				feedback += ". Rewrite it as a needs_input envelope carrying both question_id and question (envelope level or in the output object); do not turn it into a completed result."
			}
			d.priorRejection[stepID] = feedback
			d.mu.Unlock()
			if count > maxEnvelopeRejects {
				d.emit("schema_fault", map[string]any{
					"step": stepID, "spawn_n": n, "rejections": count,
					"detail": "persistent envelope rejection: the result contract is broken, not the model; reported to the owner",
				})
				// the in-process retry bound is spent: the step goes BLOCKED
				// with the result_invalid cause code — no driver relaunch
				// through the recovery lane, no on_fail routing (rewind stays
				// reserved for review FAIL verdicts).
				if err := d.transition(TransitionRequest{
					Kind: TransStepFailed, StepID: stepID, SpawnN: n,
					Failure: &StepFailure{Cause: FailResultInvalid, Detail: verr.Error()},
				}); err != nil {
					return fmt.Errorf("driver: step %s result-invalid block failed: %w", stepID, err)
				}
				d.mu.Lock()
				blocked := d.exec.WorkflowStatus == StatusBlocked
				d.mu.Unlock()
				if blocked {
					return nil // blocked, not relaunchable: a clean stop
				}
				return fmt.Errorf("driver: step %s dispatch %d result rejected (%d consecutive rejections, limit %d) but the block did not land: %w",
					stepID, n, count, maxEnvelopeRejects, verr)
			}
			continue // in-process re-dispatch: the step is still in_progress
		}
		if res.Outcome != delivered && d.log != nil {
			// The verdict binding re-classified the envelope (the #106
			// lesson): a gate's completed envelope carrying a fail verdict
			// routes as the gate-fail report it claims to be. News, not
			// state — the delivered envelope stays archived verbatim, the
			// rewind itself is written by the transitions that follow.
			verdict, _ := failVerdictInOutput(res.Output)
			_ = d.log.Append(d.wi.ID, EvGateVerdictBound, map[string]any{
				"step": stepID, "spawn_n": n, "verdict": verdict,
				"delivered_outcome": delivered, "bound_outcome": res.Outcome,
			})
		}
		d.mu.Lock()
		delete(d.rejects, stepID)
		delete(d.priorRejection, stepID)
		d.mu.Unlock()
		if err := d.transition(TransitionRequest{
			Kind:   TransStepResult,
			StepID: stepID,
			Result: res,
			SpawnN: n,
			WallMs: d.takeUnbooked(stepID),
		}); err != nil {
			return fmt.Errorf("driver: step %s result transition failed: %w", stepID, err)
		}

		switch res.Outcome {
		case OutcomeCompleted:
			continue // §6.5 step 2: next selection
		case OutcomeGateFailed:
			if err := d.transition(TransitionRequest{
				Kind:   TransGateFail,
				StepID: stepID,
				Gate:   d.verdictFor(stepID, res),
				SpawnN: n,
			}); err != nil {
				return fmt.Errorf("driver: gate %s fail policy failed: %w", stepID, err)
			}
			continue // §6.5 step 3: terminal check or rewind target on the next plan
		case OutcomeNeedsInput:
			continue // the plan pass parks on the persisted question until answered
		case OutcomeIncomplete:
			return nil // §6.5 step 5: blocked is durable; the loop exits
		default:
			return fmt.Errorf("driver: step %s dispatch %d produced unknown outcome %q", stepID, n, res.Outcome)
		}
	}
}

// plan is one selection pass: terminal or blocked workflows stop the
// loop (nil error); a current step holding an open question parks the
// loop until its answer (or another transition) wakes it; otherwise the
// earliest non-done step (next_step_by_order, 01 §6.5) is dispatched —
// and an empty selection over a running workflow is the §6.5 step 6
// check: every step done, so the loop writes completed and stops.
func (d *Driver) plan(ctx context.Context) (stop bool, stepID string, err error) {
	for {
		d.mu.Lock()
		status := d.exec.WorkflowStatus
		stop = isTerminalStatus(status) || status == StatusBlocked
		var pending bool
		if !stop && d.exec.CurrentStep != "" {
			if st := d.exec.Steps[d.exec.CurrentStep]; st != nil {
				pending = st.PendingQuestion != nil
			}
		}
		var next string
		if !stop && !pending {
			for _, spec := range d.workflow.Steps {
				st := d.exec.Steps[spec.ID]
				if st == nil || st.Status != capsule.StepDone {
					next = spec.ID
					break
				}
			}
		}
		spawnN := d.spawnN
		d.mu.Unlock()

		if stop {
			return true, "", nil
		}
		if pending {
			// no step process exists while a human is asked: the
			// supervisor runs no dispatch timer and no stall clock
			d.announceWaiting()
			if err := d.awaitAnswer(ctx); err != nil {
				return false, "", err
			}
			continue // re-plan: answered (or otherwise woken)
		}
		if next == "" {
			// §6.5 step 6: not a failure terminal (that exits above),
			// so an empty selection means every step is done.
			if err := d.transition(TransitionRequest{
				Kind:   TransTerminal,
				Status: StatusCompleted,
				SpawnN: spawnN,
			}); err != nil {
				return false, "", fmt.Errorf("driver: workflow completion transition failed: %w", err)
			}
			return true, "", nil
		}
		return false, next, nil
	}
}

// prepareDispatch snapshots everything the spawn needs from under the
// mutex: the resume flag (an in_progress step is a re-dispatch — answer
// continuation, crash recovery, recovery-lane re-derivation), the
// rewind verdict about to be consumed by the start write (it must reach
// the dispatched process's prompt inputs), the step's archived answers
// (a resumed step continues with its Q&A history), and the effective
// model candidate. Parking on an open question is plan()'s decision —
// a pending question can only exist on the current in_progress step,
// which plan sees before dispatching here.
func (d *Driver) prepareDispatch(stepID string) (*StepSpawn, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.exec.Steps[stepID]
	sp := &StepSpawn{
		StepID: stepID,
		WIID:   d.wi.ID,
		Resume: st != nil && st.Status == capsule.StepInProgress,
	}
	if st != nil {
		sp.RewindInput = st.PendingRewindInput
		if len(st.Answers) > 0 {
			sp.Answers = append([]capsule.ArchivedAnswer(nil), st.Answers...)
		}
		if sp.Resume && st.Supervision != nil && st.Supervision.NextDispatch == capsule.NextDispatchFresh {
			sp.Fresh = true
			sp.Resume = false
		}
	}
	// the in-process rejection retry lane: the prior dispatch's
	// validation error rides the prompt as visible feedback
	sp.PriorRejection = d.priorRejection[stepID]
	if st != nil && len(st.Attempts) > 0 && st.Attempts[len(st.Attempts)-1].FailureCause == FailEndedWithoutResult {
		sp.PriorEndedWithoutResult = true
	}
	if spec := d.workflow.step(stepID); spec != nil {
		sp.Model = d.modelForLocked(stepID)
	}
	sp.SpawnN = d.spawnN + 1
	return sp, sp.SpawnN, nil
}

// awaitAnswer parks the loop while the current step holds an open
// question. Every successful transition pushes a wake, so the wait
// re-plans on the answer, an operator rewind/terminate, or a terminal —
// and exits promptly on ctx.
func (d *Driver) awaitAnswer(ctx context.Context) error {
	for {
		d.mu.Lock()
		status := d.exec.WorkflowStatus
		var pending bool
		if !isTerminalStatus(status) && status != StatusBlocked && d.exec.CurrentStep != "" {
			if st := d.exec.Steps[d.exec.CurrentStep]; st != nil {
				pending = st.PendingQuestion != nil
			}
		}
		d.mu.Unlock()
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wake:
		}
	}
}

// Answer delivers one owner answer for the current step's open
// question: the D9 consumption write (archive + clear in one atomic
// transition), then the loop's wake re-plans and re-dispatches the step
// in place with the answer carried in its spawn. A question-id
// mismatch, or no open question at all, is refused — the same binding
// the daemon's answer relay enforces on its side.
func (d *Driver) Answer(questionID, answer, answeredBy string) error {
	step := ""
	d.mu.Lock()
	step = d.exec.CurrentStep
	d.mu.Unlock()
	if err := d.transition(TransitionRequest{
		Kind:       TransAnswer,
		QuestionID: questionID,
		Reason:     answer,
		AnsweredBy: answeredBy,
		SpawnN:     d.currentSpawnN(),
	}); err != nil {
		return err
	}
	// the human answered: the driver is between steps again (the
	// supervisor's stall clock restarts from here)
	d.mu.Lock()
	d.waitingAnnounced = ""
	d.mu.Unlock()
	d.fact(supervise.StepAnswered{Step: step, SpawnN: d.currentSpawnN(), QuestionID: questionID})
	return nil
}

// ResumeCurrent is the owner's third §6.4 verdict after an incomplete
// result: write running back over the parked workflow (03 §4.2: the
// owner's follow-up writes running back) and wake the loop, whose own
// dispatch re-dispatches the frozen in_progress current step in place
// as a fresh derivation. Only a blocked workflow accepts it — while
// running, the loop owns starts, and a stray resume would race the
// loop's own dispatch ordinal.
//
// The verdict consumes NO derivation ordinal (polyforge#116): the fresh
// ordinal is the loop's own step_start write on that dispatch, so the
// spawn ledger records exactly one derivation per dispatch that ran —
// the re-dispatch the owner asked for lands at the next ordinal with no
// phantom gap. The verdict alone re-dispatches nothing: it is a state
// write plus a wake; the caller re-invokes Run (the claim/start path
// does: bridge.Start → ResumeCurrent → Run).
func (d *Driver) ResumeCurrent() error {
	d.mu.Lock()
	blocked := d.exec.WorkflowStatus == StatusBlocked
	d.mu.Unlock()
	if !blocked {
		return fmt.Errorf("driver: resume is the owner's blocked-step verdict; workflow is %q", d.Snapshot().WorkflowStatus)
	}
	return d.transition(TransitionRequest{
		Kind:   TransUnblock,
		SpawnN: d.currentSpawnN(),
	})
}

// Snapshot answers a deep copy of the current execution state (the
// daemon's observe/report face may read it freely).
func (d *Driver) Snapshot() *capsule.Execution {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneExec(d.exec)
}

// SpawnN answers the current derivation counter (the fence key callers
// must carry in TransitionRequest.SpawnN).
func (d *Driver) SpawnN() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spawnN
}

func (d *Driver) isInitialized() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.initialized
}

func (d *Driver) currentSpawnN() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spawnN
}

func (d *Driver) now() time.Time {
	if d.cfg.Now != nil {
		return d.cfg.Now()
	}
	return time.Now()
}

func (d *Driver) stamp() string { return d.now().UTC().Format(time.RFC3339Nano) }

// wakeLoopLocked nudges a parked Run loop; coalescing by design.
func (d *Driver) wakeLoopLocked() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Spawn seam. The production adapter (spawn.go, a later lane) bridges
// StepSpawn onto the runtime's SpawnParams (model pin, resume session,
// spawn journal) and assembles the step prompt — the step definition
// plus declared inputs plus, for inherit steps, the producing step's
// handoff artifact and, when present, the consumed rewind verdict.

// StepSpawner derives one step process per SpawnStep call. The driver
// serializes: at most one live step run per Driver at any time.
type StepSpawner interface {
	SpawnStep(ctx context.Context, sp StepSpawn) (StepRun, error)
}

// StepSpawn is the driver's order for one step-process derivation.
type StepSpawn struct {
	StepID string
	// WIID is the parent work item — the result envelope must bind it.
	WIID string
	// SpawnN is this derivation's ordinal — the step process's fence
	// identity; its result envelope must echo it.
	SpawnN int64
	// Model is the resolved candidate ("" = the harness adapter's
	// default; no candidate list is in effect).
	Model string
	// Resume marks a re-dispatch of an in_progress step (answer
	// continuation, crash recovery, recovery-lane re-derivation).
	Resume bool
	// Answers is the step's archived Q&A history — a resumed step
	// continues from it, never from a blank slate.
	Answers []capsule.ArchivedAnswer
	// RewindInput is the gate FAIL verdict this dispatch consumes (set
	// on a rewind re-entry; the start transition clears the persisted
	// field in the same write that starts the step).
	RewindInput *capsule.RewindInput
	// Fresh orders a new conversation for a retry (2nd+ failure, or a
	// candidate switch): no resume of the old transcript; the previous
	// transcript, when one is readable, rides the prompt as a read-only
	// reference (v2-converge 2.3 retry shape — a context-overflow style
	// failure must not repeat on every model).
	Fresh bool
	// PriorRejection is the verbatim validation error the PREVIOUS
	// dispatch's envelope was rejected for (the in-process retry lane):
	// the step author sees exactly what to fix — digests computed
	// programmatically, required fields present — instead of blindly
	// re-authoring. Empty on a fresh dispatch.
	PriorRejection string
	// PriorEndedWithoutResult tells the retry that the previous dispatch's
	// turn ENDED without the agent writing the result envelope (cause
	// ended_without_result).
	PriorEndedWithoutResult bool
}

// StepRun is one spawned step execution awaiting its result envelope.
type StepRun interface {
	// Wait blocks until the step process returns its result envelope.
	// An error means no business result (infrastructure failure or
	// cancellation): the recovery lane owns what follows.
	Wait(ctx context.Context) (*StepResult, error)
	// Terminate tears the step process down, bounded.
	Terminate(bound time.Duration) error
}

// ---------------------------------------------------------------------------
// Result envelope (R13).

// StepResult is the R13 result envelope every step process returns.
// SpawnN is the fencing key; ResultID the idempotency key (the R14
// accept staging in result.go rides on it).

// ---------------------------------------------------------------------------
// Result validation (the core's accept gate; the full output-schema
// validation is validate.go's later lane — this is the envelope,
// output-shape, handoff and artifact-manifest layer).

// validateResultStrict is the accept-path check before the step_result
// transition. It builds a ResultSpec from the driver's current state and
// delegates to ValidateStepResult (the staging gates: shape, binding and
// fencing, the needs_input question pair, the verdict binding, the
// human-gate guarantee, the output schema and the evidence duty). A
// failure is an infrastructure-class rejection — Run surfaces it and the
// recovery lane owns re-derivation.
func (d *Driver) validateResultStrict(res *StepResult, stepID string, n int64) error {
	spec := d.buildResultSpec(stepID, n)
	return ValidateStepResult(res, spec)
}

// buildResultSpec assembles the read-only validation snapshot from the
// driver's current durable state (gate policy, human-answer duty, the
// answers of the current attempt chain and the command-evidence duty).
func (d *Driver) buildResultSpec(stepID string, n int64) ResultSpec {
	spec := ResultSpec{
		WIID:   d.wi.ID,
		StepID: stepID,
		SpawnN: n,
	}
	d.mu.Lock()
	if st, ok := d.exec.Steps[stepID]; ok {
		spec.HumanAnswers = currentChainAnswers(st)
	}
	d.mu.Unlock()
	// Declared outputs, gate-ness and duties (gate-ness feeds the
	// verdict binding — the #106 lesson).
	if ss := d.workflow.step(stepID); ss != nil {
		spec.Gate = ss.Gate
		spec.RequiresHumanAnswer = ss.RequiresHumanAnswer
		spec.RequireCommandEvidence = ss.RequiresCommandEvidence
	}
	if stepID == verificationStepID {
		spec.RequireCommandEvidence = true
	}
	return spec
}

// parseOutput coerces a result's output payload to a JSON object map
// (the capsule Step.Output shape); an absent payload is an empty
// object.
func parseOutput(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("output is not a JSON object: %w", err)
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

// verdictFor assembles the gate FAIL policy the transition applies:
// the workflow's on_fail rewind target (absent = no rewind edge, the
// workflow fails on the gate) plus the verdict text — the result's
// output payload in full, or a synthesized note when the gate produced
// none.
func (d *Driver) verdictFor(stepID string, res *StepResult) *GateVerdict {
	v := &GateVerdict{
		GateStepID:  stepID,
		VerdictText: strings.TrimSpace(string(res.Output)),
	}
	if v.VerdictText == "" {
		v.VerdictText = fmt.Sprintf("gate %s FAIL (spawn %d)", stepID, res.SpawnN)
	}
	if spec := d.workflow.step(stepID); spec != nil && spec.OnFail != nil {
		v.TargetStep = spec.OnFail.Target
	}
	return v
}

// ---------------------------------------------------------------------------
// Execution load + candidate resolution.

// loadExecution reads execution.json; a missing file yields a fresh
// skeleton (all steps pending) with existed=false — the init transition
// persists it. A torn file is a hard error (never guess execution
// state). Recorded steps outside the workflow are refused (the bundle
// digest pins the graph); missing step records are ensured pending.
func loadExecution(path string, wi WIInfo, wf *Workflow) (*capsule.Execution, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return skeletonExecution(wi, wf), false, nil
		}
		return nil, false, err
	}
	var exec capsule.Execution
	if err := json.Unmarshal(raw, &exec); err != nil {
		return nil, false, fmt.Errorf("driver: execution.json unparsable: %w", err)
	}
	if exec.Steps == nil {
		exec.Steps = map[string]*capsule.Step{}
	}
	for id := range exec.Steps {
		if _, ok := wf.indexOf(id); !ok {
			return nil, false, fmt.Errorf("driver: execution.json records step %q which is not in the workflow (bundle divergence?)", id)
		}
	}
	for _, spec := range wf.Steps {
		if _, ok := exec.Steps[spec.ID]; !ok {
			exec.Steps[spec.ID] = &capsule.Step{Status: capsule.StepPending}
		}
	}
	return &exec, true, nil
}

func skeletonExecution(wi WIInfo, wf *Workflow) *capsule.Execution {
	exec := &capsule.Execution{
		CapsuleID:      capsuleIDFor(wi.ID),
		WorkItemID:     wi.ID,
		WorkflowStatus: StatusRunning,
		Steps:          map[string]*capsule.Step{},
	}
	for _, spec := range wf.Steps {
		exec.Steps[spec.ID] = &capsule.Step{Status: capsule.StepPending}
	}
	return exec
}

func capsuleIDFor(wi string) string { return "cap_" + wi }

// effectiveCandidates answers the step's effective candidate list: the
// polyforge#79 post-precedence map when the claim/recovery supplied
// one, the bundle's own resolution otherwise.
func (d *Driver) effectiveCandidates(stepID string) []string {
	if d.cfg.StepCandidates != nil {
		if list, ok := d.cfg.StepCandidates[stepID]; ok {
			return list
		}
	}
	if spec := d.workflow.step(stepID); spec != nil {
		return spec.ModelCandidates
	}
	return nil
}

// modelForLocked resolves the step's effective model for the NEXT
// dispatch (the candidate at the recomputed pointer; "" when no
// candidate list is in effect). The recomputation runs over the
// EFFECTIVE list (polyforge#79): the post-precedence list the claim
// carried — or the recovery re-derived — never the raw bundle parse
// alone. Model-class infra failures alone advance (bounded, capped at list end), a gate-FAIL
// rewind resets to the head.
func (d *Driver) modelForLocked(stepID string) string {
	list := d.effectiveCandidates(stepID)
	if len(list) == 0 {
		return ""
	}
	st := d.exec.Steps[stepID]
	if st == nil {
		return list[0]
	}
	return list[CandidatePointer(st.Attempts, list)]
}
