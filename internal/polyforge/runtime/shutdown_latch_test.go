package runtime

// shutdown_latch_test.go — the shutdown latch (BeginShutdown) and the
// agent-start tokens (admit): once the latch is down no agent starts, the
// latch waits only for agent starts already under way, and a step parked
// on a hung push or hub call holds neither the latch nor Restart.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// A recovery whose commit-dirty push hangs does not hold the latch, and
// once the push returns it relaunches nothing: the job stays RECOVERING
// (persisted so) for the next daemon start.
func TestShutdownLatchHungRecoveryPushRelaunchesNothing(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	t.Cleanup(r.Close)
	const a = "wi_sl_rec"
	startRunning(t, r, a)
	release := stallPushes(t, r, a)
	ja := r.jobOf(a)
	spawns := sp.callCount()
	recovered := make(chan struct{})
	go func() { r.recoverOp(context.Background(), ja, "agent exited", ""); close(recovered) }()
	waitBusy(t, r, a)

	actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	within(t, promptly, "BeginShutdown during the hung recovery push", func() { r.BeginShutdown(actx) })
	if actx.Err() != nil {
		t.Fatal("BeginShutdown waited out its bound on a recovery push")
	}
	release()
	select {
	case <-recovered:
	case <-time.After(20 * time.Second):
		t.Fatal("the recovery never returned")
	}
	if n := sp.callCount() - spawns; n != 0 {
		t.Fatalf("the recovery relaunched %d agent(s) behind the latch", n)
	}
	if ji, _ := r.Info(a); ji.State != StateRecovering || ji.Agent != nil {
		t.Fatalf("job after the latched recovery = state %s agent %v, want RECOVERING with no agent", ji.State, ji.Agent != nil)
	}
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].State != string(StateRecovering) {
		t.Fatalf("persisted records = %+v, want the recovering record", recs)
	}
}

// An agent start under way when the latch comes down is waited for: once
// BeginShutdown returned its handle is bound (the live set is final), and
// a start after the latch is refused.
func TestShutdownLatchWaitsForAgentStartUnderWay(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	t.Cleanup(r.Close)
	const a, b = "wi_sl_start_a", "wi_sl_start_b"
	t.Cleanup(func() { terminateJobAgent(r, a) })
	inner := r.cfg.AgentStarter
	entered := make(chan struct{})
	proceed := make(chan struct{})
	r.cfg.AgentStarter = func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
		if in.WI == a {
			close(entered)
			<-proceed
		}
		return inner(ctx, in)
	}
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background(), a) }()
	<-entered

	latched := make(chan struct{})
	go func() {
		actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r.BeginShutdown(actx)
		close(latched)
	}()
	select {
	case <-latched:
		t.Fatal("BeginShutdown returned while an agent start was under way")
	case <-time.After(200 * time.Millisecond):
	}
	close(proceed)
	select {
	case <-latched:
	case <-time.After(20 * time.Second):
		t.Fatal("BeginShutdown never returned once the start bound its handle")
	}
	if ji, ok := r.Info(a); !ok || ji.Agent == nil {
		t.Fatalf("after BeginShutdown the started agent is not in the live set (ok=%v)", ok)
	}
	if err := recvErr(t, started, "the Start"); err != nil {
		t.Fatalf("Start under way at the latch: %v", err)
	}
	if err := r.Start(context.Background(), b); !IsClaimDeferred(err) {
		t.Fatalf("Start after the latch = %v, want the deferral", err)
	}
}

// An agent start that never returns does not hold the latch past its
// bound.
func TestShutdownLatchBoundedByContext(t *testing.T) {
	r, _, _ := newTestRuntime(t, newFakeHub(), &scriptedSpawner{})
	if !r.admit() {
		t.Fatal("admit refused before the latch")
	}
	actx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	within(t, promptly, "BeginShutdown with a start that never binds", func() { r.BeginShutdown(actx) })
	if r.admit() {
		t.Fatal("admit granted after the latch")
	}
	r.release()
}

// A shutdown while Restart still waits on a lane step (a slow recovery
// push — local work, which Restart waits for) makes Restart return at
// once, and the step relaunches nothing once the push returns.
func TestShutdownDuringRestartLaneStepReturnsPromptly(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.RestartHubWait = 100 * time.Millisecond
	const a = "wi_sl_lane"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	rec, _ := restartRecord(t, r, hub, a)
	url, release := stallingRemote(t)
	rec.RemoteURL = url
	writeStateFile(t, r, []*jobRecord{rec})
	if err := os.WriteFile(filepath.Join(rec.CapsuleDir, "dirty.txt"), []byte("unflushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Restart(context.Background()) }()
	waitBusy(t, r, a)
	actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	within(t, promptly, "BeginShutdown during the lane's push", func() { r.BeginShutdown(actx) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Restart during the shutdown: %v", err)
		}
	case <-time.After(promptly):
		t.Fatal("Restart kept waiting for the lane step after the shutdown latched")
	}
	release()
	waitFor(t, "the lane to end", func() bool { return !r.isRestarting() })
	if n := sp.callCount(); n != 0 {
		t.Fatalf("the lane relaunched %d agent(s) behind the latch", n)
	}
	if ji, _ := r.Info(a); ji.Agent != nil && ji.Agent.Alive() {
		t.Fatal("a live agent behind the latch")
	}
}

// An agent start that binds after the latch stopped waiting for it is
// torn down by its binder, and the claim is deferred like any the
// shutdown caught: not RUNNING, journal entry kept, no RTStarted.
func TestShutdownLateBoundClaimIsDeferred(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const a = "wi_sl_late_claim"
	inner := r.cfg.AgentStarter
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var h AgentHandle
	r.cfg.AgentStarter = func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
		close(entered)
		<-proceed
		var err error
		h, err = inner(ctx, in)
		return h, err
	}
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background(), a) }()
	<-entered
	actx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if r.BeginShutdown(actx) {
		t.Fatal("BeginShutdown settled with an agent start under way")
	}
	close(proceed)
	if err := recvErr(t, started, "the late Start"); !IsClaimDeferred(err) {
		t.Fatalf("late-bound Start = %v, want the deferral", err)
	}
	if h == nil || h.Alive() {
		t.Fatal("the late-bound agent was not torn down")
	}
	if r.jobOf(a) != nil {
		t.Fatal("the deferred claim left its job")
	}
	if journaled(r, a) == nil {
		t.Fatal("the deferred claim dropped its journal entry (the next start must replay it)")
	}
	for {
		m, ok := toOB.Pop()
		if !ok {
			break
		}
		if m.Kind == protocol.RTStarted {
			t.Fatalf("a late-bound claim announced %+v", m)
		}
	}
}

// A recovery relaunch that binds after the latch stopped waiting is torn
// down: the job stays RECOVERING with no agent and nothing is announced.
func TestShutdownLateBoundRecoveryStaysRecovering(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const a = "wi_sl_late_rec"
	startRunning(t, r, a)
	for {
		if _, ok := toOB.Pop(); !ok {
			break
		}
	}
	inner := r.cfg.AgentStarter
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var h AgentHandle
	r.cfg.AgentStarter = func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
		close(entered)
		<-proceed
		var err error
		h, err = inner(ctx, in)
		return h, err
	}
	done := make(chan struct{})
	go func() { r.recoverOp(context.Background(), r.jobOf(a), "agent exited", ""); close(done) }()
	<-entered
	actx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if r.BeginShutdown(actx) {
		t.Fatal("BeginShutdown settled with a relaunch under way")
	}
	close(proceed)
	<-done
	if h == nil || h.Alive() {
		t.Fatal("the late-bound relaunch was not torn down")
	}
	if ji, _ := r.Info(a); ji.State != StateRecovering || ji.Agent != nil {
		t.Fatalf("job after the late relaunch = %s agent %v, want RECOVERING with no agent", ji.State, ji.Agent != nil)
	}
	if m, ok := toOB.Pop(); ok {
		t.Fatalf("a late-bound relaunch announced %+v", m)
	}
}
