package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// DaemonClient speaks the daemon's UDS HTTP face: HTTP over the local
// unix socket (/run/polyforge.sock by default). The route family is the
// v0 daemon surface minus publish_request
// (docs/design/polyforge-vnext/04-local-runtime.md §5.1):
//
//	POST /jobs/{id}/claim|resume            body {"priority":"normal"}
//	GET  /jobs/{id}/observe?after=<cursor>  answer = protocol.JobView
//	POST /jobs/{id}/answer                  body = the five-field relay
//	                                        message itself (00 §3.4a)
//	POST /jobs/{id}/pause  {}               POST /jobs/{id}/wrap {"reason"}
//	POST /jobs/{id}/ship  {}                 GET  /status
//	POST /mode {"action":...}               POST /batch_submit {"wi_ids":[]}
//	POST /shutdown
//
// pf watch lane (v2-converge §3): GET /jobs/{id}/watch?after=<seq> (one WI's
// authoritative snapshot + events after a cursor) and the daemon-held chat
// subscriptions under /chats/{chat}/ (subscribe, unsubscribe, subscriptions,
// ack, heartbeat, attention) — see protocol/watch.go.
//
// Error envelope: the daemon answers non-2xx with the SAME typed
// envelope the aihub face uses (common/api.Error: {code, detail} at the
// top level, DAEMON-* codes per 04 §5.2). The phase-3 daemon assembly
// must serve exactly this shape.
type DaemonClient struct {
	Sock string
	HTTP *http.Client
	// Origin, when set, rides on the submit-family bodies so the daemon
	// can record where the job came from (chat id + owner).
	Origin *SubmitOrigin
}

// SubmitOrigin is the optional "origin" of a submit-family request body.
type SubmitOrigin struct {
	ChatID string `json:"chat_id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// ChatRecord is the POST /chat/init answer.
type ChatRecord struct {
	ChatID         string `json:"chat_id"`
	Harness        string `json:"harness"`
	HarnessSession string `json:"harness_session"`
	Created        bool   `json:"created"`
}

// ChatInit asks the daemon for the chat id of a harness session
// (created on first use).
func (d *DaemonClient) ChatInit(harness, session string) (json.RawMessage, error) {
	return d.do("POST", "/chat/init", "", map[string]any{"harness": harness, "harness_session": session}, nil)
}

// withOrigin adds the origin to a request body when one is configured.
func (d *DaemonClient) withOrigin(body map[string]any) map[string]any {
	if d.Origin != nil {
		body["origin"] = d.Origin
	}
	return body
}

// NewDaemonClient builds a client dialing the given unix socket.
func NewDaemonClient(sock string) *DaemonClient {
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

// do issues one request and returns the raw response body. Any non-2xx
// answer is decoded into *api.Error; a body without the envelope
// degrades to a synthesized "HTTP-<status>" code rather than masking
// the real status.
func (d *DaemonClient) do(method, path, query string, body any, header map[string]string) (json.RawMessage, error) {
	u := "http://daemon" + path // host is decorative; transport dials the socket
	if query != "" {
		u += "?" + query
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
	}
	var req *http.Request
	var err error
	if payload != nil {
		req, err = http.NewRequest(method, u, bytes.NewReader(payload))
	} else {
		req, err = http.NewRequest(method, u, nil)
	}
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, parseWireError(resp.StatusCode, raw)
	}
	return json.RawMessage(raw), nil
}

// parseWireError decodes a non-2xx body into *api.Error.
func parseWireError(status int, body []byte) *api.Error {
	var ae api.Error
	if json.Unmarshal(body, &ae) == nil && ae.Code != "" {
		return &ae
	}
	return &api.Error{
		Code:   fmt.Sprintf("HTTP-%d", status),
		Detail: strings.TrimSpace(string(body)),
	}
}

// AnswerPayload is the answer relay message (00 §3.4a, single authority):
// the daemon forwards it verbatim, never parses or rewrites it.
type AnswerPayload struct {
	QuestionID string `json:"question_id"`
	AnswerID   string `json:"answer_id"`
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
}

// ModeRequest is the POST /mode body. Action is one of
// set|drain|abort-drain|get_recommendation; Mode (interactive|batch|
// drain) and MaxConcurrentJobs apply to action=set only. There is no
// "get" action — mode queries go through GET /status. Project (#113)
// scopes get_recommendation to one project; absent, the daemon merges
// ALL mapped projects.
type ModeRequest struct {
	Action            string `json:"action"`
	Mode              string `json:"mode,omitempty"`
	MaxConcurrentJobs *int   `json:"max_concurrent_jobs,omitempty"`
	Project           string `json:"project,omitempty"`
}

// SubmitResponse is the claim-family answer.
type SubmitResponse struct {
	Accepted   bool   `json:"accepted"`
	WorkItemID string `json:"work_item_id"`
}

// StatusResponse is the GET /status answer: local bookkeeping + mode +
// queue counters. It is NOT workflow execution state (INV-1) — just
// "what this machine is managing".
type StatusResponse struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name,omitempty"`
	Mode        string `json:"mode"`
	// Workspaces is the mapped workspace set (#113): every [workspace]
	// path the daemon routes capsules into, in mapping declaration
	// order — a multi-workspace daemon never presents one workspace as
	// if it were the only one.
	Workspaces   []string    `json:"workspaces"`
	ShuttingDown bool        `json:"shutting_down"`
	Jobs         []StatusJob `json:"jobs"`
	Queue        QueueStats  `json:"queue"`
}

// StatusJob is one jobs[] bookkeeping entry.
type StatusJob struct {
	WorkItemID string `json:"work_item_id"`
	Status     string `json:"status"`
	// SpawnN is the v1.5 B2 derivation counter; SessionFingerprint is
	// the PUBLIC session handle (B3) — the session model's identity
	// pair, replacing the deleted attempt-id lineage handle.
	SpawnN             int    `json:"spawn_n,omitempty"`
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
	AgentPID           int    `json:"agent_pid,omitempty"`
	// NeedsInput/QuestionID are the D6 owner face: a job presented as
	// needs_input carries its pending question's id.
	NeedsInput bool `json:"needs_input,omitempty"`
	// RepairReason accompanies Status "repair_pending" (v2 §6):
	// capsule_missing | project_unmapped.
	RepairReason string `json:"repair_reason,omitempty"`
	QuestionID   string `json:"question_id,omitempty"`
	// LastProgressAgoSec is the F15c owner face: whole seconds since the
	// job's stream last proved agent progress (nil = none yet — rendered
	// "-" in the LAST-PROGRESS column).
	LastProgressAgoSec *int64 `json:"last_progress_ago_sec,omitempty"`
	// Origin is how the job was submitted (absent on legacy records).
	Origin *JobOrigin `json:"origin,omitempty"`
}

// JobOrigin mirrors the daemon-side origin record.
type JobOrigin struct {
	Kind   string `json:"kind"`
	ChatID string `json:"chat_id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// QueueStats is the local job queue counters.
type QueueStats struct {
	Pending       int `json:"pending"`
	Running       int `json:"running"`
	MaxConcurrent int `json:"max_concurrent"`
}

// ShipRepoOutcome is one repo's ship result.
type ShipRepoOutcome struct {
	Name       string `json:"name"`
	OriginURL  string `json:"origin_url"`
	Head       string `json:"head"`
	Branch     string `json:"branch"`
	Pushed     bool   `json:"pushed"`
	Reused     bool   `json:"reused"`
	PRURL      string `json:"pr_url,omitempty"`
	CompareURL string `json:"compare_url,omitempty"`
}

// ShipResponse is the ship answer: the fixed-name branch plus one
// outcome line per inventory repo of the sealed capsule.
type ShipResponse struct {
	WorkItemID string            `json:"work_item_id"`
	Branch     string            `json:"branch"`
	Initiator  string            `json:"initiator,omitempty"`
	Repos      []ShipRepoOutcome `json:"repos"`
}

// ---------------------------------------------------------------------------
// Routes.
// ---------------------------------------------------------------------------

// Claim submits a single-WI interactive claim request. priority defaults
// to "normal" server-side; empty keeps the default.
func (d *DaemonClient) Claim(wiID, priority string) (json.RawMessage, error) {
	return d.submit(wiID, "claim", priority)
}

// Resume submits a resume request — the daemon claims the paused WI
// into a new session (attach semantics belong to the daemon restart
// path, not the CLI).
func (d *DaemonClient) Resume(wiID, priority string) (json.RawMessage, error) {
	return d.submit(wiID, "resume", priority)
}

// ResumeBlocked submits the §6.4 owner move for a workflow parked blocked
// (polyforge#116): the daemon returns the WI to claimable at the hub (the
// owner's resume door) and re-establishes execution over the parked capsule
// synchronously — the claim/start path's owner move unblocks the workflow
// and the loop re-dispatches the frozen step in place as a fresh derivation.
func (d *DaemonClient) ResumeBlocked(wiID string) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/resume-blocked", "", d.withOrigin(map[string]any{}), nil)
}

// AbandonRepair asks the daemon to give up a repair_pending WI it holds:
// it ends the kept session with verdict ("canceled" | "failed") and drops
// the parked record. The caller has already passed the aihub human-key
// authorization probe (CloseAuth).
func (d *DaemonClient) AbandonRepair(wiID, verdict string) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/abandon-repair", "", map[string]any{"verdict": verdict}, nil)
}

func (d *DaemonClient) submit(wiID, intent, priority string) (json.RawMessage, error) {
	if priority == "" {
		priority = "normal"
	}
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/"+intent, "",
		d.withOrigin(map[string]any{"priority": priority}), nil)
}

// Observe pulls the observe projection (protocol.JobView). after is the
// last received event cursor (first call: 0).
func (d *DaemonClient) Observe(wiID string, after int64) (json.RawMessage, error) {
	return d.do("GET", "/jobs/"+url.PathEscape(wiID)+"/observe",
		"after="+strconv.FormatInt(after, 10), nil, nil)
}

// Answer relays an answer to a needs_input step. The relay message is
// the request body itself; the caller identity travels in the X-PF-User
// header and must match answered_by (00 §3.4a identity rule).
func (d *DaemonClient) Answer(wiID string, payload AnswerPayload) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/answer", "", payload,
		map[string]string{"X-PF-User": payload.AnsweredBy})
}

// Pause asks the daemon to pause a running WI. The v1.5 operator pause
// carries clean_pause=true in the body as well: the daemon's own hub
// transition can set the flag from it (wired at merge), independent of
// the CLI-side PATCH the pause verb also issues.
func (d *DaemonClient) Pause(wiID string) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/pause", "", map[string]any{"clean_pause": true}, nil)
}

// Wrap asks the daemon to wrap (or fail, when reason=="fail") a running
// WI — same final-flush requirement as pause.
func (d *DaemonClient) Wrap(wiID, reason string) (json.RawMessage, error) {
	if reason == "" {
		reason = "wrap"
	}
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/wrap", "",
		map[string]any{"reason": reason}, nil)
}

// Ship asks the daemon to push a closed WI's sealed capsule commits to
// each inventory repo's origin as the fixed-name branch polyforge/<id>.
func (d *DaemonClient) Ship(wiID string) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/ship", "", map[string]any{}, nil)
}

// Status returns the local jobs bookkeeping.
func (d *DaemonClient) Status() (json.RawMessage, error) {
	return d.do("GET", "/status", "", nil, nil)
}

// Mode issues one POST /mode operation.
func (d *DaemonClient) Mode(req ModeRequest) (json.RawMessage, error) {
	return d.do("POST", "/mode", "", req, nil)
}

// BatchSubmit submits the human-confirmed wave; the daemon never widens
// or narrows the set.
func (d *DaemonClient) BatchSubmit(wiIDs []string) (json.RawMessage, error) {
	return d.do("POST", "/batch_submit", "", d.withOrigin(map[string]any{"wi_ids": wiIDs}), nil)
}

// Shutdown triggers graceful shutdown (quiesce).
func (d *DaemonClient) Shutdown() (json.RawMessage, error) {
	return d.do("POST", "/shutdown", "", map[string]any{}, nil)
}

// ---------------------------------------------------------------------------
// pf watch lane routes.
// ---------------------------------------------------------------------------

// WatchSnapshot reads one WI's authoritative state plus the events after
// the cursor (first call: 0).
func (d *DaemonClient) WatchSnapshot(wiID string, after int64) (*protocol.WatchSnapshot, error) {
	raw, err := d.do("GET", "/jobs/"+url.PathEscape(wiID)+"/watch", "after="+strconv.FormatInt(after, 10), nil, nil)
	if err != nil {
		return nil, err
	}
	var s protocol.WatchSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode the watch snapshot: %w", err)
	}
	return &s, nil
}

func chatPath(chat, tail string) string { return "/chats/" + url.PathEscape(chat) + "/" + tail }

// ChatSubscriptions lists a chat's daemon-held subscriptions.
func (d *DaemonClient) ChatSubscriptions(chat string) (protocol.ChatSubscriptions, error) {
	var out protocol.ChatSubscriptions
	raw, err := d.do("GET", chatPath(chat, "subscriptions"), "", nil, nil)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

// ChatSubscribe subscribes (or unsubscribes) a chat to a WI.
func (d *DaemonClient) ChatSubscribe(chat, wi string, subscribe bool) (protocol.ChatSubscribeAnswer, error) {
	var out protocol.ChatSubscribeAnswer
	tail := "subscribe"
	if !subscribe {
		tail = "unsubscribe"
	}
	raw, err := d.do("POST", chatPath(chat, tail), "", map[string]any{"wi": wi}, nil)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

// ChatAck records a watcher's cursor/notified state for one subscription.
func (d *DaemonClient) ChatAck(chat string, a protocol.ChatAck) error {
	_, err := d.do("POST", chatPath(chat, "ack"), "", a, nil)
	return err
}

// ChatHeartbeat marks the chat's watcher live.
func (d *DaemonClient) ChatHeartbeat(chat string, pid int) error {
	_, err := d.do("POST", chatPath(chat, "heartbeat"), "", map[string]any{"pid": pid}, nil)
	return err
}

// ChatAttention lists WIs that need a human and have no live watcher
// (marking them shown to the chat).
func (d *DaemonClient) ChatAttention(chat, owner string) ([]protocol.ChatAttentionItem, error) {
	q := url.Values{"mark": {"1"}}
	if owner != "" {
		q.Set("owner", owner)
	}
	raw, err := d.do("GET", chatPath(chat, "attention"), q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var out protocol.ChatAttention
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
