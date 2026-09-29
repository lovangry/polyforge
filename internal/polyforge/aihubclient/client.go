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
		return &api.Error{Code: "AUTH-401", Detail: fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
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
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/work-items", req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetWorkItemRaw reads the detail record undecoded (--json passthrough).
func (c *Client) GetWorkItemRaw(idOrSlug string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug), nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// PatchWorkItem updates the PATCHable field set. body must contain only
// keys from {goal, content, labels, priority, milestone, waiting_on,
// attrs} — an immutable key answers CORE-400 server-side.
func (c *Client) PatchWorkItem(id string, body map[string]any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("PATCH", "/api/v1/work-items/"+url.PathEscape(id), body, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// CancelWorkItem terminates a queued/paused/blocked WI (no active
// generation).
func (c *Client) CancelWorkItem(id string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(id)+"/cancel", map[string]any{}, &raw); err != nil {
		return nil, err
	}
	return raw, nil
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
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/work-items?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ReadyQueueRaw is the raw-body variant of Queue (same endpoint).
func (c *Client) ReadyQueueRaw(project string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/projects/"+url.PathEscape(project)+"/ready-queue", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
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
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/memories/search?"+v.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
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
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Visible     *bool  `json:"visible,omitempty"`
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
}

// RotateSecretRequest is the M5 body (grace | rollback).
type RotateSecretRequest struct {
	Mode         string `json:"mode"`
	GraceSeconds int64  `json:"grace_seconds,omitempty"`
}

// CreateUser issues a user (M1).
func (c *Client) CreateUser(req UserCreateRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/users", req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ListUsers lists users (M1).
func (c *Client) ListUsers() (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/users", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetUser reads one user (M1).
func (c *Client) GetUser(id string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/users/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// PatchUser updates display_name and/or role (M1).
func (c *Client) PatchUser(id string, req UserPatchRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("PATCH", "/api/v1/users/"+url.PathEscape(id), req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// CreateApiKey mints a key (M2).
func (c *Client) CreateApiKey(req ApiKeyCreateRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/api-keys", req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ListApiKeys lists key metadata, never material (M2).
func (c *Client) ListApiKeys() (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/api-keys", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// RevokeApiKey sets revoked_at (M2).
func (c *Client) RevokeApiKey(id string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("DELETE", "/api/v1/api-keys/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// CreateProject creates a project (M3).
func (c *Client) CreateProject(req ProjectCreateRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/projects", req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ListProjects lists visible projects (M3).
func (c *Client) ListProjects() (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/projects", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetProject reads one project by id or name (M3).
func (c *Client) GetProject(idOrName string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("GET", "/api/v1/projects/"+url.PathEscape(idOrName), nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// PatchProject updates a project and/or its membership rows (M3).
func (c *Client) PatchProject(idOrName string, req ProjectPatchRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("PATCH", "/api/v1/projects/"+url.PathEscape(idOrName), req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Backup triggers the on-demand snapshot (M4).
func (c *Client) Backup() (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/admin/backup", map[string]any{}, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// RotateSecret rotates the service secret (M5).
func (c *Client) RotateSecret(req RotateSecretRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Do("POST", "/api/v1/service/rotate-secret", req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
