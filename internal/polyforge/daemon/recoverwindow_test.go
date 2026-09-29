package daemon

// recoverwindow_test.go: the recovery-window regressions of the e2e bug
// lane. Two pins:
//
//   - Bug 2: the NEW instance's flush(wrap) arriving inside the recovery
//     window (state still RECOVERING, no close anywhere) is answered with
//     the typed retryable transient, never the semantically wrong "a
//     close is already in progress".
//
//   - Bug 4 + the record-stream assembly: the daemon.Config Observe seam
//     carries the supervision windows into the assembled observe lane,
//     and the record path the production binding feeds (RecordTap →
//     tapRecord → adapter → monitor) actually drives detection — with the
//     real-daemon spawner half pinned by the runtime lane's
//     DefaultSpawner pointer test and s07's real-`pf daemon` e2e run.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// ---------------------------------------------------------------------------
// Bug 2: flush(wrap) in the recovery window.

// parkingSpawner parks every spawn AFTER the first (the recovery engine's
// re-derivation), holding the runtime in StateRecovering until released —
// the recovery window, stretched to a test-controllable length.
type parkingSpawner struct {
	fakeSpawner
	park chan struct{}
}

func (s *parkingSpawner) Spawn(ctx context.Context, p runtime.SpawnParams) (runtime.AgentHandle, error) {
	if s.callCount() >= 1 {
		<-s.park
	}
	return s.fakeSpawner.Spawn(ctx, p)
}

func TestFlushWrapDuringRecoveryWindowAnswersRetryableTransient(t *testing.T) {
	park := make(chan struct{})
	var sp *parkingSpawner
	td := newTestDaemon(t, func(c *Config) {
		sp = &parkingSpawner{park: park}
		c.Spawner = sp
	})
	td.submitClaim(t, "wi_rw")
	td.waitMonitored(t, "wi_rw")

	// A confirmed agent death arms the recovery engine; the parked
	// re-derivation holds it in StateRecovering.
	sp.agentOf(0).Terminate(0)
	waitState(t, td, "wi_rw", "recovering")

	// The NEW instance's flush(wrap) in the window: no close is in
	// progress anywhere, so the answer must be the typed retryable
	// transient — never "a close is already in progress".
	_, err := td.client.Flush("wi_rw", protocol.FlushRequest{
		WorkItem: "wi_rw", Reason: protocol.FlushWrap, FinalStatus: "wrapped",
	})
	if err == nil {
		t.Fatal("recovery-window flush(wrap) succeeded; want the retryable DAEMON-409")
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
		t.Fatalf("recovery-window flush(wrap) error = %v, want DAEMON-409", err)
	}
	if !strings.Contains(ae.Detail, "instance just spawned") {
		t.Fatalf("recovery-window answer must name the typed transient, got %q", ae.Detail)
	}
	if strings.Contains(ae.Detail, "already in progress") {
		t.Fatalf("no close is in progress; the answer must not claim one: %q", ae.Detail)
	}

	// The window passes: the re-derivation completes and the (retrying)
	// flush now takes the ordinary winner path — one seal, one
	// end-generation, terminal.
	close(park)
	waitState(t, td, "wi_rw", "running")
	// The monitor's adoption of the new instance lags the RUNNING flip
	// by the attachment pipeline; the flush must name the ADOPTED id, so
	// wait for the view to carry it before playing the agent's retry.
	var rjv protocol.JobView
	deadline := time.Now().Add(5 * time.Second)
	for {
		rraw, rerr := td.client.Observe("wi_rw", 0)
		if rerr != nil {
			t.Fatalf("observe wi_rw: %v", rerr)
		}
		if err := json.Unmarshal(rraw, &rjv); err != nil || rjv.AttemptID == "" {
			t.Fatalf("wi_rw view undecodable: %v (%s)", err, rraw)
		}
		if rjv.AttemptID != "ra_wi_rw" { // adopted: the #1 lineage id
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("monitor never adopted the recovered instance (view id %q)", rjv.AttemptID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := td.client.Flush("wi_rw", protocol.FlushRequest{
		WorkItem: "wi_rw", AttemptID: rjv.AttemptID,
		Reason: protocol.FlushWrap, FinalStatus: "wrapped",
	}); err != nil {
		t.Fatalf("post-window flush(wrap): %v", err)
	}
	waitGone(t, td, "wi_rw")
	if td.hub.endCount() != 1 {
		t.Fatalf("end-generation calls = %d, want 1", td.hub.endCount())
	}
	if st := td.hub.lastEnd().Status; st != protocol.FinishWrapped {
		t.Fatalf("end status = %q, want wrapped", st)
	}
}

// ---------------------------------------------------------------------------
// Bug 4 + the record-stream assembly: injected windows + the tap path.

func TestObserveWindowsInjectionDrivesHangDetection(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) {
		// The Bug-4 seam: the supervision windows the assembly passes
		// through (zero would keep the 10min/2min production defaults,
		// and this test would hang instead of detect).
		c.Observe = ObserveWindows{
			HangWindow:   80 * time.Millisecond,
			ProbeTimeout: 300 * time.Millisecond,
		}
	})
	td.submitClaim(t, "wi_win")
	td.waitMonitored(t, "wi_win")

	// The agent's liveness probe will never be acknowledged.
	agent := td.spawner.agentOf(0)
	agent.mu.Lock()
	agent.ackFn = func(method string) error { return errors.New("probe lost") }
	agent.mu.Unlock()

	// One record through the PRODUCTION record path — the spawner tap →
	// daemon tapRecord → adapter → monitor — opens a turn: observable
	// stream, turn in flight. Silence past the injected 80ms window arms
	// the watchdog.
	td.tapRecord("wi_win", []byte(`{"type":"agent_start"}`))

	// The watchdog probes (the probe really reached the agent), the probe
	// fails, and exactly one hang recovery re-derives the agent.
	deadline := time.Now().Add(5 * time.Second)
	for td.spawner.callCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("hang recovery never re-derived the agent (spawns=%d)", td.spawner.callCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := agent.sentMethods(); !containsStr(got, protocol.ControlProbe) {
		t.Fatalf("the liveness probe never reached the agent: %v", got)
	}

	// The detection surfaces journaled the episode: hang_suspected incident
	// and the hang recovery, through the injected windows.
	var sawSuspect, sawHang bool
	for _, e := range td.obs.Events().Since("wi_win") {
		switch e.Kind {
		case observe.EvInfraIncident:
			if strings.Contains(string(e.Body), observe.HangSuspected) {
				sawSuspect = true
			}
		case observe.EvNeedsRecovery:
			if strings.Contains(string(e.Body), protocol.RecoverHang) {
				sawHang = true
			}
		}
	}
	if !sawSuspect {
		t.Fatal("no hang_suspected incident: the injected HangWindow did not drive the watchdog")
	}
	if !sawHang {
		t.Fatal("no hang recovery: the injected windows did not drive detection")
	}

	// The storm guard: the recovered agent's fresh stream (no records,
	// no turn) never re-arms, so the whole episode is ONE recovery.
	time.Sleep(250 * time.Millisecond)
	if n := td.spawner.callCount(); n != 2 {
		t.Fatalf("spawns = %d after the recovery settled, want 2 (storm guard)", n)
	}
	for _, e := range td.obs.Events().Since("wi_win") {
		if e.Kind == observe.EvNeedsRecovery && !strings.Contains(string(e.Body), protocol.RecoverHang) {
			t.Fatalf("second recovery outside the hang episode: %+v", e)
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
