package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// DaemonClient is the client for the daemon's local unix-socket face
// (04-local-runtime.md §5.1: the daemon listens on /run/polyforge.sock,
// local machine only, no network face). The wire contract is the daemon
// implementation itself: internal/daemon/api.go (same repo, main) — an
// HTTP mux over the unix domain socket with a /jobs/{id}/... route family
// plus /status, /mode, /batch_submit and /shutdown.
//
// Wire facts pinned by internal/daemon/api.go:
//
//   - Routes:
//     claim/resume/takeover  POST /jobs/{wi_id}/claim|resume|takeover
//     body {"priority":"normal"} (optional)
//     observe                GET  /jobs/{wi_id}/observe?after=<cursor>
//     answer                 POST /jobs/{wi_id}/answer
//     body = the five-field relay message itself
//     (00 §3.4a), NOT wrapped; caller identity
//     presented via the X-PF-User header.
//     publish_request        POST /jobs/{wi_id}/publish_request {"reason"}
//     status                 GET  /status
//     mode                   POST /mode {"action":set|drain|abort-drain|
//     get_recommendation, "mode":..., "max_concurrent_jobs":N}
//     batch_submit           POST /batch_submit {"wi_ids":[...],
//     "priorities":{...}}  (underscore)
//     shutdown               POST /shutdown
//   - Error envelope: {"error":{"code":<DAEMON-*>,"detail":...}} — same
//     outer shape as the aihub face but without a reason field.
//   - The daemon performs the layer-two read-through itself when the WI is
//     not local (observe answers with events:[], state_snapshot:<progress>,
//     head_seq, read_through:true, freshness_note) and answers DAEMON-404
//     only when neither layer has anything.
//   - mode has no "get" action: the current mode is queried via GET /status.
//
// CLIENT-SIDE INFERENCES (recorded; not yet pinned by the daemon face):
//
//   - pause/wrap: 00 §5.1 routes the pause/wrap verbs to the daemon face
//     (the closing flush_evidence is daemon-generated, 02 §6.4), but the
//     daemon operations table (04 §5.1) has no pause/wrap row and
//     internal/daemon/api.go exposes no such route today. The natural
//     projection into the existing /jobs/{id}/... family is used:
//     pause   POST /jobs/{wi_id}/pause  {}
//     wrap    POST /jobs/{wi_id}/wrap   {"reason":"wrap"|"fail"}
//     until the daemon lands its own routes (spec gap, see README).
//   - pause/wrap request bodies carry no generation token: the daemon
//     holds the token and signs the Core end-generation call itself.
type DaemonClient struct {
	Sock string
	HTTP *http.Client
}

// NewDaemonClient builds a client dialing the given unix socket.
func NewDaemonClient(sock string) *DaemonClient {
	// All requests on one connection pool; the daemon is local so no
	// aggressive timeout beyond a generous one for long-running ops.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &DaemonClient{
		Sock: sock,
		HTTP: &http.Client{Transport: tr, Timeout: 120 * time.Second},
	}
}

func (d *DaemonClient) do(method, path, query string, body any, header map[string]string) ([]byte, error) {
	u := "http://daemon" + path // host is decorative; transport dials the socket
	if query != "" {
		u += "?" + query
	}
	return execute(d.HTTP, method, u, body, header)
}

// AnswerPayload is the answer relay message (00-architecture.md §3.4a,
// single authority, mirrored by internal/daemon/answers.go AnswerRelay):
// the daemon forwards it verbatim, never parses or rewrites it.
type AnswerPayload struct {
	QuestionID string `json:"question_id"`
	AnswerID   string `json:"answer_id"`
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
}

// ObserveResponse is the layer-one event-stream observe response
// (04 §5.1a): fixed three segments, plus compacted=true when the cursor
// fell out of the ring buffer (then events is absent). When the daemon
// performed the layer-two read-through itself (no local Capsule), the
// read_through/freshness_note/head_seq fields are set instead.
type ObserveResponse struct {
	Events        []DaemonEvent   `json:"events,omitempty"`
	StateSnapshot json.RawMessage `json:"state_snapshot"`
	NextCursor    int64           `json:"next_cursor"`
	Compacted     bool            `json:"compacted,omitempty"`
	// Layer-two (daemon-side read-through) markers.
	ReadThrough   bool   `json:"read_through,omitempty"`
	FreshnessNote string `json:"freshness_note,omitempty"`
	HeadSeq       *int64 `json:"head_seq,omitempty"`
}

// DaemonEvent is one coarse ring-buffer event (nine-type closed set,
// 04 §5.1a). Events are a live view, never persisted.
type DaemonEvent struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// StatusResponse is the GET /status answer (internal/daemon/api.go
// hStatus): local bookkeeping + mode + queue counters. It is NOT workflow
// execution state (INV-D1) — just "what this machine is managing".
type StatusResponse struct {
	MachineID    string      `json:"machine_id"`
	Mode         string      `json:"mode"`
	ShuttingDown bool        `json:"shutting_down"`
	Jobs         []DaemonJob `json:"jobs"`
	Queue        QueueStats  `json:"queue"`
}

// DaemonJob is one jobs[] bookkeeping entry. The daemon face exposes the
// identity fields only (no generation/pid/status in the HTTP answer).
type DaemonJob struct {
	WorkItemID       string `json:"work_item_id"`
	HarnessAdapter   string `json:"harness_adapter"`
	HarnessSessionID string `json:"harness_session_id"`
	RingLatest       int64  `json:"ring_latest"`
}

// QueueStats is the local job queue counters.
type QueueStats struct {
	Pending       int `json:"pending"`
	Running       int `json:"running"`
	MaxConcurrent int `json:"max_concurrent"`
}

// SubmitResponse is the claim-family answer (hSubmit): accepted plus the id.
type SubmitResponse struct {
	Accepted   bool   `json:"accepted"`
	WorkItemID string `json:"work_item_id"`
}

// ModeRequest is the POST /mode body (hMode). Action is one of
// set|drain|abort-drain|get_recommendation; Mode (interactive|batch|drain)
// and MaxConcurrentJobs apply to action=set only. There is no "get"
// action — mode queries go through GET /status.
type ModeRequest struct {
	Action            string `json:"action"`
	Mode              string `json:"mode,omitempty"`
	MaxConcurrentJobs *int   `json:"max_concurrent_jobs,omitempty"`
}

// Claim submits a single-WI interactive claim request (00 §3.4a submit).
// priority defaults to "normal" server-side; empty keeps the default.
func (d *DaemonClient) Claim(wiID, priority string) ([]byte, error) {
	return d.submit(wiID, "claim", priority)
}

// Resume submits a resume request — always a fresh claim, generation+1.
func (d *DaemonClient) Resume(wiID, priority string) ([]byte, error) {
	return d.submit(wiID, "resume", priority)
}

// Takeover submits an admin force-takeover request.
func (d *DaemonClient) Takeover(wiID, priority string) ([]byte, error) {
	return d.submit(wiID, "takeover", priority)
}

// submit is the shared shape of POST /jobs/{id}/claim|resume|takeover.
func (d *DaemonClient) submit(wiID, intent, priority string) ([]byte, error) {
	if priority == "" {
		priority = "normal"
	}
	return d.do("POST", "/jobs/"+pathEscape(wiID)+"/"+intent, "",
		map[string]any{"priority": priority}, nil)
}

// Observe pulls the layer-one incremental event stream after the given
// cursor (first call: after=0).
func (d *DaemonClient) Observe(wiID string, after int64) ([]byte, error) {
	q := "after=" + itoa64(after)
	return d.do("GET", "/jobs/"+pathEscape(wiID)+"/observe", q, nil, nil)
}

// Status returns the local jobs bookkeeping (status()).
func (d *DaemonClient) Status() ([]byte, error) {
	return d.do("GET", "/status", "", nil, nil)
}

// Answer relays an answer to a needs_input step. The relay message is the
// request body itself (hAnswer decodes AnswerRelay directly); the caller
// identity travels in the X-PF-User header and must match answered_by
// (00 §3.4a identity rule, DAEMON-403 otherwise).
func (d *DaemonClient) Answer(wiID string, payload AnswerPayload) ([]byte, error) {
	return d.do("POST", "/jobs/"+pathEscape(wiID)+"/answer", "", payload,
		map[string]string{"X-PF-User": payload.AnsweredBy})
}

// Mode issues one POST /mode operation.
func (d *DaemonClient) Mode(req ModeRequest) ([]byte, error) {
	return d.do("POST", "/mode", "", req, nil)
}

// BatchSubmit submits the human-confirmed wave; the daemon never widens
// or narrows the set (00 §3.4b).
func (d *DaemonClient) BatchSubmit(wiIDs []string) ([]byte, error) {
	return d.do("POST", "/batch_submit", "", map[string]any{"wi_ids": wiIDs}, nil)
}

// Pause asks the daemon to pause a running WI: WI Agent stops at a safe
// boundary, the daemon executes the boundary publish (final flush) and
// closes the generation with the daemon-generated flush_evidence
// (02 §6.4, 04 §6.2 A-path). Route is a client-side inference (see the
// file header note).
func (d *DaemonClient) Pause(wiID string) ([]byte, error) {
	return d.do("POST", "/jobs/"+pathEscape(wiID)+"/pause", "", map[string]any{}, nil)
}

// Wrap asks the daemon to wrap (or fail, when reason=="fail") a running
// WI — same final-flush requirement as pause (02 §6.4). Route is a
// client-side inference (see the file header note).
func (d *DaemonClient) Wrap(wiID, reason string) ([]byte, error) {
	return d.do("POST", "/jobs/"+pathEscape(wiID)+"/wrap", "",
		map[string]any{"reason": reason}, nil)
}

// PublishRequest is the internal WI-Agent-facing operation (04 §5.1);
// exposed on the client for completeness; the CLI itself never drives it.
func (d *DaemonClient) PublishRequest(wiID, reason string) ([]byte, error) {
	return d.do("POST", "/jobs/"+pathEscape(wiID)+"/publish_request", "",
		map[string]any{"reason": reason}, nil)
}

// Shutdown triggers graceful shutdown (quiesce) — the daemon process
// lifecycle termination path, NOT drain (00 §3.5, INV-13).
func (d *DaemonClient) Shutdown() ([]byte, error) {
	return d.do("POST", "/shutdown", "", map[string]any{}, nil)
}
