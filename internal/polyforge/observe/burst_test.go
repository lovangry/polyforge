package observe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// A burst of recovery requests across many work items while nobody
// consumes the OB queue: every request must still be delivered (the
// queue never drops), so no work item stays "recovering" with its
// request lost.
func TestRecoveryBurstNeverDropped(t *testing.T) {
	const n = 300
	rt := supervise.NewQueue[Attach]()
	ob := supervise.NewQueue[protocol.OBMsg]()
	o, err := New(Config{RT: rt, OB: ob, DataDir: t.TempDir(), PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	handles := make([]*fakeHandle, n)
	for i := 0; i < n; i++ {
		wi := fmt.Sprintf("wi_%03d", i)
		dir := filepath.Join(t.TempDir(), "capsule")
		if err := os.MkdirAll(filepath.Join(dir, "runtime-state"), 0o755); err != nil {
			t.Fatal(err)
		}
		handles[i] = newFakeHandle(wi)
		rt.Push(Attach{
			Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: wi, SpawnN: 1},
			Handle: handles[i],
			WI:     WIInfo{WorkItem: wi, Project: "proj", CapsuleDir: dir, BundleName: "b"},
		})
	}
	soon(t, 5*time.Second, "all monitors registered", func() bool {
		for i := 0; i < n; i++ {
			if o.lookup(fmt.Sprintf("wi_%03d", i)) == nil {
				return false
			}
		}
		return true
	})
	for _, h := range handles {
		h.die()
	}
	soon(t, 10*time.Second, "every recovery request queued", func() bool { return ob.Len() >= n })

	seen := map[string]int{}
	for m, ok := ob.Pop(); ok; m, ok = ob.Pop() {
		if m.Kind == protocol.OBNeedsRecovery {
			seen[m.WorkItem]++
		}
	}
	if len(seen) != n {
		t.Fatalf("recovery requests delivered for %d of %d work items", len(seen), n)
	}
	for wi, c := range seen {
		if c != 1 {
			t.Fatalf("%s: %d requests, want exactly 1", wi, c)
		}
	}
}
