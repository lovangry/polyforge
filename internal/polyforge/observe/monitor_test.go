package observe

// monitor_test.go: the detection surface — death, hang (probe-confirmed),
// provider errors (storm-guarded), recovery lifecycle — plus the
// select-never-blocks proofs.
//
// The non-blocking tests are the MUTATION-HONEST kind: the fake handle's
// SendControl and the fake push both block on a channel the TEST
// controls. If an implementation regressed to calling either inline in a
// select case, the "monitor still processes records meanwhile" assertion
// below would time out and the suite would go red.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

func TestDeathDetectionSendsNeedsRecovery(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)

	h.die()

	msg := waitOB(t, ob, "WINeedsRecovery(death)")
	if msg.Kind != protocol.OBNeedsRecovery {
		t.Fatalf("kind: got %s", msg.Kind)
	}
	if msg.Reason != protocol.RecoverDeath {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverDeath)
	}
	if msg.AttemptID != "att_1" {
		t.Fatalf("attempt-id fencing: got %s", msg.AttemptID)
	}
	if msg.WorkItem != "wi_test" {
		t.Fatalf("work item: got %s", msg.WorkItem)
	}
	soon(t, 500*time.Millisecond, "needs_recovery event written", func() bool {
		return len(eventsOf(m, EvNeedsRecovery)) == 1
	})
}

func TestStormGuardSingleNeedsRecoveryPerAttempt(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)

	// Signal 1: the process died.
	h.die()
	msg := waitOB(t, ob, "first WINeedsRecovery")
	if msg.Reason != protocol.RecoverDeath {
		t.Fatalf("reason: got %s", msg.Reason)
	}

	// Signal 2..n: the stream also went silent mid-turn and the probe
	// would fail, the provider also errored — everything a dying agent
	// can still emit. Exactly ONE WINeedsRecovery may leave the monitor
	// for this attempt ("已发后静默到恢复完成").
	feed(h, fxTurnStart)
	feed(h, fxMsgEndErr("upstream API error (401)"))
	feed(h, fxTurnEndErr("upstream API error (401)"))
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("control path closed")
	})
	expectNoOB(t, ob, 250*time.Millisecond, "storm-suppressed duplicate signals")

	// Recovery: a NEW attempt re-arms the guard. Its death is reported
	// again — with the new attempt-id.
	h2 := newFakeHandle("att_2")
	m.rtCh <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_test", AttemptID: "att_2"}, Handle: h2}
	soon(t, 500*time.Millisecond, "recovery adopted", func() bool {
		return m.currentAttempt() == "att_2"
	})
	h2.die()
	msg2 := waitOB(t, ob, "WINeedsRecovery for the new attempt")
	if msg2.AttemptID != "att_2" {
		t.Fatalf("new attempt must fence its own messages: %+v", msg2)
	}
	// Exactly two messages total for the whole scenario.
	expectNoOB(t, ob, 250*time.Millisecond, "no third message")
}

func TestHangDetectionProbeConfirmed(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)

	// A turn opens, then the stream goes silent past the window.
	feed(h, fxTurnStart)
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		if msg.Method != protocol.ControlProbe {
			t.Errorf("probe goroutine must send pf/probe, got %s", msg.Method)
		}
		return protocol.ControlAck{}, errors.New("no ack: agent wedged")
	})

	msg := waitOB(t, ob, "WINeedsRecovery(hang)")
	if msg.Reason != protocol.RecoverHang {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverHang)
	}
	if h.sentCount() != 1 {
		t.Fatalf("exactly one confirmation probe, got %d", h.sentCount())
	}
	if len(eventsOf(m, EvInfraIncident)) == 0 {
		t.Fatal("hang_suspected infra incident must be written")
	}
}

func TestHangProbeClearedRestartsWindow(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)

	feed(h, fxTurnStart)
	var probes atomic.Int64
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		probes.Add(1)
		return protocol.ControlAck{Method: msg.Method, OK: true}, nil
	})

	// The window expires, the probe is answered: kill CANCELLED, window
	// restarts, no recovery message — and no probe storm while the
	// restarted window is young.
	time.Sleep(120 * time.Millisecond)
	if probes.Load() == 0 {
		t.Fatal("the probe must have been sent after the silent window")
	}
	expectNoOB(t, ob, 100*time.Millisecond, "a cleared probe never recovers")
	if len(eventsOf(m, EvInfraIncident)) == 0 {
		t.Fatal("hang_suspected must be written")
	}
}

func TestProviderErrorNeedsTwoConsecutiveStrikes(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	m.writeExec(t, 1, "code")

	const detail = "upstream API error (503)"
	strike := func() {
		feed(h, fxTurnStart)
		feed(h, fxMsgEndErr(detail))
		feed(h, fxTurnEndErr(detail))
	}
	// Strike one: an immediate infra_incident event, but NO recovery —
	// one failed turn may be a transient the harness retries.
	strike()
	soon(t, 500*time.Millisecond, "first incident event", func() bool {
		return len(eventsOf(m, EvInfraIncident)) >= 1
	})
	expectNoOB(t, ob, 150*time.Millisecond, "one strike must not recover")

	// Strike two, same step, no clean turn between → WINeedsRecovery.
	strike()
	msg := waitOB(t, ob, "WINeedsRecovery(provider_error)")
	if msg.Reason != protocol.RecoverProviderError {
		t.Fatalf("reason: got %s", msg.Reason)
	}
}

func TestProviderErrorCleanTurnResetsChain(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	m.writeExec(t, 1, "code")

	const detail = "upstream API error (503)"
	strike := func() {
		feed(h, fxTurnStart)
		feed(h, fxMsgEndErr(detail))
		feed(h, fxTurnEndErr(detail))
	}
	clean := func() {
		feed(h, fxTurnStart)
		feed(h, fxMsgEndStop)
		feed(h, fxTurnEndStop)
	}

	strike()
	soon(t, 500*time.Millisecond, "first incident event", func() bool {
		return len(eventsOf(m, EvInfraIncident)) >= 1
	})
	// A successful turn between the failures breaks the chain: the
	// next failure starts a NEW chain at strike one.
	clean()
	strike()
	expectNoOB(t, ob, 250*time.Millisecond, "strike after a clean turn must not recover")
}

// ---------------------------------------------------------------------------
// select non-blocking proofs (mutation-honest: a blocking implementation
// turns these red through the soon() timeout)

func TestMonitorNotBlockedByAnswerAckWait(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.HangWindow = time.Hour // keep the probe out of this test
	})

	// Arm a pending question.
	fc := FlushCall{Req: protocol.FlushRequest{Reason: protocol.FlushNeedsInput, QuestionID: "q1", Question: "A or B?"}, Resp: make(chan protocol.FlushAck, 1)}
	m.flushCh <- fc
	if ack := <-fc.Resp; !ack.OK {
		t.Fatalf("needs_input ack: %+v", ack)
	}

	// The answer's pf/answer delivery blocks until the test releases it.
	release := make(chan struct{})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		<-release
		return protocol.ControlAck{Method: msg.Method, OK: true}, nil
	})
	ac := AnswerCall{
		Relay:  AnswerRelay{QuestionID: "q1", AnswerID: "a1", AnswerText: "A", AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "alice"},
		Caller: "alice",
		Resp:   make(chan error, 1),
	}
	m.answerCh <- ac

	// WHILE the ack wait is parked in its sub-goroutine, the monitor
	// must keep classifying records. (A blocking implementation parks
	// the whole select loop and this goes red.)
	feed(h, fxTextDelta)
	soon(t, 300*time.Millisecond, "record classified while answer ack is parked", func() bool {
		return m.stream.heartbeat().Records >= 1
	})

	close(release)
	select {
	case err := <-ac.Resp:
		if err != nil {
			t.Fatalf("answer outcome: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answer never settled after release")
	}
}

func TestMonitorNotBlockedByPushInFlight(t *testing.T) {
	h := newFakeHandle("att_1")
	release := make(chan struct{})
	pushes := make(chan struct{}, 16)
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, error) {
			pushes <- struct{}{}
			<-release // the "long" push
			return "sha_slow", true, nil
		}
	})

	// execution.json v1 appears → an async push starts and parks.
	m.writeExec(t, 1, "code")
	soon(t, 500*time.Millisecond, "push started", func() bool { return len(pushes) > 0 })

	// The monitor must keep observing while the push is in flight.
	feed(h, fxTextDelta)
	soon(t, 300*time.Millisecond, "record classified while push is in flight", func() bool {
		return m.stream.heartbeat().Records >= 1
	})

	// Only ONE push may be in flight per WI: the parked push holds the
	// gate, and a v2 bump must NOT start a second one yet.
	m.writeExec(t, 2, "code")
	time.Sleep(80 * time.Millisecond)
	if got := len(pushes); got != 1 {
		t.Fatalf("pushes started: got %d, want 1 (single in-flight gate)", got)
	}

	close(release)
	soon(t, 500*time.Millisecond, "first push settles", func() bool {
		return len(eventsOf(m, EvPublished)) >= 1
	})
	// Now the still-dirty v2 gets its turn.
	soon(t, 500*time.Millisecond, "second push after gate freed", func() bool {
		return len(pushes) >= 2
	})
}

func TestExpectedExitAfterWrapIsSilent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)

	// A successful wrap seals the attempt...
	fc := FlushCall{Req: protocol.FlushRequest{Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped}, Resp: make(chan protocol.FlushAck, 1)}
	m.flushCh <- fc
	ack := <-fc.Resp
	if !ack.OK {
		t.Fatalf("wrap ack: %+v", ack)
	}
	msg := waitOB(t, ob, "WICompleted")
	if msg.Kind != protocol.OBCompleted || msg.Reason != protocol.FinishWrapped {
		t.Fatalf("WICompleted: %+v", msg)
	}

	// ...so the agent's process exit afterwards is EXPECTED, not a death.
	h.die()
	expectNoOB(t, ob, 250*time.Millisecond, "post-wrap exit")
}

func TestMonitorNotBlockedByProbeWait(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)

	// A turn opens, then the stream goes silent past the hang window.
	feed(h, fxTurnStart)
	probeEntered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		if msg.Method == protocol.ControlProbe {
			probeEntered <- struct{}{}
			<-release // the "long" probe wait
			return protocol.ControlAck{Method: msg.Method, OK: true}, nil
		}
		return protocol.ControlAck{Method: msg.Method, OK: true}, nil
	})

	// The probe is now CONFIRMED parked. While it waits, the monitor
	// must keep classifying records (an inline probe wait parks the
	// whole select loop and this goes red).
	select {
	case <-probeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the liveness probe never started")
	}
	feed(h, fxTextDelta)
	soon(t, 400*time.Millisecond, "record classified while probe is parked", func() bool {
		return m.stream.heartbeat().Records >= 2
	})

	// The probe answers: kill cancelled, window restarts, no recovery.
	close(release)
	expectNoOB(t, ob, 200*time.Millisecond, "a cleared probe never recovers")
}

// TestPollVersionStagnantSchedulesFallbackCommit pins the C3'
// belt-and-braces: an agent that rewrites execution.json (mtime advances)
// while leaving the top-level Version frozen must not starve the capsule
// chain — after the stagnation bound (3 rewrites here) the monitor
// schedules a commit anyway and records one version_stagnant event.
// The Version-driven rule stays primary: a proper Version bump publishes
// without any stagnation episode.
func TestPollVersionStagnantSchedulesFallbackCommit(t *testing.T) {
	var pushes atomic.Int64
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.VersionStagnantTicks = 3
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, error) {
			pushes.Add(1)
			return "sha_stag", true, nil
		}
	})

	// v1 published normally: the primary Version-driven semantic.
	m.writeExec(t, 1, "s1")
	soon(t, 2*time.Second, "version-driven publish", func() bool {
		return pushes.Load() == 1
	})

	// The agent now violates the contract: four rewrites, Version stays 1.
	for i := 0; i < 4; i++ {
		m.writeExec(t, 1, "s1")
		time.Sleep(5 * time.Millisecond) // let a poll tick observe each mtime
	}
	soon(t, 2*time.Second, "stagnation fallback commit", func() bool {
		return pushes.Load() == 2
	})
	if evs := eventsOf(m, EvVersionStagnant); len(evs) != 1 {
		t.Fatalf("exactly one version_stagnant event per episode, got %d", len(evs))
	}
	// The fallback stops after one commit until a NEW episode: no push
	// storm while the file is left alone.
	time.Sleep(60 * time.Millisecond)
	if n := pushes.Load(); n != 2 {
		t.Fatalf("stale stagnant episode must not keep pushing, pushes=%d", n)
	}
}

// TestPollVersionBumpClearsStagnation: a real Version bump in the middle
// of a stagnation episode re-arms the primary semantic — the episode's
// partial count is discarded, no fallback commit fires, no event lands.
func TestPollVersionBumpClearsStagnation(t *testing.T) {
	var pushes atomic.Int64
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.VersionStagnantTicks = 3
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, error) {
			pushes.Add(1)
			return "sha_bump", true, nil
		}
	})

	m.writeExec(t, 1, "s1")
	soon(t, 2*time.Second, "initial publish", func() bool {
		return pushes.Load() == 1
	})
	// two stagnant rewrites (below the bound), then a proper bump
	m.writeExec(t, 1, "s1")
	time.Sleep(5 * time.Millisecond)
	m.writeExec(t, 1, "s1")
	time.Sleep(5 * time.Millisecond)
	m.writeExec(t, 2, "s2") // the agent fixes its write path
	soon(t, 2*time.Second, "version-driven publish of v2", func() bool {
		return pushes.Load() == 2
	})
	if evs := eventsOf(m, EvVersionStagnant); len(evs) != 0 {
		t.Fatalf("a resolved episode must leave no event, got %d", len(evs))
	}
	time.Sleep(40 * time.Millisecond)
	if n := pushes.Load(); n != 2 {
		t.Fatalf("no fallback commit may fire after a bump, pushes=%d", n)
	}
}
