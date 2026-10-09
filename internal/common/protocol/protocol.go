// Package protocol defines the three in-process/in-machine contracts of v1
// (design authority: docs/design/polyforge-vnext/v1-architecture-spec.md +
// v1-spec-full.md §二 §五, rulings C2/C3):
//
//  1. Runtime ↔ Observe messages (5 kinds, every message carries the
//     per-WI derivation counter spawn_n).
//  2. The agent-side control surface, split across two transports (ruling C2):
//     - stdio control channel (daemon→agent): answer, stop_notice, probe
//     - UDS proxy (agent→daemon): flush(reason)  [replaces publish_request]
//  3. The pf CLI ↔ daemon UDS HTTP API surface (04 §5).
package protocol

import "encoding/json"

// ---------------------------------------------------------------------------
// 1. Runtime ↔ Observe messages (v1-spec-full §二)
// ---------------------------------------------------------------------------

// RTMsg is a Runtime→Observe message.
type RTMsg struct {
	Kind RTMsgKind `json:"kind"`
	// v1.5 B2: the per-WI monotonic derivation counter — the
	// session-model fence key.
	SpawnN   int    `json:"spawn_n"`
	WorkItem string `json:"work_item"`
	AgentPID int    `json:"agent_pid,omitempty"`
	Reason   string `json:"reason,omitempty"` // WIFinished: wrapped|failed
	Detail   string `json:"detail,omitempty"`
	// SessionID (v1.5): the WI session secret, passed runtime→observe
	// IN-PROCESS so the monitor can run the periodic session-verify (M4).
	// It never leaves the daemon: not in logs, not in capsule, not in
	// agent env.
	SessionID string `json:"session_id,omitempty"`
}

// OBMsg is an Observe→Runtime message. Observe must attach the derivation
// counter (spawn_n) of the agent handle it was monitoring when the event
// fired; Runtime drops messages whose counter no longer matches the
// current derivation.
type OBMsg struct {
	Kind     OBMsgKind `json:"kind"`
	SpawnN   int       `json:"spawn_n"`
	WorkItem string    `json:"work_item"`
	Reason   string    `json:"reason"` // WINeedsRecovery: death|hang|provider_error|idle|...
	Detail   string    `json:"detail,omitempty"`
	// SessionFP (v1.5 NEW-1 fence): OBSuperseded carries the SUPERSEDED
	// session's public fingerprint. The runtime fences the message on it
	// (session-level verdict): a stale buffered message must never kill a
	// successor job of the same WI claimed after a takeover-back.
	SessionFP string `json:"session_fp,omitempty"`
}

type RTMsgKind string

const (
	RTStarted   RTMsgKind = "wi_started"   // new attempt began; carries agent handle
	RTRecovered RTMsgKind = "wi_recovered" // recovery succeeded; new handle, same attempt lineage
	RTFinished  RTMsgKind = "wi_finished"  // terminal: stop monitoring
)

type OBMsgKind string

const (
	OBNeedsRecovery OBMsgKind = "wi_needs_recovery" // agent died/hung; Runtime recovers
	OBCompleted     OBMsgKind = "wi_completed"      // flush(wrap) landed; final state attached
	// OBSuperseded (v1.5 M4): the periodic session revalidation learned
	// this WI is held by another daemon. Runtime's response is NOT a
	// recovery — it kills the tree, quarantines the capsule and drops the
	// record; the daemon makes no hub transition (the new owner owns it).
	OBSuperseded OBMsgKind = "wi_superseded"
)

// Recovery causes carried by WINeedsRecovery.
const (
	RecoverDeath         = "agent_death"
	RecoverHang          = "hang"
	RecoverProviderError = "provider_error"
	// RecoverIdle is the F5 watchdog cause: the session is alive but
	// nobody drives it — no progress record, no turn in flight past the
	// idle window, and the liveness probe went unacknowledged.
	RecoverIdle = "idle"
)

// Terminal reasons carried by WIFinished.
const (
	FinishWrapped = "wrapped"
	FinishFailed  = "failed"
	FinishBlocked = "blocked"
)

// Non-terminal release reasons carried by WIFinished (v1.5 review
// Findings 2 and 4): the runtime's OBSERVE RELEASE. RTFinished is not a
// terminal-only message — it is also the "this daemon no longer
// supervises the work item" signal that stops the monitor loop, prunes
// the latestSpawn entry and makes the answer ledger forget the work
// item. FinishPaused rides the operator pause close (session-end's 4th
// verdict, M3); FinishSuperseded rides the OBSuperseded response (M4).
const (
	FinishPaused     = "paused"
	FinishSuperseded = "superseded"
)

// ---------------------------------------------------------------------------
// 2. Agent-side control surface (ruling C2: two transports, two method sets)
// ---------------------------------------------------------------------------

// --- stdio control channel: daemon writes one JSON line to agent stdin,
// agent acknowledges on stdout. One method only (polyforge#106: the
// answer relay and the stop-notice delivery died with the WI-Agent
// engine; the probe is the channel's kept occupant). ---

const (
	// ControlProbe asks the agent to prove liveness within the 2-minute
	// budget. Payload: {"method":"pf/probe","nonce":<hex>}
	ControlProbe = "pf/probe"
)

// ControlMsg is the stdio control-channel envelope. Seq is the numeric
// correlation token (monotonic per sender); pf/answer additionally
// carries the question id in ID and pf/probe its nonce in Nonce — the
// ack MUST echo one of them so concurrent waits cannot steal each
// other's acks.
type ControlMsg struct {
	Method string `json:"method"`
	Seq    int64  `json:"seq,omitempty"`
	ID     string `json:"id,omitempty"`     // answer: question id
	Answer string `json:"answer,omitempty"` // answer: human text
	Nonce  string `json:"nonce,omitempty"`  // probe: echo this back
}

// ControlAck is the agent's stdout reply to a control method. It MUST
// echo the correlation token of the message it answers: the numeric Seq
// and/or Ref (pf/answer → the question id, pf/probe → the nonce). The
// daemon-side wait matches on (method, correlation) — an ack that
// echoes neither token matches nothing.
type ControlAck struct {
	Method string `json:"method"` // echoes the method
	Seq    int64  `json:"seq,omitempty"`
	Ref    string `json:"ref,omitempty"` // echoes answer question id / probe nonce
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// --- UDS proxy: the agent calls daemon endpoints over the UDS socket. ---

// Flush reasons (ruling C3: only these three block the agent).
const (
	FlushNeedsInput = "needs_input" // human answer required before continuing
	FlushWrap       = "wrap"        // final result; seal + push before ack
	FlushShutdown   = "shutdown"    // daemon is closing; commit what exists
)

// FlushRequest is the agent→daemon flush(reason) call on the UDS proxy.
type FlushRequest struct {
	WorkItem string `json:"work_item"`
	// v1.5 B2: the per-WI monotonic derivation counter. The flush fence
	// validates spawn_n against the monitor's current/latest-announced
	// derivation; an absent counter (0) is refused fail-closed.
	SpawnN int    `json:"spawn_n"`
	Reason string `json:"reason"` // one of the Flush* constants
	// needs_input only:
	QuestionID string `json:"question_id,omitempty"`
	Question   string `json:"question,omitempty"`
	Step       string `json:"step,omitempty"`
	// wrap only:
	FinalStatus string `json:"final_status,omitempty"` // wrapped|failed
	Result      []byte `json:"result,omitempty"`
}

// FlushAck answers a flush. Per review ruling, the ack distinguishes local
// commit from server push: LocalCommit is the capsule-local git commit;
// Pushed means aihub accepted the push.
type FlushAck struct {
	OK          bool   `json:"ok"`
	LocalCommit string `json:"local_commit,omitempty"` // local commit SHA
	Pushed      bool   `json:"pushed"`
	// PushedTo (bare repo ref) has never been written by any flush path
	// yet; kept for the multi-machine visibility answers it will carry.
	PushedTo string `json:"pushed_to,omitempty"`
	Error    string `json:"error,omitempty"`
}

// FlushTimeout is the hard bound on any flush call (v1-spec-full §五).
const FlushTimeout = 60 * 1e9 // 60 seconds (ns, for time.Duration)

// ---------------------------------------------------------------------------
// 3. pf CLI ↔ daemon UDS HTTP API (04 §5.1; v0 surface minus publish_request)
// ---------------------------------------------------------------------------

// Daemon job states.
const (
	JobRunning    = "running"
	JobNeedsInput = "needs_input"
	JobSealing    = "sealing"
	JobDone       = "done"
)

// JobView is the observe projection served over UDS (04 §5.1a).
// SessionFingerprint is the PUBLIC session handle (B3: never the
// secret). The v1 attempt_id and workflow-generation fields are deleted
// with the lineage they named (v1.5 session model).
type JobView struct {
	WorkItem           string               `json:"work_item"`
	Project            string               `json:"project"`
	Status             string               `json:"status"`
	SpawnN             int                  `json:"spawn_n"`
	SessionFingerprint string               `json:"session_fingerprint,omitempty"`
	BundleName         string               `json:"bundle_name"`
	CurrentStep        string               `json:"current_step"`
	Steps              map[string]*StepView `json:"steps"`
	Budget             json.RawMessage      `json:"budget,omitempty"`
	Pending            *PendingView         `json:"pending_question,omitempty"`
	Events             []EventEntry         `json:"events"`
}

type StepView struct {
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	StartedAt string `json:"started_at,omitempty"`
	Model     string `json:"model,omitempty"`
	Output    string `json:"output,omitempty"`
}

type PendingView struct {
	ID       string `json:"id"`
	Step     string `json:"step"`
	Question string `json:"question"`
	AskedAt  string `json:"asked_at"`
}

// EventEntry is one line of the daemon events file + observe output.
// Seq is the observe ring's per-WI monotonic append ordinal, assigned at
// Append time; served entries carry it so a client can resume with
// ?after=<last seq>. The durable events file keeps its wire-stable shape
// and does NOT write a seq (Seq is a live-ring concept that a daemon
// restart legitimately renumbers from 1), so Seq marshals away there
// via omitempty.
type EventEntry struct {
	At   string          `json:"at"`
	Kind string          `json:"kind"`
	WI   string          `json:"work_item,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	Seq  int64           `json:"seq,omitempty"`
}

// Daemon modes (04 §3).
const (
	ModeInteractive = "interactive" // claim nothing on its own
	ModeBatch       = "batch"       // recommendation → confirm → execute
	ModeDrain       = "drain"       // autonomously claims queued work
)
