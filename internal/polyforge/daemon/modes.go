package daemon

// modes.go: the auto switch + admission (v2-converge §5), the pending-job
// queue (04 §6.1d) and the daemon's own small state file. The daemon has
// ONE switch (auto on|off) and ONE number (max concurrent jobs): auto
// off = only explicit pf submit; auto on = the daemon also pulls the
// aihub ready queue by itself (the former drain loop). Manual submits
// stay legal either way. The switch changes ONLY how future work is
// sourced; it never touches running jobs and is fully independent from
// the shutting_down latch. Jobs/attempts bookkeeping is NOT here — it
// lives in Runtime's state.json and is never duplicated (04 §4.1 INV-D1).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// Admission error shapes (04 §5.2); the HTTP layer maps them to codes.
var errShuttingDown = errors.New("daemon is shutting down (DAEMON-410)")

// ModeCtl stores the auto switch and admits against the shutting_down
// latch. The latch gates BOTH external submissions and the auto-pull's
// next fetch (04 §6.5 step 2) — two different block paths for the same
// "no new jobs from any source" outcome.
type ModeCtl struct {
	mu           sync.Mutex
	auto         bool
	shuttingDown bool
}

// NewModeCtl starts with auto off.
func NewModeCtl() *ModeCtl { return &ModeCtl{} }

// Auto reports the auto-pull switch.
func (m *ModeCtl) Auto() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.auto
}

// HeartbeatMode is the mode string the machines heartbeat still carries
// for aihub compatibility: "auto" | "manual".
func (m *ModeCtl) HeartbeatMode() string {
	if m.Auto() {
		return protocol.HeartbeatModeAuto
	}
	return protocol.HeartbeatModeManual
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

// SetAuto flips the switch; refused once shutting_down is latched.
func (m *ModeCtl) SetAuto(on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return errShuttingDown
	}
	m.auto = on
	return nil
}

// AdmitSubmit decides a single claim/resume: always legal (also
// while auto is on) except once shutting_down is latched (DAEMON-410).
func (m *ModeCtl) AdmitSubmit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return errShuttingDown
	}
	return nil
}

// AdmitAutoPull decides whether the auto loop may fetch the ready queue:
// only with auto on, never once shutting_down is latched.
func (m *ModeCtl) AdmitAutoPull() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.auto && !m.shuttingDown
}

// ---------------------------------------------------------------------------
// Pending-job queue (04 §6.1d): priority DESC (urgent > high > normal >
// low), FIFO within a priority, no preemption, ordered backfill. One
// queue and one cap shared by manual submits and auto pulls.

var priorityRank = map[string]int{
	"urgent": 0, "high": 1, "normal": 2, "low": 3,
}

func rankOf(priority string) int {
	if r, ok := priorityRank[priority]; ok {
		return r
	}
	return priorityRank["normal"]
}

// qEntry is one pending submit. The JSON tags are the persisted shape
// (submit-queue.json, v2-converge §5): the answered `accepted` must
// survive a daemon restart.
type qEntry struct {
	WorkItemID string    `json:"work_item_id"`
	Priority   string    `json:"priority,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Intent     string    `json:"intent"` // claim | resume
	// SubmittedAt is the wall time the submit was accepted (replay
	// evidence; EnqueuedAt is the FIFO key and equals it for live submits).
	SubmittedAt time.Time `json:"submitted_at"`
	// Origin is the typed submit origin (identity model; chat_id lives in
	// Origin.ChatID). Persisted so a replayed claim hands it to the job
	// record exactly like a live submit.
	Origin runtime.Origin `json:"origin"`
}

type jobQueue struct {
	mu      sync.Mutex
	pending []qEntry
	// inflight holds entries Take()n but not yet Settled: the dispatcher
	// popped them and the claim has not finished. They stay in the
	// persisted file so a crash between the pop and the claim journal
	// cannot lose an accepted submit; replay's aihub check + claiming
	// dedupe make a re-attempt harmless.
	inflight map[string]qEntry
	running  map[string]bool
	maxConc  int
	// persist, when non-nil, atomically rewrites the queue file from the
	// given snapshot (pending + inflight). Called under mu.
	persist func([]qEntry) error
}

func NewJobQueue(maxConc int) *jobQueue {
	return &jobQueue{running: map[string]bool{}, inflight: map[string]qEntry{}, maxConc: maxConc}
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

// snapshotLocked is the persisted view: inflight first (older), then
// pending. Caller holds mu.
func (q *jobQueue) snapshotLocked() []qEntry {
	out := make([]qEntry, 0, len(q.inflight)+len(q.pending))
	for _, e := range q.inflight {
		out = append(out, e)
	}
	out = append(out, q.pending...)
	return out
}

func (q *jobQueue) persistLocked() error {
	if q.persist == nil {
		return nil
	}
	return q.persist(q.snapshotLocked())
}

// Enqueue appends one entry and persists the queue BEFORE returning: the
// caller answers `accepted` only on a nil error, so an accepted submit is
// always on disk. A persist failure rolls the entry back.
func (q *jobQueue) Enqueue(e qEntry) error {
	now := time.Now()
	if e.EnqueuedAt.IsZero() {
		e.EnqueuedAt = now
	}
	if e.SubmittedAt.IsZero() {
		e.SubmittedAt = e.EnqueuedAt
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, e)
	if err := q.persistLocked(); err != nil {
		q.pending = q.pending[:len(q.pending)-1]
		return err
	}
	return nil
}

// Settle ends the inflight state of a Take()n entry (the claim attempt
// finished — success or failure) and persists the dequeue.
func (q *jobQueue) Settle(wi string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.inflight[wi]; !ok {
		return nil
	}
	delete(q.inflight, wi)
	return q.persistLocked()
}

// Flush persists the current state (used after a replay pass that only
// dropped entries).
func (q *jobQueue) Flush() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.persistLocked()
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
	q.inflight[e.WorkItemID] = e // persisted until Settle; a persist error here is non-fatal (the entry is merely re-listed on disk)
	_ = q.persistLocked()
	return &e
}

func (q *jobQueue) MarkRunning(wi string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.running[wi] = true
}

// Reconcile frees the slots of jobs the runtime no longer holds (a
// wrap/pause close ends a job without telling the queue).
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
// The daemon's own state file: auto switch + concurrency only (the 04 §4.1
// shape minus jobs[], which is Runtime's state.json). Same atomic-write
// discipline as every daemon-side state file.

const daemonStateFile = "daemon-state.json"

type daemonState struct {
	Version           int    `json:"version"`
	MachineID         string `json:"machine_id"`
	Auto              bool   `json:"auto"`
	MaxConcurrentJobs int    `json:"max_concurrent_jobs"`
}

func (d *Daemon) statePath() string {
	return filepath.Join(d.cfg.DataDir, daemonStateFile)
}

func (d *Daemon) loadDaemonState() error {
	d.modes.mu.Lock()
	d.modes.auto = false
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
	d.modes.mu.Lock()
	d.modes.auto = st.Auto
	d.modes.mu.Unlock()
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
		Auto:              d.modes.Auto(),
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

// persistModeQuietly lands the auto/max bookkeeping after an ops change;
// a persist failure is loud but never fails the change that already
// happened in memory.
func (d *Daemon) persistModeQuietly() {
	if err := d.persistDaemonState(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: state persist failed: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// Persisted submit queue (v2-converge §5): <DataDir>/submit-queue.json,
// atomic tmp+fsync+rename on every enqueue / dequeue.

const submitQueueFile = "submit-queue.json"

type submitQueueState struct {
	Version int      `json:"version"`
	Entries []qEntry `json:"entries"`
}

func (d *Daemon) queuePath() string {
	return filepath.Join(d.cfg.DataDir, submitQueueFile)
}

func (d *Daemon) persistQueue(entries []qEntry) error {
	blob, err := json.MarshalIndent(&submitQueueState{Version: 1, Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	path := d.queuePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadPersistedQueue reads the previous run's queue file (missing = none).
// An unparsable file is set aside as submit-queue.json.corrupt and
// reported — never silently dropped, never fatal to the start.
func (d *Daemon) loadPersistedQueue() []qEntry {
	raw, err := os.ReadFile(d.queuePath())
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "daemon: %s unreadable: %v\n", submitQueueFile, err)
		}
		return nil
	}
	var st submitQueueState
	if err := json.Unmarshal(raw, &st); err != nil {
		_ = os.Rename(d.queuePath(), d.queuePath()+".corrupt")
		fmt.Fprintf(os.Stderr, "daemon: %s unparsable (set aside as .corrupt): %v\n", submitQueueFile, err)
		return nil
	}
	return st.Entries
}
