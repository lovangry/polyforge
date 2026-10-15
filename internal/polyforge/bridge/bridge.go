package bridge

// bridge.go: connects the daemon's runtime (job/claim/session machinery)
// to the v2 Go driver (internal/polyforge/driver). The driver owns the
// step state machine; this bridge lets the runtime treat a running
// Driver.Run as an AgentHandle for its supervision surface.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/driver"
	rt "github.com/silgrid/polyforge/internal/polyforge/runtime"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// Handle implements rt.AgentHandle over a running Driver.Run.
type Handle struct {
	mu      sync.Mutex
	done    chan error
	doneSet bool
	doneErr error
	cancel  context.CancelFunc
	pid     int
	started string
	// drv is the underlying Driver (Answer/Terminate operator
	// seams); set by Start.
	drv *driver.Driver
}

// Answer delivers an owner answer to the driver's pending question.
func (h *Handle) Answer(questionID, answer, answeredBy string) error {
	if h.drv == nil {
		return fmt.Errorf("bridge: driver not bound")
	}
	return h.drv.Answer(questionID, answer, answeredBy)
}

func (h *Handle) PID() int          { return h.pid }
func (h *Handle) StartedAt() string { return h.started }
func (h *Handle) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.doneSet
}
func (h *Handle) Done() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.doneSet
}
func (h *Handle) Terminate(bound time.Duration) error {
	// The join matters (astra r2 #1): a cancel that merely returns lets
	// the old driver goroutine keep living into the relaunch — its last
	// journal appends could land AFTER the replacement read the journal,
	// so both dispatch the same "fresh" ordinal and the stale-envelope
	// fence is defeated again. Terminate now blocks, bounded, until the
	// driver's Run actually returned (done closes exactly then); the
	// caller (killAgentTree → recoverCore → bridge.Start) therefore
	// reads the journal only once the previous writer is gone.
	h.cancel()
	select {
	case <-h.done:
		h.mu.Lock()
		err := h.doneErr
		h.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-time.After(bound):
		return fmt.Errorf("bridge: driver did not exit within %s", bound)
	}
}

// Config carries what the bridge needs from the runtime's job.
type Config struct {
	WI            string
	CapsuleDir    string
	BundleName    string
	SessionHandle string
	SpawnN        int64
	Spawner       rt.Spawner
	// StepCandidates is the polyforge#79 effective per-step candidate
	// map the runtime computed at claim time (or rehydrated/re-derived
	// at recovery); nil = no yaml layer, the bundle's own resolution.
	// Forwarded to the driver verbatim.
	StepCandidates map[string][]string
	// StepHarness is the polyforge#159 effective per-step harness map
	// (same lifecycle as StepCandidates); nil = the bundle's own choice,
	// else pi. Forwarded to the driver verbatim.
	StepHarness map[string]string
	// InterruptedCause is the recovery lane's cause string when THIS
	// start is a re-derivation ("" on a fresh claim). Operator-shaped
	// causes archive nothing — a deliberate stop is not a model verdict.
	InterruptedCause string
	// ClaudeConfigDir is the step claude processes' CLAUDE_CONFIG_DIR
	// (polyforge#171: <harness-root>/claude-config, hard-set by the
	// spawner) — the claude resume decision locates a previous dispatch's
	// native session explicitly by it; the locator never consults the
	// daemon's process env.
	ClaudeConfigDir string
	// Log receives the driver's events (the daemon's durable per-WI
	// event log); nil = driver events go to stderr only.
	Log driver.EventLogger
	// Link is the WI's supervision link (facts out, verdicts in, harness
	// tracker); nil = unsupervised.
	Link *supervise.Link
	// Limits are the global supervision defaults (X, Y, T).
	Limits supervise.Defaults
}

// causeOperatorShutdown is the recovery lane's cause for a session
// attach after a graceful operator shutdown (runtime/restart.go): an
// interruption is a stop, not a model verdict, so it archives nothing.
const causeOperatorShutdown = "operator daemon shutdown"

// interruptedFromCause maps the recovery lane's cause onto the driver's
// interrupted-dispatch archive. An empty cause or an operator shutdown
// archives nothing; every other cause is archived verbatim as an
// audit-only attempt (it is never model-class, so it never advances the
// candidate pointer).
func interruptedFromCause(cause string) *driver.InterruptedDispatch {
	if cause == "" || cause == causeOperatorShutdown {
		return nil
	}
	return &driver.InterruptedDispatch{Detail: cause}
}

// Start instantiates the driver over the capsule and runs it.
func Start(ctx context.Context, cfg Config) (*Handle, error) {
	if cfg.CapsuleDir == "" || cfg.BundleName == "" {
		return nil, fmt.Errorf("bridge: capsule dir or bundle name empty")
	}

	wiInfo := driver.WIInfo{
		ID:         cfg.WI,
		CapsuleDir: cfg.CapsuleDir,
		BundleName: cfg.BundleName,
	}

	// Build the step spawn adapter (per-step process derivation).
	adapter, err := driver.NewStepSpawnAdapter(driver.StepSpawnAdapter{
		WI:              wiInfo,
		Spawner:         cfg.Spawner,
		Link:            cfg.Link,
		ClaudeConfigDir: cfg.ClaudeConfigDir,
	})
	if err != nil {
		return nil, fmt.Errorf("bridge: adapter: %w", err)
	}

	// The driver's STEP-ordinal seed is the capsule journal max (the
	// driver-owned derivation ledger), NOT the job's relaunch ordinal:
	// step ordinals are the observable contract (drop paths, archive
	// dirs, the e2e fakepi's action indexing — all 1-based for a fresh
	// capsule), while cfg.SpawnN (≥1 since the observe-lane fix) is the
	// job ordinal the RTStarted emission needs. A fresh capsule seeds 0
	// → first dispatch is ordinal 1; a relaunch seeds past every ordinal
	// the predecessor's dispatches used, so stale drops stay fenced.
	// A journal read failure other than absence refuses the start — the
	// ledger is the fence's source of truth, never silently fresh.
	stepSeed, err := capsule.SpawnJournalMaxN(cfg.CapsuleDir)
	if err != nil {
		return nil, fmt.Errorf("bridge: spawn journal unreadable: %w", err)
	}

	if cfg.Link != nil {
		cfg.Link.Reset() // a new driver epoch: no fence or verdict of a predecessor may leak in
	}
	drv, err := driver.New(driver.Config{
		WI:             wiInfo,
		BundleDir:      adapter.BundleDir,
		Workflow:       adapter.Workflow, // parsed once, shared with the adapter
		Spawner:        adapter,
		SpawnN:         int64(stepSeed),
		Log:            cfg.Log,
		Link:           cfg.Link,
		Limits:         cfg.Limits,
		StepCandidates: cfg.StepCandidates,
		StepHarness:    cfg.StepHarness,
		Interrupted:    interruptedFromCause(cfg.InterruptedCause),
	})
	if err != nil {
		return nil, fmt.Errorf("bridge: driver.New: %w", err)
	}

	// The §6.4 owner move on the claim/start path (polyforge#116): a
	// capsule that arrives BLOCKED (the parked §6.5-step-5 verdict —
	// chain-durable since the bounded close seals it) must not re-wedge
	// the fresh driver: Run would read blocked and exit at once (the
	// bounded close again, forever). The owner's resume verdict unblocks
	// the workflow; the loop's own dispatch then re-dispatches the frozen
	// in_progress step in place as a fresh derivation (the parked journal
	// and prior step output survive — the resumed lane continues the
	// parked capsule, never a pre-blocked re-derivation). The call is the
	// owner's verdict exactly because no live loop can leave the blocked
	// state (Run exits on it): every claim of a parked capsule arrives
	// here. ResumeCurrent's blocked-only guard makes the call refuse on
	// any other capsule state; a parked capsule without a current step is
	// the error surfaced below (the D5 compensation closes the claim).
	if drv.Snapshot().WorkflowStatus == driver.StatusBlocked {
		if err := drv.ResumeCurrent(); err != nil {
			return nil, fmt.Errorf("bridge: owner resume of the parked workflow: %w", err)
		}
	}

	dctx, cancel := context.WithCancel(ctx)
	h := &Handle{
		done:    make(chan error),
		cancel:  cancel,
		pid:     os.Getpid(),
		started: fmt.Sprintf("driver-%d", cfg.SpawnN+1),
	}

	h.drv = drv
	go func() {
		err := drv.Run(dctx)
		h.mu.Lock()
		h.doneSet = true
		h.doneErr = err
		close(h.done)
		h.mu.Unlock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bridge: %s: driver.Run: %v\n", cfg.WI, err)
		}
	}()

	// Advance the spawn counter (the driver derivation is ordinal N+1).
	_ = drv // the runtime advances its own counter via the job

	return h, nil
}
