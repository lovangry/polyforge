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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/workflow"
	"gopkg.in/yaml.v3"
)

// WIInfo identifies the work item one Driver drives.
type WIInfo struct {
	ID           string
	CapsuleDir   string
	BundleName   string
	BundleDigest string
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
	// Log receives progress events; nil disables emission.
	Log EventLogger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// InterruptedDispatch records one derivation that ended without a
// result: its classified fault and the recovery cause. The driver
// archives it as the step's infra_failure attempt (the candidate
// pointer's input) when the recovery lane re-derives.
type InterruptedDispatch struct {
	FaultClass FaultClass
	Detail     string
}

// Driver is the per-WI execution driver. One Driver per work item; Run
// is the only loop and is not re-entrant — the daemon serializes
// starts, and the spawn fence plus the transition mutex keep any stray
// concurrent writer honest anyway.
type Driver struct {
	wi       WIInfo
	workflow *Workflow
	// extWF is the extended (driver) workflow: context_policy, declared
	// inputs/outputs, handoff duty. Loaded via ParseDriverWorkflow — the
	// single authoritative source for both sequencing and prompt assembly.
	extWF    *DriverWorkflow
	exec     *capsule.Execution
	execPath string

	mu     sync.Mutex // serializes every transition (the single write entry)
	spawnN int64
	// initialized records that execution.json exists on disk (loaded,
	// or written by the init transition).
	initialized bool

	cfg Config
	log EventLogger
	// degraded latches when atomicPersist succeeds at rename but fails
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
		loaded, err := loadWorkflow(bundleDir)
		if err != nil {
			return nil, err
		}
		wf = loaded
	}
	// Load the extended workflow (context_policy, inputs, outputs, handoff).
	// Single source of truth for sequencing + prompt assembly (review fix 5).
	var ext *DriverWorkflow
	if bundleDir != "" {
		if extLoaded, err := LoadDriverWorkflow(bundleDir); err == nil {
			ext = extLoaded
		}
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
		extWF:          ext,
		exec:           exec,
		execPath:       execPath,
		spawnN:         cfg.SpawnN,
		initialized:    existed,
		cfg:            cfg,
		log:            cfg.Log,
		wake:           make(chan struct{}, 1),
		rejects:        make(map[string]int),
		priorRejection: make(map[string]string),
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
	// writer performs it). ONE infra_failure attempt for the derivation
	// that died without a result, BEFORE the re-derivation dispatches —
	// the archived-attempt recomputation then indexes it (a
	// provider-class archive is what advances the candidate pointer;
	// operator-class causes never reach this hook — the bridge filters
	// them).
	if d.cfg.Interrupted != nil {
		if err := d.transition(TransitionRequest{
			Kind:       TransInterrupted,
			FaultClass: string(d.cfg.Interrupted.FaultClass),
			Reason:     d.cfg.Interrupted.Detail,
			SpawnN:     d.currentSpawnN(),
		}); err != nil {
			return fmt.Errorf("driver: interrupted-dispatch archive failed: %w", err)
		}
	}
	for {
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

		run, err := d.cfg.Spawner.SpawnStep(ctx, *spawn)
		if err != nil {
			return fmt.Errorf("driver: step %s spawn %d failed: %w", stepID, n, err)
		}
		res, err := run.Wait(ctx)
		if err != nil {
			_ = run.Terminate(5 * time.Second)
			return fmt.Errorf("driver: step %s dispatch %d ended without a result: %w", stepID, n, err)
		}
		delivered := res.Outcome
		if verr := d.validateResultStrict(res, stepID, n); verr != nil {
			// Envelope-authoring errors are STEP-CONTENT failures, not
			// infrastructure (the #97/#107 lessons: a hand-transcribed digest
			// typo, a missing handoff_ref — each used to cost a whole driver
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
			d.priorRejection[stepID] = fmt.Sprintf("dispatch %d: %v", n, verr)
			d.mu.Unlock()
			if count > maxEnvelopeRejects {
				return fmt.Errorf("driver: step %s dispatch %d result rejected (%d consecutive rejections, limit %d): %w",
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
	}
	// the in-process rejection retry lane: the prior dispatch's
	// validation error rides the prompt as visible feedback
	sp.PriorRejection = d.priorRejection[stepID]
	if spec := d.workflow.step(stepID); spec != nil {
		sp.ContextMode = spec.ContextPolicy
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
	return d.transition(TransitionRequest{
		Kind:       TransAnswer,
		QuestionID: questionID,
		Reason:     answer,
		AnsweredBy: answeredBy,
		SpawnN:     d.currentSpawnN(),
	})
}

// Rewind is the operator rewind verdict: archive and reset
// [toStep, current), position back to toStep (the owner's §6.4 move
// after an incomplete result, or an in-flight correction). See
// applyRewindLocked for the open-question refusal while running.
func (d *Driver) Rewind(toStep, reason string) error {
	return d.transition(TransitionRequest{
		Kind:   TransRewind,
		StepID: toStep,
		Reason: reason,
		SpawnN: d.currentSpawnN(),
	})
}

// Terminate lands a terminal workflow_status with a reason (the owner's
// kill verdict from blocked, or the daemon's close path). completed is
// refused unless every step is done — that status is the loop's alone.
func (d *Driver) Terminate(status, reason string) error {
	return d.transition(TransitionRequest{
		Kind:   TransTerminal,
		Status: status,
		Reason: reason,
		SpawnN: d.currentSpawnN(),
	})
}

// ResumeCurrent is the owner's third §6.4 verdict after an incomplete
// result: unblock the workflow and re-dispatch the current step in
// place (03 §4.2: the owner's follow-up writes running back). Only a
// blocked workflow accepts it — while running, the loop owns starts,
// and a stray resume would race the loop's own dispatch ordinal.
func (d *Driver) ResumeCurrent() error {
	d.mu.Lock()
	blocked := d.exec.WorkflowStatus == StatusBlocked
	cur := d.exec.CurrentStep
	n := d.spawnN
	d.mu.Unlock()
	if !blocked {
		return fmt.Errorf("driver: resume is the owner's blocked-step verdict; workflow is %q", d.Snapshot().WorkflowStatus)
	}
	if cur == "" {
		return fmt.Errorf("driver: blocked workflow without a current step")
	}
	return d.transition(TransitionRequest{
		Kind:   TransStepStart,
		StepID: cur,
		SpawnN: n + 1, // the resume re-dispatch is a fresh derivation
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
	// ContextMode is the step's context_policy ("inherit" or "fresh";
	// the adapter decides what inheritance means at prompt assembly).
	ContextMode string
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
	// PriorRejection is the verbatim validation error the PREVIOUS
	// dispatch's envelope was rejected for (the in-process retry lane):
	// the step author sees exactly what to fix — digests computed
	// programmatically, required fields present — instead of blindly
	// re-authoring. Empty on a fresh dispatch.
	PriorRejection string
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
// delegates to result.ValidateStepResult (the R14 staging gates: shape,
// binding/fencing, idempotency, output schema, handoff duty, artifact
// verification under os.OpenRoot jail). A failure is an infrastructure-class
// rejection — Run surfaces it and the recovery lane owns re-derivation.
func (d *Driver) validateResultStrict(res *StepResult, stepID string, n int64) error {
	spec := d.buildResultSpec(stepID, n)
	return ValidateStepResult(res, spec)
}

// buildResultSpec assembles the read-only validation snapshot from the
// driver's current durable state (the extended workflow's declared outputs
// and handoff duty, plus the accepted result ids already committed).
func (d *Driver) buildResultSpec(stepID string, n int64) ResultSpec {
	spec := ResultSpec{
		WIID:        d.wi.ID,
		StepID:      stepID,
		SpawnN:      n,
		AcceptedIDs: make(map[string]bool),
		CapsuleRoot: d.cfg.WI.CapsuleDir,
	}
	// Accepted IDs: harvest from the step's committed attempt records.
	d.mu.Lock()
	if st, ok := d.exec.Steps[stepID]; ok {
		for _, at := range st.Attempts {
			if at.AcceptedResultID != "" {
				spec.AcceptedIDs[at.AcceptedResultID] = true
			}
		}
	}
	d.mu.Unlock()
	// Declared outputs, handoff duty and gate-ness from the extended
	// workflow (gate-ness feeds the verdict binding — the #106 lesson).
	if d.extWF != nil {
		for _, ds := range d.extWF.Steps {
			if ds.ID == stepID {
				spec.Outputs = ds.Outputs
				spec.HandoffRequired = ds.Handoff
				spec.Gate = ds.Gate
				break
			}
		}
	} else if d.workflow != nil {
		// Fallback: the sequencing workflow carries the same Handoff
		// declaration (tests inject Workflow directly without a bundle dir).
		if ss := d.workflow.step(stepID); ss != nil {
			spec.HandoffRequired = ss.Handoff
			spec.Gate = ss.Gate
		}
	}
	return spec
}

// validateResultLegacy is retained for test compatibility during migration.
func (d *Driver) validateResult(res *StepResult, stepID string, n int64) error {
	if res == nil {
		return fmt.Errorf("nil result envelope")
	}
	if res.StepID != "" && res.StepID != stepID {
		return fmt.Errorf("envelope names step %q, dispatch was %q", res.StepID, stepID)
	}
	if res.SpawnN != n {
		return fmt.Errorf("%w: envelope spawn %d, dispatch %d", ErrStaleFence, res.SpawnN, n)
	}
	if res.WIID != "" && res.WIID != d.wi.ID {
		return fmt.Errorf("envelope names work item %q, this driver drives %q", res.WIID, d.wi.ID)
	}
	if res.ResultID == "" {
		return fmt.Errorf("missing result_id (idempotency key)")
	}
	if res.ProtocolVersion != protocolVersion {
		return fmt.Errorf("protocol_version %d, this driver speaks %d", res.ProtocolVersion, protocolVersion)
	}
	switch res.Outcome {
	case OutcomeCompleted, OutcomeGateFailed, OutcomeNeedsInput, OutcomeIncomplete:
	default:
		return fmt.Errorf("unknown outcome %q", res.Outcome)
	}
	if _, err := parseOutput(res.Output); err != nil {
		return err
	}
	if res.Outcome != OutcomeCompleted {
		return nil // only the accept path carries artifacts to verify
	}
	if spec := d.workflow.step(stepID); spec != nil && spec.Handoff && res.HandoffRef == nil {
		return fmt.Errorf("step %q declares handoff but the result carries no handoff_ref", stepID)
	}
	for i := range res.Artifacts {
		if err := d.verifyArtifact(&res.Artifacts[i]); err != nil {
			return fmt.Errorf("artifact %q: %w", res.Artifacts[i].Path, err)
		}
	}
	if res.HandoffRef != nil {
		if err := d.verifyArtifact(res.HandoffRef); err != nil {
			return fmt.Errorf("handoff_ref: %w", err)
		}
	}
	return nil
}

// verifyArtifact checks one manifest entry against the capsule's bytes:
// the path must be capsule-relative (no absolute paths, no parent
// escapes), the file must exist with the declared size and SHA-256.
func (d *Driver) verifyArtifact(a *ArtifactRef) error {
	if a.Path == "" {
		return fmt.Errorf("empty path")
	}
	if filepath.IsAbs(a.Path) || strings.HasPrefix(a.Path, "/") {
		return fmt.Errorf("path must be capsule-relative")
	}
	for _, seg := range strings.Split(a.Path, "/") {
		if seg == ".." {
			return fmt.Errorf("parent escape in path")
		}
	}
	full := filepath.Join(d.wi.CapsuleDir, filepath.FromSlash(a.Path))
	st, err := os.Stat(full)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if a.Bytes > 0 && st.Size() != a.Bytes {
		return fmt.Errorf("size %d, manifest says %d", st.Size(), a.Bytes)
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if a.Bytes > 0 && size != a.Bytes {
		return fmt.Errorf("size %d, manifest says %d", size, a.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("sha256 %s, manifest says %s", got, a.SHA256)
	}
	return nil
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
// the workflow's on_fail declaration (terminate is the default when
// absent) plus the verdict text — the result's output payload in full,
// or a synthesized note when the gate produced none.
func (d *Driver) verdictFor(stepID string, res *StepResult) *GateVerdict {
	v := &GateVerdict{
		GateStepID:  stepID,
		VerdictText: strings.TrimSpace(string(res.Output)),
	}
	if v.VerdictText == "" {
		v.VerdictText = fmt.Sprintf("gate %s FAIL (spawn %d, result %s)", stepID, res.SpawnN, res.ResultID)
	}
	if spec := d.workflow.step(stepID); spec != nil && spec.OnFail != nil {
		v.Action = spec.OnFail.Action
		v.TargetStep = spec.OnFail.Target
		v.IncludeVerdict = spec.OnFail.IncludeVerdict
	}
	if v.Action != "rewind" {
		v.Action = "terminate"
		v.TargetStep = ""
	}
	return v
}

// ---------------------------------------------------------------------------
// Workflow graph (the driver's read of the bundle frontmatter).

// Workflow is the execution graph: the step order IS the declaration
// order (rule V3), plus the rewind bounds (rule V6 defaults 4/2).
type Workflow struct {
	Steps      []StepSpec
	MaxTotal   int
	MaxPerStep int
}

// StepSpec is one workflow step as the driver needs it: identity and
// order, the candidate chain, gate policy and context policy. The
// prompt body and declared artifact INPUTS are the spawn adapter's to
// assemble (spawn.go); this is the sequencing surface.
type StepSpec struct {
	ID              string
	Gate            bool
	ContextPolicy   string // "inherit" or "fresh" (default fresh)
	ModelCandidates []string
	OnFail          *OnFail
	// Handoff marks a step that must produce a handoff artifact for the
	// next inherit step (validated on its completed results).
	Handoff bool
}

// OnFail is the gate FAIL policy (01 §4.1): rewind with a target, or
// terminate (the default).
type OnFail struct {
	Action         string // "rewind" | "terminate"
	Target         string
	IncludeVerdict bool
}

func (w *Workflow) indexOf(stepID string) (int, bool) {
	for i, s := range w.Steps {
		if s.ID == stepID {
			return i, true
		}
	}
	return 0, false
}

func (w *Workflow) step(stepID string) *StepSpec {
	if i, ok := w.indexOf(stepID); ok {
		return &w.Steps[i]
	}
	return nil
}

const (
	defaultMaxRewindTotal   = 4
	defaultMaxRewindPerStep = 2
)

// loadWorkflow reads the materialized bundle's workflow.md frontmatter
// into the driver's sequencing surface. The common/workflow reader
// deliberately exposes only the candidate chain (the F4 lane); the
// driver needs the graph fields too (gate/on_fail/rewind bounds/
// context policy), so it reuses the shared frontmatter SPLITTER and
// parses the block with its own richer (loose, read-only) schema —
// bundle validation stays aihub's job at WI creation.
func loadWorkflow(bundleDir string) (*Workflow, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "workflow.md"))
	if err != nil {
		return nil, fmt.Errorf("driver: bundle workflow.md unreadable: %w", err)
	}
	front, ok := workflow.SplitFrontmatter(raw)
	if !ok {
		return nil, fmt.Errorf("driver: bundle workflow.md has no frontmatter block")
	}
	var doc fmDoc
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, fmt.Errorf("driver: bundle frontmatter unparsable: %w", err)
	}
	wf := &Workflow{MaxTotal: defaultMaxRewindTotal, MaxPerStep: defaultMaxRewindPerStep}
	if doc.Rewind != nil {
		if doc.Rewind.MaxTotal != nil {
			wf.MaxTotal = *doc.Rewind.MaxTotal
		}
		if doc.Rewind.MaxPerStep != nil {
			wf.MaxPerStep = *doc.Rewind.MaxPerStep
		}
	}
	for _, s := range doc.Steps {
		if s.ID == "" {
			return nil, fmt.Errorf("driver: workflow step without an id")
		}
		spec := StepSpec{
			ID:              s.ID,
			Gate:            s.Gate,
			ModelCandidates: s.ModelCandidates,
		}
		if s.ContextPolicy != "" {
			spec.ContextPolicy = s.ContextPolicy
		} else {
			spec.ContextPolicy = "fresh"
		}
		if s.Handoff {
			spec.Handoff = true
		}
		if s.OnFail != nil {
			of := &OnFail{Action: s.OnFail.Action, Target: s.OnFail.Target}
			if s.OnFail.IncludeVerdict == nil || *s.OnFail.IncludeVerdict {
				of.IncludeVerdict = true
			}
			spec.OnFail = of
		}
		if spec.ModelCandidates == nil && doc.Defaults != nil {
			spec.ModelCandidates = doc.Defaults.ModelCandidates
		}
		wf.Steps = append(wf.Steps, spec)
	}
	return wf, nil
}

// The loose frontmatter schema the driver parses (a superset of the
// common/workflow read surface; unknown keys are ignored).
type fmDoc struct {
	Defaults *fmDefaults `yaml:"defaults"`
	Rewind   *fmRewind   `yaml:"rewind"`
	Steps    []fmStep    `yaml:"steps"`
}

type fmDefaults struct {
	ModelCandidates []string `yaml:"model_candidates"`
}

type fmRewind struct {
	MaxTotal   *int `yaml:"max_total"`
	MaxPerStep *int `yaml:"max_per_step"`
}

type fmStep struct {
	ID              string    `yaml:"id"`
	Gate            bool      `yaml:"gate"`
	Handoff         bool      `yaml:"handoff"`
	ContextPolicy   string    `yaml:"context_policy"`
	ModelCandidates []string  `yaml:"model_candidates"`
	OnFail          *fmOnFail `yaml:"on_fail"`
}

type fmOnFail struct {
	Action         string `yaml:"action"`
	Target         string `yaml:"target"`
	IncludeVerdict *bool  `yaml:"include_verdict"`
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
	if exec.WorkflowStatus == "" {
		exec.WorkflowStatus = StatusRunning // normalize the legacy/absent field in memory
	}
	return &exec, true, nil
}

func skeletonExecution(wi WIInfo, wf *Workflow) *capsule.Execution {
	exec := &capsule.Execution{
		CapsuleID:      capsuleIDFor(wi.ID),
		WorkItemID:     wi.ID,
		BundleDigest:   wi.BundleDigest,
		WorkflowStatus: StatusRunning,
		Steps:          map[string]*capsule.Step{},
	}
	for _, spec := range wf.Steps {
		exec.Steps[spec.ID] = &capsule.Step{Status: capsule.StepPending}
	}
	return exec
}

func capsuleIDFor(wi string) string { return "cap_" + wi }

// candidateOrdinal recomputes the effective model-candidate ordinal
// from the attempts[] archive alone (01 §6.3 step 4 — the pointer is
// NOT a persisted field, so a crash-recovered driver derives the same
// ordinal as its predecessor): ordinal = 1 + the count of infra_failure
// entries after the last gate_fail_rewind entry, capped by the list
// length. 1-BASED (ordinal 1 is the first dispatch's candidate).
// listLen <= 0 is the no-candidate fallback: 0, meaning the harness
// adapter's default stays in effect. The core loop itself never
// archives infra failures — that is the recovery integration's write —
// but the recomputation runs on every dispatch so the pointer is
// correct the moment that lane starts feeding it.
func candidateOrdinal(st *capsule.Step, listLen int) int {
	if listLen <= 0 {
		return 0
	}
	// Delegate to the unified fault-class-aware CandidatePointer (R4):
	// provider-class infra failures advance; host/operator/schema do NOT.
	// Build a candidates list of the right length for the pointer formula.
	cands := make([]string, listLen)
	for i := range cands {
		cands[i] = fmt.Sprintf("c%d", i)
	}
	ord := CandidatePointer(attemptsToPointer(st), cands)
	if ord < 0 {
		return 0
	}
	return ord + 1 // CandidatePointer is 0-based; driver ordinal is 1-based
}

// attemptsToPointer converts the capsule attempt archive to the
// candidates.go input shape (fault-class aware).
func attemptsToPointer(st *capsule.Step) []StepAttempt {
	if st == nil {
		return nil
	}
	out := make([]StepAttempt, len(st.Attempts))
	for i, a := range st.Attempts {
		fc := FaultClass(a.FaultClass)
		// Legacy attempts (predating the FaultClass field) carry an empty
		// class on infra failures: default them to provider so the B-chain
		// formula keeps advancing on the historical data.
		if fc == "" && a.Cause == CauseInfraFailure {
			fc = FaultProvider
		}
		out[i] = StepAttempt{Cause: a.Cause, FaultClass: fc}
	}
	return out
}

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
// dispatch (the candidate at the recomputed ordinal; "" when no
// candidate list is in effect). The recomputation runs over the
// EFFECTIVE list (polyforge#79): the post-precedence list the claim
// carried — or the recovery re-derived — never the raw bundle parse
// alone. Rotation semantics are untouched: provider-class infra
// failures alone advance (bounded, capped at list end), a gate-FAIL
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
	ord := candidateOrdinal(st, len(list))
	if ord <= 0 {
		return ""
	}
	return list[ord-1]
}
