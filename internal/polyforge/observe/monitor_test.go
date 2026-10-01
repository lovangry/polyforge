package observe

// monitor_test.go: the detection surface — death, hang (probe-confirmed),
// provider errors (fatal → instant recovery; non-fatal → storm-guarded
// chain), the idle watchdog (F5), recovery lifecycle — plus the
// select-never-blocks proofs.
//
// The non-blocking tests are the MUTATION-HONEST kind: the fake handle's
// SendControl and the fake push both block on a channel the TEST
// controls. If an implementation regressed to calling either inline in a
// select case, the "monitor still processes records meanwhile" assertion
// below would time out and the suite would go red.

import (
	"context"
	"encoding/json"
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
	if msg.SpawnN != 1 {
		t.Fatalf("spawn fencing: got %d", msg.SpawnN)
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
	m.rtCh <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_test", SpawnN: 2}, Handle: h2}
	soon(t, 500*time.Millisecond, "recovery adopted", func() bool {
		return m.currentSpawn() == 2
	})
	h2.die()
	msg2 := waitOB(t, ob, "WINeedsRecovery for the new attempt")
	if msg2.SpawnN != 2 {
		t.Fatalf("new derivation must fence its own messages: %+v", msg2)
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

func TestProviderErrorNonFatalChainStillStormGuarded(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	m.writeExec(t, 1, "code")

	// NON-FATAL mid-turn reports (the turn has not ended on them):
	// each becomes an immediate infra_incident event, and the recovery
	// decision stays storm-guarded — the same step needs two consecutive
	// incidents before one WINeedsRecovery goes out.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndErr("upstream API error (503)"))
	soon(t, 500*time.Millisecond, "first incident event", func() bool {
		return len(eventsOf(m, EvInfraIncident)) >= 1
	})
	expectNoOB(t, ob, 150*time.Millisecond, "one non-fatal report must not recover")

	// Second consecutive report in the same turn, a different error
	// text (the dedup keeps one report per failure): the chain reaches
	// two and the storm guard releases ONE recovery.
	feed(h, fxMsgEndErr("upstream API error (429)"))
	msg := waitOB(t, ob, "WINeedsRecovery(provider_error)")
	if msg.Reason != protocol.RecoverProviderError {
		t.Fatalf("reason: got %s", msg.Reason)
	}
}

func TestProviderErrorCleanTurnResetsChain(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	m.writeExec(t, 1, "code")

	// A single NON-FATAL report per turn, each turn ending clean: the
	// clean turn_end resets the chain, so the strikes never accumulate.
	strike := func(detail string) {
		feed(h, fxTurnStart)
		feed(h, fxMsgEndErr(detail))
		feed(h, fxMsgEndStop)
		feed(h, fxTurnEndStop)
	}
	strike("upstream API error (503)")
	soon(t, 500*time.Millisecond, "first incident event", func() bool {
		return len(eventsOf(m, EvInfraIncident)) >= 1
	})
	strike("upstream API error (503)")
	expectNoOB(t, ob, 250*time.Millisecond, "a clean turn between failures starts a new chain; two isolated strikes never accumulate")
}

func TestFatalProviderErrorRecoversInstantly(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	m.writeExec(t, 1, "code")

	const detail = "upstream API error (503)"
	// ONE full failed turn — the turn ENDS on the provider error
	// (turn_end stopReason=error) — is the whole judgment: the F5
	// pilot shape. The harness's RPC session does not self-retry
	// after a fatal turn error, so waiting for a second consecutive
	// incident is waiting for an event that never comes.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndErr(detail))
	feed(h, fxTurnEndErr(detail))
	msg := waitOB(t, ob, "WINeedsRecovery(provider_error)")
	if msg.Reason != protocol.RecoverProviderError {
		t.Fatalf("reason: got %s", msg.Reason)
	}
	soon(t, 500*time.Millisecond, "needs_recovery event written", func() bool {
		return len(eventsOf(m, EvNeedsRecovery)) == 1
	})

	// Storm guard: the same attempt stays silent — a further fatal
	// turn is logged as an incident but never produces a second
	// recovery (“已发后静默到恢复完成”).
	feed(h, fxTurnStart)
	feed(h, fxMsgEndErr(detail))
	feed(h, fxTurnEndErr(detail))
	expectNoOB(t, ob, 250*time.Millisecond, "fatal recovery fires exactly once per attempt")
}

// ---------------------------------------------------------------------------
// idle watchdog (F5): quiet stream with NO turn in flight → probe →
// recover-or-restart, with the needs_input and RECOVERING exemptions.

func TestIdleWatchdogRecoversOnce(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
	})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		if msg.Method != protocol.ControlProbe {
			t.Errorf("idle watchdog must send pf/probe, got %s", msg.Method)
		}
		return protocol.ControlAck{}, errors.New("no ack: session wedged")
	})

	// A turn completes cleanly, then the stream goes quiet with NO turn
	// in flight — outside the hang window by definition. Past the idle
	// window the watchdog probes; the probe goes unacknowledged.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	feed(h, fxTurnEndStop)

	msg := waitOB(t, ob, "WINeedsRecovery(idle)")
	if msg.Reason != protocol.RecoverIdle {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverIdle)
	}
	if !hasIncidentKind(m, IdleSuspected) {
		t.Fatal("idle_suspect infra incident must be written")
	}

	// Storm guard: the recovery fired once and only once — RECOVERING
	// suppresses every further idle probe and signal for this attempt.
	expectNoOB(t, ob, 150*time.Millisecond, "idle recovery fires exactly once")
	if h.sentCount() != 1 {
		t.Fatalf("exactly one confirmation probe, got %d", h.sentCount())
	}
}

func TestIdleProbeAckedNoRecovery(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
	})
	var probes atomic.Int64
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		probes.Add(1)
		return protocol.ControlAck{Method: msg.Method, OK: true}, nil
	})

	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	feed(h, fxTurnEndStop)

	// The window expires, the probe is answered: the agent is alive but
	// on legal slow work — the window restarts and nothing recovers.
	// No probe storm either: a restarted window spaces the probes.
	time.Sleep(150 * time.Millisecond)
	if probes.Load() == 0 {
		t.Fatal("the probe must have been sent after the idle window")
	}
	if probes.Load() > 4 {
		t.Fatalf("probe storm: %d probes in one restarted window set", probes.Load())
	}
	expectNoOB(t, ob, 100*time.Millisecond, "a live-but-quiet session never recovers")
	if !hasIncidentKind(m, IdleCleared) {
		t.Fatal("idle_cleared must be written when the probe is acknowledged")
	}
}

func TestIdleWatchdogRecoversWithNoStream(t *testing.T) {
	h := newFakeHandle("att_1")
	_, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
	})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("no ack: session wedged")
	})

	// The starved-stream shape: not ONE record ever arrived (the idle
	// anchor — set at attach — is the window baseline), no turn is in
	// flight. Past the window: probe, no ack, one recovery.
	msg := waitOB(t, ob, "WINeedsRecovery(idle)")
	if msg.Reason != protocol.RecoverIdle {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverIdle)
	}
	expectNoOB(t, ob, 150*time.Millisecond, "idle recovery fires exactly once")
	if h.sentCount() != 1 {
		t.Fatalf("exactly one confirmation probe, got %d", h.sentCount())
	}
}

func TestIdleExemptWhilePendingQuestion(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
	})

	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	feed(h, fxTurnEndStop)

	// Arm a pending question (flush(needs_input)): waiting for a human
	// answer is a designed standstill — never idle.
	resp := make(chan protocol.FlushAck, 1)
	m.flushCh <- FlushCall{
		Req: protocol.FlushRequest{
			WorkItem:   "wi_test",
			SpawnN:     1,
			Reason:     protocol.FlushNeedsInput,
			QuestionID: "q_1",
			Question:   "which way?",
			Step:       "code",
		},
		Resp: resp,
	}
	select {
	case <-resp:
	case <-time.After(2 * time.Second):
		t.Fatal("flush(needs_input) was never answered")
	}

	time.Sleep(150 * time.Millisecond)
	if h.sentCount() != 0 {
		t.Fatalf("no idle probe while a question is pending, got %d", h.sentCount())
	}
	expectNoOB(t, ob, 100*time.Millisecond, "needs_input is exempt from the idle judgment")
}

func TestIdleSuppressedWhileRecovering(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
	})

	// Enter RECOVERING deterministically: one fatal provider error is an
	// instant recovery, no wall-clock involved.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndErr("upstream API error (503)"))
	feed(h, fxTurnEndErr("upstream API error (503)"))
	msg := waitOB(t, ob, "WINeedsRecovery(provider_error)")
	if msg.Reason != protocol.RecoverProviderError {
		t.Fatalf("reason: got %s", msg.Reason)
	}

	// A clean turn closes the stream state (no turn in flight, fresh
	// last record), then the idle window elapses: RECOVERING suppresses
	// the idle judgment — no probe, no second signal.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	feed(h, fxTurnEndStop)
	time.Sleep(150 * time.Millisecond)
	if h.sentCount() != 0 {
		t.Fatalf("no idle probe while RECOVERING, got %d probes", h.sentCount())
	}
	expectNoOB(t, ob, 100*time.Millisecond, "RECOVERING suppresses the idle watchdog")
	if hasIncidentKind(m, IdleSuspected) {
		t.Fatal("no idle_suspect may be written while RECOVERING")
	}
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
	fc := FlushCall{Req: protocol.FlushRequest{SpawnN: 1, Reason: protocol.FlushNeedsInput, QuestionID: "q1", Question: "A or B?"}, Resp: make(chan protocol.FlushAck, 1)}
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
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			pushes <- struct{}{}
			<-release // the "long" push
			return "sha_slow", true, gitops.PushTimings{}, nil
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
	fc := FlushCall{Req: protocol.FlushRequest{SpawnN: 1, Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped}, Resp: make(chan protocol.FlushAck, 1)}
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
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			pushes.Add(1)
			return "sha_stag", true, gitops.PushTimings{}, nil
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
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			pushes.Add(1)
			return "sha_bump", true, gitops.PushTimings{}, nil
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
	// The async observe-flush published variant carries the timing fields
	// (here the fake's zero timings — presence is the assertion).
	for _, e := range eventsOf(m, EvPublished) {
		eventBodyInt64(t, e, "commit_ms")
		eventBodyInt64(t, e, "push_ms")
	}
	if evs := eventsOf(m, EvVersionStagnant); len(evs) != 0 {
		t.Fatalf("a resolved episode must leave no event, got %d", len(evs))
	}
	time.Sleep(40 * time.Millisecond)
	if n := pushes.Load(); n != 2 {
		t.Fatalf("no fallback commit may fire after a bump, pushes=%d", n)
	}
}

// ---------------------------------------------------------------------------
// F15 progress semantics: UI noise is not life. The three pinned shapes
// of the pilot's double-blind wedge.

// fxExtUI is the extension-UI chatter record the pilot probe measured
// streaming continuously out of a background-task extension while the
// agent's turn was stuck.
const fxExtUI = `{"type":"extension_ui_request","request":{"method":"setStatus","payload":"…"},"seq":0}`

// TestHangDetectionSurvivesUINoise is shape A — the on-machine incident:
// a turn is open, NO progress record arrives, but the extension UI keeps
// streaming records continuously. Under the old "any record is life"
// rule the hang window never armed (and the turn being open held off the
// idle watchdog too — the double blind). Under the F15 rule the noise is
// invisible to the watchdogs: hang suspects, probes, the probe fails,
// one WINeedsRecovery(hang).
func TestHangDetectionSurvivesUINoise(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("no ack: agent wedged")
	})

	// The turn opens (progress stops here), then the UI chatter floods.
	feed(h, fxTurnStart)
	feed(h, fxMsgStart)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				feed(h, fxExtUI)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	msg := waitOB(t, ob, "WINeedsRecovery(hang) through UI noise")
	if msg.Reason != protocol.RecoverHang {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverHang)
	}
	if !hasIncidentKind(m, HangSuspected) {
		t.Fatal("hang_suspected must be written")
	}
}

// TestSlowProgressTurnNotHang is shape B — legal slow work: a turn is
// open and ordinary progress records keep arriving. No watchdog fires,
// no probe is sent, nothing recovers.
func TestSlowProgressTurnNotHang(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, nil)
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		t.Errorf("no probe may be sent while progress flows, got %s", msg.Method)
		return protocol.ControlAck{Method: msg.Method, OK: true}, nil
	})

	feed(h, fxTurnStart)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				feed(h, fxTextDelta) // message_update: real turn progress
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	expectNoOB(t, ob, 250*time.Millisecond, "a progressing turn never recovers")
	if h.sentCount() != 0 {
		t.Fatalf("no probe while progress flows, got %d", h.sentCount())
	}
	for _, kind := range incidentKinds(m) {
		if kind == HangSuspected || kind == IdleSuspected {
			t.Fatalf("no watchdog may suspect a progressing turn, saw %s", kind)
		}
	}
}

// TestIdleFiresThroughUINoise is shape C — the idle backstop with the
// noise ingredient: no turn in flight, no progress record, but the
// extension UI keeps chattering. The idle watchdog (anchored on
// lastProgressAt since F15) still arms, probes and recovers; the noise
// cannot hold the window open the way it held the old lastRecordAt.
func TestIdleFiresThroughUINoise(t *testing.T) {
	h := newFakeHandle("att_1")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
	})
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("no ack: session wedged")
	})

	// A turn completes cleanly, then only UI chatter arrives.
	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	feed(h, fxTurnEndStop)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				feed(h, fxExtUI)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	msg := waitOB(t, ob, "WINeedsRecovery(idle) through UI noise")
	if msg.Reason != protocol.RecoverIdle {
		t.Fatalf("reason: got %s, want %s", msg.Reason, protocol.RecoverIdle)
	}
	if !hasIncidentKind(m, IdleSuspected) {
		t.Fatal("idle_suspect must be written")
	}
}

// TestNoProgressEventAndThrottle pins the F15c owner face: progress
// silence past the idle window lands ONE no_progress event per window in
// the events log (whatever the turn state), keeps reporting at window
// cadence while the stall continues, goes quiet while progress flows,
// and reports again when a fresh silent episode begins.
func TestNoProgressEventAndThrottle(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
		c.HangWindow = time.Hour // keep the hang lane out of this test
		h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
			return protocol.ControlAck{Method: msg.Method, OK: true}, nil
		})
	})

	// A clean turn, then silence. The first no_progress lands once...
	feed(h, fxTurnStart)
	feed(h, fxMsgEndStop)
	feed(h, fxTurnEndStop)
	soon(t, 500*time.Millisecond, "first no_progress event", func() bool {
		return len(eventsOf(m, EvNoProgress)) == 1
	})
	// ...and a continuing stall keeps reporting at window cadence (the
	// idle probe acks restart the window; the throttle spaces the
	// events), never more than one per window.
	time.Sleep(150 * time.Millisecond)
	if n := len(eventsOf(m, EvNoProgress)); n < 2 || n > 5 {
		t.Fatalf("no_progress cadence: %d events over ~4 windows, want 2..5", n)
	}

	// Progress resumes and keeps flowing: the gate reads lastProgressAt,
	// so no further event lands while the stream proves life.
	base := len(eventsOf(m, EvNoProgress))
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				feed(h, fxTextDelta)
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	time.Sleep(150 * time.Millisecond)
	if n := len(eventsOf(m, EvNoProgress)); n != base {
		t.Fatalf("no no_progress while progress flows: %d -> %d", base, n)
	}
	close(stop)

	// Silence again: a fresh episode reports again.
	soon(t, 500*time.Millisecond, "no_progress after the fresh episode", func() bool {
		return len(eventsOf(m, EvNoProgress)) > base
	})
	// The event body carries the facts the owner face renders.
	e := eventsOf(m, EvNoProgress)[0]
	var body map[string]any
	if err := json.Unmarshal(e.Body, &body); err != nil {
		t.Fatalf("no_progress body: %v", err)
	}
	for _, key := range []string{"silent_for_ms", "window_ms", "in_flight", "spawn_n"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("no_progress body must carry %q: %s", key, e.Body)
		}
	}
}

// TestNoProgressExemptWhilePendingQuestion: needs_input is designed
// stillness — the no_progress owner face stays quiet while a human
// answer is pending (the QUESTION column already owns that signal).
func TestNoProgressExemptWhilePendingQuestion(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.IdleWindow = 40 * time.Millisecond
		c.HangWindow = time.Hour
	})

	resp := make(chan protocol.FlushAck, 1)
	m.flushCh <- FlushCall{
		Req: protocol.FlushRequest{
			WorkItem: "wi_test", SpawnN: 1, Reason: protocol.FlushNeedsInput,
			QuestionID: "q_np", Question: "which way?", Step: "code",
		},
		Resp: resp,
	}
	select {
	case <-resp:
	case <-time.After(2 * time.Second):
		t.Fatal("flush(needs_input) was never answered")
	}

	time.Sleep(150 * time.Millisecond)
	if n := len(eventsOf(m, EvNoProgress)); n != 0 {
		t.Fatalf("no no_progress while a question is pending, got %d", n)
	}
}
