package observe

// monitor.go: the per-WI monitor goroutine — the observe lane's eyes and
// brain (v1-spec-full §四). ONE goroutine watches ONE work item; its ONLY
// execution shape is a select loop whose CASES NEVER BLOCK. Every long
// operation — a push, an ack wait, a liveness probe — runs in a
// sub-goroutine and reports back through a buffered channel that the
// select drains on a later pass. The gate (a size-1 channel used as a
// semaphore) is the per-WI "only one push in flight" invariant.
//
// Input sources drained by the select:
//   - the agent stdout record stream (Handle.Records) → classification;
//   - the confirmed process-exit signal (Handle.Exit) → death detection;
//   - the execution.json poll (~500ms tick; mtime+Version change);
//   - RT messages (WIRecovered rides rtCh; WIFinished rides stopCh);
//   - CLI answer requests and agent flush calls (HTTP face → channels);
//   - results of the sub-goroutines (push/probe/answer/wrap);
//   - the stop signal.
//
// RECOVERING: after WINeedsRecovery is sent the monitor enters the
// RECOVERING state and suppresses every further recovery signal of the
// same derivation (storm guard) until RTRecovered arrives with a new
// handle. The select keeps running — the old handle's channels must
// keep being drained or Runtime's stdout reader would block forever —
// but no detection consequence fires.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// wrapRetries is the §五 bound: a wrap flush retries three times before
// the work item is transitioned to blocked.
const wrapRetries = 3

// sameErrorCap is the V8-repair retry bound: after this many consecutive
// async push failures carrying the IDENTICAL error, the retry round is
// halted (one degraded push_backpressure event with reason "same_error")
// until the next commit boundary — an execution.json Version bump — or a
// successful push re-arms it. The pilot's poison-chain storm (#48) was
// 185 consecutive identical failures at a ~10s cadence with no bound;
// the cap turns every such deterministic-failure shape into at most
// sameErrorCap attempts per round. Wrap/seal pushes are already bounded
// (wrapRetries) and are not affected.
const sameErrorCap = 5

// defaultVersionStagnantTicks is the belt-and-braces bound for C3': the
// number of consecutive execution.json REWRITES (mtime advanced) that
// carry an unchanged top-level Version before the monitor also treats
// the capsule as dirty and schedules a commit anyway — plus one
// version_stagnant event per episode. The Version-driven rule stays the
// primary archival semantic; this only catches an agent that violates it.
const defaultVersionStagnantTicks = 3

// PushFunc is the injectable capsule commit+push operation (the seam the
// tests use; production binds gitops.CommitPush). ctx bounds the call;
// returning ctx.Err() reports the attempt as failed while the underlying
// git keeps running — the per-WI git lock serializes any successor. The
// returned timings are the observed-only latency samples (commit_ms /
// push_ms) the published event bodies carry; nothing judgmental consumes
// them.
type PushFunc func(ctx context.Context, o gitops.Opts, message string) (localSHA string, pushed bool, timings gitops.PushTimings, err error)

// WIInfo carries the per-WI static facts a monitor needs; supplied by
// Runtime alongside the RTStarted protocol message.
type WIInfo struct {
	WorkItem   string
	Project    string
	CapsuleDir string
	BundleName string
	Git        gitops.Opts
	// Transcript is the capsule-side declared session path of the
	// current derivation (session/session-<instance>.jsonl): the
	// live-transcript sync's destination and the recovery engine's
	// resume source.
	Transcript string
	// LiveTranscript is the file the current derivation appends to (the
	// runtime's recorded live path; "" = locate on demand below).
	LiveTranscript string
	// SpawnAt is the current derivation's birth moment (the on-demand
	// locator's floor).
	SpawnAt time.Time
}

// Handle is the agent face a monitor watches. Implemented by the Runtime
// lane's process adapter; Observe owns no process code. The handle rides
// the RT channel WITH the protocol lifecycle message it belongs to — the
// five messages stay the only control vocabulary, and the handle is the
// attachment Observe needs to observe and control.
type Handle interface {
	// Exit fires ONCE when the underlying agent process has exited. The
	// adapter waits on the process first, so the signal is ALREADY the
	// confirmed death ("确认后" — no second confirmation needed).
	Exit() <-chan struct{}
	// Records yields the raw stdout record blobs (one JSON record per
	// delivery); the channel closes when the stream ends.
	Records() <-chan []byte
	// SendControl writes one stdio control-channel message (pf/answer,
	// pf/probe, pf/stop_notice) and waits for the agent's ack, bounded
	// by ctx. Contract: it returns ErrAnswerAckPending when the message
	// was WRITTEN but the ack did not arrive within ctx (delivered,
	// consumption pending); any other error is a delivery failure.
	SendControl(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error)
}

// AnswerCall is one CLI answer submission routed to the monitor.
type AnswerCall struct {
	Relay  AnswerRelay
	Caller string
	Resp   chan error // buffered 1; receives the settled relay outcome
}

// FlushCall is one agent flush(reason) routed to the monitor.
type FlushCall struct {
	Req  protocol.FlushRequest
	Resp chan protocol.FlushAck // buffered 1
}

type pushResult struct {
	version  int64
	sha      string
	pushed   bool
	stagnant bool // the push was scheduled by the stagnation fallback
	timings  gitops.PushTimings
	err      error
}

type probeResult struct {
	ack protocol.ControlAck
	err error
}

type wrapResult struct {
	ok          bool
	refused     bool // the V4 evidence gate refused the seal before any push attempt
	sha         string
	pushed      bool
	timings     gitops.PushTimings // the settled (successful, or last) attempt's latency samples
	err         string
	version     int64
	finalStatus string
	call        FlushCall
}

// monitorCfg carries the tunables (defaults in observe.go Config).
type monitorCfg struct {
	PollInterval time.Duration
	HangWindow   time.Duration
	// IdleWindow bounds the F5 idle watchdog (zero = disabled — a
	// zero window would fire on every tick; production default 10min).
	IdleWindow        time.Duration
	ProbeTimeout      time.Duration
	AnswerAckWindow   time.Duration
	PushRetryBackoff  time.Duration
	BackpressureSteps int
	BackpressureAfter time.Duration
	FlushBound        time.Duration
	// VersionStagnantTicks is the C3' belt-and-braces bound (0 = the
	// default 3): consecutive same-Version rewrites before a fallback
	// commit. Injectable so tests shrink the window.
	VersionStagnantTicks int
	// revalidate is the v1.5 M4 session-verify cadence (0 = disabled:
	// no verifier, no checks — the unit-test default).
	revalidate time.Duration
	// verify is the M4 aihub seam (nil = revalidation disabled).
	verify SessionVerifier
	push   PushFunc
	// locateLive resolves the live harness transcript on demand (F3);
	// nil = the transcript sync is disabled.
	locateLive func(capsuleDir string, spawnAt time.Time) string
}

// Monitor supervises one work item.
type Monitor struct {
	wi   WIInfo
	cfg  monitorCfg
	log  *EventLog
	ob   chan<- protocol.OBMsg
	ctrl int64 // control-message correlation counter (atomic): the ack wait's (method, correlation) dual key

	// handle attachment (monitor-goroutine only; captured into spawned
	// sub-goroutines at spawn time).
	handle Handle
	// spawnN is the v1.5 B2 derivation counter of the CURRENTLY ADOPTED
	// derivation (guarded by stateMu for the HTTP fence's read). It is
	// fed from RTMsg.SpawnN at RTStarted/RTRecovered adoption; the flush
	// fence (http.go serveFlush) compares FlushRequest.SpawnN against it
	// and against the hub's latest ANNOUNCED counter (Observe.spawnKnown).
	spawnN int
	// sessionID is the v1.5 session secret carried in-process on the
	// RTStarted/RTRecovered message (M4 revalidation input). It lives
	// ONLY here — never in events, never in the capsule, never in a log
	// line — and is read solely by checkRevalidate on this goroutine
	// (set at attach/onRecovered, both monitor-goroutine contexts).
	sessionID string
	records   <-chan []byte
	exitCh    <-chan struct{}
	stream    *streamState

	rtCh     chan Attach
	stopCh   chan struct{}
	stop     sync.Once
	answerCh chan AnswerCall
	flushCh  chan FlushCall
	// deferredFlush holds one flush call whose (serveFlush-validated)
	// spawn_n names the hub's LATEST ANNOUNCED derivation while the
	// monitor still holds its predecessor — the recovery-adoption window.
	// The call is released by onRecovered once the announced counter is
	// adopted; the fence never lets an OLDER counter reach this field.
	deferredFlush *FlushCall

	// execution.json polling state (monitor goroutine only).
	execPath      string
	execMtime     time.Time
	currentStep   string
	dirtyVersion  int64 // latest Version seen that has no confirmed push
	pushedVersion int64
	// C3' belt-and-braces stagnation tracking: lastVersion/lastSeen are the
	// latest Version observed in the file and whether one was seen at
	// all; stagnantTicks counts consecutive rewrites that did not bump
	// it; stagnantDirty arms the fallback commit once the bound is hit.
	lastSeen      bool
	lastVersion   int64
	stagnantTicks int
	stagnantDirty bool

	// the single-in-flight push gate.
	gate     chan struct{}
	pushDone chan pushResult
	wrapDone chan wrapResult

	// backpressure bookkeeping (monitor goroutine only).
	failedPushes int
	firstFailAt  time.Time
	lastPushOK   time.Time
	pushDegraded bool
	timeDegraded bool
	retryAt      time.Time
	// same-error retry-cap bookkeeping (V8 repair; monitor goroutine
	// only): lastPushErr/sameErrStreak count consecutive failures with
	// the identical error text; retryHalted/retryHaltedAt mark the
	// halted round and the Version it halted at (a later dirtyVersion is
	// the next commit boundary that re-arms the round).
	lastPushErr   string
	sameErrStreak int
	retryHalted   bool
	retryHaltedAt int64

	// hang detection.
	probeInFlight bool
	probeDone     chan probeResult
	// hangAnchor is the hang window's probe-cleared restart point: the
	// moment the last ACKED liveness probe answered while a turn was in
	// flight. The criterion itself is anchored on lastProgressAt (the
	// F15 semantics: UI-noise records never count as life), so an anchor
	// is only needed to restart the window after a probe the agent
	// survived.
	hangAnchor time.Time
	// idle detection (F5). idleAnchor is the idle window's baseline
	// when the stream has produced no progress record yet (set at
	// attach/recovery adoption) and its restart point after a probe the
	// agent answered; like the hang criterion, the measured clock is
	// lastProgressAt, never the noise clock. probeIdle remembers WHICH
	// criterion armed the in-flight probe so the result lands as the
	// right recovery reason.
	idleAnchor time.Time
	probeIdle  bool
	// noProgressAt is the F15c no_progress event's throttle stamp: the
	// last emission of the owner-facing progress-silence event (zero =
	// none this attempt). At most one event per idle window; cleared by
	// a probe round and by recovery adoption (resumed progress clears it
	// implicitly — the gate reads lastProgressAt first).
	noProgressAt time.Time

	// live-transcript sync state (F3; monitor goroutine only — the push
	// sub-goroutines receive the values as arguments, never over m).
	trDst            string    // the capsule's declared session path
	trSrc            string    // the runtime-recorded live path (may be "")
	trSpawnAt        time.Time // the derivation's birth moment
	transcriptWarned bool      // one transcript_missing event per attempt

	// ledger is the hub's SHARED, daemon-wide answer ledger (one
	// answers.json for the whole daemon, keyed per work item inside),
	// handed down by Observe.newMonitor. A monitor never owns one: an
	// answer accepted before a daemon restart must still be visible to
	// the monitor that comes back for the same work item. Unit tests
	// that build a monitor directly hand it a memory-only ledger.
	ledger *AnswerLedger

	// stateMu guards the fields every goroutine (HTTP view, flush/answer
	// sub-goroutines) reads.
	stateMu    sync.Mutex
	pending    *protocol.PendingView
	sealing    bool
	wrapped    bool // attempt reached a terminal state via flush(wrap)
	blocked    bool
	recovering bool
	// superseded is the v1.5 M4 state: the periodic session-verify
	// answered matches=false — another daemon holds the work item. The
	// monitor stops supervising (detection, pushes and revalidation are
	// suppressed; flushes are answered with the STALE_SPAWN refusal) but
	// keeps draining its channels until Runtime's release (RTFinished)
	// stops the loop. Observe makes NO hub transition (the new owner
	// owns it). stateMu-guarded like the rest of the suppression set.
	superseded bool

	// revalidation bookkeeping (M4; monitor goroutine only except where
	// noted). verifyDone is created with the monitor (never nil) so the
	// select case below is always backed by a real channel.
	lastVerifyAt   time.Time
	verifyInFlight bool
	verifyDone     chan verifyResult
}

func (m *Monitor) execJSONPath() string {
	return filepath.Join(m.wi.CapsuleDir, capsule.ExecutionJSON)
}

// ---------------------------------------------------------------------------
// main loop

func (m *Monitor) run() {
	defer func() {
		_ = m.log.Append(m.wi.WorkItem, EvJobStopped, map[string]any{
			"spawn_n": m.currentSpawn(),
		})
	}()
	_ = m.log.Append(m.wi.WorkItem, EvJobStarted, map[string]any{
		"spawn_n": m.spawnN,
	})
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case at := <-m.rtCh:
			m.onRecovered(at)
		case <-m.exitCh:
			m.onExit()
		case rec, ok := <-m.records:
			if !ok {
				m.records = nil // stream end; Exit carries the death signal
				continue
			}
			m.stream.note(rec)
		case <-ticker.C:
			m.onTick()
		case res := <-m.pushDone:
			m.onPushResult(res)
		case wr := <-m.wrapDone:
			m.onWrapResult(wr)
		case pr := <-m.probeDone:
			m.onProbeResult(pr)
		case res := <-m.verifyDone:
			m.onVerifyResult(res)
		case call := <-m.answerCh:
			m.onAnswer(call)
		case call := <-m.flushCh:
			m.onFlush(call)
		}
	}
}

// stop requests the monitor loop to exit (idempotent).
func (m *Monitor) requestStop() { m.stop.Do(func() { close(m.stopCh) }) }

// onTick is the ~500ms heartbeat: drain stream incidents, check the hang
// criterion, poll execution.json, maybe schedule a push, check
// backpressure.
func (m *Monitor) onTick() {
	m.releaseDeferredFlush()
	m.drainIncidents()
	m.checkHang()
	m.checkIdle()
	m.checkNoProgress()
	m.pollExecution()
	m.schedulePush()
	m.checkBackpressure()
	m.checkRevalidate()
}

// releaseDeferredFlush re-examines a flush parked in the adoption window:
// the adoption it waits for may have completed between the serveFlush
// fence and this monitor pass (the fence read the OLD current counter,
// onRecovered ran, and only then did the call reach onFlush) — a
// deferred call stranded that way is released here within one poll
// tick instead of waiting out the flush bound. A call whose counter is
// still ahead stays parked; onRecovered owns the immediate release path.
func (m *Monitor) releaseDeferredFlush() {
	if m.deferredFlush == nil {
		return
	}
	call := *m.deferredFlush
	if call.Req.SpawnN == m.spawnN {
		m.deferredFlush = nil
		m.onFlush(call)
	}
}

// ---------------------------------------------------------------------------
// detection: provider errors (fatal → instant recovery; the chain
// stays the storm-guarded backstop for non-fatal reports)

// drainIncidents is the provider-error detection surface. Every
// classified incident becomes an IMMEDIATE infra_incident event (the
// operator learns about a dead provider in the same tick the harness
// reported it — v0 #48's "provider-error instant incidents"), while
// the RECOVERY decision splits on fatality:
//
//   - FATAL (the turn ENDED on the error: turn_end stopReason=error) →
//     WINeedsRecovery fires on THIS single event, outside the
//     storm-count chain. The v1 pilot (F5) proved the decision must
//     be as instant as the news: a real harness's RPC session does
//     NOT self-retry after a fatal turn error — the turn is wrongly
//     terminated, nobody sends the next prompt, and the session sits
//     permanently idle (35+ minutes observed on the real machine).
//     The storm guard's "2 consecutive incidents in the same step"
//     then waits for a second event that never comes, and the hang
//     window is blind too (its criterion is silence WITH a turn in
//     flight; the turn already ended). One fatal event is the whole
//     judgment.
//   - non-fatal (a mid-turn report the harness may still recover
//     from) → the existing event face only, plus the storm-guarded
//     chain: the same step needs two consecutive incidents — counted
//     in stream order — before one WINeedsRecovery goes out.
func (m *Monitor) drainIncidents() {
	fatalDetail := ""
	for _, in := range m.stream.drain() {
		_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
			"kind": in.Kind, "detail": in.Detail, "fatal": in.Fatal,
			"at": in.At.UTC().Format(time.RFC3339Nano),
		})
		if in.Fatal && fatalDetail == "" {
			fatalDetail = in.Detail
		}
	}
	if fatalDetail != "" {
		// Single-event recovery: the fatal incident never joins the
		// chain arithmetic (already judged here); reset it so the
		// drained reports cannot re-fire the storm path. The storm guard
		// in needRecovery keeps this attempt silent until recovery lands.
		m.stream.resetChain()
		step := m.currentStep
		if step == "" {
			step = "-"
		}
		m.needRecovery(protocol.RecoverProviderError,
			fmt.Sprintf("step %s: fatal provider error ended the turn: %s",
				step, fatalDetail))
		return
	}
	if m.stream.chainValue() >= 2 {
		m.stream.resetChain()
		step := m.currentStep
		if step == "" {
			step = "-"
		}
		m.needRecovery(protocol.RecoverProviderError,
			fmt.Sprintf("step %s: 2 consecutive stream incidents; latest: %s",
				step, m.stream.latestIncidentDetail()))
	}
}

// ---------------------------------------------------------------------------
// detection: death

// onExit handles the CONFIRMED process-exit signal (the handle adapter
// already waited on the process — this is the confirmation). An expected
// exit (wrap sealed, blocked, or a signal already sent for this attempt)
// is silent.
func (m *Monitor) onExit() {
	m.exitCh = nil // fires once per handle
	m.stateMu.Lock()
	suppressed := m.recovering || m.wrapped || m.blocked || m.sealing || m.superseded
	m.stateMu.Unlock()
	if suppressed {
		return
	}
	m.needRecovery(protocol.RecoverDeath, "agent process exited")
}

// needRecovery is the single choke point for the storm guard: after one
// WINeedsRecovery is sent for an attempt, every further signal of the
// SAME attempt is suppressed ("已发后静默到恢复完成") — the select keeps
// draining, the Runtime sees exactly one message. SEALING likewise drops
// same-attempt signals (§三).
func (m *Monitor) needRecovery(reason, detail string) {
	m.stateMu.Lock()
	if m.recovering || m.wrapped || m.blocked || m.sealing || m.superseded {
		m.stateMu.Unlock()
		return
	}
	m.recovering = true
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvNeedsRecovery, map[string]any{
		"reason": reason, "detail": detail, "spawn_n": m.currentSpawn(),
	})
	m.sendOB(protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, SpawnN: m.currentSpawn(),
		WorkItem: m.wi.WorkItem, Reason: reason, Detail: detail,
	})
}

// sendOB is non-blocking by construction: the monitor may never block in
// a case, so a full OB channel drops the message (with an event) rather
// than stalling supervision. Runtime drains the channel in production.
func (m *Monitor) sendOB(msg protocol.OBMsg) {
	select {
	case m.ob <- msg:
	default:
		_ = m.log.Append(m.wi.WorkItem, EvNeedsRecovery, map[string]any{
			"dropped": msg.Kind, "reason": "OB channel full",
		})
	}
}

// ---------------------------------------------------------------------------
// detection: hang (stream silence with a turn in flight → probe → kill)

func (m *Monitor) checkHang() {
	if m.probeInFlight {
		return
	}
	// The ruled criterion: silence WHILE A TURN IS IN FLIGHT. An idle
	// agent (waiting for work or for a human answer) is never a hang.
	if !m.stream.heartbeat().Silent(time.Now(), m.cfg.HangWindow, m.hangAnchor) {
		return
	}
	m.stateMu.Lock()
	suppressed := m.recovering || m.wrapped || m.blocked || m.sealing || m.superseded
	m.stateMu.Unlock()
	if suppressed {
		return
	}
	m.probeInFlight = true
	m.probeIdle = false
	_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
		"kind": HangSuspected,
		"detail": fmt.Sprintf("progress-silent past the %.0fs window with a turn in flight; sending liveness probe",
			m.cfg.HangWindow.Seconds()),
	})
	handle := m.handle // captured: no race with a concurrent recovery
	nonce, _ := randomNonce()
	// the correlation tokens the ack wait matches on: the nonce (echoed
	// as Ref) and the numeric seq
	ctrlSeq := atomic.AddInt64(&m.ctrl, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.ProbeTimeout)
		defer cancel()
		ack, err := handle.SendControl(ctx, protocol.ControlMsg{
			Method: protocol.ControlProbe, Nonce: nonce, Seq: ctrlSeq,
		})
		m.probeDone <- probeResult{ack: ack, err: err}
	}()
}

// checkIdle is the F5 idle watchdog — the general backstop the hang
// criterion is structurally blind to. v0 #44's runtime watchdog judged
// ACTIVITY (procfs I/O/CPU motion), so it caught an agent that was
// merely idle; the v1 port kept only the in-flight criterion (stream
// silence WITH a turn open) and lost that layer. The pilot's F5 shape
// is exactly the hole: a fatal provider error ENDS the turn (fatal
// recovery now catches that on the spot), but any other path that
// leaves the session running with no open turn and no arriving prompt
// — a wrongly-terminated turn, a lost driver — leaves the stream
// quiet with turnDepth 0, which no in-flight window ever covers.
// Criterion: no PROGRESS record (the F15 semantics: UI noise never
// counts) AND no in-flight turn for longer than IdleWindow ⇒ liveness
// probe (the same probe mechanism, the same ProbeTimeout bound) ⇒ no
// ack ⇒ WINeedsRecovery(idle); ack ⇒ the window restarts and nothing
// recovers (the agent is on legal slow work). Exemptions: a pending
// question (needs_input is a designed standstill — waiting for a human
// answer is not idleness) and every suppressed state (RECOVERING
// already shipped its one signal; the storm semantics are unchanged).
func (m *Monitor) checkIdle() {
	if m.cfg.IdleWindow <= 0 {
		return // disabled: a zero window would fire on every tick
	}
	if m.probeInFlight {
		return // one probe at a time, whichever criterion armed it
	}
	hb := m.stream.heartbeat()
	if hb.InFlight {
		return // a turn is open: the hang criterion owns that silence
	}
	eff := hb.LastProgressAt
	if m.idleAnchor.After(eff) {
		eff = m.idleAnchor // pre-first-record baseline / probe-cleared restart
	}
	if eff.IsZero() || time.Since(eff) < m.cfg.IdleWindow {
		return
	}
	m.stateMu.Lock()
	pending := m.pending != nil
	suppressed := m.recovering || m.wrapped || m.blocked || m.sealing || m.superseded
	m.stateMu.Unlock()
	if pending {
		return // needs_input: waiting for a human answer is designed stillness
	}
	if suppressed {
		return
	}
	m.probeInFlight = true
	m.probeIdle = true
	_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
		"kind": IdleSuspected,
		"detail": fmt.Sprintf("no progress record and no turn in flight past the %.0fs idle window; sending liveness probe",
			m.cfg.IdleWindow.Seconds()),
	})
	handle := m.handle // captured: no race with a concurrent recovery
	nonce, _ := randomNonce()
	ctrlSeq := atomic.AddInt64(&m.ctrl, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.ProbeTimeout)
		defer cancel()
		ack, err := handle.SendControl(ctx, protocol.ControlMsg{
			Method: protocol.ControlProbe, Nonce: nonce, Seq: ctrlSeq,
		})
		m.probeDone <- probeResult{ack: ack, err: err}
	}()
}

// checkNoProgress is the F15c owner face: the same progress-silence
// signal the hang/idle watchdogs judge (no PROGRESS record for longer
// than the idle window, REGARDLESS of whether a turn is open — an open
// turn hands the silence to the hang watchdog, a closed one to the
// idle watchdog, and the event covers both) also lands in the events
// log, so the owner's observe face shows a stalled job even while the
// watchdogs are still inside their probe/confirmation cycle — the
// pilot's wedge produced ZERO events and no owner-visible surface for
// 2.7 hours. Throttled to at most one event per idle window; cleared
// by a probe round and by recovery adoption, and resumed progress
// clears it implicitly (the gate reads lastProgressAt first). Exempt
// while a question is pending (needs_input already owns the QUESTION
// face) and in every suppressed state.
func (m *Monitor) checkNoProgress() {
	if m.cfg.IdleWindow <= 0 {
		return // disabled with the idle watchdog it shares the window with
	}
	hb := m.stream.heartbeat()
	eff := hb.LastProgressAt
	if m.idleAnchor.After(eff) {
		eff = m.idleAnchor // pre-first-record baseline / probe-cleared restart
	}
	if eff.IsZero() || time.Since(eff) < m.cfg.IdleWindow {
		return
	}
	m.stateMu.Lock()
	pending := m.pending != nil
	suppressed := m.recovering || m.wrapped || m.blocked || m.sealing || m.superseded
	m.stateMu.Unlock()
	if pending || suppressed {
		return
	}
	if !m.noProgressAt.IsZero() && time.Since(m.noProgressAt) < m.cfg.IdleWindow {
		return // throttle: at most one no_progress event per window
	}
	m.noProgressAt = time.Now()
	_ = m.log.Append(m.wi.WorkItem, EvNoProgress, map[string]any{
		"silent_for_ms": time.Since(eff).Milliseconds(),
		"window_ms":     m.cfg.IdleWindow.Milliseconds(),
		"in_flight":     hb.InFlight,
		"detail": fmt.Sprintf("no agent progress record for %.0fs (turn in flight: %v); UI noise is not progress",
			time.Since(eff).Seconds(), hb.InFlight),
		"spawn_n": m.currentSpawn(),
	})
}

func (m *Monitor) onProbeResult(pr probeResult) {
	m.probeInFlight = false
	idleProbe := m.probeIdle
	m.probeIdle = false
	// A probe round clears the no_progress throttle (F15c): the probe
	// itself was an intervention on the same silence the event reports,
	// and a fresh episode deserves a fresh event.
	m.noProgressAt = time.Time{}
	if pr.err == nil && pr.ack.OK {
		if idleProbe {
			// The agent answered with no turn open: legal slow work or
			// a live-but-quiet session. The idle window restarts;
			// nothing recovers.
			m.idleAnchor = time.Now()
			_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
				"kind": IdleCleared, "detail": "liveness probe acknowledged with no turn in flight; idle window restarted",
			})
			return
		}
		// The agent answered: kill cancelled, window restarts.
		m.hangAnchor = time.Now()
		_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
			"kind": HangCleared, "detail": "liveness probe acknowledged; hang window restarted",
		})
		return
	}
	if idleProbe {
		m.needRecovery(protocol.RecoverIdle,
			"no stream record and no turn in flight past the idle window; liveness probe failed")
		return
	}
	m.needRecovery(protocol.RecoverHang, "stream silent with turn in flight; liveness probe failed")
}

func randomNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------------------
// recovery lifecycle

// onRecovered adopts the new handle Runtime derived (RTRecovered). The
// monitor leaves RECOVERING, resets the detection surfaces and re-arms
// them against the new derivation.
func (m *Monitor) onRecovered(at Attach) {
	if at.Handle == nil || at.Msg.SpawnN <= 0 {
		return // malformed attachment (no announced derivation): never adopted
	}
	m.handle = at.Handle
	m.records = at.Handle.Records()
	m.exitCh = at.Handle.Exit()
	// The stream pointer is swapped under stateMu: the HTTP/status read
	// face reads it cross-goroutine (see lastProgressAt), and every other
	// use stays on this goroutine.
	m.stateMu.Lock()
	m.stream = newStreamState()
	m.stateMu.Unlock()
	m.probeInFlight = false
	m.probeIdle = false
	m.hangAnchor = time.Now()
	m.idleAnchor = time.Now()    // the successor derivation gets a fresh idle baseline
	m.noProgressAt = time.Time{} // ...and a fresh no_progress episode
	// The successor derivation carries its own transcript facts (its
	// declared capsule path, its live file, its spawn moment): adopt
	// them for the sync points that follow (F3).
	if at.WI.WorkItem != "" {
		m.setTranscriptInfo(at.WI)
	} else {
		m.stateMu.Lock()
		m.transcriptWarned = false
		m.stateMu.Unlock()
	}
	m.stateMu.Lock()
	m.recovering = false
	m.spawnN = at.Msg.SpawnN
	m.sessionID = at.Msg.SessionID
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvAgentRecovered, map[string]any{
		"spawn_n": at.Msg.SpawnN,
	})
	// Release the flush that arrived during the adoption window: the
	// counter it carries is now current, so it takes the ordinary path; a
	// counter that STILL does not match (a superseded announcement) is
	// answered with the stale-refusal shape — the fence's promise holds.
	if m.deferredFlush != nil {
		call := *m.deferredFlush
		m.deferredFlush = nil
		if call.Req.SpawnN == at.Msg.SpawnN {
			m.onFlush(call)
		} else {
			call.Resp <- protocol.FlushAck{
				OK: false, Error: "STALE_SPAWN: flush carries spawn_n " + strconv.Itoa(call.Req.SpawnN) +
					"; the current derivation is " + strconv.Itoa(at.Msg.SpawnN),
			}
		}
	}
	// F9: the successor may inherit answers that were delivered to its
	// dead predecessor but never consumed — re-send them now.
	m.redeliverAwaiting()
}

// ---------------------------------------------------------------------------
// execution.json polling + async commit/push

// pollExecution is the C3 engine: the agent writes execution.json and
// moves on (zero-blocking); the monitor notices mtime/Version changes on
// its ~500ms tick and triggers the async commit+push itself.
func (m *Monitor) pollExecution() {
	st, err := os.Stat(m.execPath)
	if err != nil {
		return
	}
	if !st.ModTime().After(m.execMtime) {
		return // mtime unchanged → not rewritten
	}
	data, err := os.ReadFile(m.execPath)
	if err != nil {
		return
	}
	var exec capsule.Execution
	if json.Unmarshal(data, &exec) != nil {
		return // torn/partial write: the next tick retries
	}
	m.execMtime = st.ModTime()
	if m.currentStep != "" && exec.CurrentStep != m.currentStep {
		// The incident chain is per-step: a step change starts a new one.
		// (A first observation from "" is not a change — incidents that
		// arrived before the first execution.json read belong to the
		// step the agent was already on.)
		m.stream.resetChain()
	}
	m.currentStep = exec.CurrentStep
	if exec.Version > m.dirtyVersion {
		m.dirtyVersion = exec.Version
	}
	// C3' belt-and-braces: the Version bump is the archival contract, but
	// an agent that rewrites the file WITHOUT bumping it must not starve
	// the capsule chain either. N consecutive same-Version rewrites (mtime
	// advanced, Version stagnant) arm one fallback commit and leave one
	// version_stagnant event; a real Version bump clears the episode. The
	// Version-driven rule above stays the primary semantic.
	if m.lastSeen && exec.Version == m.lastVersion {
		m.stagnantTicks++
		if m.stagnantTicks >= m.stagnantBound() {
			m.stagnantTicks = 0
			m.stagnantDirty = true
			_ = m.log.Append(m.wi.WorkItem, EvVersionStagnant, map[string]any{
				"version": exec.Version,
				"detail":  fmt.Sprintf("execution.json rewritten %d times without a Version bump; scheduling a fallback commit", m.stagnantBound()),
				"spawn_n": m.currentSpawn(),
			})
		}
	} else {
		m.stagnantTicks = 0
		if m.lastSeen && exec.Version > m.lastVersion {
			m.stagnantDirty = false // the primary semantic resumed
		}
	}
	m.lastSeen = true
	m.lastVersion = exec.Version
	m.applyPendingFromExec(&exec)
}

// stagnantBound answers the injectable stagnation bound (default 3).
func (m *Monitor) stagnantBound() int {
	if m.cfg.VersionStagnantTicks > 0 {
		return m.cfg.VersionStagnantTicks
	}
	return defaultVersionStagnantTicks
}

// applyPendingFromExec reconciles the pending-question projection with
// the authoritative file (the agent persists pending_question in
// execution.json BEFORE calling flush(needs_input); both sources are
// idempotent by question id).
func (m *Monitor) applyPendingFromExec(exec *capsule.Execution) {
	var pv *protocol.PendingView
	if s := exec.Steps[exec.CurrentStep]; s != nil && s.PendingQuestion != nil {
		q := s.PendingQuestion
		if q.ID == "" {
			// F10: a schema-invalid pending (id missing — e.g. the agent wrote
			// the wire field name instead of the schema's `id`) must NOT
			// clobber a valid pending the flush face already recorded, and
			// must not become an unanswerable projection: ignore it for the
			// pending projection, one event per invalid sighting.
			_ = m.log.Append(m.wi.WorkItem, "pending_invalid", map[string]any{
				"step": q.Step, "hint": "execution.json pending_question.id missing; see skill §7.2",
			})
			return
		}
		pv = &protocol.PendingView{
			ID: q.ID, Step: q.Step, Question: q.Question, AskedAt: q.AskedAt,
		}
	}
	m.applyPending(pv)
}

// applyPending records/clears the pending question, idempotent by id, and
// emits the needs_input event on first sight of a new question. A nil
// view after a successful execution read means the agent consumed the
// question itself: clear the projection quietly (the answered event
// belongs to the answer path).
func (m *Monitor) applyPending(pv *protocol.PendingView) {
	m.stateMu.Lock()
	switch {
	case pv == nil:
		if m.pending != nil {
			// The agent consumed the question itself: clear the projection
			// quietly (the answered event belongs to the answer path).
			m.ledger.clearAwaitingQuestion(m.wi.WorkItem, m.pending.ID)
			m.pending = nil
		}
		m.stateMu.Unlock()
		return
	case m.pending != nil && m.pending.ID == pv.ID:
		// Idempotent re-present of the SAME question id (a recovered
		// derivation re-flushing its persisted pending question, or the
		// execution.json poll reconciling): refresh the asked_at — the one
		// observable a repeated flush moves — but never open a second
		// pending, never re-announce, and never let a text-less re-present
		// erase the recorded question. One question id is one needs_input,
		// minted once (F9).
		m.pending.AskedAt = pv.AskedAt
		if pv.Question != "" {
			m.pending.Question = pv.Question
		}
		if pv.Step != "" {
			m.pending.Step = pv.Step
		}
		m.stateMu.Unlock()
		return
	default:
		// First sight of this question id, or a genuinely new question.
		m.pending = pv
	}
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvNeedsInput, map[string]any{
		"question_id": pv.ID, "step": pv.Step, "question": pv.Question,
	})
}

// transcriptSnapshot captures the current derivation's transcript-sync
// inputs (monitor goroutine only); the push sub-goroutines receive the
// VALUES, never the monitor fields — no cross-goroutine reads.
type transcriptSnapshot struct {
	dst     string
	src     string
	spawnAt time.Time
}

func (m *Monitor) transcriptSnapshot() transcriptSnapshot {
	return transcriptSnapshot{dst: m.trDst, src: m.trSrc, spawnAt: m.trSpawnAt}
}

// setTranscriptInfo adopts the per-derivation transcript facts of an
// attachment (monitor goroutine only; observe.attach and onRecovered).
// transcriptWarned is stateMu-guarded: a push sub-goroutine claims the
// once-per-attempt warning concurrently with this reset.
func (m *Monitor) setTranscriptInfo(wi WIInfo) {
	m.trDst = wi.Transcript
	m.trSrc = wi.LiveTranscript
	m.trSpawnAt = wi.SpawnAt
	m.stateMu.Lock()
	m.transcriptWarned = false
	m.stateMu.Unlock()
}

// claimTranscriptWarning claims the once-per-attempt transcript_missing
// warning (push sub-goroutines): true exactly once per derivation.
func (m *Monitor) claimTranscriptWarning() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.transcriptWarned {
		return false
	}
	m.transcriptWarned = true
	return true
}

// syncTranscript is the F3 commit-boundary transcript sync: copy the
// current derivation's live harness transcript onto the capsule's
// declared session path, READ-ONLY w.r.t. the source, so the transcript
// rides the very commit the caller is about to make (ruling C1). Runs in
// the push sub-goroutines BEFORE the push itself; a missing source is
// the typed advisory transcript_missing (once per attempt), never an
// error — the commit proceeds without the transcript.
func (m *Monitor) syncTranscript(s transcriptSnapshot) {
	if s.dst == "" {
		return // no declared session path: nothing to sync onto
	}
	src := s.src
	if src == "" && m.cfg.locateLive != nil {
		src = m.cfg.locateLive(m.wi.CapsuleDir, s.spawnAt)
	}
	if src == "" {
		if m.claimTranscriptWarning() {
			_ = m.log.Append(m.wi.WorkItem, EvTranscriptMissing, map[string]any{
				"detail":  "live harness transcript not found at the commit boundary; committing without it",
				"spawn_n": m.currentSpawn(),
			})
		}
		return
	}
	if src == s.dst {
		return // a resumed derivation already writes into the capsule
	}
	if err := copyFileReadonly(src, s.dst); err != nil {
		if m.claimTranscriptWarning() {
			_ = m.log.Append(m.wi.WorkItem, EvTranscriptMissing, map[string]any{
				"detail":  fmt.Sprintf("transcript sync failed: %v", err),
				"spawn_n": m.currentSpawn(),
			})
		}
	}
}

// copyFileReadonly copies src onto dst without ever opening the source
// for writing (the source is the harness's own session store).
func copyFileReadonly(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tsync.tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// schedulePush starts one async commit+push sub-goroutine when a newer
// execution Version exists, the retry backoff has elapsed, and the
// single-in-flight gate is free. The C3' stagnation fallback rides the
// same gate: a stagnant episode (mtime advanced, Version frozen) also
// arms a commit, so a contract-violating agent cannot starve the chain.
func (m *Monitor) schedulePush() {
	m.stateMu.Lock()
	superseded := m.superseded
	m.stateMu.Unlock()
	if superseded {
		return // M4: a superseded daemon's pushes are fenced server-side;
		// observe does not keep offering them either
	}
	stagnant := m.stagnantDirty
	if m.dirtyVersion <= m.pushedVersion && !stagnant {
		return
	}
	if !m.retryAt.IsZero() && !time.Now().After(m.retryAt) {
		return
	}
	// Same-error retry cap (V8 repair): a halted round stays halted
	// until the next commit boundary — an execution.json Version the
	// round did NOT already see — re-arms it. dirtyVersion only moves
	// forward on fresh agent work (pollExecution), so a frozen poisoned
	// tree cannot spin the retry loop; the wrap/seal path stays
	// available (bounded by wrapRetries) and is not gated here.
	if m.retryHalted {
		if m.dirtyVersion <= m.retryHaltedAt {
			return
		}
		m.retryHalted = false
		m.sameErrStreak = 0
		m.lastPushErr = ""
	}
	// try-acquire the gate: at most ONE push in flight per WI.
	select {
	case m.gate <- struct{}{}:
	default:
		return // one already in flight; the next tick retries
	}
	m.retryAt = time.Time{}
	m.stagnantDirty = false      // consumed by this scheduling pass
	version := m.dirtyVersion    // the seq trailer is gitops' single authority (derived in-lock)
	tr := m.transcriptSnapshot() // captured BEFORE the spawn: no shared-field reads
	go func() {
		// The transcript sync rides the commit pipeline (F3): the copy
		// precedes the push in THIS goroutine, so the transcript lands in
		// the commit it belongs to — and never blocks the select loop.
		m.syncTranscript(tr)
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.FlushBound)
		defer cancel()
		message := fmt.Sprintf("observe: execution v%d", version)
		if stagnant {
			// never a lie about the version: the message names the
			// contract violation this commit compensates for
			message = fmt.Sprintf("observe: execution v%d (mtime advanced without a Version bump)", version)
		}
		sha, pushed, timings, err := m.cfg.push(ctx, m.wi.Git, message)
		m.pushDone <- pushResult{version: version, sha: sha, pushed: pushed, stagnant: stagnant, timings: timings, err: err}
	}()
}

func (m *Monitor) onPushResult(res pushResult) {
	<-m.gate // release the single-in-flight slot
	// 兜底: an honest gitops result is either (pushed, nil) or a non-nil
	// error — a clean-tree CommitPush pushes the standing HEAD, so a
	// failed push always carries its error. Should a PushFunc ever
	// report (nil error, pushed=false) anyway, publishing it would mark
	// the version server-reached when it was not and silence the §九
	// backpressure episode forever; classify it retry-worthy instead.
	if res.err == nil && !res.pushed {
		res.err = fmt.Errorf("push reported success without pushing (sha=%q)", res.sha)
	}
	if res.err == nil {
		if res.version > m.pushedVersion {
			m.pushedVersion = res.version
		}
		m.lastPushOK = time.Now()
		m.failedPushes = 0
		m.firstFailAt = time.Time{}
		m.pushDegraded = false
		m.timeDegraded = false
		m.lastPushErr = ""
		m.sameErrStreak = 0
		m.retryHalted = false
		_ = m.log.Append(m.wi.WorkItem, EvPublished, map[string]any{
			"version": res.version, "local_commit": res.sha, "pushed": res.pushed,
			"commit_ms": res.timings.CommitMS, "push_ms": res.timings.PushMS,
		})
		return
	}
	// A failed stagnation fallback re-arms itself: the Version-driven
	// condition alone would never retry it (dirtyVersion never moved
	// past pushedVersion on that path).
	if res.stagnant {
		m.stagnantDirty = true
	}
	m.failedPushes++
	if m.firstFailAt.IsZero() {
		m.firstFailAt = time.Now()
	}
	m.retryAt = time.Now().Add(m.cfg.PushRetryBackoff)
	// Same-error streak (V8 repair): identical error text N times in a
	// row halts this round's async retries (one degraded event) — a
	// deterministic failure (dead server config, poisoned tree) answers
	// every retry with the same words, and no backoff cadence can bound
	// that on its own (#48: 185 identical failures, ~1h).
	if key := res.err.Error(); key == m.lastPushErr {
		m.sameErrStreak++
	} else {
		m.lastPushErr = key
		m.sameErrStreak = 1
	}
	if m.sameErrStreak >= sameErrorCap && !m.retryHalted {
		m.retryHalted = true
		m.retryHaltedAt = m.dirtyVersion
		_ = m.log.Append(m.wi.WorkItem, EvPushBackpressure, map[string]any{
			"reason": "same_error", "failed_pushes": m.failedPushes,
			"streak": m.sameErrStreak, "error": m.lastPushErr,
			"note": "async push retry halted for this round (" +
				strconv.Itoa(sameErrorCap) + " consecutive identical errors); " +
				"resumes at the next execution.json Version bump (commit boundary) or a successful push; " +
				"wrap/seal pushes stay available and bounded",
		})
	}
	// V8 classification: the LOCAL gitlink precheck refusal is its own
	// event kind — see EvPushPoisonedLocal for why it stays out of the
	// push_failed family. It still feeds the bookkeeping above (the
	// streak, the backoff, the steps ladder): a poisoned tree is a
	// persistent failure and the same-error cap must bound its retries
	// exactly like any other deterministic shape.
	if gitops.IsLocalGitlink(res.err) {
		_ = m.log.Append(m.wi.WorkItem, EvPushPoisonedLocal, map[string]any{
			"version": res.version, "error": res.err.Error(),
			"detail": "local gitlink precheck refused to create the commit (GIT-412 LOCAL_GITLINK_DETECTED); " +
				"not an occurred boundary-sync failure — excluded from push_failed and the M8 read-through by design",
		})
		return
	}
	_ = m.log.Append(m.wi.WorkItem, EvPushFailed, map[string]any{
		"version": res.version, "error": res.err.Error(),
	})
}

// checkBackpressure writes the DEGRADATION events (§九): N consecutive
// failed pushes, or T since the last successful push. Degradation never
// kills the agent — the events are the whole story; local commits keep
// accumulating and the next success resets the episode.
func (m *Monitor) checkBackpressure() {
	if m.failedPushes == 0 {
		return
	}
	if !m.pushDegraded && m.failedPushes >= m.cfg.BackpressureSteps {
		m.pushDegraded = true
		_ = m.log.Append(m.wi.WorkItem, EvPushBackpressure, map[string]any{
			"reason": "steps", "failed_pushes": m.failedPushes,
			"note": "agent continues; commits accumulate locally",
		})
	}
	if !m.timeDegraded {
		now := time.Now()
		overTime := !m.lastPushOK.IsZero() && now.Sub(m.lastPushOK) >= m.cfg.BackpressureAfter
		if !overTime && m.lastPushOK.IsZero() &&
			now.Sub(m.firstFailAt) >= m.cfg.BackpressureAfter {
			overTime = true // never pushed successfully since this episode began
		}
		if overTime {
			m.timeDegraded = true
			_ = m.log.Append(m.wi.WorkItem, EvPushBackpressure, map[string]any{
				"reason": "time", "failed_pushes": m.failedPushes,
				"note": "agent continues; commits accumulate locally",
			})
		}
	}
}

// ---------------------------------------------------------------------------
// answer relay

func (m *Monitor) onAnswer(call AnswerCall) {
	m.stateMu.Lock()
	var fp PendingSource = frozenPending{}
	if m.pending != nil {
		fp = frozenPending{id: m.pending.ID, step: m.pending.Step}
	}
	m.stateMu.Unlock()
	handle := m.handle
	wi := m.wi.WorkItem
	ledger := m.ledger
	go func() {
		delivered, err := ledger.SubmitFor(wi, call.Relay, call.Caller, fp, m.deliverVia(handle))
		if err == nil {
			if delivered {
				// A fresh consumption (never an idempotent replay): announce
				// it and clear the pending projection.
				_ = m.log.Append(wi, EvAnswered, map[string]any{
					"question_id": call.Relay.QuestionID, "answer_id": call.Relay.AnswerID,
				})
				m.clearPendingByID(call.Relay.QuestionID)
			}
		}
		call.Resp <- err
	}()
}

// clearPendingByID drops the pending projection once the answer's
// consumption is observed (only if it is still the same question).
func (m *Monitor) clearPendingByID(questionID string) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.pending != nil && m.pending.ID == questionID {
		m.pending = nil
	}
}

// ---------------------------------------------------------------------------
// attach-time answer redelivery (F9)

// deliverVia builds the stdio pf/answer delivery closure for one handle
// (the shared body of the manual answer path and the attach-time
// redelivery — the only two legal senders of an answer relay).
func (m *Monitor) deliverVia(handle Handle) Deliver {
	return func(relay AnswerRelay) error {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.AnswerAckWindow)
		defer cancel()
		seq := atomic.AddInt64(&m.ctrl, 1) // control sequence, monotonic per monitor
		ack, serr := handle.SendControl(ctx, protocol.ControlMsg{
			Method: protocol.ControlAnswer,
			Seq:    seq,
			ID:     relay.QuestionID,
			Answer: relay.AnswerText,
		})
		if serr != nil {
			return serr // ErrAnswerAckPending passes through by contract
		}
		if !ack.OK {
			return fmt.Errorf("agent refused pf/answer: %s", ack.Error)
		}
		return nil
	}
}

// redeliverAwaiting closes the answer relay's recovery gap: an answer
// that was DELIVERED but never acked/consumed stays in the ledger as
// re-deliverable, and the agent instance that held it may have died.
// When a new instance attaches (RTStarted/RTRecovered landing), every
// such answer whose question is STILL the open pending question (the
// execution.json projection — the id is minted once and lives there) is
// re-sent to the new handle from a sub-goroutine — the select loop never
// blocks, the attempt is recorded, and a failed re-delivery stays in the
// ledger untouched (the manual answer path and the NEXT attachment both
// re-try it). Answers bound to older, consumed questions are not resent.
func (m *Monitor) redeliverAwaiting() {
	if m.handle == nil {
		return
	}
	qid, step := m.openQuestionID()
	if qid == "" {
		return
	}
	for _, relay := range m.ledger.Awaiting(m.wi.WorkItem) {
		if relay.QuestionID != qid {
			continue // bound to a different (older) question: not this attach's business
		}
		m.spawnRedelivery(relay, qid, step)
	}
}

// openQuestionID answers the currently open pending question's id and
// step: the capsule's execution.json is the authority (the id is minted
// once and persists there; a recovered derivation re-presents from it),
// the monitor's projection the fallback when the file cannot be read.
// "" when no question is open.
func (m *Monitor) openQuestionID() (string, string) {
	if data, err := os.ReadFile(m.execPath); err == nil {
		var exec capsule.Execution
		if json.Unmarshal(data, &exec) == nil {
			if s := exec.Steps[exec.CurrentStep]; s != nil && s.PendingQuestion != nil && s.PendingQuestion.ID != "" {
				return s.PendingQuestion.ID, s.PendingQuestion.Step
			}
		}
	}
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.pending != nil {
		return m.pending.ID, m.pending.Step
	}
	return "", ""
}

// spawnRedelivery re-delivers one awaiting answer to the currently
// attached handle. The attempt is recorded before the send; the ledger's
// own SubmitFor owns the idempotency semantics (a retryable entry
// re-delivers, an in-flight one is refused for now, a finalized one no
// longer sits in the awaiting set).
func (m *Monitor) spawnRedelivery(relay AnswerRelay, qid, step string) {
	handle := m.handle
	wi := m.wi.WorkItem
	ledger := m.ledger
	log := m.log
	_ = log.Append(wi, EvAnswerRedelivery, map[string]any{
		"question_id": relay.QuestionID, "answer_id": relay.AnswerID,
		"spawn_n": m.currentSpawn(),
		"detail":  "re-delivering the delivered-but-unconsumed answer to the newly attached instance",
	})
	go func() {
		delivered, err := ledger.SubmitFor(wi, relay, relay.AnsweredBy,
			frozenPending{id: qid, step: step}, m.deliverVia(handle))
		if err == nil {
			if delivered {
				_ = log.Append(wi, EvAnswered, map[string]any{
					"question_id": relay.QuestionID, "answer_id": relay.AnswerID,
					"redelivered": true,
				})
				m.clearPendingByID(relay.QuestionID)
			}
			return
		}
		if !errors.Is(err, ErrAnswerAckPending) && !errors.Is(err, ErrAnswerInFlight) {
			// Delivery failed (agent dead pre-ack, control path lost): the
			// entry stays re-deliverable in the ledger — record why.
			_ = log.Append(wi, EvAnswerRedelivery, map[string]any{
				"question_id": relay.QuestionID, "answer_id": relay.AnswerID,
				"error": err.Error(),
			})
		}
	}()
}

// ---------------------------------------------------------------------------
// flush(reason) handling (the UDS face entry)

func (m *Monitor) onFlush(call FlushCall) {
	// M4: a superseded session's flushes are refused with the stale
	// shape — the work item belongs to another daemon now, and no local
	// seal may speak for it.
	m.stateMu.Lock()
	superseded := m.superseded
	m.stateMu.Unlock()
	if superseded {
		call.Resp <- protocol.FlushAck{
			OK: false, Error: "STALE_SPAWN: this session was superseded (another daemon holds the work item); the flush is refused",
		}
		return
	}
	// The B2 spawn fence, monitor half: a request that carries NO counter
	// while the monitor runs a session-model derivation (spawnN > 0)
	// names nothing and is refused fail-closed — the belt-and-braces
	// twin of the serveFlush fence for direct-channel callers. A monitor
	// that never adopted a counter (legacy attach, spawnN == 0) keeps
	// serving legacy callers unchanged until the cleanup commit.
	if n := call.Req.SpawnN; m.spawnN > 0 && n == 0 {
		call.Resp <- protocol.FlushAck{
			OK: false, Error: "STALE_SPAWN: flush carries no spawn_n; the current derivation is " +
				strconv.Itoa(m.spawnN),
		}
		return
	}
	// The recovery-adoption window: the request names a derivation the
	// monitor has not adopted yet (the serveFlush fence has already
	// proven it is the hub's latest announced counter, never an older
	// one). Queue it — the RTRecovered attachment this counter belongs to
	// is already in flight on rtCh; onRecovered releases the call the
	// moment the counter becomes current. Only one such call can exist
	// (the window is one spawn deep); a second answers an explicit
	// refusal.
	if n := call.Req.SpawnN; n != 0 && n != m.spawnN {
		if m.deferredFlush == nil {
			m.deferredFlush = &call
			return
		}
		call.Resp <- protocol.FlushAck{
			OK: false, Error: "flush adoption window busy; retry: spawn_n " + strconv.Itoa(n) +
				" is still ahead of the monitor (holding " + strconv.Itoa(m.spawnN) + ")",
		}
		return
	}
	switch call.Req.Reason {
	case protocol.FlushNeedsInput:
		m.flushNeedsInput(call)
	case protocol.FlushWrap:
		m.flushWrap(call)
	case protocol.FlushShutdown:
		m.flushShutdown(call)
	default:
		call.Resp <- protocol.FlushAck{OK: false, Error: "unknown flush reason: " + call.Req.Reason}
	}
}

// flushNeedsInput records the pending question (idempotent by question
// id) and acks IMMEDIATELY: the ack is the agent's license to block on
// the human, and it never touches the push path ("不阻塞 push" — the
// question itself reaches the capsule through the agent's own
// execution.json write and is committed by the async push like any
// other change).
//
// Degrade semantics (§五): this path cannot fail the agent. A malformed
// request is the only refusal — and since F9 the HTTP face fail-closes
// an empty question_id with DAEMON-400 before the call ever reaches
// here, so this guard is the belt-and-braces for direct-channel callers.
func (m *Monitor) flushNeedsInput(call FlushCall) {
	req := call.Req
	if req.QuestionID == "" {
		_ = m.log.Append(m.wi.WorkItem, EvNeedsInput, map[string]any{
			"error": "flush(needs_input) without question_id; degraded",
		})
		call.Resp <- protocol.FlushAck{OK: false, Error: "question_id required"}
		return
	}
	m.applyPending(&protocol.PendingView{
		ID: req.QuestionID, Step: req.Step, Question: req.Question,
		AskedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	call.Resp <- protocol.FlushAck{OK: true}
}

// flushWrap seals the attempt: commit+push with the single-in-flight
// guarantee, retried up to three times inside the flush bound. Success
// distinguishes the local commit from the server push in the ack; after
// three failures the work item transitions to blocked and the monitor
// reports OBCompleted(blocked).
func (m *Monitor) flushWrap(call FlushCall) {
	m.stateMu.Lock()
	if m.sealing {
		m.stateMu.Unlock()
		call.Resp <- protocol.FlushAck{OK: false, Error: "wrap flush already in flight"}
		return
	}
	m.sealing = true
	m.stateMu.Unlock()

	version := m.dirtyVersion
	finalStatus := call.Req.FinalStatus
	tr := m.transcriptSnapshot() // captured before the goroutine (F3)
	go func() {
		// V4 evidence gate (fail-closed, in THIS sub-goroutine — the
		// select never blocks): a seal attempt whose executed
		// verification step carries no structured command evidence is
		// refused before any commit/push is attempted, with the typed
		// DAEMON-400 VERIFICATION_EVIDENCE_MISSING ack naming the work
		// item and the step. The refusal is NOT terminal: the agent
		// appends the evidence (taken from the actual command results)
		// and re-flushes. See wrapEvidenceMissing for the predicate.
		if missing, detail := m.wrapEvidenceMissing(); missing {
			m.wrapDone <- wrapResult{
				ok: false, refused: true, err: detail,
				version: version, finalStatus: finalStatus, call: call,
			}
			return
		}
		// seal flush: the transcript copy must ride the seal commit (C1)
		// — sync before the first push attempt, inside the gate.
		m.syncTranscript(tr)
		deadline := time.Now().Add(m.cfg.FlushBound)
		var (
			lastSHA, lastErr string
			pushed, ok       bool
			psha             string
			pt               gitops.PushTimings
		)
		for try := 1; try <= wrapRetries && !ok; try++ {
			m.gate <- struct{}{} // wait out any in-flight async push
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			sha, p, t, err := m.cfg.push(ctx, m.wi.Git, "wrap: "+finalStatus)
			cancel()
			<-m.gate
			psha, pushed, pt = sha, p, t
			if err == nil {
				ok = true
				break
			}
			lastSHA, lastErr = sha, err.Error()
			if !time.Now().Before(deadline) {
				break // the 60s protocol bound is spent
			}
		}
		m.wrapDone <- wrapResult{
			ok: ok, sha: pickSHA(ok, psha, lastSHA), pushed: pushed, timings: pt,
			err: lastErr, version: version, finalStatus: finalStatus, call: call,
		}
	}()
}

func pickSHA(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// V4 wrap gate: verification evidence (fail-closed)

// verificationStepID is the step id every reference bundle's verification
// gate carries (bug-fix / chore / feature-full / quick-fix), and the exact
// steps key the M4 read-through (tools/gate-metrics.go) hardcodes — the
// wrap gate and the metric must judge the same face, or the gate would
// seal exactly what the metric then counts as evidence-less.
const verificationStepID = "verification"

// wrapEvidenceMissing answers whether the seal must be refused: some
// EXECUTED verification step in the capsule's execution.json carries no
// structured command evidence — no readable {argv, exit_code, duration_ms}
// entry in steps.verification.output.commands. "Executed" matches the M4
// denominator exactly (any status other than empty/pending); an absent
// or unexecuted verification step is outside both the metric and this
// gate. An unreadable/unparsable file fails OPEN here (same tolerance as
// pollExecution's torn-read path: the write is tmp+rename atomic, so a
// persistent read failure means no step was ever observed — nothing to
// gate). Reads happen in the flushWrap sub-goroutine; the select loop
// never blocks on this check.
func (m *Monitor) wrapEvidenceMissing() (bool, string) {
	data, err := os.ReadFile(m.execPath)
	if err != nil {
		return false, ""
	}
	var exec capsule.Execution
	if json.Unmarshal(data, &exec) != nil {
		return false, ""
	}
	st := exec.Steps[verificationStepID]
	if st == nil || st.Status == "" || st.Status == capsule.StepPending {
		return false, "" // not executed: outside the M4 denominator, outside the gate
	}
	if stepHasCommandEvidence(st.Output) {
		return false, ""
	}
	detail := fmt.Sprintf("work item %s: step %q (status %s) records no structured command evidence in steps.%s.output.commands — append at least one readable {argv, exit_code, duration_ms} entry taken directly from the command result (pf-execute §4 command audit), then re-flush",
		m.wi.WorkItem, verificationStepID, st.Status, verificationStepID)
	return true, detail
}

// stepHasCommandEvidence reports whether the step's output carries at
// least one readable command-audit entry — the M4 evidence predicate,
// mirrored bit for bit from tools/gate-metrics.go's commandEvidence
// (INV-D3: exit_code readable; any non-empty output is NOT evidence).
func stepHasCommandEvidence(output map[string]any) bool {
	if output == nil {
		return false
	}
	cmds, ok := output["commands"].([]any)
	if !ok {
		return false // old schema: no commands field at all
	}
	for _, c := range cmds {
		if commandEvidenceReadable(c) {
			return true
		}
	}
	return false
}

// commandEvidenceReadable judges ONE output.commands entry: a readable
// {argv, exit_code, duration_ms} record — argv a non-empty argv array or
// a non-blank command string, exit_code and duration_ms both present.
func commandEvidenceReadable(v any) bool {
	e, ok := v.(map[string]any)
	if !ok {
		return false
	}
	switch argv := e["argv"].(type) {
	case []any:
		if len(argv) == 0 {
			return false
		}
	case string:
		if strings.TrimSpace(argv) == "" {
			return false
		}
	default:
		return false
	}
	return e["exit_code"] != nil && e["duration_ms"] != nil
}

func (m *Monitor) onWrapResult(wr wrapResult) {
	if wr.refused {
		// The V4 gate's refusal: un-arm the seal guard, answer the typed
		// DAEMON-400, and leave the attempt RUNNING — no blocked transition,
		// no OBCompleted, no push bookkeeping (no commit was attempted).
		// The agent's contract (pf-execute §4) is to append the missing
		// evidence and re-flush.
		m.stateMu.Lock()
		m.sealing = false
		m.stateMu.Unlock()
		ack := protocol.FlushAck{
			OK:    false,
			Error: "DAEMON-400 VERIFICATION_EVIDENCE_MISSING: " + wr.err,
		}
		_ = m.log.Append(m.wi.WorkItem, EvWrapEvidenceRefused, map[string]any{
			"error": ack.Error, "final_status": wr.finalStatus,
			"spawn_n": m.currentSpawn(),
		})
		wr.call.Resp <- ack
		return
	}
	ack := protocol.FlushAck{OK: wr.ok, LocalCommit: wr.sha, Pushed: wr.pushed && wr.ok}
	if wr.ok {
		m.stateMu.Lock()
		m.sealing = false
		// Terminal for the attempt: a later process exit is expected
		// and never a recovery signal.
		m.wrapped = true
		m.stateMu.Unlock()
		if wr.version > m.pushedVersion {
			m.pushedVersion = wr.version
		}
		m.lastPushOK = time.Now()
		m.failedPushes = 0
		m.pushDegraded = false
		m.timeDegraded = false
		m.lastPushErr = ""
		m.sameErrStreak = 0
		m.retryHalted = false
		_ = m.log.Append(m.wi.WorkItem, EvPublished, map[string]any{
			"wrap": true, "local_commit": wr.sha, "pushed": wr.pushed, "final_status": wr.finalStatus,
			"commit_ms": wr.timings.CommitMS, "push_ms": wr.timings.PushMS,
		})
		reason := wr.finalStatus
		if reason != protocol.FinishFailed {
			reason = protocol.FinishWrapped
		}
		m.sendOB(protocol.OBMsg{
			Kind: protocol.OBCompleted, SpawnN: m.currentSpawn(),
			WorkItem: m.wi.WorkItem, Reason: reason,
		})
	} else {
		m.stateMu.Lock()
		m.sealing = false
		m.blocked = true
		m.stateMu.Unlock()
		ack.Error = "wrap flush failed after " + strconv.Itoa(wrapRetries) + " retries"
		if wr.err != "" {
			ack.Error += ": " + wr.err
		}
		_ = m.log.Append(m.wi.WorkItem, EvWrapBlocked, map[string]any{
			"error": wr.err, "local_commit": wr.sha,
		})
		m.sendOB(protocol.OBMsg{
			Kind: protocol.OBCompleted, SpawnN: m.currentSpawn(),
			WorkItem: m.wi.WorkItem, Reason: protocol.FinishBlocked,
			Detail: "wrap flush failed 3 times; transitioned to blocked",
		})
	}
	wr.call.Resp <- ack
}

// flushShutdown is the daemon-close flush: commit what exists, ack the
// result, never wait (§五: a failed shutdown flush does not hold the
// agent — the data is in the local commit).
func (m *Monitor) flushShutdown(call FlushCall) {
	tr := m.transcriptSnapshot() // captured before the goroutine (F3)
	go func() {
		m.gate <- struct{}{}
		// the closing flush commits what exists — the transcript included
		// (F3): copy before the push, inside the gate.
		m.syncTranscript(tr)
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.FlushBound)
		sha, pushed, timings, err := m.cfg.push(ctx, m.wi.Git, "shutdown flush")
		cancel()
		<-m.gate
		ack := protocol.FlushAck{OK: err == nil, LocalCommit: sha, Pushed: pushed && err == nil}
		if err != nil {
			ack.Error = err.Error()
			_ = m.log.Append(m.wi.WorkItem, EvPushFailed, map[string]any{
				"shutdown": true, "error": ack.Error,
			})
		} else {
			_ = m.log.Append(m.wi.WorkItem, EvPublished, map[string]any{
				"shutdown": true, "local_commit": sha, "pushed": pushed,
				"commit_ms": timings.CommitMS, "push_ms": timings.PushMS,
			})
		}
		call.Resp <- ack
	}()
}

// ---------------------------------------------------------------------------
// state read face (HTTP view + sub-goroutines)

// currentSpawn answers the derivation counter the monitor has ADOPTED
// (B2 fence input for the HTTP read face; stateMu-guarded — serveFlush
// reads it cross-goroutine).
func (m *Monitor) currentSpawn() int {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.spawnN
}

// canonicalWI answers the CANONICAL work-item id this monitor is keyed
// by (the F2 authority: a slug or any other submitted spelling that
// reached the monitor through lookup is an alias only — memory
// attribution must never record it).
func (m *Monitor) canonicalWI() string {
	return m.wi.WorkItem
}

// markClosing arms the expected-exit suppression (the daemon-initiated
// close path; see Observe.MarkClosing). The flag is the same one the
// flush(wrap) path arms, so onExit's suppression set needs no new
// member.
func (m *Monitor) markClosing() {
	m.stateMu.Lock()
	m.sealing = true
	m.stateMu.Unlock()
}
