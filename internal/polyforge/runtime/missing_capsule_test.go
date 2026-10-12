package runtime

// missing_capsule_test.go pins the mid-flight no-silent-rebuild guard
// (#103): a recorded non-terminal work item whose capsule directory is
// absent under the active workspace is a typed hard failure — Restart
// parks that WI alone as repair_pending (v2 §6; the daemon still starts), the resume
// entry refuses with *MissingCapsuleError, the recovery engine refuses
// with a loud blocked verdict — and NO code path rebuilds, re-clones or
// re-materializes the scene: the clone stays confined to the
// fresh-claim/takeover lane (prepareCapsule).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func assertNothingMaterialized(t *testing.T, r *Runtime, wi string, sp *scriptedSpawner) {
	t.Helper()
	if _, err := os.Stat(testCapsuleDir(r, wi)); !os.IsNotExist(err) {
		t.Errorf("capsule dir %s must NOT be created (no silent rebuild)", testCapsuleDir(r, wi))
	}
	if _, err := os.Stat(r.gitDirFor(wi)); !os.IsNotExist(err) {
		t.Errorf("external git dir %s must NOT be created (no clone from remote)", r.gitDirFor(wi))
	}
	if sp.callCount() != 0 {
		t.Errorf("no derivation may spawn against a missing capsule, got %d", sp.callCount())
	}
}

// parked asserts the v2 §6 posture: Restart returned nil, the WI is
// parked repair_pending with the session kept, the durable event named the
// path/project, no hub transition fired and nothing was rebuilt.
func assertParked(t *testing.T, r *Runtime, hub *fakeHub, sp *scriptedSpawner, wi, kind string, events *[]string) {
	t.Helper()
	ji, ok := r.Info(wi)
	if !ok || ji.RepairPending != kind {
		t.Fatalf("%s must be parked repair_pending(%s), got ok=%v info=%+v", wi, kind, ok, ji)
	}
	if ji.SessionID == "" {
		t.Error("the parked record must keep its session")
	}
	found := false
	for _, k := range *events {
		if k == kind {
			found = true
		}
	}
	if !found {
		t.Errorf("durable event %q not emitted (got %v)", kind, *events)
	}
	if len(hub.attaches) != 0 || hub.endCount() != 0 || hub.claimCount() != 0 {
		t.Fatalf("no hub transition may fire for a parked WI (attaches=%d ends=%d claims=%d)",
			len(hub.attaches), hub.endCount(), hub.claimCount())
	}
	assertNothingMaterialized(t, r, wi, sp)
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].WorkItemID != wi || recs[0].RepairPending != kind || recs[0].SessionID == "" {
		t.Fatalf("record (with its session) must be kept and marked, got %+v", recs)
	}
}

func tapEvents(r *Runtime) *[]string {
	var kinds []string
	r.cfg.EventTap = func(w, kind string, body map[string]any) { kinds = append(kinds, kind) }
	return &kinds
}

// Restart with a mid-flight (verify-matches running) record whose capsule
// is absent at the active base: that WI alone parks as repair_pending
// (session kept, event written, no hub transition, nothing rebuilt).
func TestRestartMissingMidflightCapsuleParks(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	events := tapEvents(r)
	wi := "wi_missing_running"
	rec := matrixRecord(r, wi, 0, "")
	hub.hold(wi, rec.SessionID, api.StatusRunning) // our session, still running
	writeStateFile(t, r, []*jobRecord{rec})
	// the capsule dir is deliberately NOT created.

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("a missing capsule must park the WI, not fail Restart: %v", err)
	}
	assertParked(t, r, hub, sp, wi, RepairCapsuleMissing, events)
	assertNoRT(t, toOB)
}

// Same guard on the paused/unclean attach branch: the park fires BEFORE
// the SessionAttach flips the hub.
func TestRestartMissingPausedCapsuleParksBeforeAttach(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	events := tapEvents(r)
	wi := "wi_missing_paused"
	rec := matrixRecord(r, wi, 0, "")
	rec.State = string(StatePaused)
	rec.CleanPause = false
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	writeStateFile(t, r, []*jobRecord{rec})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	assertParked(t, r, hub, sp, wi, RepairCapsuleMissing, events)
}

// Same guard on the NEW-3 pending-close replay branch: the close must not
// be "replayed" against a capsule that is not there.
func TestRestartMissingPendingCloseCapsuleParks(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	events := tapEvents(r)
	wi := "wi_missing_pclose"
	rec := matrixRecord(r, wi, 0, "")
	rec.State = string(StatePaused)
	rec.CleanPause = true
	rec.StopReason = protocol.FinishWrapped // the pending terminal close
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	writeStateFile(t, r, []*jobRecord{rec})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	assertParked(t, r, hub, sp, wi, RepairCapsuleMissing, events)
}

// RepairResume answers the typed error while the capsule is still absent
// (WI stays parked, session kept), and held=false for a WI not parked.
func TestRepairResumeStillMissingTypedError(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	events := tapEvents(r)
	wi := "wi_repair_still"
	rec := matrixRecord(r, wi, 0, "")
	hub.hold(wi, rec.SessionID, api.StatusRunning)
	writeStateFile(t, r, []*jobRecord{rec})
	if err := r.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	held, err := r.RepairResume(context.Background(), wi)
	var miss *MissingCapsuleError
	if !held || !errors.As(err, &miss) || miss.Dir != testCapsuleDir(r, wi) {
		t.Fatalf("want held + *MissingCapsuleError(%s), got held=%v err=%v", testCapsuleDir(r, wi), held, err)
	}
	assertParked(t, r, hub, sp, wi, RepairCapsuleMissing, events)
	if held, _ := r.RepairResume(context.Background(), "wi_not_here"); held {
		t.Error("an unknown WI is not held")
	}
}

// The resume entry itself (restartRunning → restartResume) refuses with
// the typed error when the capsule vanishes after start.
func TestRestartResumeMissingCapsuleTypedError(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	wi := "wi_missing_resume"
	rec := matrixRecord(r, wi, 0, "")
	r.mu.Lock()
	r.jobs[wi] = jobFromRecord(rec)
	r.mu.Unlock()

	err := r.restartRunning(context.Background(), rec)
	var miss *MissingCapsuleError
	if !errors.As(err, &miss) {
		t.Fatalf("want *MissingCapsuleError, got %T: %v", err, err)
	}
	if miss.Dir != testCapsuleDir(r, wi) {
		t.Errorf("typed error dir = %q, want %q", miss.Dir, testCapsuleDir(r, wi))
	}
	assertNothingMaterialized(t, r, wi, sp)
}

// The recovery engine refuses a vanished-after-start capsule with the
// loud blocked verdict (surfaced to the operator) and no rebuild.
func TestRecoverCoreMissingCapsuleBoundedFail(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	j := seedJob(r, "wi_missing_recover")
	if err := os.RemoveAll(j.capsuleDir); err != nil {
		t.Fatal(err)
	}

	var tapped []map[string]any
	r.cfg.EventTap = func(w, kind string, body map[string]any) {
		if kind == "missing_midflight_capsule" {
			tapped = append(tapped, body)
		}
	}

	r.recoverOp(context.Background(), j, "daemon_restart", "daemon restart (crash mid-run)")

	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("expected RTFinished(blocked), got %+v", msg)
	}
	if !strings.Contains(msg.Detail, j.wi) || !strings.Contains(msg.Detail, j.capsuleDir) {
		t.Errorf("blocked detail must name the WI and the missing path, got %q", msg.Detail)
	}
	if len(tapped) != 1 {
		t.Fatalf("expected exactly one missing_midflight_capsule event, got %d", len(tapped))
	}
	if hub.status[j.wi] != api.StatusBlocked {
		t.Errorf("hub status = %q, want blocked (the surfaced verdict)", hub.status[j.wi])
	}
	assertNothingMaterialized(t, r, j.wi, sp)
}

// One bad WI never takes the others down (v2 §6), and after the operator
// restores the capsule, RepairResume resumes the SAME session in place
// through the restart-recovery path (no re-claim, no SessionEnd).
func TestRestartParksOnlyBadWIAndRepairResumeSameSession(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	events := tapEvents(r)

	bad := matrixRecord(r, "wi_bad", 0, "")
	good := matrixRecord(r, "wi_good", 0, "")
	hub.hold(bad.WorkItemID, bad.SessionID, api.StatusRunning)
	hub.hold(good.WorkItemID, good.SessionID, api.StatusRunning)
	if err := os.MkdirAll(good.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, r, []*jobRecord{bad, good})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart must not fail for one bad WI: %v", err)
	}
	if n := sp.callCount(); n != 1 {
		t.Fatalf("exactly the good WI recovers (1 derivation), got %d", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered || msg.WorkItem != good.WorkItemID {
		t.Fatalf("expected RTRecovered for the good WI, got %+v", msg)
	}
	if ji, _ := r.Info(bad.WorkItemID); ji.RepairPending != RepairCapsuleMissing || ji.SessionID != bad.SessionID {
		t.Fatalf("bad WI must be parked with its session kept, got %+v", ji)
	}
	if ji, _ := r.Info(good.WorkItemID); ji.RepairPending != "" {
		t.Fatalf("good WI must not be parked: %+v", ji)
	}
	_ = events

	// the operator restores the capsule; `pf resume` continues in place
	if err := os.MkdirAll(bad.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := r.RepairResume(context.Background(), bad.WorkItemID)
	if !held || err != nil {
		t.Fatalf("RepairResume: held=%v err=%v", held, err)
	}
	if n := sp.callCount(); n != 2 {
		t.Fatalf("the repaired WI must resume (2 derivations total), got %d", n)
	}
	ji, _ := r.Info(bad.WorkItemID)
	if ji.RepairPending != "" || ji.SessionID != bad.SessionID {
		t.Fatalf("repaired WI: mark must clear and the SAME session continue, got %+v", ji)
	}
	if hub.endCount() != 0 || hub.claimCount() != 0 {
		t.Fatalf("in-place resume must not end or re-claim (ends=%d claims=%d)", hub.endCount(), hub.claimCount())
	}
	// a second call is not parked any more: falls through to the normal path
	if held, _ := r.RepairResume(context.Background(), bad.WorkItemID); held {
		t.Error("a repaired WI is no longer held")
	}
}

func parkOne(t *testing.T, hub *fakeHub, wi string) (*Runtime, *jobRecord, *[]string) {
	t.Helper()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	r.cfg.EndRetryBase = 10 * time.Millisecond
	r.cfg.EndRetryMax = 20 * time.Millisecond
	t.Cleanup(r.Close)
	events := tapEvents(r)
	rec := matrixRecord(r, wi, 0, "")
	hub.hold(wi, rec.SessionID, api.StatusRunning)
	writeStateFile(t, r, []*jobRecord{rec})
	if err := r.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertParked(t, r, hub, sp, wi, RepairCapsuleMissing, events)
	return r, rec, events
}

// Abandon: the daemon ends the kept session with the verdict (the stored
// secret), drops the parked record and writes an event.
func TestAbandonRepairEndsSessionAndDropsRecord(t *testing.T) {
	for _, verdict := range []string{api.StatusCanceled, api.StatusFailed} {
		t.Run(verdict, func(t *testing.T) {
			hub := newFakeHub()
			wi := "wi_abandon_" + verdict
			r, rec, events := parkOne(t, hub, wi)
			held, err := r.AbandonRepair(wi, verdict, "owner gave up")
			if !held || err != nil {
				t.Fatalf("AbandonRepair: held=%v err=%v", held, err)
			}
			end := hub.lastEnd()
			if end.Status != verdict || end.SessionID != rec.SessionID || end.Note != "owner gave up" {
				t.Fatalf("SessionEnd must carry the verdict and the stored secret, got %+v", end)
			}
			if hub.status[wi] != verdict {
				t.Errorf("hub status = %q, want %s", hub.status[wi], verdict)
			}
			if r.jobOf(wi) != nil || len(loadRecords(t, r)) != 0 {
				t.Fatal("the parked record must be dropped")
			}
			found := false
			for _, k := range *events {
				found = found || k == EventRepairAbandoned
			}
			if !found {
				t.Errorf("want a %s event, got %v", EventRepairAbandoned, *events)
			}
			if held, _ := r.AbandonRepair(wi, verdict, ""); held {
				t.Error("a dropped WI is no longer held")
			}
		})
	}
}

// An unacknowledged abandon keeps the record in the durable end_pending
// lane WITH the abandon verdict (not blocked) and retries until it lands.
func TestAbandonRepairSessionEndFailureRetriesWithVerdict(t *testing.T) {
	hub := newFakeHub()
	wi := "wi_abandon_retry"
	r, rec, _ := parkOne(t, hub, wi)
	hub.mu.Lock()
	hub.end[wi] = fmt.Errorf("aihub unreachable (scripted)")
	hub.mu.Unlock()

	held, err := r.AbandonRepair(wi, api.StatusCanceled, "n")
	if !held || err != nil {
		t.Fatalf("held=%v err=%v", held, err)
	}
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].State != string(StateEndPending) || recs[0].EndStatus != api.StatusCanceled ||
		recs[0].RepairPending != "" || recs[0].SessionID != rec.SessionID {
		t.Fatalf("durable record must be end_pending(canceled) with the secret, got %+v", recs)
	}
	if ji, _ := r.Info(wi); ji.RepairPending != "" {
		t.Errorf("an abandoned WI must not read repair_pending: %+v", ji)
	}
	hub.mu.Lock()
	delete(hub.end, wi)
	hub.mu.Unlock()
	waitFor(t, "the retry to land the canceled close", func() bool { return r.jobOf(wi) == nil })
	if hub.lastEnd().Status != api.StatusCanceled {
		t.Fatalf("retry must send the abandon verdict, got %+v", hub.lastEnd())
	}
	waitFor(t, "state.json to drop the record", func() bool { return len(loadRecords(t, r)) == 0 })
}

func TestAbandonRepairRejectsOtherVerdictsAndNonParked(t *testing.T) {
	hub := newFakeHub()
	r, _, _ := parkOne(t, hub, "wi_abandon_bad")
	if _, err := r.AbandonRepair("wi_abandon_bad", api.StatusWrapped, ""); err == nil {
		t.Error("wrapped is not an abandon verdict")
	}
	if held, _ := r.AbandonRepair("wi_unknown", api.StatusCanceled, ""); held {
		t.Error("an unknown WI is not held")
	}
	if hub.endCount() != 0 {
		t.Errorf("no SessionEnd may fire, got %d", hub.endCount())
	}
}
