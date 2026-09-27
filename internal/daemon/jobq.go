package daemon

// jobq.go: the pending-job queue (04 §6.1d): priority DESC (urgent > high >
// normal > low), FIFO within a priority (enqueue time), NO preemption, and
// ordered backfill when a running job frees a concurrency slot. One queue
// and one max_concurrent_jobs counter are shared across Interactive, Batch
// and Drain — no per-source exceptions (04 §12 ruling 0).

import (
	"errors"
	"sync"
	"time"
)

// DefaultMaxConcurrentJobs is the cross-mode default (04 §6.1d).
const DefaultMaxConcurrentJobs = 4

// ErrBadConcurrency rejects a non-positive max_concurrent_jobs (DAEMON-400
// shape, 04 §12 ruling 0: n ≥ 1).
var ErrBadConcurrency = errors.New("daemon: max_concurrent_jobs must be ≥ 1")

// Priority ranks; lower value = higher precedence.
var priorityRank = map[string]int{"urgent": 0, "high": 1, "normal": 2, "low": 3}

// RankOf maps a WI priority string to its rank (unknown → normal).
func RankOf(priority string) int {
	r, ok := priorityRank[priority]
	if !ok {
		return priorityRank["normal"]
	}
	return r
}

// QEntry is one queued job intent.
type QEntry struct {
	WorkItemID string
	Priority   string
	EnqueuedAt time.Time
	// Intent: claim | resume | takeover (drives the claim call shape).
	Intent string
}

// JobQueue is the pending queue plus the running-slot counter.
type JobQueue struct {
	mu      sync.Mutex
	pending []QEntry
	running map[string]bool // wiID → running
	maxConc int
}

// NewJobQueue creates a queue with the default concurrency cap.
func NewJobQueue() *JobQueue {
	return &JobQueue{running: map[string]bool{}, maxConc: DefaultMaxConcurrentJobs}
}

// SetMaxConcurrent updates the cap; it never preempts already-running jobs
// (04 §6.1d: the cap only constrains simultaneous SPAWNING).
func (q *JobQueue) SetMaxConcurrent(n int) error {
	if n < 1 {
		return ErrBadConcurrency
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.maxConc = n
	return nil
}

// MaxConcurrent returns the current cap.
func (q *JobQueue) MaxConcurrent() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.maxConc
}

// Enqueue appends a pending entry.
func (q *JobQueue) Enqueue(e QEntry) {
	if e.EnqueuedAt.IsZero() {
		e.EnqueuedAt = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, e)
}

// LenPending reports the pending depth.
func (q *JobQueue) LenPending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// Running reports the running count.
func (q *JobQueue) Running() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.running)
}

// IsRunning reports whether a WI currently holds a slot.
func (q *JobQueue) IsRunning(wi string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running[wi]
}

// Take pops the next pending entry IF a slot is free (04 §6.1d backfill:
// strictly priority desc, then FIFO). Callers mark the slot via MarkRunning
// after a successful spawn (or re-enqueue on failure).
func (q *JobQueue) Take() *QEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 || len(q.running) >= q.maxConc {
		return nil
	}
	// Stable min selection: priority rank, then enqueue time, then
	// insertion order.
	best := 0
	for i := 1; i < len(q.pending); i++ {
		a, b := q.pending[i], q.pending[best]
		if RankOf(a.Priority) < RankOf(b.Priority) ||
			(RankOf(a.Priority) == RankOf(b.Priority) && a.EnqueuedAt.Before(b.EnqueuedAt)) {
			best = i
		}
	}
	e := q.pending[best]
	q.pending = append(q.pending[:best], q.pending[best+1:]...)
	return &e
}

// MarkRunning occupies a slot for a spawned job.
func (q *JobQueue) MarkRunning(wi string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.running[wi] = true
}

// Complete releases a finished job's slot (04 §6.1d: the freed quota is
// backfilled by the next Take — same ordering, no re-confirmation).
func (q *JobQueue) Complete(wi string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.running, wi)
}

// Drop removes a WI from pending (e.g. it became non-claimable).
func (q *JobQueue) Drop(wi string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.pending[:0]
	for _, e := range q.pending {
		if e.WorkItemID != wi {
			out = append(out, e)
		}
	}
	q.pending = out
}
