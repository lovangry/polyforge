package runtime

// b8_close_outside_opmu_test.go — repo review 2026-10-16 B-8, the
// remainder: the closes' and the recovery's network steps (chain push,
// task-repo WIP, SessionEnd), a failed start's compensation and the
// restart's journal-replay clone run outside the runtime's opMu, so one
// work item's hung push no longer freezes the stop, pause, resume and
// wrap of the others.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// stallingRemote is hangingRemote with an explicit release: the push of
// a job pointed at it hangs until release() closes the listener and
// every connection (later pushes then fail at once).
func stallingRemote(t *testing.T) (url string, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	var once sync.Once
	release = func() {
		once.Do(func() {
			ln.Close()
			mu.Lock()
			for _, c := range conns {
				c.Close()
			}
			mu.Unlock()
		})
	}
	t.Cleanup(release)
	return "http://" + ln.Addr().String() + "/capsule.git", release
}

// startRunning starts wi to RUNNING (its agent is collected at cleanup).
// Safe off the test goroutine: a failure is reported with Errorf.
func startRunning(t *testing.T, r *Runtime, wi string) {
	t.Helper()
	t.Cleanup(func() { terminateJobAgent(r, wi) })
	if err := r.Start(context.Background(), wi); err != nil {
		t.Errorf("Start(%s): %v", wi, err)
	}
}

// stallPushes points wi's chain push at a stalling remote.
func stallPushes(t *testing.T, r *Runtime, wi string) (release func()) {
	t.Helper()
	url, release := stallingRemote(t)
	j := r.jobOf(wi)
	if j == nil {
		t.Fatalf("%s: no job", wi)
	}
	j.mu.Lock()
	j.git.RemoteURL = url
	j.mu.Unlock()
	return release
}

// waitBusy blocks until wi's network step runs outside opMu.
func waitBusy(t *testing.T, r *Runtime, wi string) {
	t.Helper()
	waitFor(t, wi+" to reach its off-lock network step", func() bool {
		j := r.jobOf(wi)
		return j != nil && j.isBusy()
	})
}

func isConflict(err error) bool {
	ae, ok := err.(*api.Error)
	return ok && ae.Code == api.ErrDaemonConflict
}

// A wrap whose seal push hangs on work item A leaves B fully operable:
// B pauses, resumes and wraps promptly while A is still pushing; a
// second close or a resume of A is refused (409) instead of queueing
// behind it; once the push fails, A's close finishes (degraded to
// blocked, the kept semantics) and its record is gone.
func TestB8HungSealPushLeavesOtherWorkItemsResponsive(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const a, b = "wi_b8r_a", "wi_b8r_b"
	ctx := context.Background()
	startRunning(t, r, a)
	startRunning(t, r, b)
	release := stallPushes(t, r, a)
	sessA := r.jobOf(a).sessionID

	stopA := make(chan error, 1)
	go func() { stopA <- r.Stop(ctx, a, protocol.FinishWrapped) }()
	waitBusy(t, r, a)
	if ji, _ := r.Info(a); ji.State != StateSealing {
		t.Fatalf("A during its push: %s, want sealing", ji.State)
	}

	within(t, 20*time.Second, "pause of B", func() {
		if err := r.Stop(ctx, b, protocol.FinishPaused); err != nil {
			t.Errorf("pause B: %v", err)
		}
	})
	hub.mu.Lock()
	hub.cleanPause[b] = false // the operator's pf resume PATCH
	hub.mu.Unlock()
	within(t, 20*time.Second, "resume of B", func() {
		if out, err := r.ResumePaused(ctx, b); err != nil || out != ResumeAttached {
			t.Errorf("ResumePaused(B) = %q, %v", out, err)
		}
	})
	within(t, 20*time.Second, "wrap of B", func() {
		if err := r.Stop(ctx, b, protocol.FinishWrapped); err != nil {
			t.Errorf("wrap B: %v", err)
		}
	})
	if r.jobOf(b) != nil {
		t.Fatal("B's wrap left its record")
	}

	within(t, 5*time.Second, "a second close of A", func() {
		if err := r.Stop(ctx, a, protocol.FinishFailed); !isConflict(err) {
			t.Errorf("second close of A during its push = %v, want DAEMON-409", err)
		}
	})
	within(t, 5*time.Second, "a resume of A", func() {
		if _, err := r.ResumePaused(ctx, a); !isConflict(err) {
			t.Errorf("resume of A during its push = %v, want DAEMON-409", err)
		}
	})
	select {
	case err := <-stopA:
		t.Fatalf("A's close returned while its push hangs: %v", err)
	default:
	}

	release()
	if err := recvErr(t, stopA, "A's close"); err != nil {
		t.Fatalf("A's close: %v", err)
	}
	ends := endsOf(hub, a, sessA)
	if len(ends) != 1 || ends[0].Status != api.StatusBlocked {
		t.Fatalf("A's session ends = %+v, want one blocked (the failed seal push degrades the wrap)", ends)
	}
	if r.jobOf(a) != nil {
		t.Fatal("A's close left its record")
	}
}

// A pause whose push hangs latches only its own work item.
func TestB8HungPausePushLeavesOtherWorkItemsResponsive(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const a, b = "wi_b8p_a", "wi_b8p_b"
	ctx := context.Background()
	startRunning(t, r, a)
	startRunning(t, r, b)
	release := stallPushes(t, r, a)

	pauseA := make(chan error, 1)
	go func() { pauseA <- r.Stop(ctx, a, protocol.FinishPaused) }()
	waitBusy(t, r, a)
	within(t, 20*time.Second, "wrap of B", func() {
		if err := r.Stop(ctx, b, protocol.FinishWrapped); err != nil {
			t.Errorf("wrap B: %v", err)
		}
	})
	within(t, 5*time.Second, "a wrap of the pausing A", func() {
		if err := r.Stop(ctx, a, protocol.FinishWrapped); !isConflict(err) {
			t.Errorf("wrap of A during its pause push = %v, want DAEMON-409", err)
		}
	})
	release()
	if err := recvErr(t, pauseA, "A's pause"); err != nil {
		t.Fatalf("A's pause: %v", err)
	}
	if ji, ok := r.Info(a); !ok || ji.State != StatePaused {
		t.Fatalf("A after its pause: %+v (ok=%v), want the retained paused record", ji, ok)
	}
}

// A recovery whose commit-dirty push hangs (on the message loop) leaves
// the other work items operable; once the push fails the recovery
// relaunches as before.
func TestB8HungRecoveryPushLeavesOtherWorkItemsResponsive(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	t.Cleanup(r.Close)
	const a, b = "wi_b8v_a", "wi_b8v_b"
	ctx := context.Background()
	startRunning(t, r, a)
	startRunning(t, r, b)
	release := stallPushes(t, r, a)
	ja := r.jobOf(a)
	spawns := sp.callCount()

	recovered := make(chan struct{})
	go func() { r.recoverOp(ctx, ja, "agent exited", ""); close(recovered) }()
	waitBusy(t, r, a)
	within(t, 20*time.Second, "pause of B", func() {
		if err := r.Stop(ctx, b, protocol.FinishPaused); err != nil {
			t.Errorf("pause B: %v", err)
		}
	})
	within(t, 5*time.Second, "a wrap of the recovering A", func() {
		if err := r.Stop(ctx, a, protocol.FinishWrapped); !isConflict(err) {
			t.Errorf("wrap of A during its recovery push = %v, want DAEMON-409", err)
		}
	})
	// a supersession verdict meanwhile never quarantines the scene under
	// the push
	r.supersedeOp(ctx, ja, "test")
	if r.jobOf(a) != ja {
		t.Fatal("a supersession during the recovery push dropped the job")
	}
	release()
	select {
	case <-recovered:
	case <-time.After(20 * time.Second):
		t.Fatal("the recovery never finished")
	}
	if ji, _ := r.Info(a); ji.State != StateRunning {
		t.Fatalf("A after its recovery: %s, want running", ji.State)
	}
	if sp.callCount() != spawns+1 {
		t.Fatalf("the recovery relaunched %d time(s), want 1", sp.callCount()-spawns)
	}
}

// A shutdown during a hung seal push does not wait for it: the claim
// abort, the persist and the retry-lane stop all return promptly, and
// state.json keeps the SEALING record with its verdict — the next
// start finishes the close.
func TestB8ShutdownDuringHungSealPush(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	const a = "wi_b8s_a"
	ctx := context.Background()
	startRunning(t, r, a)
	release := stallPushes(t, r, a)
	stopA := make(chan error, 1)
	go func() { stopA <- r.Stop(ctx, a, protocol.FinishWrapped) }()
	waitBusy(t, r, a)

	within(t, 10*time.Second, "the runtime's shutdown steps", func() {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		r.BeginShutdown(actx)
		if err := r.Persist(); err != nil {
			t.Errorf("persist: %v", err)
		}
		r.Close()
	})
	var rec *jobRecord
	for _, jr := range loadRecords(t, r) {
		if jr.WorkItemID == a {
			rec = jr
		}
	}
	if rec == nil || rec.State != string(StateSealing) || rec.StopReason != protocol.FinishWrapped {
		t.Fatalf("state.json during the hung push = %+v, want the SEALING record with its wrapped verdict", rec)
	}
	release()
	_ = recvErr(t, stopA, "A's close")
}

// The restart's journal replay clones outside Restart and opMu: a hung
// replay clone neither holds Restart (the daemon's socket waits on it)
// nor the other work items; a shutdown defers it with its entry kept.
func TestB8RestartReplayCloneOutsideOpMu(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	const slow, other = "wi_b8j_slow", "wi_b8j_other"
	hub.reposFor = map[string][]api.ProjectRepo{slow: {{Name: "app", URL: hangingRemote(t)}}}
	// a claim that landed before a crash: journaled, never registered
	sess := fakeSession(slow)
	if err := r.noteClaiming(slow, sess, "key-"+slow, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.SessionClaim(api.SessionClaimRequest{WorkItemID: slow, SessionID: sess, MachineID: "m-test", IdempotencyKey: "key-" + slow}); err != nil {
		t.Fatal(err)
	}
	within(t, 20*time.Second, "Restart with a hung replay clone", func() {
		if err := r.Restart(bg()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	waitClaiming(t, r, slow)
	within(t, 20*time.Second, "Start of another work item", func() {
		startRunning(t, r, other)
	})
	actx, cancel := context.WithTimeout(bg(), 20*time.Second)
	defer cancel()
	within(t, 25*time.Second, "BeginShutdown", func() { r.BeginShutdown(actx) })
	r.replays.Wait()
	if got := journaled(r, slow); got == nil || got.SessionID != sess {
		t.Fatalf("journal after the deferred replay = %+v, want the original entry", got)
	}
	if n := len(endsOf(hub, slow, sess)); n != 0 {
		t.Fatalf("the deferred replay ended its session %d time(s)", n)
	}
	r.Close()
}

// A failed start's compensation (SessionEnd blocked) holds only its own
// work item's lock: a hung end holds neither another work item's start
// nor its close, and a start of the SAME work item meanwhile answers the
// typed transient instead of replaying the claim being ended.
func TestB8AbandonClaimEndOutsideOpMu(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const bad, other = "wi_b8a_bad", "wi_b8a_other"
	g := hub.hangCall(t, "end", bad)
	// the claim's task-repo clone fails: an unclonable remote
	hub.reposFor = map[string][]api.ProjectRepo{bad: {{Name: "app", URL: "/nonexistent/app.git"}}}
	startBad := make(chan error, 1)
	go func() { startBad <- r.Start(bg(), bad) }()
	g.waitEntered(t, "the compensation")

	within(t, 20*time.Second, "Start of another work item", func() {
		startRunning(t, r, other)
	})
	within(t, 20*time.Second, "wrap of another work item", func() {
		if err := r.Stop(bg(), other, protocol.FinishWrapped); err != nil {
			t.Errorf("wrap: %v", err)
		}
	})
	within(t, 5*time.Second, "a second start of the ending work item", func() {
		err := r.Start(bg(), bad)
		ae, ok := err.(*api.Error)
		if !ok || ae.Code != api.ErrDaemonHubUnavailable {
			t.Errorf("start during the compensation = %v, want the DAEMON-503 transient", err)
		}
	})
	g.open()
	if err := recvErr(t, startBad, "the failed Start"); err == nil {
		t.Fatal("the failed start answered nil")
	}
	if journaled(r, bad) != nil {
		t.Fatal("the settled compensation kept its journal entry")
	}
	if r.Busy(bad) {
		t.Fatal("the work item's lock outlived the compensation")
	}
}

func bg() context.Context { return context.Background() }
