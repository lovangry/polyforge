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
			AttemptID:  "att_1",
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
