// Package aihubclient is the shared HTTP client for the aihub server, used
// by runtime (session claim/attach/force-end/end), observe (read-through)
// and the CLI verbs. Wire types come from internal/common/api; error
// decoding maps the typed errStatus envelope back into *api.Error.
package aihubclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// Client talks to one aihub server.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Do performs a typed request. body may be nil. out receives the decoded
// JSON response when non-nil. Non-2xx answers decode as *api.Error.
func (c *Client) Do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return typedError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// raw performs one CLI-facing passthrough call: out is the undecoded
// response body so --json mode hands the server payload through verbatim.
func (c *Client) raw(method, path string, body any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do(method, path, body, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// typedError maps a non-2xx response body back into *api.Error: the
// typed envelope when one is present, a status-synthesized code
// otherwise (a proxy's HTML 502/504 page, a bare 404). Shared by Do and
// the text/markdown read so every error carries the same envelope.
func typedError(status int, data []byte) error {
	var ae api.Error
	if json.Unmarshal(data, &ae) == nil && ae.Code != "" {
		return &ae
	}
	return &api.Error{
		Code:   fmt.Sprintf("HTTP-%d", status),
		Detail: fmt.Sprintf("HTTP %d: %.200s", status, data),
	}
}

func (c *Client) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	var wi api.WorkItem
	if err := c.Do("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug), nil, &wi); err != nil {
		return nil, err
	}
	return &wi, nil
}

// Queue returns the ready queue's executable items for autonomous
// claiming (the daemon auto-pull).
func (c *Client) Queue(project string) ([]api.WorkItem, error) {
	var resp struct {
		Items []api.WorkItem `json:"items"`
	}
	if err := c.Do("GET", "/api/v1/projects/"+url.PathEscape(project)+"/ready-queue", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ---------------------------------------------------------------------------
// CLI-facing endpoints (v1 CLI lane additions).
//
// These return the RAW response body (json.RawMessage) so the CLI's
// --json mode can pass the server payload through verbatim; the human
// renderers decode their own view structs from the same bytes. Endpoint
// surface is the v0 openapi face minus the deleted SYNC-*/revision-chain
// rows (git IS the revision chain in v1).
//
// Only additive methods live here: no existing signature above changes.
// ---------------------------------------------------------------------------

// CreateWorkItemRequest is the work-item create body. v1 trim vs v0: the
// repo-inventory declarations are gone (the capsule IS a git worktree,
// v1-architecture-spec key decision 1).
type CreateWorkItemRequest struct {
	Project    string         `json:"project"`
	Goal       string         `json:"goal"`
	Content    *string        `json:"content,omitempty"`
	ParentID   *string        `json:"parent_id,omitempty"`
	Priority   string         `json:"priority,omitempty"`
	Milestone  *string        `json:"milestone,omitempty"`
	Labels     []string       `json:"labels,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
	BundleName *string        `json:"bundle_name,omitempty"`
}

// CreateWorkItem creates a work item and returns the raw detail record.
func (c *Client) CreateWorkItem(req CreateWorkItemRequest) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/work-items", req)
}

// GetWorkItemRaw reads the detail record undecoded (--json passthrough).
func (c *Client) GetWorkItemRaw(idOrSlug string) (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug), nil)
}

// PatchWorkItem updates the PATCHable field set. body must contain only
// keys from {goal, content, labels, priority, milestone, waiting_on,
// attrs} — an immutable key answers CORE-400 server-side.
func (c *Client) PatchWorkItem(id string, body map[string]any) (json.RawMessage, error) {
	return c.raw("PATCH", "/api/v1/work-items/"+url.PathEscape(id), body)
}

// CancelWorkItem terminates a queued/paused/blocked WI (no active
// generation).
func (c *Client) CancelWorkItem(id string) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/work-items/"+url.PathEscape(id)+"/cancel", map[string]any{})
}

// ListWorkItemsRaw lists work items undecoded (0 limit = the server
// default cap).
func (c *Client) ListWorkItemsRaw(project, status string, limit int) (json.RawMessage, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.raw("GET", "/api/v1/work-items?"+q.Encode(), nil)
}

// ReadyQueueRaw is the raw-body variant of Queue (same endpoint).
func (c *Client) ReadyQueueRaw(project string) (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/projects/"+url.PathEscape(project)+"/ready-queue", nil)
}

// SearchMemories runs the memory retrieval (06 §5.1). q is required;
// memType, tags and limit are optional filters.
func (c *Client) SearchMemories(project, q, memType string, tags []string, limit int) (json.RawMessage, error) {
	v := url.Values{}
	v.Set("project", project)
	v.Set("q", q)
	if memType != "" {
		v.Set("type", memType)
	}
	if len(tags) > 0 {
		v.Set("tags", strings.Join(tags, ","))
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	return c.raw("GET", "/api/v1/memories/search?"+v.Encode(), nil)
}

// GetMemory reads one memory's metadata (GET /api/v1/memories/{id}, the
// v1 per-id memory face): the default JSON response is the memory.Entry
// metadata and carries NO content — the body only comes back from
// GetMemoryContent (?format=content). Raw so the CLI's --json mode can
// hand the server metadata through verbatim.
func (c *Client) GetMemory(id string) (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/memories/"+url.PathEscape(id), nil)
}

// GetMemoryContent reads one memory's body (GET
// /api/v1/memories/{id}?format=content): the server answers the bare
// body (no front-matter) as text/markdown, NOT JSON, so the body bytes are returned undecoded — a
// JSON decode would refuse plain markdown. Auth and the typed-error
// envelope are exactly Do's (the shared non-2xx path), and the response
// is bounded to the same 32 MiB cap.
func (c *Client) GetMemoryContent(id string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/memories/"+url.PathEscape(id)+"?format=content", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, typedError(resp.StatusCode, data)
	}
	return data, nil
}

// UserCreateRequest is the M1 create body. v1 field: user_type
// (human|machine), not v0's "kind".
type UserCreateRequest struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"` // required for human users (CORE-400 otherwise)
	UserType    string `json:"user_type,omitempty"`
	Role        string `json:"role,omitempty"` // writer|admin
}

// UserPatchRequest is the M1 PATCH body.
type UserPatchRequest struct {
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
}

// ApiKeyCreateRequest is the M2 mint body (plaintext returned exactly
// once by the server).
type ApiKeyCreateRequest struct {
	UserID    string  `json:"user_id"`
	ProjectID *string `json:"project_id,omitempty"`
}

// ProjectCreateRequest is the M3 create body.
type ProjectCreateRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Repos       []api.ProjectRepo `json:"repos,omitempty"` // task-repo inventory (served back on every claim)
	Visible     *bool             `json:"visible,omitempty"`
}

// ProjectMember is one membership row (role: viewer|writer|maintainer).
type ProjectMember struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// ProjectPatchRequest is the M3 PATCH body: member upserts plus the
// physical remove list, mirroring the v0 wire contract.
type ProjectPatchRequest struct {
	Description   *string          `json:"description,omitempty"`
	Members       *[]ProjectMember `json:"members,omitempty"`
	RemoveMembers *[]string        `json:"remove_members,omitempty"`
	Visible       *bool            `json:"visible,omitempty"`
	Repos         *[]RepoRef       `json:"repos,omitempty"`
}

// RepoRef is one task-repo inventory entry (name + clone URL).
type RepoRef struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// CreateUser issues a user (M1).
func (c *Client) CreateUser(req UserCreateRequest) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/users", req)
}

// ListUsers lists users (M1).
func (c *Client) ListUsers() (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/users", nil)
}

// GetUser reads one user (M1).
func (c *Client) GetUser(id string) (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/users/"+url.PathEscape(id), nil)
}

// PatchUser updates display_name and/or role (M1).
func (c *Client) PatchUser(id string, req UserPatchRequest) (json.RawMessage, error) {
	return c.raw("PATCH", "/api/v1/users/"+url.PathEscape(id), req)
}

// CreateApiKey mints a key (M2).
func (c *Client) CreateApiKey(req ApiKeyCreateRequest) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/api-keys", req)
}

// ListApiKeys lists key metadata, never material (M2).
func (c *Client) ListApiKeys() (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/api-keys", nil)
}

// RevokeApiKey sets revoked_at (M2).
func (c *Client) RevokeApiKey(id string) (json.RawMessage, error) {
	return c.raw("DELETE", "/api/v1/api-keys/"+url.PathEscape(id), nil)
}

// CreateProject creates a project (M3).
func (c *Client) CreateProject(req ProjectCreateRequest) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/projects", req)
}

// ListProjects lists visible projects (M3).
func (c *Client) ListProjects() (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/projects", nil)
}

// GetProject reads one project by id or name (M3).
func (c *Client) GetProject(idOrName string) (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/projects/"+url.PathEscape(idOrName), nil)
}

// PatchProject updates a project and/or its membership rows (M3).
func (c *Client) PatchProject(idOrName string, req ProjectPatchRequest) (json.RawMessage, error) {
	return c.raw("PATCH", "/api/v1/projects/"+url.PathEscape(idOrName), req)
}

// Backup triggers the on-demand snapshot (M4).
func (c *Client) Backup() (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/admin/backup", map[string]any{})
}

// ---------------------------------------------------------------------------
// Machines faces (M3 heartbeat wiring).

// MachineHeartbeat is the POST /api/v1/machines/heartbeat body: machine
// identity plus the daemon's live projection. No client timestamp — the
// server stamps last_heartbeat_at itself (single clock, no skew).
// live_wi_ids carries NO omitempty: every heartbeat restates the full
// projection (an idle daemon sends []), because the server treats an
// absent field as keep-old and a held-back empty list would leave ghost
// WI ids in the machines read face forever.
type MachineHeartbeat struct {
	MachineID      string   `json:"machine_id"`
	RuntimeVersion string   `json:"runtime_version,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	LiveWIIDs      []string `json:"live_wi_ids"`
}

// MachineHeartbeatResult is the heartbeat response. On first registration
// the server also issues the machine identity's api key (plaintext shown
// once, JSON field api_key) — this struct deliberately does not carry it:
// the daemon heartbeats with its OPERATOR-issued credential, so a key it
// never asked for must not flow through the client surface.
type MachineHeartbeatResult struct {
	Registered bool   `json:"registered"` // true only on the first call
	MachineID  string `json:"machine_id"`
	UserID     string `json:"user_id"`
}

// Heartbeat posts one machine heartbeat (upsert: first call registers).
func (c *Client) Heartbeat(req MachineHeartbeat) (*MachineHeartbeatResult, error) {
	var out MachineHeartbeatResult
	if err := c.Do("POST", "/api/v1/machines/heartbeat", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MachineStatus is one GET /api/v1/machines row (admin read face): the
// stored liveness projection plus the server-computed stale boolean.
type MachineStatus struct {
	ID                  string   `json:"id"`
	UserID              string   `json:"user_id"`
	RuntimeVersion      string   `json:"runtime_version,omitempty"`
	Mode                string   `json:"mode,omitempty"`
	LiveWIIDs           []string `json:"live_wi_ids"`
	LastHeartbeatAt     string   `json:"last_heartbeat_at"`
	HeartbeatAgeSeconds *int64   `json:"heartbeat_age_seconds,omitempty"`
	Stale               bool     `json:"stale"`
}

// MachinesList is the GET /api/v1/machines response.
type MachinesList struct {
	Machines          []MachineStatus `json:"machines"`
	StaleAfterSeconds int64           `json:"stale_after_seconds"`
	Count             int             `json:"count"`
}

// ListMachinesRaw reads the machines liveness face undecoded (--json
// passthrough: the server body byte-for-byte).
func (c *Client) ListMachinesRaw() (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/machines", nil)
}

// ---------------------------------------------------------------------------
// v1.5 session model client (v1.5-api-contract.md §3).

// SessionClaim performs the CAS claim: set the session only if the WI is
// queued and unheld (M1 joint gate). Idempotent on idempotency_key.
func (c *Client) SessionClaim(req api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	var out api.SessionClaimResponse
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(req.WorkItemID)+"/claim", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SessionAttach resumes a paused WI under the same session (M3).
func (c *Client) SessionAttach(wi string, req api.SessionAttachRequest) (*api.SessionClaimResponse, error) {
	var out api.SessionClaimResponse
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/session-attach", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ForceEndSession is the admin/maintainer recovery door: it ends the held
// session (WI running|paused -> queued, old secret fenced); any daemon then
// claims the WI normally. Authorization is server-side (admin or project
// maintainer).
func (c *Client) ForceEndSession(wi, reason string) error {
	return c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/force-end-session",
		api.ForceEndSessionRequest{Reason: reason}, nil)
}

// SessionVerify is the compare-only holder check (B3: no secret echo).
func (c *Client) SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error) {
	var out api.SessionVerifyResponse
	req := api.SessionVerifyRequest{SessionID: sessionID}
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/session-verify", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SessionEnd is the session-fenced terminal verdict (and the paused
// transition — the merge amend).
func (c *Client) SessionEnd(wi string, req api.SessionEndRequest) error {
	return c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/session-end", req, nil)
}

// ResumeBlockedWorkItem is the §6.4 owner resume verdict's hub door
// (polyforge#116): `pf resume` on a workflow parked blocked — status
// blocked → queued for the owner's immediate re-claim; the daemon then
// re-establishes execution over the parked capsule. Authorization is
// server-side (the claim gate: any project member).
func (c *Client) ResumeBlockedWorkItem(idOrSlug string) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/work-items/"+url.PathEscape(idOrSlug)+"/resume-blocked", map[string]any{})
}

// TerminateBlockedWorkItem is the §6.4 owner terminate verdict's hub
// door (polyforge#116): `pf wrap --fail` on a parked workflow — status
// blocked → failed. Authorization is server-side (admin or project
// maintainer, the unblock gate).
func (c *Client) TerminateBlockedWorkItem(idOrSlug string) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/work-items/"+url.PathEscape(idOrSlug)+"/terminate-blocked", map[string]any{})
}
