// Package aihubclient is the shared HTTP client for the aihub server, used
// by runtime (claim/end-generation), observe (read-through) and the CLI
// verbs. Wire types come from internal/common/api; error decoding maps the
// typed errStatus envelope back into *api.Error.
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
		var ae api.Error
		if json.Unmarshal(data, &ae) == nil && ae.Code != "" {
			return &ae
		}
		// No typed envelope in the body (a proxy's HTML 502/504 page, a
		// bare 404): synthesize the code from the HTTP status itself.
		return &api.Error{
			Code:   fmt.Sprintf("HTTP-%d", resp.StatusCode),
			Detail: fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, data),
		}
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

// ClaimRequest asks aihub to open a generation on a work item.
type ClaimRequest struct {
	WorkItemID     string `json:"work_item_id"`
	Intent         string `json:"intent"` // claim|resume|takeover
	MachineID      string `json:"machine_id"`
	Owner          string `json:"owner"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ClaimResponse carries the attempt identity and the attempt-scoped git push
// credential (generation fencing, main layer).
type ClaimResponse struct {
	Claim      api.Claim          `json:"claim"`
	Credential api.PushCredential `json:"credential"`
	WorkItem   api.WorkItem       `json:"work_item"`
}

func (c *Client) Claim(req ClaimRequest) (*ClaimResponse, error) {
	var out ClaimResponse
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(req.WorkItemID)+"/claim", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EndGeneration closes the current generation with a terminal verdict.
type EndGenerationRequest struct {
	AttemptID  string `json:"attempt_id"`
	Generation int64  `json:"generation"`
	Status     string `json:"status"` // wrapped|failed|blocked
	Note       string `json:"note,omitempty"`
}

func (c *Client) EndGeneration(wi string, req EndGenerationRequest) error {
	return c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/end-generation", req, nil)
}

func (c *Client) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	var wi api.WorkItem
	if err := c.Do("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug), nil, &wi); err != nil {
		return nil, err
	}
	return &wi, nil
}

// GetProgress reads the §5.2 progress read-through projection.
func (c *Client) GetProgress(idOrSlug string) (*api.ProgressResponse, error) {
	var p api.ProgressResponse
	if err := c.Do("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug)+"/progress", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProgressRaw reads the progress projection undecoded (--json
// passthrough: the server body reaches the caller verbatim).
func (c *Client) GetProgressRaw(idOrSlug string) (json.RawMessage, error) {
	return c.raw("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug)+"/progress", nil)
}

// ListWorkItems filters by project and status (queue views).
func (c *Client) ListWorkItems(project string, status string) ([]api.WorkItem, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if status != "" {
		q.Set("status", status)
	}
	var resp struct {
		Items []api.WorkItem `json:"items"`
		Total int            `json:"total"`
	}
	if err := c.Do("GET", "/api/v1/work-items?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// Queue returns ready work items for autonomous claiming (drain mode).
// Both executable segments count as takeable: items (classified
// requires_human_session=false) and unclassified (classification missing —
// per the ready-queue contract the segment that means "takeable now by an
// agent"; a claim resolves the classification). needs_human_session is
// NEVER included — a human session is a claim precondition there.
func (c *Client) Queue(project string) ([]api.WorkItem, error) {
	var resp struct {
		Items   []api.WorkItem `json:"items"`
		Unknown []api.WorkItem `json:"unclassified"`
	}
	if err := c.Do("GET", "/api/v1/projects/"+url.PathEscape(project)+"/ready-queue", nil, &resp); err != nil {
		return nil, err
	}
	return append(resp.Items, resp.Unknown...), nil
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

// ListWorkItemsRaw is the raw-body variant of ListWorkItems (same wire
// filter keys).
func (c *Client) ListWorkItemsRaw(project, status string) (json.RawMessage, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if status != "" {
		q.Set("status", status)
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

// UserCreateRequest is the M1 create body. v1 field: user_type
// (human|machine), not v0's "kind".
type UserCreateRequest struct {
	DisplayName string `json:"display_name"`
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

// RotateSecretRequest is the M5 body (grace | rollback).
type RotateSecretRequest struct {
	Mode         string `json:"mode"`
	GraceSeconds int64  `json:"grace_seconds,omitempty"`
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

// RotateSecret rotates the service secret (M5).
func (c *Client) RotateSecret(req RotateSecretRequest) (json.RawMessage, error) {
	return c.raw("POST", "/api/v1/service/rotate-secret", req)
}

// ---------------------------------------------------------------------------
// Migration face (admin): the one-shot record import paths.
// ---------------------------------------------------------------------------

// ImportWorkItemRequest is the POST /api/v1/admin/import-work-item body;
// it mirrors core.ImportRequest (the target server owns the domain).
type ImportWorkItemRequest struct {
	Project        string          `json:"project"`
	ID             string          `json:"id"`
	Seq            *int64          `json:"seq"`
	Goal           string          `json:"goal"`
	Content        *string         `json:"content"`
	Status         string          `json:"status"`
	Priority       string          `json:"priority"`
	Milestone      *string         `json:"milestone"`
	Labels         []string        `json:"labels"`
	Attrs          map[string]any  `json:"attrs"`
	Reporter       string          `json:"reporter"`
	Assignee       *string         `json:"assignee"`
	WaitingOn      *string         `json:"waiting_on"`
	BundleName     *string         `json:"bundle_name"`
	BundleDigest   *string         `json:"bundle_digest"`
	BudgetBaseline json.RawMessage `json:"workflow_budget_baseline"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	ClosedAt       *string         `json:"closed_at"`
	// Overwrite: a same-id record is rewritten in place (new version
	// wins) and the answer carries outcome=overwritten.
	Overwrite bool `json:"overwrite"`
}

// ImportResult is the import answer.
type ImportResult struct {
	Outcome  string        `json:"outcome"` // imported | skipped-existing | overwritten
	WorkItem *api.WorkItem `json:"work_item"`
}

// ImportWorkItem imports one migration record (admin face).
func (c *Client) ImportWorkItem(req ImportWorkItemRequest) (*ImportResult, error) {
	var out ImportResult
	if err := c.Do("POST", "/api/v1/admin/import-work-item", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RebuildMemoryIndex triggers the explicit reconcile/backfill (admin).
func (c *Client) RebuildMemoryIndex() (int, error) {
	var out struct {
		Reindexed int `json:"reindexed"`
	}
	if err := c.Do("POST", "/api/v1/memories/rebuild-index", map[string]any{}, &out); err != nil {
		return 0, err
	}
	return out.Reindexed, nil
}

// ---------------------------------------------------------------------------
// v1.5 session model client (v1.5-api-contract.md §3). Additive: the
// generation-era Claim/EndGeneration above remain until the cleanup commit.

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

// ForceTakeover atomically swaps the session to this daemon's new secret.
func (c *Client) ForceTakeover(wi string, req api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	var out api.SessionClaimResponse
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/force-takeover", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

// SessionEnd replaces EndGeneration: session-fenced terminal verdict.
func (c *Client) SessionEnd(wi string, req api.SessionEndRequest) error {
	return c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/session-end", req, nil)
}
