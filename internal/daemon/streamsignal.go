package daemon

// streamsignal.go: the OPTIONAL stream-observation faces of AgentHandle —
// the daemon's half of the polyforge#48 stream-heartbeat criterion (04
// §6.2 runtime note, stream-heartbeat revision).
//
// WHY A NEW SIGNAL AT ALL. The 04 §6.2 hang watchdog judged liveness from
// the MAIN agent pid's /proc counters (I/O + CPU jiffies) alone. The
// pilot of 2026-09-29 00:45 measured that criterion failing in
// production: the agent was hung by every human standard (no TCP
// connection in flight, no session write for 12+ minutes, zero io
// increment) yet /proc/<pid>/stat kept ticking +1 jiffy every ~10s —
// the node runtime's epoll timer accounting. "ANY counter moved ⇒
// activity" read that idle noise as work and the watchdog NEVER fired.
// The probe was measuring the wrong thing: process scheduling, not
// agent progress.
//
// THE RULED CRITERION. The supervisor's stdout reader already sees every
// protocol record the harness emits — it has to, it is the ack carrier.
// A record on that stream is PROOF OF AGENT PROGRESS in a way a jiffy
// never was: pi emits records for run start, turn start, every message
// boundary and every streamed text delta, and a step command or subagent
// streams records throughout. Silence on the stream WHILE A TURN IS IN
// FLIGHT is the hang. So:
//
//	inFlight && now-lastRecordAt > window  ⇒  hung (confirmation probe, then kill)
//
// The procfs probe DEMOTES to auxiliary evidence: it is attached to the
// incident for the operator (and still carries the judgment for a handle
// with no stream face at all — an adopted agent after a daemon restart),
// but it no longer overrides the stream. The wall backstop stays exactly
// as it was: the absolute bound that holds when every probe lies.
//
// Both faces are ADDITIVE, the same discipline AgentExitProbe follows in
// watchdog.go: a handle that does not implement them keeps its
// pre-existing behavior, and nothing in AgentHandle changes.

import (
	"context"
	"time"
)

// StreamHeartbeat is one point-in-time reading of a supervised agent's
// protocol record stream.
type StreamHeartbeat struct {
	// Observable reports whether this handle can answer at all. False
	// means "no stream under supervision" (an ADOPTED agent after a
	// daemon restart re-attaches to a pid, not to a stdout pipe): the
	// watchdog degrades that job to the procfs criterion plus the
	// backstop, and says so.
	Observable bool
	// LastRecordAt is when the reader last classified a record that
	// represents AGENT progress. Deliberately NOT every record: the
	// `response` echo of a command the DAEMON itself just sent is
	// produced by the harness's RPC reader thread, independent of any
	// model turn, so counting it would let the daemon's own probe
	// traffic reset the hang window it is trying to measure.
	LastRecordAt time.Time
	// LastAnyRecordAt includes the response echoes: auxiliary evidence
	// only ("the harness process still reads stdin"), never a criterion.
	LastAnyRecordAt time.Time
	// InFlight reports whether a TURN is open right now: the stream
	// carried turn_start (or agent_start) and no matching turn_end /
	// agent_end / agent_settled yet. Silence with NO turn in flight is
	// an idle agent waiting for work or for a human answer — never a
	// hang, which is exactly the needs_input exemption falling out of
	// the criterion instead of being special-cased (04 §6.2 note).
	InFlight bool
	// Records is the total count of progress records seen on this
	// instance's stream (diagnostics / test assertions).
	Records int64
}

// Silent reports whether the stream has been silent for at least window
// WHILE A TURN IS IN FLIGHT — the ruled hang criterion. An unobservable
// stream, or an idle one, is never silent in this sense.
func (h StreamHeartbeat) Silent(now time.Time, window time.Duration) bool {
	if !h.Observable || !h.InFlight || h.LastRecordAt.IsZero() {
		return false
	}
	return now.Sub(h.LastRecordAt) >= window
}

// AgentStreamProbe is the OPTIONAL stream-observation extension of
// AgentHandle (mainagent.Proc implements it; adopted handles and simple
// test fakes do not).
type AgentStreamProbe interface {
	// StreamHeartbeat reads the current heartbeat. Non-blocking.
	StreamHeartbeat() StreamHeartbeat
	// DrainStreamIncidents removes and returns the infra incidents the
	// classifier queued since the last drain (provider errors and retry
	// records). Non-blocking; drains to empty.
	DrainStreamIncidents() []StreamIncident
}

// Stream incident kinds (the closed set the classifier emits).
const (
	// IncidentProviderError is an upstream model/provider failure the
	// harness reported INSIDE its ordinary records. Measured shape (pi
	// 0.87.1, `pi --mode rpc`, 2026-09-29): there is NO `type:"error"`
	// record and no pi-side `result` record — the failure rides
	// message.stopReason=="error" plus message.errorMessage on the
	// assistant message_start/message_end AND on turn_end, e.g.
	// "sub2api-openai API error (401): 401 status code (no body)".
	IncidentProviderError = "provider_error"
	// IncidentRetry is a harness-reported retry of an upstream call.
	IncidentRetry = "retry"
)

// StreamIncident is one classified infra event from the record stream.
// polyforge#48 item 3: before this, an error turn was counted as bulk
// (a stderr line at most) and the operator learned about a dead provider
// only from the hang that followed minutes later. An incident is an
// IMMEDIATE signal: logged, and appended to the job's observe ring so
// `pf observe` shows it in the same tick the harness reported it.
type StreamIncident struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	// Fatal reports whether the turn ENDED on this error (turn_end with
	// stopReason=error). A non-fatal incident is a mid-turn report the
	// harness may still recover from.
	Fatal bool `json:"fatal"`
}

// AgentLivenessProbe is the OPTIONAL confirmation-probe face (the ruled
// step between "the window expired" and "kill"): the daemon sends one
// control-shaped probe down the SAME channel every other control method
// uses and waits turn-scale for the agent's ack.
//
// The ack is deliberately the AGENT's, not the harness's: a harness
// whose upstream model call is wedged still answers its RPC layer
// instantly (the measured `response` record is emitted before any model
// work), so accepting a transport-level echo would rebuild the very
// false negative polyforge#48 is about. Only a pf/probe ack — which
// pf-execute can only produce at a turn boundary — proves the agent
// loop is still turning.
type AgentLivenessProbe interface {
	// ProbeLiveness sends one probe and waits for the ack, bounded by
	// ctx. A nil error means the agent answered.
	ProbeLiveness(ctx context.Context) error
}

// ErrAnswerAckPending is the TWO-PHASE answer outcome (polyforge#48
// item 5, folded from #49): the relay was written to the agent's control
// channel — delivery is confirmed — but the agent had not acked within
// the ack window. It is NOT a delivery failure and NOT an error-grade
// outcome: the pilot of 2026-09-29 01:10 measured the answer landing
// 45-90 seconds AFTER the daemon had already reported DAEMON-500, because
// a real agent's first turn over an answer (half a megabyte of context
// plus the ack orchestration) outruns any fake-calibrated window.
//
// Callers treat it as "delivered, consumption pending": the CLI says so
// instead of printing an error, and the ledger entry stays REDELIVERABLE
// so a crash continuation re-delivers it idempotently.
var ErrAnswerAckPending = errAnswerAckPending{}

type errAnswerAckPending struct{}

func (errAnswerAckPending) Error() string {
	return "daemon: answer delivered, agent consumption ack still pending (not an error)"
}
