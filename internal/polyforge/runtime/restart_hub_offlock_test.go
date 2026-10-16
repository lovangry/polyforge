package runtime

// restart_hub_offlock_test.go — the restart reconciliation's hub calls
// (journal-replay verify and claim, per-record verify) run outside opMu
// and Restart's wait for them is bounded (RestartHubWait): a hung hub at
// startup no longer holds the daemon's socket, the unreconciled records
// show waiting for aihub and refuse their doors meanwhile, and they end up
// exactly as a synchronous reconciliation would leave them once the hub
// answers.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// eventKinds is a concurrency-safe EventTap recorder.
type eventKinds struct {
	mu   sync.Mutex
	seen map[string][]string // wi → kinds
}

func (e *eventKinds) tap(wi, kind string, _ map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen == nil {
		e.seen = map[string][]string{}
	}
	e.seen[wi] = append(e.seen[wi], kind)
}

func (e *eventKinds) count(wi, kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, k := range e.seen[wi] {
		if k == kind {
			n++
		}
	}
	return n
}

// recvStarted drains RT messages until RTStarted arrived for every wi.
func recvStarted(t *testing.T, q *supervise.Queue[protocol.RTMsg], wis ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, wi := range wis {
		want[wi] = true
	}
	for len(want) > 0 {
		m := recvRT(t, q)
		if m.Kind == protocol.RTStarted {
			delete(want, m.WorkItem)
		}
	}
}

// restartRecord writes a crash-mid-run record of wi (capsule included)
// the hub still holds as running, answering its live sleeper's pid.
func restartRecord(t *testing.T, r *Runtime, hub *fakeHub, wi string) (*jobRecord, int) {
	t.Helper()
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	hub.hold(wi, rec.SessionID, api.StatusRunning)
	initGitCapsule(t, r.restoredJob(rec))
	return rec, pid
}

// A restart whose first record's session verify hangs returns after the
// bounded wait: both records (the second queued behind the first) show
// RECOVERING awaiting the hub with their deferred event, refuse a close
// and a resume, every Start answers the typed transient — and once the
// hub answers both resume in place, are handed to observe, and the
// runtime claims again.
func TestOffLockRestartHungVerifyReturnsAndFinishes(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	r.cfg.RestartHubWait = 200 * time.Millisecond
	ev := &eventKinds{}
	r.cfg.EventTap = ev.tap
	const a, b, c = "wi_rs_a", "wi_rs_b", "wi_rs_c"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a); terminateJobAgent(r, b); terminateJobAgent(r, c) })
	recA, pidA := restartRecord(t, r, hub, a)
	recB, pidB := restartRecord(t, r, hub, b)
	writeStateFile(t, r, []*jobRecord{recA, recB})
	g := hub.hangCall(t, "verify", a)

	within(t, promptly, "Restart with a hung verify", func() {
		if err := r.Restart(context.Background()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	g.waitEntered(t, "the restart verify of "+a)
	for _, wi := range []string{a, b} {
		ji, ok := r.Info(wi)
		if !ok || ji.State != StateRecovering || !ji.AwaitingHub {
			t.Fatalf("%s while its verify is pending = state %s awaiting %v (ok=%v), want RECOVERING awaiting the hub", wi, ji.State, ji.AwaitingHub, ok)
		}
		if n := ev.count(wi, EventSessionVerifyDeferred); n != 1 {
			t.Fatalf("%s: %d session_verify_deferred events, want 1", wi, n)
		}
	}
	if recs := loadRecords(t, r); len(recs) != 2 {
		t.Fatalf("the unreconciled records must be kept, got %+v", recs)
	}
	within(t, promptly, "the doors during the pending reconciliation", func() {
		ctx := context.Background()
		for _, wi := range []string{a, b} {
			if err := r.Stop(ctx, wi, protocol.FinishWrapped); !isConflictErr(err) {
				t.Errorf("Stop(%s) during its pending reconciliation = %v, want DAEMON-409", wi, err)
			}
			if _, err := r.ResumePaused(ctx, wi); !isConflictErr(err) {
				t.Errorf("ResumePaused(%s) during its pending reconciliation = %v, want DAEMON-409", wi, err)
			}
		}
		if err := r.Start(ctx, c); !isTransient(err) {
			t.Errorf("Start(%s) during the reconciliation = %v, want the typed transient", c, err)
		}
	})
	if sp.callCount() != 0 || hub.endCount() != 0 {
		t.Fatalf("an unanswered verify must decide nothing: %d spawns, %d ends", sp.callCount(), hub.endCount())
	}

	g.open()
	waitFor(t, "both records to resume once the hub answers", func() bool {
		for _, wi := range []string{a, b} {
			ji, ok := r.Info(wi)
			if !ok || ji.State != StateRunning || ji.Agent == nil || ji.AwaitingHub {
				return false
			}
		}
		return !r.isRestarting()
	})
	waitDead(t, pidA)
	waitDead(t, pidB)
	recvStarted(t, toOB, a, b) // handed to observe by the lane (Restart had returned)
	for _, rec := range loadRecords(t, r) {
		if rec.State != string(StateRunning) || rec.AwaitingHub {
			t.Fatalf("persisted record after the reconciliation = %+v, want running, not awaiting", rec)
		}
	}
	if n := ev.count(a, EventSessionVerifyDeferred); n != 1 {
		t.Fatalf("%s: the deferred event repeated (%d)", a, n)
	}
	if hub.endCount() != 0 {
		t.Fatalf("the resumes must not close a session, got %+v", hub.lastEnd())
	}
	if err := r.Start(context.Background(), c); err != nil {
		t.Fatalf("Start after the reconciliation finished: %v", err)
	}
}

// A responsive hub keeps the synchronous shape: Restart returns with the
// records reconciled, nothing is shown waiting and no deferred event is
// raised.
func TestOffLockRestartResponsiveHubIsSynchronous(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	ev := &eventKinds{}
	r.cfg.EventTap = ev.tap
	const a = "wi_rs_sync"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	recA, _ := restartRecord(t, r, hub, a)
	writeStateFile(t, r, []*jobRecord{recA})
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if ji, ok := r.Info(a); !ok || ji.State != StateRunning || ji.Agent == nil || ji.AwaitingHub {
		t.Fatalf("after Restart = state %s agent %v awaiting %v (ok=%v), want RUNNING with its agent", ji.State, ji.Agent != nil, ji.AwaitingHub, ok)
	}
	if r.isRestarting() || ev.count(a, EventSessionVerifyDeferred) != 0 {
		t.Fatal("a responsive restart left the reconciliation latched or raised a deferral")
	}
}

// A journal replay whose claim hangs at startup: Restart returns after
// the bounded wait, a Start of the work item answers the typed transient
// meanwhile (never a second claim), and once the hub answers the replay
// runs the work item to a live agent with exactly the one claim.
func TestOffLockRestartHungJournalReplay(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	dataDir := filepath.Join(t.TempDir(), "data")
	r, _, _ := newTestRuntimeAt(t, hub, sp, dataDir)
	r.cfg.RestartHubWait = 200 * time.Millisecond
	const x = "wi_rs_journal"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, x) })
	session := fakeSession(x)
	if err := r.noteClaiming(x, session, randomKey(), nil); err != nil {
		t.Fatal(err)
	}
	hub.hold(x, session, api.StatusRunning)
	g := hub.hangCall(t, "claim", x)
	within(t, promptly, "Restart with a hung journal replay", func() {
		if err := r.Restart(context.Background()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	g.waitEntered(t, "the replayed claim")
	if err := r.Start(context.Background(), x); !isTransient(err) {
		t.Fatalf("Start(%s) during its pending replay = %v, want the typed transient", x, err)
	}
	g.open()
	waitFor(t, "the replay to reach a live agent", func() bool {
		ji, ok := r.Info(x)
		return ok && ji.State == StateRunning && ji.Agent != nil && !r.isRestarting()
	})
	r.replays.Wait()
	if n := hub.claimCount(); n != 1 {
		t.Fatalf("%s claimed %d times, want the one replay", x, n)
	}
	if items := r.loadClaiming(); len(items) != 0 {
		t.Fatalf("the settled replay left journal entries %+v", items)
	}
}

// A shutdown during the restart's hung verify stops the reconciliation:
// AbortClaims does not wait on it, and once the hub answers nothing is
// decided — no agent, no session end, the record kept for the next start.
func TestOffLockShutdownDuringHungRestartVerify(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.RestartHubWait = 200 * time.Millisecond
	const a = "wi_rs_sd"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	recA, _ := restartRecord(t, r, hub, a)
	writeStateFile(t, r, []*jobRecord{recA})
	g := hub.hangCall(t, "verify", a)
	within(t, promptly, "Restart with a hung verify", func() {
		if err := r.Restart(context.Background()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	g.waitEntered(t, "the restart verify")
	actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	within(t, promptly, "AbortClaims during the hung restart verify", func() { r.AbortClaims(actx) })
	g.open()
	waitFor(t, "the reconciliation lane to stop", func() bool { return !r.isRestarting() })
	if sp.callCount() != 0 || hub.endCount() != 0 {
		t.Fatalf("a reconciliation the shutdown caught decided something: %d spawns, %d ends", sp.callCount(), hub.endCount())
	}
	if err := r.persist(); err != nil {
		t.Fatal(err)
	}
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].SessionID != recA.SessionID {
		t.Fatalf("the record must be kept for the next start, got %+v", recs)
	}
}

// The deferred-verify retry lane (awaitVerify) verifies outside opMu too:
// with its verify hung, another work item starts and wraps promptly, the
// deferred one refuses its close (DAEMON-409), and it resumes once the hub
// answers.
func TestOffLockDeferredVerifyHung(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	r.cfg.EndRetryBase = 10 * time.Millisecond
	r.cfg.EndRetryMax = 20 * time.Millisecond
	const a, b = "wi_dv_a", "wi_dv_b"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a); terminateJobAgent(r, b) })
	recA, _ := restartRecord(t, r, hub, a)
	writeStateFile(t, r, []*jobRecord{recA})
	hub.lookupErr[a] = errLookupBoom
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if ji, _ := r.Info(a); !ji.AwaitingHub {
		t.Fatalf("%s after an unreachable restart verify = state %s awaiting %v, want awaiting the hub", a, ji.State, ji.AwaitingHub)
	}
	g := hub.hangCall(t, "verify", a)
	hub.mu.Lock()
	delete(hub.lookupErr, a)
	hub.mu.Unlock()
	g.waitEntered(t, "the deferred verify")

	ctx := context.Background()
	within(t, promptly, "Start and wrap of another work item", func() {
		if err := startLive(r, toOB, b); err != nil {
			t.Error(err)
			return
		}
		if err := r.Stop(ctx, b, protocol.FinishWrapped); err != nil {
			t.Errorf("wrap %s: %v", b, err)
		}
	})
	within(t, promptly, "Stop of the deferred work item", func() {
		if err := r.Stop(ctx, a, protocol.FinishWrapped); !isConflictErr(err) {
			t.Errorf("Stop(%s) during its deferred verify = %v, want DAEMON-409", a, err)
		}
	})
	g.open()
	waitFor(t, "the deferred work item to resume", func() bool {
		ji, ok := r.Info(a)
		return ok && ji.State == StateRunning && ji.Agent != nil && !ji.AwaitingHub
	})
}

// pausedAttachRecord writes the graceful-shutdown record of wi (the hub
// holds it paused, clean_pause unset): the restart attaches it.
func pausedAttachRecord(t *testing.T, r *Runtime, hub *fakeHub, wi string) *jobRecord {
	t.Helper()
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	initGitCapsule(t, r.restoredJob(rec))
	writeTranscript(t, jobFromRecord(rec))
	return rec
}

// A record whose verify answered and whose attach hangs past the wait is
// NOT shown waiting for aihub (its verify is not pending any more), and
// it resumes once the attach answers.
func TestOffLockRestartHungAttachNotShownAwaiting(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.RestartHubWait = 200 * time.Millisecond
	ev := &eventKinds{}
	r.cfg.EventTap = ev.tap
	const a = "wi_rs_attach"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	rec := pausedAttachRecord(t, r, hub, a)
	writeStateFile(t, r, []*jobRecord{rec})
	g := hub.hangCall(t, "attach", a)
	within(t, promptly, "Restart with a hung attach", func() {
		if err := r.Restart(context.Background()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	g.waitEntered(t, "the restart attach")
	if ji, _ := r.Info(a); ji.AwaitingHub || ev.count(a, EventSessionVerifyDeferred) != 0 {
		t.Fatalf("a record whose verify answered was shown waiting for aihub (state %s, awaiting %v)", ji.State, ji.AwaitingHub)
	}
	if err := r.Stop(context.Background(), a, protocol.FinishWrapped); !isConflictErr(err) {
		t.Fatalf("Stop during the restart attach = %v, want DAEMON-409", err)
	}
	g.open()
	waitFor(t, "the attached record to resume", func() bool {
		ji, ok := r.Info(a)
		return ok && ji.State == StateRunning && ji.Agent != nil && !ji.AwaitingHub && !r.isRestarting()
	})
	if n := attachCount(hub, a); n != 1 {
		t.Fatalf("attached %d times, want 1", n)
	}
}

// A shutdown during the restart lane's hung attach relaunches nothing
// once the hub answers: the shutdown already tore the daemon down.
func TestOffLockShutdownDuringHungRestartAttach(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.RestartHubWait = 200 * time.Millisecond
	const a = "wi_rs_attach_sd"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	rec := pausedAttachRecord(t, r, hub, a)
	writeStateFile(t, r, []*jobRecord{rec})
	g := hub.hangCall(t, "attach", a)
	within(t, promptly, "Restart with a hung attach", func() {
		if err := r.Restart(context.Background()); err != nil {
			t.Errorf("Restart: %v", err)
		}
	})
	g.waitEntered(t, "the restart attach")
	actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	within(t, promptly, "AbortClaims during the hung attach", func() { r.AbortClaims(actx) })
	g.open()
	waitFor(t, "the reconciliation lane to end", func() bool { return !r.isRestarting() })
	if n := sp.callCount(); n != 0 {
		t.Fatalf("the lane relaunched %d agent(s) behind the shutdown", n)
	}
	if err := r.persist(); err != nil {
		t.Fatal(err)
	}
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].SessionID != rec.SessionID {
		t.Fatalf("the record must be kept for the next start, got %+v", recs)
	}
}
