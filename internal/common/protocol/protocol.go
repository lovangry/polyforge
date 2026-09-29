// Package protocol defines the three in-process/in-machine contracts of v1
// (design authority: docs/design/polyforge-vnext/v1-architecture-spec.md +
// v1-spec-full.md §二 §五, rulings C2/C3):
//
//  1. Runtime ↔ Observe messages (5 kinds, every message carries attempt-id).
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
	Kind      RTMsgKind `json:"kind"`
	AttemptID string    `json:"attempt_id"` // fences stale messages
	WorkItem  string    `json:"work_item"`
	AgentPID  int       `json:"agent_pid,omitempty"`
	Reason    string    `json:"reason,omitempty"` // WIFinished: wrapped|failed
	Detail    string    `json:"detail,omitempty"`
}

// OBMsg is an Observe→Runtime message. Observe must attach the attempt-id of
// the agent handle it was monitoring when the event fired; Runtime drops
// messages whose attempt-id no longer matches the current attempt.
type OBMsg struct {
	Kind      OBMsgKind `json:"kind"`
	AttemptID string    `json:"attempt_id"`
	WorkItem  string    `json:"work_item"`
	Reason    string    `json:"reason"`  // WINeedsRecovery: death|hang|provider_error|...
	Detail    string    `json:"detail,omitempty"`
}

type RTMsgKind string

const (
	RTStarted  RTMsgKind = "wi_started"   // new attempt began; carries agent handle
	RTRecovered RTMsgKind = "wi_recovered" // recovery succeeded; new handle, same attempt lineage
	RTFinished  RTMsgKind = "wi_finished"  // terminal: stop monitoring
)

type OBMsgKind string

const (
	OBNeedsRecovery OBMsgKind = "wi_needs_recovery" // agent died/hung; Runtime recovers
	OBCompleted     OBMsgKind = "wi_completed"      // flush(wrap) landed; final state attached
)

// Recovery causes carried by WINeedsRecovery.
const (
	RecoverDeath         = "agent_death"
	RecoverHang          = "hang"
	RecoverProviderError = "provider_error"
	RecoverStreamStall   = "stream_stall"
)

// Terminal reasons carried by WIFinished.
const (
	FinishWrapped = "wrapped"
	FinishFailed  = "failed"
	FinishBlocked = "blocked"
)

// ---------------------------------------------------------------------------
// 2. Agent-side control surface (ruling C2: two transports, two method sets)
// ---------------------------------------------------------------------------

// --- stdio control channel: daemon writes one JSON line to agent stdin,
// agent acknowledges on stdout. Three methods only. ---

const (
	// ControlAnswer relays a human answer to a pending question. Payload:
	// {"method":"pf/answer","id":<qid>,"answer":<text>,"seq":<monotonic>}
	ControlAnswer = "pf/answer"
	// ControlStopNotice warns the agent of an imminent daemon shutdown so it
	// can finish the current sentence cleanly. Payload: {"method":"pf/stop_notice"}
	ControlStopNotice = "pf/stop_notice"
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
	WorkItem  string `json:"work_item"`
	AttemptID string `json:"attempt_id"`
	Reason    string `json:"reason"` // one of the Flush* constants
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
	PushedTo    string `json:"pushed_to,omitempty"` // bare repo ref, e.g. wi_xxx:refs/heads/main
	Error       string `json:"error,omitempty"`
}

// FlushTimeout is the hard bound on any flush call (v1-spec-full §五).
const FlushTimeout = 60 * 1e9 // 60 seconds (ns, for time.Duration)

// ---------------------------------------------------------------------------
// 3. pf CLI ↔ daemon UDS HTTP API (04 §5.1; v0 surface minus publish_request)
// ---------------------------------------------------------------------------

// Daemon job states.
const (
	JobQueued     = "queued"
	JobRunning    = "running"
	JobPaused     = "paused"
	JobNeedsInput = "needs_input"
	JobSealing    = "sealing"
	JobDone       = "done"
)

// JobView is the observe projection served over UDS (04 §5.1a).
type JobView struct {
	WorkItem    string          `json:"work_item"`
	Project     string          `json:"project"`
	Status      string          `json:"status"`
	Generation  int64           `json:"generation"`
	AttemptID   string          `json:"attempt_id"`
	BundleName  string          `json:"bundle_name"`
	CurrentStep string          `json:"current_step"`
	Steps       map[string]*StepView `json:"steps"`
	Budget      json.RawMessage `json:"budget,omitempty"`
	Pending     *PendingView    `json:"pending_question,omitempty"`
	Events      []EventEntry    `json:"events"`
}

type StepView struct {
	Status    string   `json:"status"`
	Attempts  int      `json:"attempts"`
	StartedAt string   `json:"started_at,omitempty"`
	Model     string   `json:"model,omitempty"`
	Output    string   `json:"output,omitempty"`
}

type PendingView struct {
	ID       string `json:"id"`
	Step     string `json:"step"`
	Question string `json:"question"`
	AskedAt  string `json:"asked_at"`
}

// EventEntry is one line of the daemon events file + observe output.
type EventEntry struct {
	At    string          `json:"at"`
	Kind  string          `json:"kind"`
	WI    string          `json:"work_item,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// Daemon modes (04 §3).
const (
	ModeInteractive = "interactive" // claim nothing on its own
	ModeBatch       = "batch"        // recommendation → confirm → execute
	ModeDrain       = "drain"        // autonomously claims queued work
)
