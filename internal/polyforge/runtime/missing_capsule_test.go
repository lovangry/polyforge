package runtime

// missing_capsule_test.go pins the mid-flight no-silent-rebuild guard
// (#103): a recorded non-terminal work item whose capsule directory is
// absent under the active workspace is a typed hard failure — Restart
// aborts naming the WI (daemon.Run then refuses startup), the resume
// entry refuses with *MissingCapsuleError, the recovery engine refuses
// with a loud blocked verdict — and NO code path rebuilds, re-clones or
// re-materializes the scene: the clone stays confined to the
// fresh-claim/takeover lane (prepareCapsule).

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func assertNothingMaterialized(t *testing.T, r *Runtime, wi string, sp *scriptedSpawner) {
	t.Helper()
	if _, err := os.Stat(r.capsuleDir(wi)); !os.IsNotExist(err) {
		t.Errorf("capsule dir %s must NOT be created (no silent rebuild)", r.capsuleDir(wi))
	}
	if _, err := os.Stat(r.gitDirFor(wi)); !os.IsNotExist(err) {
		t.Errorf("external git dir %s must NOT be created (no clone from remote)", r.gitDirFor(wi))
	}
	if sp.callCount() != 0 {
		t.Errorf("no derivation may spawn against a missing capsule, got %d", sp.callCount())
	}
}

// Restart with a mid-flight (verify-matches running) record whose capsule
// is absent at the active base: hard error naming the WI, no hub
// transition, nothing materialized or cloned.
func TestRestartMissingMidflightCapsuleFailsHard(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_missing_running"
	rec := matrixRecord(r, wi, 0, "")
	hub.hold(wi, rec.SessionID, api.StatusRunning) // our session, still running
	writeStateFile(t, r, []*jobRecord{rec})
	// the capsule dir is deliberately NOT created: the operator never
	// moved it to the new base.

	err := r.Restart(context.Background())
	if err == nil {
		t.Fatal("a missing mid-flight capsule must fail Restart hard (daemon startup aborts)")
	}
	var miss *MissingCapsuleError
	if !errors.As(err, &miss) {
		t.Fatalf("want *MissingCapsuleError, got %T: %v", err, err)
	}
	if miss.WI != wi || !strings.Contains(err.Error(), wi) {
		t.Errorf("error must name the affected work item %q, got %q", wi, err)
	}
	if !strings.Contains(err.Error(), r.capsuleDir(wi)) {
		t.Errorf("error must name the missing capsule path, got %q", err)
	}
	// no hub transition fired before the failure
	if len(hub.attaches) != 0 || hub.endCount() != 0 || hub.claimCount() != 0 {
		t.Fatalf("no hub transition may precede the hard failure (attaches=%d ends=%d claims=%d)",
			len(hub.attaches), hub.endCount(), hub.claimCount())
	}
	assertNothingMaterialized(t, r, wi, sp)
	assertNoRT(t, toOB)
	// the record survives on disk untouched (the next restart re-checks
	// after the operator restores the capsule)
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].WorkItemID != wi {
		t.Fatalf("record must be kept, got %+v", recs)
	}
}

// Same guard on the paused/unclean attach branch: the failure must fire
// BEFORE the SessionAttach flips the hub.
func TestRestartMissingPausedCapsuleFailsBeforeAttach(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	wi := "wi_missing_paused"
	rec := matrixRecord(r, wi, 0, "")
	rec.State = string(StatePaused)
	rec.CleanPause = false
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	writeStateFile(t, r, []*jobRecord{rec})

	err := r.Restart(context.Background())
	var miss *MissingCapsuleError
	if !errors.As(err, &miss) {
		t.Fatalf("want *MissingCapsuleError, got %T: %v", err, err)
	}
	if len(hub.attaches) != 0 {
		t.Fatalf("SessionAttach must never fire for a missing capsule, got %d", len(hub.attaches))
	}
	assertNothingMaterialized(t, r, wi, sp)
}

// Same guard on the NEW-3 pending-close replay branch (retained pause
// carrying a terminal stop_reason): the close must not be "replayed"
// against a capsule that is not there.
func TestRestartMissingPendingCloseCapsuleFailsHard(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	wi := "wi_missing_pclose"
	rec := matrixRecord(r, wi, 0, "")
	rec.State = string(StatePaused)
	rec.CleanPause = true
	rec.StopReason = protocol.FinishWrapped // the pending terminal close
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	writeStateFile(t, r, []*jobRecord{rec})

	err := r.Restart(context.Background())
	var miss *MissingCapsuleError
	if !errors.As(err, &miss) {
		t.Fatalf("want *MissingCapsuleError, got %T: %v", err, err)
	}
	if hub.endCount() != 0 {
		t.Fatalf("SessionEnd must never fire for a missing capsule, got %d", hub.endCount())
	}
	assertNothingMaterialized(t, r, wi, sp)
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
	if miss.Dir != r.capsuleDir(wi) {
		t.Errorf("typed error dir = %q, want %q", miss.Dir, r.capsuleDir(wi))
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

	r.recoverOp(context.Background(), j, "daemon_restart")

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
