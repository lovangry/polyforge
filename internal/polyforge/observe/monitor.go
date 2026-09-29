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
// same attempt-id (storm guard) until WIRecovered arrives with a new
// handle. The select keeps running — the old handle's channels must
// keep being drained or Runtime's stdout reader would block forever —
// but no detection consequence fires.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// PushFunc is the injectable capsule commit+push operation (the seam the
// tests use; production binds gitops.CommitPush). ctx bounds the call;
// returning ctx.Err() reports the attempt as failed while the underlying
// git keeps running — the per-WI git lock serializes any successor.
type PushFunc func(ctx context.Context, o gitops.Opts, message string) (localSHA string, pushed bool, err error)

// WIInfo carries the per-WI static facts a monitor needs; supplied by
// Runtime alongside the RTStarted protocol message.
type WIInfo struct {
	WorkItem   string
	Project    string
	CapsuleDir string
	BundleName string
	Git        gitops.Opts
}

// Handle is the agent face a monitor watches. Implemented by the Runtime
// lane's process adapter; Observe owns no process code. The handle rides
// the RT channel WITH the protocol lifecycle message it belongs to — the
// five messages stay the only control vocabulary, and the handle is the
// attachment Observe needs to observe and control.
type Handle interface {
	// AttemptID is the attempt this handle belongs to.
	AttemptID() string
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
	version int64
	sha     string
	pushed  bool
	err     error
}

type probeResult struct {
	ack protocol.ControlAck
	err error
}

type wrapResult struct {
	ok          bool
	sha         string
	pushed      bool
	err         string
	version     int64
	finalStatus string
	call        FlushCall
}

// monitorCfg carries the tunables (defaults in observe.go Config).
type monitorCfg struct {
	PollInterval      time.Duration
	HangWindow        time.Duration
	ProbeTimeout      time.Duration
	AnswerAckWindow   time.Duration
	PushRetryBackoff  time.Duration
	BackpressureSteps int
	BackpressureAfter time.Duration
	FlushBound        time.Duration
	push              PushFunc
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
	handle    Handle
	attemptID string // guarded by stateMu for the HTTP read face
	records   <-chan []byte
	exitCh    <-chan struct{}
	stream    *streamState

	rtCh     chan Attach
	stopCh   chan struct{}
	stop     sync.Once
	answerCh chan AnswerCall
	flushCh  chan FlushCall

	// execution.json polling state (monitor goroutine only).
	execPath      string
	execMtime     time.Time
	currentStep   string
	dirtyVersion  int64 // latest Version seen that has no confirmed push
	pushedVersion int64

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

	// hang detection.
	probeInFlight bool
	probeDone     chan probeResult
	hangAnchor    time.Time // probe-cleared window restart

	ledger *AnswerLedger

	// stateMu guards the fields every goroutine (HTTP view, flush/answer
	// sub-goroutines) reads.
	stateMu    sync.Mutex
	pending    *protocol.PendingView
	sealing    bool
	wrapped    bool // attempt reached a terminal state via flush(wrap)
	blocked    bool
	recovering bool
}

func (m *Monitor) execJSONPath() string {
	return filepath.Join(m.wi.CapsuleDir, capsule.ExecutionJSON)
}

// ---------------------------------------------------------------------------
// main loop

func (m *Monitor) run() {
	defer func() {
		_ = m.log.Append(m.wi.WorkItem, EvJobStopped, map[string]any{
			"attempt_id": m.currentAttempt(),
		})
	}()
	_ = m.log.Append(m.wi.WorkItem, EvJobStarted, map[string]any{
		"attempt_id": m.attemptID,
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
	m.drainIncidents()
	m.checkHang()
	m.pollExecution()
	m.schedulePush()
	m.checkBackpressure()
}

// ---------------------------------------------------------------------------
// detection: provider errors (incident chain → storm-guarded recovery)

// drainIncidents is the provider-error detection surface: every
// classified incident becomes an IMMEDIATE infra_incident event (the
// operator learns about a dead provider in the same tick the harness
// reported it), while the RECOVERY decision is storm-guarded: the same
// step needs two consecutive incidents — counted in stream order —
// before one WINeedsRecovery goes out.
func (m *Monitor) drainIncidents() {
	for _, in := range m.stream.drain() {
		_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
			"kind": in.Kind, "detail": in.Detail, "fatal": in.Fatal,
			"at": in.At.UTC().Format(time.RFC3339Nano),
		})
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
	suppressed := m.recovering || m.wrapped || m.blocked || m.sealing
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
	if m.recovering || m.wrapped || m.blocked || m.sealing {
		m.stateMu.Unlock()
		return
	}
	m.recovering = true
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvNeedsRecovery, map[string]any{
		"reason": reason, "detail": detail, "attempt_id": m.currentAttempt(),
	})
	m.sendOB(protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, AttemptID: m.currentAttempt(),
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
	suppressed := m.recovering || m.wrapped || m.blocked || m.sealing
	m.stateMu.Unlock()
	if suppressed {
		return
	}
	m.probeInFlight = true
	_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
		"kind": HangSuspected,
		"detail": fmt.Sprintf("stream silent past the %.0fs window with a turn in flight; sending liveness probe",
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

func (m *Monitor) onProbeResult(pr probeResult) {
	m.probeInFlight = false
	if pr.err == nil && pr.ack.OK {
		// The agent answered: kill cancelled, window restarts.
		m.hangAnchor = time.Now()
		_ = m.log.Append(m.wi.WorkItem, EvInfraIncident, map[string]any{
			"kind": HangCleared, "detail": "liveness probe acknowledged; hang window restarted",
		})
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

// onRecovered adopts the new handle Runtime derived (WIRecovered). The
// monitor leaves RECOVERING, resets the detection surfaces and re-arms
// them against the new attempt.
func (m *Monitor) onRecovered(at Attach) {
	if at.Handle == nil || at.Msg.AttemptID == "" {
		return
	}
	m.handle = at.Handle
	m.records = at.Handle.Records()
	m.exitCh = at.Handle.Exit()
	m.stream = newStreamState()
	m.probeInFlight = false
	m.hangAnchor = time.Now()
	m.stateMu.Lock()
	m.recovering = false
	m.attemptID = at.Msg.AttemptID
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvAgentRecovered, map[string]any{
		"attempt_id": at.Msg.AttemptID,
	})
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
	m.applyPendingFromExec(&exec)
}

// applyPendingFromExec reconciles the pending-question projection with
// the authoritative file (the agent persists pending_question in
// execution.json BEFORE calling flush(needs_input); both sources are
// idempotent by question id).
func (m *Monitor) applyPendingFromExec(exec *capsule.Execution) {
	var pv *protocol.PendingView
	if s := exec.Steps[exec.CurrentStep]; s != nil && s.PendingQuestion != nil {
		q := s.PendingQuestion
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
	case pv != nil && m.pending == nil:
		m.pending = pv
	case pv != nil && m.pending != nil && m.pending.ID != pv.ID:
		m.pending = pv
	case pv == nil && m.pending != nil:
		m.ledger.clearAwaitingQuestion(m.wi.WorkItem, m.pending.ID)
		m.pending = nil
		m.stateMu.Unlock()
		return
	default:
		m.stateMu.Unlock()
		return
	}
	m.stateMu.Unlock()
	_ = m.log.Append(m.wi.WorkItem, EvNeedsInput, map[string]any{
		"question_id": pv.ID, "step": pv.Step, "question": pv.Question,
	})
}

// schedulePush starts one async commit+push sub-goroutine when a newer
// execution Version exists, the retry backoff has elapsed, and the
// single-in-flight gate is free.
func (m *Monitor) schedulePush() {
	if m.dirtyVersion <= m.pushedVersion {
		return
	}
	if !m.retryAt.IsZero() && !time.Now().After(m.retryAt) {
		return
	}
	// try-acquire the gate: at most ONE push in flight per WI.
	select {
	case m.gate <- struct{}{}:
	default:
		return // one already in flight; the next tick retries
	}
	m.retryAt = time.Time{}
	version := m.dirtyVersion // the seq trailer is gitops' single authority (derived in-lock)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.FlushBound)
		defer cancel()
		sha, pushed, err := m.cfg.push(ctx, m.wi.Git,
			fmt.Sprintf("observe: execution v%d", version))
		m.pushDone <- pushResult{version: version, sha: sha, pushed: pushed, err: err}
	}()
}

func (m *Monitor) onPushResult(res pushResult) {
	<-m.gate // release the single-in-flight slot
	if res.err == nil {
		if res.version > m.pushedVersion {
			m.pushedVersion = res.version
		}
		m.lastPushOK = time.Now()
		m.failedPushes = 0
		m.firstFailAt = time.Time{}
		m.pushDegraded = false
		m.timeDegraded = false
		_ = m.log.Append(m.wi.WorkItem, EvPublished, map[string]any{
			"version": res.version, "local_commit": res.sha, "pushed": res.pushed,
		})
		return
	}
	m.failedPushes++
	if m.firstFailAt.IsZero() {
		m.firstFailAt = time.Now()
	}
	m.retryAt = time.Now().Add(m.cfg.PushRetryBackoff)
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
		delivered, err := ledger.SubmitFor(wi, call.Relay, call.Caller, fp, func(relay AnswerRelay) error {
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
		})
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
// flush(reason) handling (the UDS face entry)

func (m *Monitor) onFlush(call FlushCall) {
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
// request is the only refusal; any daemon-side trouble degrades to an
// events-file entry with no sync push.
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
	go func() {
		deadline := time.Now().Add(m.cfg.FlushBound)
		var (
			lastSHA, lastErr string
			pushed, ok       bool
			psha             string
		)
		for try := 1; try <= wrapRetries && !ok; try++ {
			m.gate <- struct{}{} // wait out any in-flight async push
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			sha, p, err := m.cfg.push(ctx, m.wi.Git, "wrap: "+finalStatus)
			cancel()
			<-m.gate
			psha, pushed = sha, p
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
			ok: ok, sha: pickSHA(ok, psha, lastSHA), pushed: pushed,
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

func (m *Monitor) onWrapResult(wr wrapResult) {
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
		_ = m.log.Append(m.wi.WorkItem, EvPublished, map[string]any{
			"wrap": true, "local_commit": wr.sha, "pushed": wr.pushed, "final_status": wr.finalStatus,
		})
		reason := wr.finalStatus
		if reason != protocol.FinishFailed {
			reason = protocol.FinishWrapped
		}
		m.sendOB(protocol.OBMsg{
			Kind: protocol.OBCompleted, AttemptID: m.currentAttempt(),
			WorkItem: m.wi.WorkItem, Reason: reason,
		})
	} else {
		m.stateMu.Lock()
		m.sealing = false
		m.blocked = true
		m.stateMu.Unlock()
		ack.Error = "wrap flush failed after " + itoa(wrapRetries) + " retries"
		if wr.err != "" {
			ack.Error += ": " + wr.err
		}
		_ = m.log.Append(m.wi.WorkItem, EvWrapBlocked, map[string]any{
			"error": wr.err, "local_commit": wr.sha,
		})
		m.sendOB(protocol.OBMsg{
			Kind: protocol.OBCompleted, AttemptID: m.currentAttempt(),
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
	go func() {
		m.gate <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.FlushBound)
		sha, pushed, err := m.cfg.push(ctx, m.wi.Git, "shutdown flush")
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
			})
		}
		call.Resp <- ack
	}()
}

// ---------------------------------------------------------------------------
// state read face (HTTP view + sub-goroutines)

func (m *Monitor) currentAttempt() string {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.attemptID
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
