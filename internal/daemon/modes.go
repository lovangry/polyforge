package daemon

// modes.go: mode storage and admission (04 §2.2/§6.7; criteria authority
// 00 §3.4). The mode field changes ONLY how future requests are admitted;
// it never touches running jobs and is fully independent from the
// shutting_down flag (INV-13: drain-idle is not shutdown and shutdown is
// not drain).

import (
	"errors"
	"sync"
)

// Modes.
const (
	ModeInteractive = "interactive"
	ModeBatch       = "batch"
	ModeDrain       = "drain"
)

// Admission error shapes (04 §5.2).
var (
	// ErrModeConflict is the DAEMON-409 shape: the request is refused by
	// mode exclusion (e.g. a single claim during Drain — Drain's internal
	// loop is the only job-creation entry in that mode).
	ErrModeConflict = errors.New("daemon: request refused by the current mode")
	// ErrShuttingDown is the DAEMON-410 shape: graceful shutdown rejects
	// every external submission source.
	ErrShuttingDown = errors.New("daemon: shutting down")
	// ErrBadMode is the DAEMON-400 shape for unknown mode values.
	ErrBadMode = errors.New("daemon: unknown mode")
)

// ModeCtl stores and admits against the current mode. The shutting_down
// flag lives beside it and gates BOTH external submissions and the Drain
// loop's next wave fetch (04 §6.5 step 2) — two different block paths for
// the same "no new jobs from any source" outcome.
type ModeCtl struct {
	mu           sync.Mutex
	mode         string
	shuttingDown bool
}

// NewModeCtl starts in Interactive.
func NewModeCtl() *ModeCtl { return &ModeCtl{mode: ModeInteractive} }

// Current returns the active mode.
func (m *ModeCtl) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// ShuttingDown reports the graceful-shutdown flag.
func (m *ModeCtl) ShuttingDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shuttingDown
}

// SetShuttingDown latches the flag (one-way, 04 §6.5 step 1: it is set
// BEFORE any other shutdown action). The flag is read from the dispatcher
// loop, the Drain wave fetch and every external admission check, so the
// latch itself must be race-free.
func (m *ModeCtl) SetShuttingDown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shuttingDown = true
}

// Set applies a mode transition with 00 §3.4's criteria:
//   - interactive → batch | drain: allowed.
//   - batch → interactive | drain: allowed.
//   - drain → anything except abort-drain: refused (Drain's only exit is
//     its explicit abort, or graceful shutdown via the flag, which is a
//     different dimension entirely).
//
// The abort-drain request is expressed as Set(ModeInteractive) while the
// current mode is drain; the daemon maps it here.
func (m *ModeCtl) Set(next string) error {
	switch next {
	case ModeInteractive, ModeBatch, ModeDrain:
	default:
		return ErrBadMode
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return ErrShuttingDown
	}
	if m.mode == ModeDrain && next != ModeInteractive {
		return ErrModeConflict
	}
	m.mode = next
	return nil
}

// AbortDrain ends Drain mode without touching anything else
// (mode(abort-drain), 04 §6.1c): running jobs are unaffected.
func (m *ModeCtl) AbortDrain() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode != ModeDrain {
		return ErrModeConflict
	}
	m.mode = ModeInteractive
	return nil
}

// AdmitSingle decides a single claim/resume/takeover request: legal in
// Interactive and Batch; refused in Drain (its internal loop is the only
// creation entry there). Every submission source is refused once
// shutting_down is latched.
func (m *ModeCtl) AdmitSingle() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return ErrShuttingDown
	}
	if m.mode == ModeDrain {
		return ErrModeConflict
	}
	return nil
}

// AdmitBatch decides a batch_submit wave: admitted in Batch (its home) and
// Interactive (a wave is just an explicit set of submissions); refused in
// Drain.
func (m *ModeCtl) AdmitBatch() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return ErrShuttingDown
	}
	if m.mode == ModeDrain {
		return ErrModeConflict
	}
	return nil
}

// AdmitDrainWave decides whether the Drain loop may fetch its next wave:
// only in drain mode and never once shutting_down is latched (04 §6.1c:
// the loop checks the flag before every fetch — the same flag that
// refuses external callers, reached by a different path).
func (m *ModeCtl) AdmitDrainWave() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode == ModeDrain && !m.shuttingDown
}
