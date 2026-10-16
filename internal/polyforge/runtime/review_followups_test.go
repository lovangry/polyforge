package runtime

// review_followups_test.go — follow-ups of the independent review of the
// B-8 remainder / persisted-flags change.

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// A paused record whose restart-time verify was deferred, resumed by the
// operator (pf resume verifies itself) before the backoff retry fires:
// the awaiting-hub latch clears — at the resume's verify, and again in
// the retry's early return — so closes are not refused "waiting for
// aihub" until the next restart.
func TestAwaitingHubClearsWhenPfResumeBeatsTheVerifyRetry(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.EndRetryBase = time.Hour // the backoff retry never fires on its own
	const wi = "wi_rf_await_resume"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, wi) })
	rec := matrixRecord(r, wi, 0, "")
	rec.State = string(StatePaused)
	rec.StopReason = protocol.FinishPaused
	rec.CleanPause = true
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	hub.cleanPause[wi] = true
	hub.lookupErr[wi] = errLookupBoom
	writeStateFile(t, r, []*jobRecord{rec})
	initGitCapsule(t, r.restoredJob(rec))
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if ji, ok := r.Info(wi); !ok || !ji.AwaitingHub || ji.State != StatePaused {
		t.Fatalf("after the outage restart: %+v (ok=%v), want paused awaiting the hub", ji, ok)
	}

	// the hub is back and the operator's pf resume lands first
	hub.mu.Lock()
	delete(hub.lookupErr, wi)
	hub.cleanPause[wi] = false
	hub.mu.Unlock()
	out, err := r.ResumePaused(context.Background(), wi)
	if err != nil || out != ResumeAttached {
		t.Fatalf("ResumePaused = %q, %v", out, err)
	}
	if ji, _ := r.Info(wi); ji.AwaitingHub {
		t.Fatal("the resume's own verify must clear the awaiting-hub latch")
	}
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].AwaitingHub {
		t.Fatalf("state.json after the resume = %+v, want the latch cleared", recs)
	}

	// the late retry: a live agent — the early return clears it too
	j := r.jobOf(wi)
	j.mu.Lock()
	j.awaitingHub = true
	j.mu.Unlock()
	if !r.verifyAttempt(context.Background(), j, false) {
		t.Fatal("the retry of a resumed job must settle")
	}
	if ji, _ := r.Info(wi); ji.AwaitingHub {
		t.Fatal("the retry's early return must clear the awaiting-hub latch")
	}
}

// A Start against a job whose close is on the wire answers the typed
// transient (the dispatcher and the resume-blocked door retry it), not a
// final 409.
func TestStartAgainstBusyJobIsTransient(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const a = "wi_rf_busy_start"
	startRunning(t, r, a)
	release := stallPushes(t, r, a)
	pauseA := make(chan error, 1)
	go func() { pauseA <- r.Stop(bg(), a, protocol.FinishPaused) }()
	waitBusy(t, r, a)
	err := r.Start(bg(), a)
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonHubUnavailable {
		t.Fatalf("Start during the close = %v, want the DAEMON-503 transient", err)
	}
	release()
	if err := recvErr(t, pauseA, "the pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
}

// A deferred verify never runs while Restart is still reconciling.
func TestVerifyAttemptWaitsForRestart(t *testing.T) {
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	j := seedJob(r, "wi_rf_restarting")
	r.mu.Lock()
	r.restarting = true
	r.mu.Unlock()
	if r.verifyAttempt(context.Background(), j, true) {
		t.Fatal("a verify attempt during Restart must retry later")
	}
}
