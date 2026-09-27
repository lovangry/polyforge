// Package freeze implements the write-freeze window and the per-WI publish
// mutex of 05-sync-revision-recovery.md §2.7, plus the immutable staging
// area the window's bytes are pinned into.
//
// Two granularities, never to be conflated:
//
//   - WRITE FREEZE: writers stop producing new writes for the duration of
//     scan → digest → manifest build → staging pin. In-flight single atomic
//     writes (the execution.json tmp+fsync+rename) complete naturally.
//   - PUBLISH MUTEX: per-WI, held from freeze acquisition until the whole
//     publish outcome (upload + manifest submit + HEAD handling) is final;
//     other WIs never block on it.
//
// Both are held by the DAEMON, never by the WI Agent (INV-11 / INV-D3).
package freeze

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrQuiesceTimeout marks a forced trigger (step done / interrupted / WI
// outcome) whose writer did not confirm within the bounded wait: an
// infrastructure-class failure — the caller escalates per 00 INV-9 and
// releases the publish mutex.
var ErrQuiesceTimeout = errors.New("freeze: writer did not confirm quiescence in the bounded window")

// ErrMilestoneAbandoned marks an optional milestone whose freeze window
// timed out: the scan is dropped, nothing is escalated, the next real
// trigger reconverges (05 §2.7 timeout handling).
var ErrMilestoneAbandoned = errors.New("freeze: milestone scan abandoned after timeout")

// Quiescer is the WI-Agent coordination face (05 §2.7 acquisition step 2).
type Quiescer interface {
	// RequestQuiesce asks the persistent WI Agent (and through it its step
	// agents) to reach a safe boundary and confirm: no in-flight writes,
	// no new writes will start. Returning nil means confirmed.
	RequestQuiesce(ctx context.Context) error
}

// Conditions describes how a freeze window may be acquired (05 §2.7).
type Conditions struct {
	// Quiescer is the normal confirmation path. Required unless the
	// process-group-dead shortcut applies.
	Quiescer Quiescer
	// ProcessGroupDead reports whether the WI Agent process AND every
	// command child / step agent of its process group have all exited
	// (shortcut condition ①: every known pid confirmed gone, not just the
	// top-level one).
	ProcessGroupDead func() bool
	// ReplacementWriting reports whether a re-derived WI Agent for the same
	// WI has ALREADY written anything to the sync surface (shortcut
	// condition ②'s negation: the shortcut only holds while the replacement
	// has not started writing).
	ReplacementWriting func() bool
	// Timeout bounds the confirmation wait (05 §12 OQ1: configurable).
	Timeout time.Duration
	// Milestone marks an optional trigger: on timeout the scan is abandoned
	// rather than escalated.
	Milestone bool
}

// Manager holds the per-WI publish mutexes.
type Manager struct {
	mu    sync.Mutex
	perWI map[string]*sync.Mutex
}

// NewManager creates a freeze manager.
func NewManager() *Manager {
	return &Manager{perWI: map[string]*sync.Mutex{}}
}

// LockWI acquires the per-WI publish mutex, blocking while another publish
// flow for the same WI is in flight (requests queue, never run concurrently
// — 05 §2.7). The returned release function must be called exactly once
// after the flow's outcome is final.
func (m *Manager) LockWI(wi string) (release func()) {
	m.mu.Lock()
	l, ok := m.perWI[wi]
	if !ok {
		l = &sync.Mutex{}
		m.perWI[wi] = l
	}
	m.mu.Unlock()
	l.Lock()
	var once sync.Once
	return func() {
		once.Do(l.Unlock)
	}
}

// Acquire obtains the write freeze (05 §2.7 acquisition protocol):
//
//  1. If every writer process is dead (condition ①) and no replacement has
//     started writing (condition ②), the freeze is granted directly — no
//     IPC, no confirmation wait; this is stronger than any ack.
//  2. Otherwise the Quiescer is asked and awaited within Conditions.Timeout.
//     On timeout: milestone → ErrMilestoneAbandoned; forced trigger →
//     ErrQuiesceTimeout (infra failure, caller escalates).
//
// Acquire does NOT manage the publish mutex itself: callers take LockWI
// first so freeze + flow are serialized per WI.
func (m *Manager) Acquire(ctx context.Context, wi string, c Conditions) error {
	if c.ProcessGroupDead != nil && c.ProcessGroupDead() {
		if c.ReplacementWriting == nil || !c.ReplacementWriting() {
			return nil // shortcut: no process holds write capability
		}
		// A live replacement has already written: the old-writers-dead
		// judgment no longer holds; fall through to the normal path.
	}
	if c.Quiescer == nil {
		return fmt.Errorf("freeze: no quiescer available for %s and the process-dead shortcut does not apply", wi)
	}
	wait := c.Timeout
	if wait <= 0 {
		wait = 30 * time.Second
	}
	qctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	err := c.Quiescer.RequestQuiesce(qctx)
	if err == nil {
		return nil
	}
	if qctx.Err() == context.DeadlineExceeded {
		if c.Milestone {
			return ErrMilestoneAbandoned
		}
		return ErrQuiesceTimeout
	}
	if c.Milestone {
		// Any other refusal on an optional trigger also just drops the scan.
		return fmt.Errorf("%w: %v", ErrMilestoneAbandoned, err)
	}
	return err
}
