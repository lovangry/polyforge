package runtime

// NEW-2 unit coverage for the release lane: the bounded re-arm that keeps
// an observe release (RTFinished) alive across a full observe channel.
// The end-to-end supersede/pause variants live in fullchan_release_test.go;
// these exercise the lane's own bounds — deferred delivery, the retry
// window, the shutdown drop, and the queue cap.

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// newLaneRuntime builds a Runtime with a capacity-1 observe channel and
// short bounds, plus a filler message occupying the slot so the next
// send must defer to the lane.
func newLaneRuntime(t *testing.T, sendBound, retryWindow time.Duration) (*Runtime, chan protocol.RTMsg) {
	t.Helper()
	cfg := Config{
		MachineID:          "m-lane",
		Workspace:          filepath.Join(t.TempDir(), "ws"),
		DataDir:            filepath.Join(t.TempDir(), "data"),
		MaxResumeFails:     2,
		MaxRecoveries:      2,
		SendBound:          sendBound,
		ReleaseRetryWindow: retryWindow,
	}
	toOB := make(chan protocol.RTMsg, 1)
	fromOB := make(chan protocol.OBMsg, 1)
	r := New(cfg, newFakeHub(), &scriptedSpawner{}, toOB, fromOB)
	toOB <- protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_filler"}
	return r, toOB
}

// drainLaneFiller removes the filler so the lane's parked release can be
// observed.
func drainLaneFiller(t *testing.T, toOB chan protocol.RTMsg) {
	t.Helper()
	select {
	case m := <-toOB:
		if m.WorkItem != "wi_filler" {
			t.Fatalf("expected the filler message, got %+v", m)
		}
	default:
		t.Fatal("precondition: the filler must still be in the channel")
	}
}

// TestReleaseLaneDeliversAfterRoomAppears: a release whose first bounded
// send expires on the full channel is parked on the lane (the caller
// returns after SendBound, never wedged) and delivered the moment the
// channel has room again — including with several releases stacked.
func TestReleaseLaneDeliversAfterRoomAppears(t *testing.T) {
	r, toOB := newLaneRuntime(t, 20*time.Millisecond, 5*time.Second)

	var wg sync.WaitGroup
	for _, wi := range []string{"wi_lane_a", "wi_lane_b"} {
		wg.Add(1)
		go func(wi string) {
			defer wg.Done()
			r.sendRelease(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: wi, Reason: protocol.FinishSuperseded})
		}(wi)
	}
	// The close callers must return after SendBound even though the
	// channel stays full — a wedged Observe never wedges the Runtime.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendRelease wedged the caller past SendBound")
	}

	// Make room: both deferred releases must arrive.
	drainLaneFiller(t, toOB)
	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case m := <-toOB:
			if m.Kind != protocol.RTFinished || m.Reason != protocol.FinishSuperseded {
				t.Fatalf("unexpected message %+v", m)
			}
			got[m.WorkItem] = true
		case <-deadline:
			t.Fatalf("deferred releases never re-armed; got %v", got)
		}
	}
}

// TestReleaseLaneDropsAfterRetryWindow: the lane is bounded — a release
// the channel cannot take within ReleaseRetryWindow is dropped for good
// instead of parked forever (a wedged Observe must not wedge the lane).
func TestReleaseLaneDropsAfterRetryWindow(t *testing.T) {
	r, toOB := newLaneRuntime(t, 10*time.Millisecond, 50*time.Millisecond)

	r.sendRelease(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: "wi_wedge", Reason: protocol.FinishPaused})
	// Past the window the drop has certainly happened (the lane's
	// deadline fires while the channel is still full).
	time.Sleep(5 * r.cfg.ReleaseRetryWindow)
	drainLaneFiller(t, toOB)
	select {
	case m := <-toOB:
		t.Fatalf("release outlived its retry window: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestReleaseLaneCloseAbandonsPending: Close (the daemon shutdown hook)
// stops the lane immediately — a pending release is abandoned rather
// than parked past the shutdown — and Close is idempotent and a no-op on
// a lane that was never built.
func TestReleaseLaneCloseAbandonsPending(t *testing.T) {
	r, toOB := newLaneRuntime(t, 10*time.Millisecond, time.Minute)

	// No lane yet: Close is a no-op.
	r.Close()
	r.Close()

	r.sendRelease(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: "wi_shutdown", Reason: protocol.FinishPaused})

	r.Close()
	r.relMu.Lock()
	select {
	case <-r.relDone:
	default:
		t.Fatal("Close must close the lane's done signal")
	}
	r.relMu.Unlock()
	r.Close() // idempotent, no panic

	// The parked release is abandoned: room appears, nothing arrives —
	// promptly, not after the (minute-long) retry window.
	drainLaneFiller(t, toOB)
	select {
	case m := <-toOB:
		t.Fatalf("release survived Close: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestReleaseLaneQueueCapIsBounded: a lane at capacity drops overflow
// loudly instead of growing without bound (releaseQueueCap releases
// armed behind a wedged channel).
func TestReleaseLaneQueueCapIsBounded(t *testing.T) {
	r, toOB := newLaneRuntime(t, 2*time.Millisecond, time.Minute)

	for i := 0; i < releaseQueueCap+50; i++ {
		r.sendRelease(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: fmt.Sprintf("wi_q_%d", i), Reason: protocol.FinishSuperseded})
	}
	r.relMu.Lock()
	pending := len(r.relQueue)
	r.relMu.Unlock()
	if pending != releaseQueueCap {
		t.Fatalf("lane queue = %d, want the %d cap", pending, releaseQueueCap)
	}
	// The lane goroutine must still be alive and delivering: drain the
	// filler and require the first parked release to come through.
	drainLaneFiller(t, toOB)
	select {
	case m := <-toOB:
		if m.Kind != protocol.RTFinished || m.WorkItem != "wi_q_0" {
			t.Fatalf("unexpected lane delivery %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lane stopped delivering after queue overflow")
	}
}
