package protocol

// watch.go: the wire shapes of the `pf watch` lane — the per-WI watch
// snapshot and the daemon-held chat subscription routes (v2-converge §3).
// The daemon serves them over its UDS face; the watcher decodes them.
//
//	GET  /jobs/{wi}/watch?after=<seq>          -> WatchSnapshot
//	GET  /chats/{id}/subscriptions             -> ChatSubscriptions
//	POST /chats/{id}/subscribe   {"wi"}        -> ChatSubscribeAnswer
//	POST /chats/{id}/unsubscribe {"wi"}        -> ChatSubscribeAnswer
//	POST /chats/{id}/ack         ChatAck       -> {"ok":true}
//	POST /chats/{id}/heartbeat   {"pid"}       -> {"ok":true}
//	GET  /chats/{id}/attention?owner=&mark=1   -> ChatAttention

// WatchSnapshot is one WI's authoritative state plus the events after a
// cursor. Execution (execution.json) is the authority; Job is the daemon's
// local bookkeeping; Events carry only what happened after `after`.
type WatchSnapshot struct {
	WorkItem string `json:"work_item"`
	// Known is false when the daemon holds neither a job record nor a
	// capsule for the WI — a WI submitted but not yet registered (queued /
	// preparing), or one this daemon never saw.
	Known bool `json:"known"`
	// Queued is true while the submit sits in the daemon's queue.
	Queued    bool            `json:"queued,omitempty"`
	Job       *WatchJob       `json:"job,omitempty"`
	Execution *WatchExecution `json:"execution,omitempty"`
	// HubStatus is the work item's aihub status, filled only when the
	// daemon no longer holds a live local job for it (closed by wrap /
	// wrap --fail / cancel / pause): execution.json is not rewritten by a
	// manual close and keeps saying `running`, so this is the only
	// authority for how the WI ended. Empty when not consulted or the hub
	// was unreachable.
	HubStatus string `json:"hub_status,omitempty"`
	// Progress is the optional live-progress block (current command, recent
	// files, tool-call count); every field is shown only when present.
	Progress *WatchProgress `json:"progress,omitempty"`
	Events   []EventEntry   `json:"events"`
	// LastSeq is the highest event seq the daemon retains for the WI (0 =
	// none): a cursor above it means the numbering restarted.
	LastSeq int64 `json:"last_seq"`
}

// WatchJob is the daemon-side job bookkeeping of a WI.
type WatchJob struct {
	Status             string       `json:"status"`
	RepairReason       string       `json:"repair_reason,omitempty"`
	QuestionID         string       `json:"question_id,omitempty"`
	LastProgressAgoSec *int64       `json:"last_progress_ago_sec,omitempty"`
	Origin             *WatchOrigin `json:"origin,omitempty"`
}

// WatchOrigin mirrors the daemon's job origin record.
type WatchOrigin struct {
	Kind   string `json:"kind"`
	ChatID string `json:"chat_id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// WatchExecution is the subset of execution.json the watcher reads:
// workflow status, current step, per-step status/start, pending question.
type WatchExecution struct {
	WorkflowStatus  string                `json:"workflow_status"`
	CurrentStep     string                `json:"current_step"`
	UpdatedAt       string                `json:"updated_at,omitempty"`
	Steps           map[string]WatchStep  `json:"steps,omitempty"`
	PendingQuestion *WatchPendingQuestion `json:"pending_question,omitempty"`
}

// WatchStep is one step's status and start moment.
type WatchStep struct {
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
}

// WatchPendingQuestion is the open question of a step; Question is
// agent-written free text and is shown only by `pf watch <wi> --once`.
type WatchPendingQuestion struct {
	ID       string `json:"id"`
	Step     string `json:"step"`
	Question string `json:"question"`
	AskedAt  string `json:"asked_at,omitempty"`
}

// WatchProgress is the optional mechanical-progress block.
type WatchProgress struct {
	Command       string   `json:"command,omitempty"`
	CommandAgeSec *int64   `json:"command_age_sec,omitempty"`
	RecentFiles   []string `json:"recent_files,omitempty"`
	ToolCalls     *int64   `json:"tool_calls,omitempty"`
}

// ChatSub is one daemon-held subscription of a chat to a WI.
type ChatSub struct {
	WI string `json:"wi"`
	// Cursor is the last event seq the chat's watcher consumed.
	Cursor int64 `json:"cursor"`
	// Notified lists the state-derived decision keys already delivered
	// (e.g. "needs_input:q-1", "completed"); replaced wholesale on ack.
	Notified []string `json:"notified,omitempty"`
	// Done is set once the WI's terminal decision was delivered: the
	// watcher stops polling it.
	Done         bool   `json:"done,omitempty"`
	SubscribedAt string `json:"subscribed_at,omitempty"`
}

// ChatSubscriptions is the GET /chats/{id}/subscriptions answer.
type ChatSubscriptions struct {
	ChatID string `json:"chat_id"`
	// StatusPath is where the resident watcher writes the progress file
	// (under the daemon data dir).
	StatusPath    string    `json:"status_path"`
	Subscriptions []ChatSub `json:"subscriptions"`
}

// ChatSubscribeAnswer answers subscribe/unsubscribe.
type ChatSubscribeAnswer struct {
	ChatID  string `json:"chat_id"`
	WI      string `json:"wi"`
	Changed bool   `json:"changed"`
}

// ChatAck records a watcher's progress for one subscription: the new
// event cursor, the full set of delivered state keys, and whether the WI
// finished.
type ChatAck struct {
	WI       string   `json:"wi"`
	Cursor   int64    `json:"cursor"`
	Notified []string `json:"notified,omitempty"`
	Done     bool     `json:"done,omitempty"`
}

// ChatAttentionItem is one WI that needs a human and has no live watcher.
type ChatAttentionItem struct {
	WI     string `json:"wi"`
	Reason string `json:"reason"` // needs_input | repair_pending
}

// ChatAttention is the GET /chats/{id}/attention answer.
type ChatAttention struct {
	ChatID string              `json:"chat_id"`
	Items  []ChatAttentionItem `json:"items"`
}
