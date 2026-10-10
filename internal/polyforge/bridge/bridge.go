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

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/driver"
	rt "github.com/silgrid/polyforge/internal/polyforge/runtime"
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
	// drv is the underlying Driver (Answer/Rewind/Terminate operator
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
	// journal appends could land AFTER the replacement read JournalMax,
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
func (h *Handle) SendControl(m protocol.ControlMsg) error {
	// Probe semantics for the driver bridge (the #99 verification
	// lesson): the driver engine has no stdio control channel, so the
	// old answer — always-fail — turned every watchdog probe into a
	// false recovery. The supervised liveness of a driver WI IS the
	// driver goroutine itself: a probe arriving while Run is live is
	// acknowledged (the engine is working — the step process's long
	// tool calls legitimately stay record-silent for the whole
	// HangWindow); a probe on a dead driver fails and recovery owns it
	// (onExit reaches the same verdict without the probe anyway).
	if m.Method == protocol.ControlProbe {
		h.mu.Lock()
		alive := !h.doneSet
		h.mu.Unlock()
		if alive {
			return nil
		}
		return fmt.Errorf("bridge: driver not running")
	}
	return fmt.Errorf("bridge: control messages not supported on driver handle")
}

func (h *Handle) WaitControlAck(ctx context.Context, m protocol.ControlMsg, bound time.Duration) error {
	if m.Method == protocol.ControlProbe {
		// the SendControl probe verdict above IS the ack: an accepted
		// probe was answered synchronously (the engine is live); there is
		// no asynchronous ack to wait for.
		return h.SendControl(m)
	}
	return fmt.Errorf("bridge: no control ack channel on driver handle")
}

// Config carries what the bridge needs from the runtime's job.
type Config struct {
	WI            string
	CapsuleDir    string
	BundleName    string
	BundleDigest  string
	SessionHandle string
	SpawnN        int64
	Spawner       rt.Spawner
	// StepCandidates is the polyforge#79 effective per-step candidate
	// map the runtime computed at claim time (or rehydrated/re-derived
	// at recovery); nil = no yaml layer, the bundle's own resolution.
	// Forwarded to the driver verbatim.
	StepCandidates map[string][]string
	// InterruptedCause is the recovery lane's cause string when THIS
	// start is a re-derivation ("" on a fresh claim). Operator-shaped
	// causes archive nothing — a deliberate stop is not a model verdict.
	InterruptedCause string
}

// interruptedFromCause maps the recovery lane's cause onto the driver's
// interrupted-dispatch archive: the cause runs through the driver's
// fault table, and an OPERATOR-classified cause archives NOTHING (an
// interruption is a stop, not a model verdict — the
// attach-after-graceful-shutdown resume keeps today's no-advance
// behavior). Everything else defaults provider-class, exactly the
// bounded side of the policy.
func interruptedFromCause(cause string) *driver.InterruptedDispatch {
	if cause == "" {
		return nil
	}
	fc := driver.ClassifyFault(errors.New(cause))
	if fc == driver.FaultOperator {
		return nil
	}
	return &driver.InterruptedDispatch{FaultClass: fc, Detail: cause}
}

// Start instantiates the driver over the capsule and runs it.
func Start(ctx context.Context, cfg Config) (*Handle, error) {
	if cfg.CapsuleDir == "" || cfg.BundleName == "" {
		return nil, fmt.Errorf("bridge: capsule dir or bundle name empty")
	}

	wiInfo := driver.WIInfo{
		ID:           cfg.WI,
		CapsuleDir:   cfg.CapsuleDir,
		BundleName:   cfg.BundleName,
		BundleDigest: cfg.BundleDigest,
	}

	// Build the step spawn adapter (per-step process derivation).
	adapter, err := driver.NewStepSpawnAdapter(driver.StepSpawnAdapter{
		WI:      wiInfo,
		Spawner: cfg.Spawner,
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
	stepSeed, err := driver.JournalMax(cfg.CapsuleDir)
	if err != nil {
		return nil, fmt.Errorf("bridge: spawn journal unreadable: %w", err)
	}

	drv, err := driver.New(driver.Config{
		WI:             wiInfo,
		Spawner:        adapter,
		SpawnN:         stepSeed,
		Log:            &stdLogger{},
		StepCandidates: cfg.StepCandidates,
		Interrupted:    interruptedFromCause(cfg.InterruptedCause),
	})
	if err != nil {
		return nil, fmt.Errorf("bridge: driver.New: %w", err)
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

type stdLogger struct{}

func (l *stdLogger) Append(wi, kind string, body interface{}) error {
	fmt.Fprintf(os.Stderr, "bridge/driver: %s: %s: %v\n", wi, kind, body)
	return nil
}
