package runtime

// NEW-2 repro: the observe release (RTFinished with FinishSuperseded /
// FinishPaused) rides the r.send path, which silently DROPS the message
// once the observe channel is full and SendBound expires. The superseded
// or paused monitor is then never released — the leak the v1.5 review's
// Finding 4 close was supposed to end. These tests hold the channel full
// across the release send, then drain it and require the release to
// still arrive.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// newFullChanRuntime builds a Runtime whose observe channel has capacity
// one and a short send bound, so a full-channel drop is quick and the
// release, if it survives, must show up on the drained channel.
func newFullChanRuntime(t *testing.T, hub Aihub, sp Spawner) (*Runtime, chan protocol.RTMsg) {
	t.Helper()
	cfg := Config{
		MachineID:     "m-fullchan",
		Workspace:     filepath.Join(t.TempDir(), "ws"),
		DataDir:       filepath.Join(t.TempDir(), "data"),
		MaxRecoveries: 2,
		SendBound:     100 * time.Millisecond,
	}
	toOB := make(chan protocol.RTMsg, 1)
	fromOB := make(chan protocol.OBMsg, 1)
	return New(cfg, hub, sp, toOB, fromOB), toOB
}

// fillObserveChan occupies the single-channel slot with a filler that is
// NOT a release message, simulating an observe lane that is not
// draining (many jobs finishing at once).
func fillObserveChan(t *testing.T, toOB chan protocol.RTMsg) {
	t.Helper()
	select {
	case toOB <- protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_filler"}:
	default:
		t.Fatal("precondition: the observe channel must be empty here")
	}
}

// drainFiller removes the filler so any re-armed release has room and
// can be observed.
func drainFiller(t *testing.T, toOB chan protocol.RTMsg) {
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

// awaitRelease requires the observe release to arrive within a generous
// window after the channel has been drained.
func awaitRelease(t *testing.T, toOB chan protocol.RTMsg, wi, reason string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-toOB:
			if m.Kind == protocol.RTFinished && m.WorkItem == wi && m.Reason == reason {
				return
			}
			// a re-send may race the drain; keep waiting for the release
			continue
		case <-deadline:
			t.Fatalf("observe release RTFinished(%s) for %s was dropped on the full channel and never re-armed; the monitor for %s is never released", reason, wi, wi)
		}
	}
}

// TestSupersededReleaseSurvivesFullObserveChannel: OBSuperseded arrives
// while the observe channel is full. The supersede close itself must not
// lose its RTFinished(superseded) release — observe must be released
// once the channel has room again.
func TestSupersededReleaseSurvivesFullObserveChannel(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB := newFullChanRuntime(t, newFakeHub(), sp)
	wi := "wi_supfull"
	j := seedJob(r, wi)
	old := installOldAgent(t, sp, j)
	oldPID := old.PID()

	fillObserveChan(t, toOB)

	// The supersede close runs synchronously; its r.send blocks against
	// the full channel for SendBound and (today) drops the release.
	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBSuperseded, SpawnN: j.spawnN,
		WorkItem: wi, Detail: "verify mismatch: held by fp abc123",
	})

	waitDead(t, oldPID)
	if r.jobOf(wi) != nil {
		t.Fatal("the superseded job must leave the live set")
	}

	// Give the (dropped) send bound time to have certainly expired, then
	// make room and require the release to still arrive.
	time.Sleep(3 * r.cfg.SendBound)
	drainFiller(t, toOB)
	awaitRelease(t, toOB, wi, protocol.FinishSuperseded)
}

// TestPausedReleaseSurvivesFullObserveChannel: the operator pause close
// runs while the observe channel is full. Its RTFinished(paused) release
// must not be lost — the paused monitor must be released once the
// channel has room again.
func TestPausedReleaseSurvivesFullObserveChannel(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	wi := "wi_pausefull"
	sess := fakeSession(wi)
	hub.hold(wi, sess, api.StatusRunning)
	sp := &scriptedSpawner{}
	r, toOB := newFullChanRuntime(t, hub, sp)
	j := seedJob(r, wi)
	initGitCapsule(t, j)
	old := installOldAgent(t, sp, j)
	oldPID := old.PID()

	fillObserveChan(t, toOB)

	if err := r.Stop(context.Background(), wi, protocol.FinishPaused); err != nil {
		t.Fatalf("pause close: %v", err)
	}
	waitDead(t, oldPID)

	// The pause close is synchronous; its release send has already been
	// attempted against the full channel. Make room and require the
	// release to still arrive.
	time.Sleep(3 * r.cfg.SendBound)
	drainFiller(t, toOB)
	awaitRelease(t, toOB, wi, protocol.FinishPaused)
}
