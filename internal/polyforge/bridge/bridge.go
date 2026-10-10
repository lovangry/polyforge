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
	//
	// polyforge#114 — the ack's SUBJECT, stated as a warning: this
	// verdict is DRIVER-alive (the daemon-side driver goroutine), not
	// step-process liveness and never step progress. A wedged step
	// process (open turn, no envelope, no exit) blocks stepRun.Wait
	// forever, so doneSet stays false and this branch acks the wedge
	// indefinitely; routing the probe down the step process's own stdio
	// channel does not help either — the wedged process's stdin reader
	// answers while its work goroutine sits parked (the transport echo
	// 04-local-runtime.md §6.2a warns about; reproduced in
	// wi_u0xTEIz0gxi4 experiment E). An ack from here therefore carries
	// NO information about the subject whose silence armed the hang
	// criterion, and the observer must not treat it as unconditional
	// proof of life: the monitor judges progress on its own record-stream
	// witness (StreamHeartbeat.LastProgressAt, which no ack moves) and
	// bounds it with the wall backstop (observe's checkWallBackstop,
	// --wall-window). This branch stays as-is: within the wall bound an
	// ack is the only thing keeping a legitimately slow, record-silent
	// step alive (the pre-#99 false-positive it exists to prevent);
	// past the bound the monitor stops believing it.
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
		// no asynchronous ack to wait for. See the #114 subject warning on
		// SendControl: this ack is driver-alive, never step progress —
		// the observer's wall backstop owns the verdict past the absolute
		// bound.
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
		WI:      wiInfo,
		Spawner: adapter,
		SpawnN:  stepSeed,
		Log:     &stdLogger{},
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

type stdLogger struct{}

func (l *stdLogger) Append(wi, kind string, body interface{}) error {
	fmt.Fprintf(os.Stderr, "bridge/driver: %s: %s: %v\n", wi, kind, body)
	return nil
}
