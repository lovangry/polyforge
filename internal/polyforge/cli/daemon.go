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
// unix socket (/run/polyforge.sock by default). The authoritative route
// list is the mount list in internal/polyforge/daemon/routes.go (mirrored
// in README "Daemon UDS routes"); the core of it:
//
//	POST /jobs/{id}/claim                   body {"priority":"normal"}
//	POST /jobs/{id}/resume-blocked|resume-paused
//	POST /jobs/{id}/answer                  body = AnswerPayload
//	                                        {question_id, answer_text, answered_by}
//	POST /jobs/{id}/pause  {}               POST /jobs/{id}/wrap {"reason"}
//	GET  /status
//	POST /ops/config {"auto":bool,"max":N}
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

// AnswerPayload is the POST /jobs/{id}/answer body. An empty QuestionID
// lets the daemon resolve the work item's pending question.
type AnswerPayload struct {
	QuestionID string `json:"question_id"`
	AnswerText string `json:"answer_text"`
	AnsweredBy string `json:"answered_by"`
}

// OpsConfigRequest is the POST /ops/config body: the auto switch and/or
// the max-concurrent-jobs number (at least one).
type OpsConfigRequest struct {
	Auto *bool `json:"auto,omitempty"`
	Max  *int  `json:"max,omitempty"`
	// MaxReset drops an explicit `pf ops max` cap: the configured one
	// (cli.toml [daemon] max_concurrent_jobs, else 4) applies again.
	MaxReset bool `json:"max_reset,omitempty"`
}

// SubmitResponse is the claim-family answer.
type SubmitResponse struct {
	Accepted   bool   `json:"accepted"`
	WorkItemID string `json:"work_item_id"`
}

// StatusResponse is the GET /status answer: local bookkeeping + auto switch +
// queue counters. It is NOT workflow execution state (INV-1) — just
// "what this machine is managing".
type StatusResponse struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name,omitempty"`
	Auto        bool   `json:"auto"`
	// Workspaces is the mapped workspace set (#113): every [workspace]
	// path the daemon routes capsules into, in mapping declaration
	// order — a multi-workspace daemon never presents one workspace as
	// if it were the only one.
	Workspaces []string `json:"workspaces"`
	// WorkflowsDir is the absolute workflows dir the daemon runs bundles
	// from (pf doctor judges the workspace yaml's step-id keys by it).
	WorkflowsDir string `json:"workflows_dir,omitempty"`
	// EnvWarnings: retired POLYFORGE_* variables the daemon found set to
	// a differing value at boot (decision 2026-10-16 G2).
	EnvWarnings []string `json:"env_warnings,omitempty"`
	// Harnesses is the daemon's view of the step harness binaries, only
	// on GET /status?harnesses=1 (polyforge#159).
	Harnesses    map[string]HarnessStatus `json:"harnesses,omitempty"`
	ShuttingDown bool                     `json:"shutting_down"`
	Jobs         []StatusJob              `json:"jobs"`
	Queue        QueueStats               `json:"queue"`
}

// HarnessStatus is one harness's daemon-side status: binary, version,
// and whether the credential variables are set ("set"/"unset" — never a
// value). polyforge#171 adds the claude entry's harness-root layout
// (the three dirs) and the claude-config settings.json env block facts
// (key → set/unset, never values) plus its pairing warnings.
type HarnessStatus struct {
	Bin             string                `json:"bin"`
	Found           bool                  `json:"found"`
	Path            string                `json:"path,omitempty"`
	Version         string                `json:"version,omitempty"`
	Verified        bool                  `json:"verified,omitempty"`
	MinVersion      string                `json:"min_version,omitempty"`
	VersionError    string                `json:"version_error,omitempty"`
	AuthTokenEnv    string                `json:"auth_token_env,omitempty"`
	APIKeyEnv       string                `json:"api_key_env,omitempty"`
	BaseURLEnv      string                `json:"base_url_env,omitempty"`
	SettingsEnv     map[string]string     `json:"settings_env,omitempty"`
	HarnessDirs     map[string]HarnessDir `json:"harness_dirs,omitempty"`
	SettingsWarning string                `json:"settings_warning,omitempty"`
	BackgroundTasks string                `json:"background_tasks,omitempty"`
}

// HarnessDir is one harness dir's existence fact under the harness root
// (polyforge#171: step_agent / step_agent_sessions / claude_config).
type HarnessDir struct {
	Path   string `json:"path,omitempty"`
	Exists bool   `json:"exists"`
}

// StatusJob is one jobs[] bookkeeping entry.
type StatusJob struct {
	WorkItemID string `json:"work_item_id"`
	// Slug is the "<project>#<seq>" display name (absent on a job record
	// that predates it: the WORK ITEM column then shows the id).
	Slug   string `json:"slug,omitempty"`
	Status string `json:"status"`
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
	// StepAgeSec / LastWorkAgoSec are the owner liveness face: whole
	// seconds since the current dispatch began and since its harness last
	// did real work (nil = none — rendered "-" in the STEP-AGE and
	// LAST-WORK columns).
	StepAgeSec     *int64 `json:"step_age_sec,omitempty"`
	LastWorkAgoSec *int64 `json:"last_work_ago_sec,omitempty"`
	// Candidate is the in-flight dispatch's `<harness>:<model>[:<effort>]`
	// (polyforge#176; absent = none in flight or the harness default).
	Candidate string `json:"candidate,omitempty"`
	// Origin is how the job was submitted (may be absent).
	Origin *JobOrigin `json:"origin,omitempty"`
	// AwaitingHub: a RECOVERING job whose restart-time session verify is
	// waiting for aihub (rendered "recovering (awaiting aihub)").
	AwaitingHub bool `json:"awaiting_hub,omitempty"`
}

// JobOrigin mirrors the daemon-side origin record.
type JobOrigin struct {
	Kind   string `json:"kind"`
	ChatID string `json:"chat_id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// QueueStats is the local job queue counters.
type QueueStats struct {
	Pending       int    `json:"pending"`
	Running       int    `json:"running"`
	MaxConcurrent int    `json:"max_concurrent"`
	MaxSource     string `json:"max_source,omitempty"` // "ops" | "config"
}

// ---------------------------------------------------------------------------
// Routes.
// ---------------------------------------------------------------------------

// ClaimWithModels submits a single-WI interactive claim request carrying
// the manual `--models` candidate list (nil keeps the default). priority
// defaults to "normal" server-side; empty keeps the default.
func (d *DaemonClient) ClaimWithModels(wiID, priority string, models []string) (json.RawMessage, error) {
	return d.submit(wiID, "claim", priority, models)
}

// ResumeBlocked submits the §6.4 owner move for a workflow parked blocked
// (polyforge#116): the daemon returns the WI to claimable at the hub (the
// owner's resume door) and re-establishes execution over the parked capsule
// synchronously — the claim/start path's owner move unblocks the workflow
// and the loop re-dispatches the frozen step in place as a fresh derivation.
func (d *DaemonClient) ResumeBlocked(wiID string) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/resume-blocked", "", d.withOrigin(map[string]any{}), nil)
}

// ResumePaused is the live-holder half of `pf resume` on a paused work
// item (wi_seWTgHeKtaa8, v2-converge §7): the CLI has already cleared
// clean_pause at the hub; this asks the LOCAL daemon — when it is the
// paused work item's holder — to re-validate the session and continue
// the current step IN PLACE (SessionAttach + the recovery continuation,
// the same semantics attaching after a restart has). A daemon that does
// not hold the work item answers DAEMON-404; a live holder whose hub
// flag was not cleared yet answers DAEMON-409.
type ResumePausedResponse struct {
	Resumed    bool   `json:"resumed"`
	WorkItemID string `json:"work_item_id"`
	// Outcome: attached (continued in place) | already_live | closed
	// (re-attached, but the recovery closed the job).
	Outcome string `json:"outcome,omitempty"`
}

func (d *DaemonClient) ResumePaused(wiID string) (*ResumePausedResponse, error) {
	raw, err := d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/resume-paused", "", d.withOrigin(map[string]any{}), nil)
	if err != nil {
		return nil, err
	}
	var out ResumePausedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode the resume-paused answer: %w", err)
	}
	return &out, nil
}

func (d *DaemonClient) submit(wiID, intent, priority string, models []string) (json.RawMessage, error) {
	if priority == "" {
		priority = "normal"
	}
	body := map[string]any{"priority": priority}
	if len(models) > 0 {
		body["models"] = models
	}
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/"+intent, "",
		d.withOrigin(body), nil)
}

// Answer relays an answer to a needs_input step. The relay message is
// the request body itself; the caller identity travels in the X-PF-User
// header (the daemon only uses it to fill an empty answered_by; it does
// not cross-check the two).
func (d *DaemonClient) Answer(wiID string, payload AnswerPayload) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/answer", "", payload,
		map[string]string{"X-PF-User": payload.AnsweredBy})
}

// Pause asks the daemon to pause a running WI (no body: the daemon's close
// path carries the clean_pause semantics itself).
func (d *DaemonClient) Pause(wiID string) (json.RawMessage, error) {
	return d.do("POST", "/jobs/"+url.PathEscape(wiID)+"/pause", "", nil, nil)
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

// Status returns the local jobs bookkeeping.
func (d *DaemonClient) Status() (json.RawMessage, error) {
	return d.do("GET", "/status", "", nil, nil)
}

// StatusWithHarnesses is GET /status?harnesses=1: the status plus the
// daemon's view of the step harness binaries (polyforge#159; pf doctor).
func (d *DaemonClient) StatusWithHarnesses() (json.RawMessage, error) {
	return d.do("GET", "/status", "harnesses=1", nil, nil)
}

// OpsConfig issues one POST /ops/config operation.
func (d *DaemonClient) OpsConfig(req OpsConfigRequest) (json.RawMessage, error) {
	return d.do("POST", "/ops/config", "", req, nil)
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
