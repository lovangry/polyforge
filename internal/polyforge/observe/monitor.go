package observe

// monitor.go: the per-WI SUPERVISOR goroutine (v2-converge 2.1-2.3; the
// guessing lanes — hang, idle, probe/ack, wall, turn-duration, the tool
// and question exemptions, no_progress — are gone). ONE goroutine
// supervises ONE work item; its only execution shape is a select loop
// whose cases never block. Every long operation (a push, a session
// verify) runs in a sub-goroutine and reports back through a buffered
// channel.
//
// Input sources drained by the select:
//   - the driver's FACTS (supervise.Link.Facts: step_started,
//     step_waiting_human/answered, step_ended, exec_committed) — an
//     unbounded in-process queue, never dropped;
//   - the confirmed driver-exit signal (Handle.Exit) → recovery;
//   - RT messages (RTRecovered rides rtQ; RTFinished rides stopCh);
//   - results of the sub-goroutines (push / verify);
//   - the tick (judging, push retry backoff, backpressure, M4).
//
// JUDGING: per dispatch the supervisor holds the harness Tracker the
// translator folds the step process's records into (only real work
// counts; UI noise does not) and applies the rules of package
// supervise each tick; the first rule that fires becomes ONE step_dead
// verdict on the link, after which the supervisor stays silent for that
// spawn until step_ended. State is per dispatch: created on
// step_started, dropped on step_ended. Between steps (and not waiting on
// a human) the supervisor also times the DRIVER: driver_stalled.
//
// RECOVERING: after WINeedsRecovery is sent (driver exit / stall) the
// monitor suppresses every further recovery signal of the same
// derivation (the recovery lane) until RTRecovered arrives.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/harness"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/sessionjsonl"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// sameErrorCap is the V8-repair retry bound: after this many consecutive
// async push failures carrying the IDENTICAL error, the retry round is
// halted (one degraded push_backpressure event with reason "same_error")
// until the next commit boundary — an execution.json Version bump — or a
// successful push re-arms it. The pilot's poison-chain storm (#48) was
// 185 consecutive identical failures at a ~10s cadence with no bound;
// the cap turns every such deterministic-failure shape into at most
// sameErrorCap attempts per round.
const sameErrorCap = 5

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
}

// Handle is the driver face a monitor watches. Implemented by the
// daemon's adapter over the runtime's agent handle; Observe owns no
// process code.
type Handle interface {
	// Exit fires ONCE when the supervised driver has returned. The
	// adapter waits on the driver first, so the signal is ALREADY the
	// confirmed exit.
	Exit() <-chan struct{}
}

type pushResult struct {
	version int64
	sha     string
	pushed  bool
	timings gitops.PushTimings
	err     error
}

// monitorCfg carries the tunables (defaults in observe.go Config).
type monitorCfg struct {
	PollInterval time.Duration
	// ModelErrorGrace is rule 1's grace after an error-ended turn.
	ModelErrorGrace time.Duration
	// DriverStall is N: the driver between steps (not waiting on a
	// human) for longer than this is stalled. <= 0 disables the clock.
	DriverStall       time.Duration
	PushRetryBackoff  time.Duration
	BackpressureSteps int
	BackpressureAfter time.Duration
	PushBound         time.Duration
	// revalidate is the v1.5 M4 session-verify cadence (0 = disabled:
	// no verifier, no checks — the unit-test default).
	revalidate time.Duration
	// verify is the M4 aihub seam (nil = revalidation disabled).
	verify SessionVerifier
	push   PushFunc
	// locateLive resolves the live harness transcript on demand (F3);
	// nil = the transcript sync is disabled.
	locateLive func(capsuleDir string, spawnAt time.Time, harness, session string) string
}

// driver phases the supervisor tracks.
const (
	phaseBetween = "between_steps"
	phaseInStep  = "in_step"
	phaseWaiting = "waiting_human"
)

// dispatchState is the supervisor's per-dispatch state (created on
// step_started, dropped on step_ended).
type dispatchState struct {
	spawnN    int64
	step      string
	startedAt time.Time
	candidate string // the dispatch's `<harness>:<model>[:<effort>]` ("" = the harness default model)
	tracker   *supervise.Tracker
	verdicted bool
}

// Monitor supervises one work item.
type Monitor struct {
	wi   WIInfo
	cfg  monitorCfg
	log  *EventLog
	ob   *supervise.Queue[protocol.OBMsg]
	link *supervise.Link

	// spawnN is the v1.5 B2 derivation counter of the CURRENTLY ADOPTED
	// derivation (guarded by stateMu for the HTTP fence's read). It is
	// fed from RTMsg.SpawnN at RTStarted/RTRecovered adoption.
	spawnN int
	// sessionID is the v1.5 session secret carried in-process on the
	// RTStarted/RTRecovered message (M4 revalidation input). It lives
	// ONLY here — never in events, never in the capsule, never in a log
	// line.
	sessionID string
	exitCh    <-chan struct{}

	rtQ    *supervise.Queue[Attach]
	stopCh chan struct{}
	stop   sync.Once

	// supervision state (monitor goroutine; the activity face reads the
	// published copy under stateMu).
	cur        *dispatchState
	phase      string
	phaseSince time.Time
	stalled    bool // driver_stalled sent for the current gap

	// execution.json state, refreshed on driver facts (monitor goroutine).
	currentStep   string
	dirtyVersion  int64 // latest Version seen that has no confirmed push
	pushedVersion int64

	// the single-in-flight push gate.
	gate     chan struct{}
	pushDone chan pushResult
	// exitFlush: the driver returned while capsule state was not yet
	// published. The recovery/terminal path assumes the commit boundary's
	// push already landed (a completed workflow wraps without a push of its
	// own), so the supervisor publishes the last Version FIRST and only then
	// reports the exit — exitFlushDeadline bounds the wait.
	exitFlush         bool
	exitFlushDeadline time.Time
	exitFlushReason   string
	exitFlushDetail   string

	// backpressure bookkeeping (monitor goroutine only).
	failedPushes int
	firstFailAt  time.Time
	lastPushOK   time.Time
	pushDegraded bool
	timeDegraded bool
	retryAt      time.Time
	// same-error retry-cap bookkeeping (V8 repair; monitor goroutine
	// only).
	lastPushErr   string
	sameErrStreak int
	retryHalted   bool
	retryHaltedAt int64

	// live-transcript sync state (F3; monitor goroutine only — the push
	// sub-goroutines receive the values as arguments, never over m).
	trDst            string    // the capsule's declared session path (latest step_started)
	trSrc            string    // the live path when known (a resumed dispatch writes into the capsule)
	trSpawnAt        time.Time // the dispatch's birth moment
	trHarness        string    // the dispatch's step harness (polyforge#159)
	trSession        string    // its harness-native session id (claude's uuid)
	transcriptWarned bool      // one transcript_missing event per dispatch

	// stateMu guards the fields every goroutine (HTTP view, push
	// sub-goroutines) reads.
	stateMu    sync.Mutex
	pending    *protocol.PendingView
	sealing    bool
	recovering bool
	// act is the published activity copy of the current dispatch (the
	// ops status face reads it): nil outside a dispatch.
	act *dispatchState
	// superseded is the v1.5 M4 state: the periodic session-verify
	// answered matches=false — another daemon holds the work item. The
	// monitor stops supervising (detection and pushes are suppressed) but keeps
	// draining its channels and KEEPS REVALIDATING until Runtime's
	// release (RTFinished, sent by supersedeOp — v1.5 review Finding 4)
	// stops the loop: the OBSuperseded emission is retryable (Finding
	// 1(b)), so the periodic verify is the re-emission engine. Observe
	// makes NO hub transition (the new owner owns it).
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

// announceStart appends the job_started event. The attaching side calls
// it synchronously before launching run(): launching first would leave a
// scheduling gap in which an event from another writer of the shared log
// (the driver's EventLogger) could land ahead of job_started.
func (m *Monitor) announceStart() {
	_ = m.log.Append(m.wi.WorkItem, EvJobStarted, map[string]any{
		"spawn_n": m.spawnN,
	})
}

func (m *Monitor) run() {
	defer func() {
		_ = m.log.Append(m.wi.WorkItem, EvJobStopped, map[string]any{
			"spawn_n": m.currentSpawn(),
		})
	}()
	// job_started is announced by the attaching side (announceStart)
	// BEFORE this goroutine is scheduled, so no event appended after the
	// attach can precede it in the log.
	// the seed read: a restarted supervisor learns the capsule's state
	// (pending question, version to publish) before any fact arrives
	m.refreshExec(0)
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			m.awaitPushInFlight()
			return
		case <-m.rtQ.Notify():
			for at, ok := m.rtQ.Pop(); ok; at, ok = m.rtQ.Pop() {
				m.onRecovered(at)
			}
		case <-m.exitCh:
			m.onExit()
		case <-m.link.Facts.Notify():
			m.drainFacts()
		case <-ticker.C:
			m.onTick()
		case res := <-m.pushDone:
			m.onPushResult(res)
		case res := <-m.verifyDone:
			m.onVerifyResult(res)
		}
	}
}

// awaitPushInFlight blocks the monitor's exit until a still-running
// commit+push sub-goroutine has finished (it is bounded by its own
// PushBound context): "the monitor is gone" must mean no writer of the
// WI's git repositories is left behind it. The result is dropped — the
// monitor is stopping.
func (m *Monitor) awaitPushInFlight() {
	if len(m.gate) == 0 {
		return
	}
	bound := m.cfg.PushBound
	if bound <= 0 {
		bound = time.Minute
	}
	select {
	case <-m.pushDone:
	case <-time.After(bound + 5*time.Second):
	}
}

// stop requests the monitor loop to exit (idempotent).
func (m *Monitor) requestStop() { m.stop.Do(func() { close(m.stopCh) }) }

// onTick is the heartbeat: judge the dispatch (or the driver), retry a
// push whose backoff elapsed, check backpressure, M4 revalidation.
func (m *Monitor) onTick() {
	m.drainFacts() // belt and braces: a wake-up can coalesce
	m.judge()
	m.schedulePush()
	m.maybeFinishExitFlush()
	m.checkBackpressure()
	m.checkRevalidate()
}

// ---------------------------------------------------------------------------
// facts in

func (m *Monitor) now() time.Time { return m.link.Clock() }

// drainFacts consumes every queued driver fact, in order.
func (m *Monitor) drainFacts() {
	facts := m.link.Facts.Drain()
	if len(facts) == 0 {
		return
	}
	for _, f := range facts {
		switch v := f.(type) {
		case supervise.StepStarted:
			m.onStepStarted(v)
		case supervise.StepEnded:
			m.onStepEnded(v)
		case supervise.StepWaitingHuman:
			m.setPhase(phaseWaiting)
			m.refreshExec(0)
		case supervise.StepAnswered:
			m.setPhase(phaseBetween)
			m.refreshExec(0)
		case supervise.ExecCommitted:
			m.refreshExec(v.Version)
		}
	}
	m.schedulePush() // event-triggered: do not wait for the next tick
}

func (m *Monitor) setPhase(p string) {
	m.phase = p
	m.phaseSince = m.now()
	m.stalled = false
}

// onStepStarted creates the per-dispatch state (a still-open older one is
// dropped: the driver runs one dispatch at a time).
func (m *Monitor) onStepStarted(f supervise.StepStarted) {
	if m.cur != nil && m.cur.spawnN < f.SpawnN {
		m.link.Release(m.cur.spawnN)
	}
	m.link.Release(f.SpawnN - 1) // fence every older ordinal
	tr := m.link.Tracker(f.SpawnN)
	tr.Begin(f)
	d := &dispatchState{spawnN: f.SpawnN, step: f.Step, startedAt: f.StartedAt, tracker: tr}
	if f.Model != "" {
		d.candidate = harness.Candidate{Harness: harness.Of(f.Harness), Model: f.Model, Effort: f.Effort}.String()
	}
	if d.startedAt.IsZero() {
		d.startedAt = m.now()
	}
	m.cur = d
	m.setPhase(phaseInStep)
	// F3: the transcript sync sources from the latest step_started
	m.trDst = f.TranscriptPath
	m.trSpawnAt = f.StartedAt
	m.trHarness, m.trSession = f.Harness, f.HarnessSession
	// a claude dispatch's native session lives outside the capsule even
	// when resumed: it is always copied
	if f.Resumed && f.Harness != "claude" {
		m.trSrc = f.TranscriptPath // a resumed dispatch appends to the capsule file itself
	} else {
		m.trSrc = ""
	}
	m.stateMu.Lock()
	m.transcriptWarned = false
	m.act = d // published last: readers that see the dispatch see its transcript facts
	m.stateMu.Unlock()
}

// onStepEnded drops the dispatch's state; the supervisor is silent for
// that spawn from here on.
func (m *Monitor) onStepEnded(f supervise.StepEnded) {
	m.link.Release(f.SpawnN)
	if m.cur != nil && m.cur.spawnN <= f.SpawnN {
		m.cur = nil
		m.stateMu.Lock()
		m.act = nil
		m.stateMu.Unlock()
	}
	m.setPhase(phaseBetween)
}

// refreshExec is the event-triggered read of the authoritative file: the
// pending-question projection and the Version to publish. version > 0 is
// the committed Version the fact named.
func (m *Monitor) refreshExec(version int64) {
	if version > m.dirtyVersion {
		m.dirtyVersion = version
	}
	data, err := os.ReadFile(m.execPath())
	if err != nil {
		return
	}
	var exec capsule.Execution
	if json.Unmarshal(data, &exec) != nil {
		return // torn/partial write: the next fact retries
	}
	m.currentStep = exec.CurrentStep
	if exec.Version > m.dirtyVersion {
		m.dirtyVersion = exec.Version
	}
	m.applyPendingFromExec(&exec)
}

func (m *Monitor) execPath() string { return m.execJSONPath() }

// ---------------------------------------------------------------------------
// judging

// judge applies the rules to the current dispatch (one verdict, then
// silence until step_ended) or times the driver between steps.
func (m *Monitor) judge() {
	m.stateMu.Lock()
	suppressed := m.recovering || m.sealing || m.superseded
	m.stateMu.Unlock()
	if suppressed {
		return
	}
	now := m.now()
	if m.cur != nil {
		if m.cur.verdicted {
			return
		}
		snap := m.cur.tracker.Snapshot(now)
		v := supervise.Judge(snap, now, m.cfg.ModelErrorGrace)
		if v == nil {
			return
		}
		m.cur.verdicted = true
		_ = m.log.Append(m.wi.WorkItem, EvSupervisorVerdict, map[string]any{
			"verdict": v.VerdictKind(), "step": v.Step, "spawn_n": v.SpawnN,
			"cause": v.Cause, "evidence": v.Evidence,
		})
		m.link.Verdict(*v)
		return
	}
	if m.phase == phaseBetween && !m.stalled && m.cfg.DriverStall > 0 {
		if gap := now.Sub(m.phaseSince); gap >= m.cfg.DriverStall {
			m.stalled = true
			_ = m.log.Append(m.wi.WorkItem, EvSupervisorVerdict, map[string]any{
				"verdict": "driver_stalled", "duration_ms": gap.Milliseconds(),
				"evidence": "the driver is between steps (not waiting on a human) past the stall bound",
			})
			m.link.Verdict(supervise.DriverStalled{Duration: gap, Phase: phaseBetween})
			// A driver that cannot consume its verdict is the point: the
			// recovery engine kills its tree (any stuck git/push child with
			// it) and relaunches it, counted against the driver-crash bound.
			m.needRecovery(protocol.RecoverDriverStalled,
				fmt.Sprintf("driver stalled between steps for %s", gap.Round(time.Second)))
		}
	}
}

// activity answers the ops-status face: when the current dispatch began
// and when its harness last did real work.
func (m *Monitor) activity() Activity {
	m.stateMu.Lock()
	d := m.act
	m.stateMu.Unlock()
	if d == nil {
		return Activity{}
	}
	now := m.now()
	snap := d.tracker.Snapshot(now)
	return Activity{Step: d.step, SpawnN: d.spawnN, StepStartedAt: d.startedAt, LastWorkAt: snap.LastWork, InDispatch: true, Candidate: d.candidate}
}

// liveProgress answers the watch face's per-dispatch reading (any
// goroutine: it reads the published dispatch pointer under stateMu and the
// tracker's own lock).
func (m *Monitor) liveProgress() LiveProgress {
	m.stateMu.Lock()
	d := m.act
	m.stateMu.Unlock()
	if d == nil {
		return LiveProgress{}
	}
	now := m.now()
	snap := d.tracker.Snapshot(now)
	p := LiveProgress{InDispatch: true, Step: d.step, RecentFiles: snap.RecentFiles, ToolCalls: snap.ToolCalls}
	if n := len(snap.Tools); n > 0 {
		cur := snap.Tools[n-1] // oldest first: the last is the newest
		p.Command = cur.Summary
		p.CommandAgeSec = int64(now.Sub(cur.Since) / time.Second)
	}
	return p
}

// ---------------------------------------------------------------------------
// detection: driver death

// onExit handles the CONFIRMED driver-exit signal (the handle adapter
// already waited on the driver — this is the confirmation). An expected
// exit (a daemon-initiated close, or a signal already sent for this
// attempt) is silent.
func (m *Monitor) onExit() {
	m.exitCh = nil // fires once per handle
	m.drainFacts() // the driver's final commit-boundary facts may still be queued
	m.stateMu.Lock()
	suppressed := m.recovering || m.sealing || m.superseded
	m.stateMu.Unlock()
	if suppressed {
		return
	}
	// Publish the driver's last commit boundary before telling the runtime:
	// the terminal wrap trusts that push (see exitFlush).
	if m.dirtyVersion > m.pushedVersion || m.pushInFlight() {
		m.exitFlush = true
		m.exitFlushDeadline = time.Now().Add(m.cfg.PushBound + time.Second)
		m.exitFlushReason, m.exitFlushDetail = protocol.RecoverDeath, "agent process exited"
		m.retryAt = time.Time{} // the flush push is not held back by a failed round's backoff
		m.retryHalted = false
		m.schedulePush()
		m.maybeFinishExitFlush()
		return
	}
	m.needRecovery(protocol.RecoverDeath, "agent process exited")
}

// pushInFlight reports whether a capsule push is running right now.
func (m *Monitor) pushInFlight() bool { return len(m.gate) > 0 }

// maybeFinishExitFlush reports the pending driver exit once the last
// Version is published (or the push failed, or the bound passed).
func (m *Monitor) maybeFinishExitFlush() {
	if !m.exitFlush {
		return
	}
	if m.pushInFlight() && time.Now().Before(m.exitFlushDeadline) {
		return // wait for the in-flight push's result
	}
	if m.dirtyVersion > m.pushedVersion && !m.pushInFlight() && m.failedPushes == 0 &&
		time.Now().Before(m.exitFlushDeadline) {
		m.schedulePush() // a newer Version arrived behind the push that just ended
		if m.pushInFlight() {
			return
		}
	}
	m.exitFlush = false
	m.needRecovery(m.exitFlushReason, m.exitFlushDetail)
}

// needRecovery is the single choke point for the recovery lane's dedupe:
// after one
// WINeedsRecovery is sent for an attempt, every further signal of the
// SAME attempt is suppressed ("已发后静默到恢复完成") — the select keeps
// draining, the Runtime sees exactly one message. SEALING likewise drops
// same-attempt signals (§三).
func (m *Monitor) needRecovery(reason, detail string) {
	m.stateMu.Lock()
	if m.recovering || m.sealing || m.superseded {
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

// sendOB queues one message for the Runtime; the queue is unbounded, so
// the monitor never blocks and a recovery request is never dropped.
func (m *Monitor) sendOB(msg protocol.OBMsg) { m.ob.Push(msg) }

// ---------------------------------------------------------------------------
// recovery lifecycle

// onRecovered adopts the new handle Runtime derived (RTRecovered). The
// monitor leaves RECOVERING and drops every per-dispatch state: the
// relaunched driver announces its own dispatches.
func (m *Monitor) onRecovered(at Attach) {
	if at.Handle == nil || at.Msg.SpawnN <= 0 {
		return // malformed attachment (no announced derivation): never adopted
	}
	m.exitCh = at.Handle.Exit()
	if m.cur != nil {
		m.link.Release(m.cur.spawnN)
	}
	m.cur = nil
	m.stateMu.Lock()
	m.act = nil
	m.stateMu.Unlock()
	m.setPhase(phaseBetween)
	// No dispatch is in flight until the successor's first step_started
	// names its transcript: nothing to sync in between, and the dead
	// dispatch's destination must never receive the successor's file.
	m.trDst, m.trSrc, m.trSpawnAt = "", "", time.Time{}
	m.trHarness, m.trSession = "", ""
	m.stateMu.Lock()
	m.transcriptWarned = false
	m.recovering = false
	m.spawnN = at.Msg.SpawnN
	m.sessionID = at.Msg.SessionID
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvAgentRecovered, map[string]any{
		"spawn_n": at.Msg.SpawnN,
	})
}

// ---------------------------------------------------------------------------
// execution.json projection + async commit/push

// applyPendingFromExec reconciles the pending-question projection with
// the authoritative file (the agent persists pending_question in
// execution.json; the projection is idempotent by question id).
func (m *Monitor) applyPendingFromExec(exec *capsule.Execution) {
	var pv *protocol.PendingView
	if s := exec.Steps[exec.CurrentStep]; s != nil && s.PendingQuestion != nil {
		q := s.PendingQuestion
		if q.ID == "" {
			// F10: a schema-invalid pending (id missing — e.g. the agent wrote
			// the wire field name instead of the schema's `id`) must NOT
			// clobber a valid recorded pending, and must not become an unanswerable projection: ignore it for the
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

// applyPending records/clears the pending question, idempotent by id. A nil
// view after a successful execution read means the agent consumed the
// question itself: clear the projection quietly (the answered event
// belongs to the answer path).
func (m *Monitor) applyPending(pv *protocol.PendingView) {
	m.stateMu.Lock()
	switch {
	case pv == nil:
		if m.pending != nil {
			// The agent consumed the question itself: clear the projection
			// quietly (the archived answer lives in execution.json).
			m.pending = nil
		}
		m.stateMu.Unlock()
		return
	case m.pending != nil && m.pending.ID == pv.ID:
		// Idempotent re-present of the SAME question id (a recovered
		// derivation re-presenting its persisted pending question, or the
		// execution.json poll reconciling): refresh the asked_at — the one
		// observable a repeat moves — but never open a second
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
	// the needs_input event is the DRIVER's (it lands in this same log at
	// the park transition); this projection only feeds the status faces
}

// transcriptSnapshot captures the current derivation's transcript-sync
// inputs (monitor goroutine only); the push sub-goroutines receive the
// VALUES, never the monitor fields — no cross-goroutine reads.
type transcriptSnapshot struct {
	dst     string
	src     string
	spawnAt time.Time
	harness string
	session string
}

func (m *Monitor) transcriptSnapshot() transcriptSnapshot {
	return transcriptSnapshot{dst: m.trDst, src: m.trSrc, spawnAt: m.trSpawnAt, harness: m.trHarness, session: m.trSession}
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
// rides the very commit the caller is about to make (v1 ruling C1 (archive/v1-architecture-spec.md)). Runs in
// the push sub-goroutines BEFORE the push itself; a missing source is
// the typed advisory transcript_missing (once per attempt), never an
// error — the commit proceeds without the transcript.
func (m *Monitor) syncTranscript(s transcriptSnapshot) {
	if s.dst == "" {
		return // no declared session path: nothing to sync onto
	}
	src := s.src
	if src == "" && m.cfg.locateLive != nil {
		src = m.cfg.locateLive(m.wi.CapsuleDir, s.spawnAt, s.harness, s.session)
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
	if err := sessionjsonl.CopyReadonly(src, s.dst); err != nil {
		if m.claimTranscriptWarning() {
			_ = m.log.Append(m.wi.WorkItem, EvTranscriptMissing, map[string]any{
				"detail":  fmt.Sprintf("transcript sync failed: %v", err),
				"spawn_n": m.currentSpawn(),
			})
		}
	}
}

// schedulePush starts one async commit+push sub-goroutine when a newer
// execution Version exists, the retry backoff has elapsed, and the
// single-in-flight gate is free. It is event-triggered: the driver's
// exec_committed facts raise dirtyVersion and the loop calls it right
// after draining them (the tick only serves the retry backoff).
func (m *Monitor) schedulePush() {
	m.stateMu.Lock()
	superseded := m.superseded
	m.stateMu.Unlock()
	if superseded {
		return // M4: a superseded daemon's pushes are fenced server-side;
		// observe does not keep offering them either
	}
	if m.dirtyVersion <= m.pushedVersion {
		return
	}
	if !m.retryAt.IsZero() && !time.Now().After(m.retryAt) {
		return
	}
	// Same-error retry cap (V8 repair): a halted round stays halted
	// until the next commit boundary — an execution.json Version the
	// round did NOT already see — re-arms it. dirtyVersion only moves
	// forward on a fresh driver commit, so a frozen poisoned tree cannot
	// spin the retry loop.
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
	version := m.dirtyVersion
	tr := m.transcriptSnapshot() // captured BEFORE the spawn: no shared-field reads
	go func() {
		// The transcript sync rides the commit pipeline (F3): the copy
		// precedes the push in THIS goroutine, so the transcript lands in
		// the commit it belongs to — and never blocks the select loop.
		m.syncTranscript(tr)
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.PushBound)
		defer cancel()
		message := fmt.Sprintf("observe: execution v%d", version)
		sha, pushed, timings, err := m.cfg.push(ctx, m.wi.Git, message)
		m.pushDone <- pushResult{version: version, sha: sha, pushed: pushed, timings: timings, err: err}
	}()
}

func (m *Monitor) onPushResult(res pushResult) {
	defer m.maybeFinishExitFlush()
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
				"resumes at the next execution.json Version bump (commit boundary) or a successful push",
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
	if gitops.IsCredentialRefused(res.err) {
		// the git face refused the session credential — this session may
		// no longer hold the work item: revalidate at the next tick, not
		// at the next interval (a superseded work item stops at once)
		m.lastVerifyAt = time.Time{}
	}
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
// state read face (HTTP view + sub-goroutines)

// currentSpawn answers the derivation counter the monitor has ADOPTED
// (stateMu-guarded — the HTTP read face reads it cross-goroutine).
func (m *Monitor) currentSpawn() int {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.spawnN
}

// markClosing arms the expected-exit suppression (the daemon-initiated
// close path; see Observe.MarkClosing).
func (m *Monitor) markClosing() {
	m.stateMu.Lock()
	m.sealing = true
	m.stateMu.Unlock()
}
