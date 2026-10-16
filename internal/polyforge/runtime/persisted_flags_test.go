package runtime

// persisted_flags_test.go — two job flags that used to live in memory
// only survive a daemon restart through the job record: the end_pending
// "chain push permanently rejected, stop pushing" verdict and the
// restart's "awaiting aihub (deferred verify)" latch. Records written
// before the fields existed load them as false.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

func TestPersistedFlagsAbsentInOldRecordsLoadFalse(t *testing.T) {
	var rec jobRecord
	if err := json.Unmarshal([]byte(`{"work_item_id":"wi_old","session_id":"s","state":"end_pending"}`), &rec); err != nil {
		t.Fatal(err)
	}
	j := jobFromRecord(&rec)
	if j.endPushGaveUp || j.awaitingHub {
		t.Fatalf("an old record loaded endPushGaveUp=%v awaitingHub=%v, want false", j.endPushGaveUp, j.awaitingHub)
	}
	// and the round trip keeps them
	j.endPushGaveUp, j.awaitingHub = true, true
	back := jobFromRecord(j.record())
	if !back.endPushGaveUp || !back.awaitingHub {
		t.Fatal("the record round trip dropped a flag")
	}
}

// A permanent chain push rejection on an end_pending close is recorded
// durably at once: after a daemon restart the blocked-close retry closes
// without pushing again.
func TestEndPushGaveUpSurvivesRestart(t *testing.T) {
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const wi = "wi_pf_gaveup"
	j := seedJob(r, wi)
	bare := initGitCapsule(t, j)
	pushes := filepath.Join(t.TempDir(), "pushes")
	hook := fmt.Sprintf("#!/bin/sh\necho x >> %q\necho 'aihub: commit is missing an integer Polyforge-Seq trailer' >&2\nexit 1\n", pushes)
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	hub.hold(wi, j.sessionID, api.StatusRunning)
	hub.end[wi] = fmt.Errorf("aihub unreachable (scripted)")
	j.mu.Lock()
	j.state = StateEndPending
	j.endNote = "boom"
	j.mu.Unlock()
	if r.endPendingAttempt(j) {
		t.Fatal("the close must stay pending while the hub refuses the end")
	}
	recs := loadRecords(t, r)
	if len(recs) != 1 || !recs[0].EndPushGaveUp {
		t.Fatalf("state.json after the permanent rejection = %+v, want end_push_gave_up", recs)
	}
	before := countLines(t, pushes)
	if before == 0 {
		t.Fatal("setup: the rejected push never reached the hook")
	}
	r.Close()

	// the next daemon start: the hub answers again
	hub.mu.Lock()
	delete(hub.end, wi)
	hub.mu.Unlock()
	r2 := New(r.cfg, hub, &scriptedSpawner{}, r.toOB, r.fromOB)
	r2.cfg.EndRetryBase = 10 * time.Millisecond
	r2.cfg.EndRetryMax = 20 * time.Millisecond
	t.Cleanup(r2.Close)
	if err := r2.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitFor(t, "the end_pending close to settle", func() bool {
		_, ok := r2.Info(wi)
		return !ok
	})
	waitRetryIdle(t, r2)
	if got := countLines(t, pushes); got != before {
		t.Fatalf("the retry after the restart pushed again (%d hook runs, was %d)", got, before)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != api.StatusBlocked {
		t.Fatalf("ends = %d (%+v), want the one blocked close", hub.endCount(), hub.lastEnd())
	}
}

// The restart's deferred-verify latch is persisted with the record, and
// the next start's verify — the hub answering — clears it.
func TestAwaitingHubSurvivesRestartAndClearsOnAnswer(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	fastRetry(r)
	wi := "wi_pf_await"
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	hub.hold(wi, rec.SessionID, api.StatusRunning)
	hub.lookupErr[wi] = errLookupBoom
	writeStateFile(t, r, []*jobRecord{rec})
	initGitCapsule(t, r.restoredJob(rec))
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	recs := loadRecords(t, r)
	if len(recs) != 1 || !recs[0].AwaitingHub {
		t.Fatalf("state.json during the outage = %+v, want awaiting_hub", recs)
	}
	r.Close()
	waitRetryIdle(t, r)

	// a restart during the outage keeps the latch
	r2 := New(r.cfg, hub, sp, r.toOB, r.fromOB)
	fastRetry(r2)
	if err := r2.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if ji, ok := r2.Info(wi); !ok || !ji.AwaitingHub {
		t.Fatalf("after a restart during the outage: %+v (ok=%v), want awaiting the hub", ji, ok)
	}
	r2.Close()
	waitRetryIdle(t, r2)

	// the hub answers at the next start: the latch clears and the job runs
	hub.mu.Lock()
	delete(hub.lookupErr, wi)
	hub.mu.Unlock()
	r3 := New(r.cfg, hub, sp, r.toOB, r.fromOB)
	fastRetry(r3)
	t.Cleanup(func() { r3.Close(); terminateJobAgent(r3, wi) })
	if err := r3.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	ji, ok := r3.Info(wi)
	if !ok || ji.AwaitingHub || ji.State != StateRunning {
		t.Fatalf("after the hub answered: %+v (ok=%v), want running, not awaiting", ji, ok)
	}
	if recs := loadRecords(t, r3); len(recs) != 1 || recs[0].AwaitingHub {
		t.Fatalf("state.json after the hub answered = %+v, want the latch cleared", recs)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}
