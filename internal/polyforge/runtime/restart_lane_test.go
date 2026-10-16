package runtime

// restart_lane_test.go — every hub call the restart reconciliation lane
// makes is one Restart's wait bounds (a close's SessionEnd, a blocked
// close, the work-item lookup of a recovery included), only the lane's
// own: a deferred verify after the lane ended is not timed; and each job
// brought to life is handed to observe exactly once — by the daemon's
// startup pass when it was live as Restart returned, by the runtime
// otherwise.

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// A lane step parked on any of its hub calls lets Restart return after
// the bounded wait.
func TestRestartWaitBoundsEveryLaneHubCall(t *testing.T) {
	cases := []struct {
		name string
		kind string // the parked hub call
		prep func(t *testing.T, r *Runtime, hub *fakeHub, wi string) *jobRecord
	}{
		{"seal-session-end", "end", func(t *testing.T, r *Runtime, hub *fakeHub, wi string) *jobRecord {
			rec, _ := restartRecord(t, r, hub, wi)
			rec.State, rec.StopReason, rec.SealedPush = string(StateSealing), protocol.FinishWrapped, true
			return rec
		}},
		{"blocked-close", "end", func(t *testing.T, r *Runtime, hub *fakeHub, wi string) *jobRecord {
			rec := matrixRecord(r, wi, 0, "") // its capsule is missing: the record is blocked
			hub.hold(wi, rec.SessionID, api.StatusRunning)
			return rec
		}},
		{"recovery-work-item-lookup", "probe", func(t *testing.T, r *Runtime, hub *fakeHub, wi string) *jobRecord {
			rec, _ := restartRecord(t, r, hub, wi) // no work-item.json in its capsule
			return rec
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setGitIdentity(t)
			hub := newFakeHub()
			sp := &scriptedSpawner{}
			r, _, _ := newTestRuntime(t, hub, sp)
			r.cfg.RestartHubWait = 200 * time.Millisecond
			const wi = "wi_rl_bound"
			t.Cleanup(func() { r.Close(); terminateJobAgent(r, wi) })
			rec := c.prep(t, r, hub, wi)
			writeStateFile(t, r, []*jobRecord{rec})
			g := hub.hangCall(t, c.kind, wi)
			within(t, promptly, "Restart with the lane's "+c.kind+" hung", func() {
				if err := r.Restart(context.Background()); err != nil {
					t.Errorf("Restart: %v", err)
				}
			})
			g.waitEntered(t, "the lane's "+c.kind)
			if !r.isRestarting() {
				t.Fatal("Restart returned with the lane finished; the hub call was never parked")
			}
			g.open()
			waitFor(t, "the lane to end", func() bool { return !r.isRestarting() })
		})
	}
}

// A deferred verify the lane scheduled runs after the lane ended: the
// attach it leads to is not the lane's hub call, so it never marks one
// outstanding.
func TestDeferredVerifyAttachIsNotALaneHubCall(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.EndRetryBase = 10 * time.Millisecond
	r.cfg.EndRetryMax = 20 * time.Millisecond
	const a = "wi_rl_dv"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	rec := pausedAttachRecord(t, r, hub, a)
	writeStateFile(t, r, []*jobRecord{rec})
	hub.lookupErr[a] = errLookupBoom
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	g := hub.hangCall(t, "attach", a)
	hub.mu.Lock()
	delete(hub.lookupErr, a)
	hub.mu.Unlock()
	g.waitEntered(t, "the deferred verify's attach")
	r.mu.Lock()
	since := r.laneHubSince
	r.mu.Unlock()
	if !since.IsZero() {
		t.Fatal("the attach of a deferred verify after the lane ended was timed as a lane hub call")
	}
	g.open()
	waitFor(t, "the deferred work item to resume", func() bool {
		ji, ok := r.Info(a)
		return ok && ji.State == StateRunning && ji.Agent != nil
	})
}

// A job live when Restart returned is the startup pass's to attach (the
// runtime does not announce it); a job a deferred verify brings to life
// afterwards is announced by the runtime once, and is not the pass's.
func TestRestartAttachAndDeferredVerifyAnnounceOnce(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	r.cfg.EndRetryBase = 50 * time.Millisecond
	r.cfg.EndRetryMax = 50 * time.Millisecond
	const live, deferred = "wi_rl_live", "wi_rl_deferred"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, live); terminateJobAgent(r, deferred) })
	recL, _ := restartRecord(t, r, hub, live)
	recD, _ := restartRecord(t, r, hub, deferred)
	writeStateFile(t, r, []*jobRecord{recL, recD})
	hub.lookupErr[deferred] = errLookupBoom
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !r.RestartAttach(live) {
		t.Fatalf("%s was live as Restart returned: the startup pass attaches it", live)
	}
	if r.RestartAttach(deferred) {
		t.Fatalf("%s was not live as Restart returned: the startup pass must leave it to the runtime", deferred)
	}
	hub.mu.Lock()
	delete(hub.lookupErr, deferred)
	hub.mu.Unlock()
	waitFor(t, "the deferred work item to resume", func() bool {
		ji, ok := r.Info(deferred)
		return ok && ji.State == StateRunning && ji.Agent != nil
	})
	time.Sleep(200 * time.Millisecond) // any duplicate announcement lands meanwhile
	started := map[string]int{}
	for {
		m, ok := toOB.Pop()
		if !ok {
			break
		}
		if m.Kind == protocol.RTStarted {
			started[m.WorkItem]++
		}
	}
	if started[live] != 0 || started[deferred] != 1 {
		t.Fatalf("RTStarted announcements = %v, want none for %s (the startup pass's) and one for %s", started, live, deferred)
	}
}

// A background retry of a work item parked on aihub with the work item's
// lock held (a claim-end close here) does not hold Restart past its
// bound: the lane's wait for that lock — seeding the record, then
// reconciling it — counts as one of its hub calls.
func TestRestartBoundCoversLaneLockWaitBehindHungRetry(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.RestartHubWait = 200 * time.Millisecond
	r.cfg.EndRetryBase = 10 * time.Millisecond
	r.cfg.EndRetryMax = 20 * time.Millisecond
	const x = "wi_rl_retry"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, x) })
	rec, _ := restartRecord(t, r, hub, x)
	writeStateFile(t, r, []*jobRecord{rec})
	g := hub.hangCall(t, "end", x)
	r.retryClaimEnd(x, x, fakeSession(x+"-earlier"), "an earlier claim's compensation")
	g.waitEntered(t, "the claim-end retry")
	if !r.Busy(x) {
		t.Fatal("the parked retry does not hold the work item's lock")
	}
	within(t, promptly, "Restart behind the parked retry", func() {
		if err := r.Restart(context.Background()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	if !r.isRestarting() {
		t.Fatal("Restart returned with its lane finished; it never waited behind the retry")
	}
	g.open()
	waitFor(t, "the lane to end", func() bool { return !r.isRestarting() })
}
