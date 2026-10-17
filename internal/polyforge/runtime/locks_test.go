package runtime

// locks_test.go — the per-work-item state lock: one lock per id, never
// two, no slot left behind once nobody holds or waits for it.

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func slotCount(r *Runtime) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.slots)
}

func TestWISlotReleasedLeavesNothing(t *testing.T) {
	r, _, _ := newTestRuntime(t, newFakeHub(), &scriptedSpawner{})
	s := r.lockWI("wi_l_a")
	if !r.wiLocked("wi_l_a") {
		t.Fatal("a held slot is not reported locked")
	}
	if _, ok := r.tryLockWI("wi_l_a"); ok {
		t.Fatal("tryLockWI took a held slot")
	}
	if n := slotCount(r); n != 1 {
		t.Fatalf("a failed tryLockWI left %d slots, want 1", n)
	}
	o, ok := r.tryLockWI("wi_l_b")
	if !ok {
		t.Fatal("tryLockWI refused another work item's free slot")
	}
	r.unlockWI("wi_l_b", o)
	r.unlockWI("wi_l_a", s)
	if n := slotCount(r); n != 0 || r.wiLocked("wi_l_a") {
		t.Fatalf("released slots left %d entries behind", n)
	}
}

// A waiter keeps the slot alive: the holder's release hands the same
// lock over instead of a second lock being minted for the id.
func TestWISlotWaiterKeepsTheSameLock(t *testing.T) {
	r, _, _ := newTestRuntime(t, newFakeHub(), &scriptedSpawner{})
	s := r.lockWI("wi_l_w")
	got := make(chan *wiSlot, 1)
	go func() { got <- r.lockWI("wi_l_w") }()
	waitFor(t, "the waiter to reference the slot", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.slots["wi_l_w"] != nil && r.slots["wi_l_w"].refs == 2
	})
	r.unlockWI("wi_l_w", s)
	w := <-got
	if w != s {
		t.Fatal("the waiter got a second lock for the same id")
	}
	if _, ok := r.tryLockWI("wi_l_w"); ok {
		t.Fatal("tryLockWI took the slot the waiter now holds")
	}
	r.unlockWI("wi_l_w", w)
	if n := slotCount(r); n != 0 {
		t.Fatalf("%d slots left", n)
	}
}

// Random lock / tryLock / unlock across a few ids from many goroutines:
// mutual exclusion holds per id (one holder at a time) and nothing is
// left in the table at the end.
func TestWISlotRandomizedExclusion(t *testing.T) {
	r, _, _ := newTestRuntime(t, newFakeHub(), &scriptedSpawner{})
	const ids, workers, rounds = 4, 16, 10000 / 16
	var holders [ids]atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < rounds; i++ {
				k := rng.Intn(ids)
				id := fmt.Sprintf("wi_l_r%d", k)
				var s *wiSlot
				if rng.Intn(2) == 0 {
					s = r.lockWI(id)
				} else {
					var ok bool
					if s, ok = r.tryLockWI(id); !ok {
						continue
					}
				}
				if n := holders[k].Add(1); n != 1 {
					t.Errorf("%s held by %d operations at once", id, n)
				}
				if rng.Intn(8) == 0 {
					time.Sleep(time.Microsecond)
				}
				holders[k].Add(-1)
				r.unlockWI(id, s)
			}
		}(int64(w))
	}
	wg.Wait()
	if n := slotCount(r); n != 0 {
		t.Fatalf("%d slots left after every holder released", n)
	}
}
