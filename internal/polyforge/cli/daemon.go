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
// v0 daemon surface minus publish_request — flush replaces it (ruling
// C2, docs/design/polyforge-vnext/04-local-runtime.md §5.1):
//
//	POST /jobs/{id}/claim|resume|takeover   body {"priority":"normal"}
//	POST /jobs/{id}/adopt-session           body = AdoptSessionRequest (#109:
//	                                        custody of an externally minted
//	                                        session secret — local UDS only)
//	GET  /jobs/{id}/observe?after=<cursor>  answer = protocol.JobView
//	POST /jobs/{id}/answer                  body = the five-field relay
//	                                        message itself (00 §3.4a)
//	POST /jobs/{id}/flush                   body = protocol.FlushRequest
//	POST /jobs/{id}/pause  {}               POST /jobs/{id}/wrap {"reason"}
//	POST /jobs/{id}/ship  {}                 GET  /status
//	POST /mode {"action":...}               POST /batch_submit {"wi_ids":[]}
//	POST /shutdown
//
// Error envelope: the daemon answers non-2xx with the SAME typed
// envelope the aihub face uses (common/api.Error: {code, detail} at the
// top level, DAEMON-* codes per 04 §5.2). The phase-3 daemon assembly
// must serve exactly this shape.
type DaemonClient struct {
	Sock string
	HTTP *http.Client
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
// "get" action — mode queries go through GET /status.
type ModeRequest struct {
	Action            string `json:"action"`
	Mode              string `json:"mode,omitempty"`
	MaxConcurrentJobs *int   `json:"max_concurrent_jobs,omitempty"`
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
	MachineID string `json:"machine_id"`
	Mode      string `json:"mode"`
	// Workspace is the active capsule base (#103): the daemon's launched
	// -workspace value — capsules live at <workspace>/capsules/<wi_id>.
	Workspace    string      `json:"workspace"`
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
	NeedsInput bool   `json:"needs_input,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	// LastProgressAgoSec is the F15c owner face: whole seconds since the
	// job's stream last proved agent progress (nil = none yet — rendered
	// "-" in the LAST-PROGRESS column).
	LastProgressAgoSec *int64 `json:"last_progress_ago_sec,omitempty"`
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

// Takeover submits an admin force-takeover request.
func (d *DaemonClient) Takeover(wiID, priority string) (json.RawMessage, error) {
	return d.submit(wiID, "takeover", priority)
}

// AdoptSessionRequest is the POST /jobs/{id}/adopt-session body. The
// session SECRET travels only over this local UDS route (never the hub
// wire): it is the custody handoff that keeps a CLI-side takeover's
// session usable after the CLI process exits (#109). Owner/Reason ride
// for the daemon's journal trace only.
type AdoptSessionRequest struct {
	SessionID string `json:"session_id"`
	Owner     string `json:"owner,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AdoptSession hands one externally minted session secret to the
// daemon's durable custody (#109). The daemon verify-backs the handoff
// against the hub (only the live holder's secret is adopted) and
// journals it; the answer carries the PUBLIC fingerprint only.
func (d *DaemonClient) AdoptSession(wiID string, req AdoptSessionRequest) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/adopt-session", "", req, nil)
}

func (d *DaemonClient) submit(wiID, intent, priority string) (json.RawMessage, error) {
	if priority == "" {
		priority = "normal"
	}
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/"+intent, "",
		map[string]any{"priority": priority}, nil)
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

// Flush is the agent-side UDS proxy call (ruling C2/C3: flush replaces
// publish_request). The CLI itself never drives it; the method exists so
// the client surface covers the full daemon route family.
func (d *DaemonClient) Flush(wiID string, req protocol.FlushRequest) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/flush", "", req, nil)
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
	return d.do("POST", "/batch_submit", "", map[string]any{"wi_ids": wiIDs}, nil)
}

// Shutdown triggers graceful shutdown (quiesce).
func (d *DaemonClient) Shutdown() (json.RawMessage, error) {
	return d.do("POST", "/shutdown", "", map[string]any{}, nil)
}
