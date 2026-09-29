package daemon

// modes.go: mode storage/admission (04 §2.2/§6.7; criteria authority
// 00 §3.4), the pending-job queue (04 §6.1d) and the daemon's own small
// state file. The mode field changes ONLY how future requests are
// admitted; it never touches running jobs and is fully independent from
// the shutting_down latch (INV-13: drain-idle is not shutdown, shutdown
// is not drain). Jobs/attempts bookkeeping is NOT here — it lives in
// Runtime's state.json and is never duplicated (04 §4.1 INV-D1).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Admission error shapes (04 §5.2); the HTTP layer maps them to codes.
var (
	errModeConflict = errors.New("request refused by the current mode (DAEMON-409)")
	errShuttingDown = errors.New("daemon is shutting down (DAEMON-410)")
	errBadMode      = errors.New("unknown mode (DAEMON-400)")
)

// ModeCtl stores and admits against the current mode. The shutting_down
// latch gates BOTH external submissions and the drain loop's next wave
// fetch (04 §6.5 step 2) — two different block paths for the same "no
// new jobs from any source" outcome.
type ModeCtl struct {
	mu           sync.Mutex
	mode         string
	shuttingDown bool
}

// NewModeCtl starts in Interactive.
func NewModeCtl() *ModeCtl { return &ModeCtl{mode: protocol.ModeInteractive} }

func (m *ModeCtl) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

func (m *ModeCtl) ShuttingDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shuttingDown
}

// SetShuttingDown latches the flag one-way (04 §6.5 step 1: BEFORE any
// other shutdown action).
func (m *ModeCtl) SetShuttingDown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shuttingDown = true
}

// Set applies a mode transition with 00 §3.4's criteria:
//   - interactive → batch | drain: allowed;
//   - batch → interactive | drain: allowed;
//   - drain → anything except abort-drain's interactive: refused —
//     Drain's only exit is its explicit abort.
func (m *ModeCtl) Set(next string) error {
	switch next {
	case protocol.ModeInteractive, protocol.ModeBatch, protocol.ModeDrain:
	default:
		return errBadMode
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return errShuttingDown
	}
	if m.mode == protocol.ModeDrain && next != protocol.ModeInteractive {
		return errModeConflict
	}
	m.mode = next
	return nil
}

// AbortDrain ends Drain mode without touching anything else (04 §6.1c);
// running jobs are unaffected.
func (m *ModeCtl) AbortDrain() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return errShuttingDown
	}
	if m.mode != protocol.ModeDrain {
		return errModeConflict
	}
	m.mode = protocol.ModeInteractive
	return nil
}

// AdmitSingle decides a single claim/resume/takeover: legal in
// Interactive and Batch; refused in Drain (its internal loop is the only
// creation entry there). Every submission source is refused once
// shutting_down is latched (DAEMON-410).
func (m *ModeCtl) AdmitSingle() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return errShuttingDown
	}
	if m.mode == protocol.ModeDrain {
		return errModeConflict
	}
	return nil
}

// AdmitBatch decides a batch_submit wave: admitted in Batch (its home)
// and Interactive (a wave is just an explicit set of submissions);
// refused in Drain.
func (m *ModeCtl) AdmitBatch() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return errShuttingDown
	}
	if m.mode == protocol.ModeDrain {
		return errModeConflict
	}
	return nil
}

// AdmitDrainWave decides whether the drain loop may fetch its next wave:
// only in drain mode, never once shutting_down is latched (04 §6.1c:
// the loop checks the latch before every fetch — the same latch that
// refuses external callers, reached by a different path).
func (m *ModeCtl) AdmitDrainWave() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode == protocol.ModeDrain && !m.shuttingDown
}

// ---------------------------------------------------------------------------
// Pending-job queue (04 §6.1d): priority DESC (urgent > high > normal >
// low), FIFO within a priority, no preemption, ordered backfill. One
// queue and one cap shared across Interactive, Batch and Drain.

var priorityRank = map[string]int{
	"urgent": 0, "high": 1, "normal": 2, "low": 3,
}

func rankOf(priority string) int {
	if r, ok := priorityRank[priority]; ok {
		return r
	}
	return priorityRank["normal"]
}

type qEntry struct {
	WorkItemID string
	Priority   string
	EnqueuedAt time.Time
	Intent     string // claim | resume | takeover
}

type jobQueue struct {
	mu      sync.Mutex
	pending []qEntry
	running map[string]bool
	maxConc int
}

func NewJobQueue(maxConc int) *jobQueue {
	return &jobQueue{running: map[string]bool{}, maxConc: maxConc}
}

func (q *jobQueue) SetMaxConcurrent(n int) error {
	if n < 1 {
		return fmt.Errorf("max_concurrent_jobs must be >= 1 (DAEMON-400)")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.maxConc = n
	return nil
}

func (q *jobQueue) MaxConcurrent() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.maxConc
}

func (q *jobQueue) Enqueue(e qEntry) {
	if e.EnqueuedAt.IsZero() {
		e.EnqueuedAt = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, e)
}

func (q *jobQueue) IsPending(wi string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.pending {
		if e.WorkItemID == wi {
			return true
		}
	}
	return false
}

func (q *jobQueue) IsRunning(wi string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running[wi]
}

func (q *jobQueue) LenPending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// SlotFree reports whether a spawn slot is open.
func (q *jobQueue) SlotFree() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.running) < q.maxConc
}

// Take pops the next pending entry IF a slot is free (04 §6.1d backfill:
// strictly priority desc, then FIFO). The caller marks the slot via
// MarkRunning after a successful start.
func (q *jobQueue) Take() *qEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 || len(q.running) >= q.maxConc {
		return nil
	}
	best := 0
	for i := 1; i < len(q.pending); i++ {
		a, b := q.pending[i], q.pending[best]
		if rankOf(a.Priority) < rankOf(b.Priority) ||
			(rankOf(a.Priority) == rankOf(b.Priority) && a.EnqueuedAt.Before(b.EnqueuedAt)) {
			best = i
		}
	}
	e := q.pending[best]
	q.pending = append(q.pending[:best], q.pending[best+1:]...)
	return &e
}

func (q *jobQueue) MarkRunning(wi string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.running[wi] = true
}

// Reconcile frees the slots of jobs the runtime no longer holds (a
// wrap/pause/flush-path close ends a job without telling the queue).
func (q *jobQueue) Reconcile(live map[string]bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for wi := range q.running {
		if !live[wi] {
			delete(q.running, wi)
		}
	}
}

// ---------------------------------------------------------------------------
// The daemon's own state file: mode + concurrency only (the 04 §4.1
// shape minus jobs[], which is Runtime's state.json). Same atomic-write
// discipline as every daemon-side state file.

const daemonStateFile = "daemon-state.json"

type daemonState struct {
	Version          int    `json:"version"`
	MachineID        string `json:"machine_id"`
	Mode             string `json:"mode"`
	MaxConcurrentJobs int   `json:"max_concurrent_jobs"`
}

func (d *Daemon) statePath() string {
	return filepath.Join(d.cfg.DataDir, daemonStateFile)
}

func (d *Daemon) loadDaemonState() error {
	d.modes.mu.Lock()
	d.modes.mode = protocol.ModeInteractive
	d.modes.mu.Unlock()
	raw, err := os.ReadFile(d.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first boot
		}
		return err
	}
	var st daemonState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("daemon: %s unparsable: %w", daemonStateFile, err)
	}
	if st.Mode != "" {
		d.modes.mu.Lock()
		d.modes.mode = st.Mode
		d.modes.mu.Unlock()
	}
	if st.MaxConcurrentJobs > 0 {
		if err := d.q.SetMaxConcurrent(st.MaxConcurrentJobs); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) persistDaemonState() error {
	st := daemonState{
		Version:           1,
		MachineID:         d.cfg.MachineID,
		Mode:              d.modes.Current(),
		MaxConcurrentJobs: d.q.MaxConcurrent(),
	}
	blob, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		return err
	}
	path := d.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// persistModeQuietly lands the mode/concurrency bookkeeping after a
// mode operation; a persist failure is loud but never fails the
// transition that already happened in memory.
func (d *Daemon) persistModeQuietly() {
	if err := d.persistDaemonState(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: state persist failed: %v\n", err)
	}
}
