package runtime

// locks.go: the per-work-item state lock. Every operation that changes one
// work item's job — inserting, replacing or dropping it in the job table,
// moving its state — holds that work item's slot; reads of the table and
// of job fields (Info, List, persist's snapshot) take only the job-table
// lock and the job's own mutex, so status and watch never wait on a work
// item's network step.
//
// Lock order: a slot first (at most one at a time), then the I/O leaf
// locks (gitops' per-work-item lock, claimMu, manualMu, persistMu — none
// nested in another), then the job-table lock mu and endMu, then a job's
// mu. Neither of the last two is held across hub, git or disk I/O, an
// EventTap call or a send.

import "sync"

// wiSlot is one work item's state lock. refs counts its holder and its
// waiters (under the job-table lock): the slot leaves the table only when
// nobody holds or waits for it, so one id never maps to two locks.
type wiSlot struct {
	mu   sync.Mutex
	refs int
}

// slotRef answers id's slot, creating it, with one more reference.
func (r *Runtime) slotRef(id string) *wiSlot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slots == nil {
		r.slots = map[string]*wiSlot{}
	}
	s := r.slots[id]
	if s == nil {
		s = &wiSlot{}
		r.slots[id] = s
	}
	s.refs++
	return s
}

// slotUnref drops one reference of id's slot and forgets the slot once
// nobody holds or waits for it.
func (r *Runtime) slotUnref(id string, s *wiSlot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.refs--; s.refs == 0 && r.slots[id] == s {
		delete(r.slots, id)
	}
}

// lockWI takes id's slot, waiting for its holder. For the doors that
// only ever meet short holders (a claim settling, the restart lane).
func (r *Runtime) lockWI(id string) *wiSlot {
	s := r.slotRef(id)
	s.mu.Lock()
	return s
}

// tryLockWI takes id's slot when it is free: false means another
// operation of the work item is under way, and the caller refuses
// instead of queueing behind it.
func (r *Runtime) tryLockWI(id string) (*wiSlot, bool) {
	s := r.slotRef(id)
	if s.mu.TryLock() {
		return s, true
	}
	r.slotUnref(id, s)
	return nil, false
}

// unlockWI releases id's slot taken by lockWI or tryLockWI.
func (r *Runtime) unlockWI(id string, s *wiSlot) {
	s.mu.Unlock()
	r.slotUnref(id, s)
}

// wiLocked reports whether an operation of id holds (or waits for) its
// slot.
func (r *Runtime) wiLocked(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slots[id] != nil
}
