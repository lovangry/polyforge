package cli

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client is the typed aihub HTTP client. The wire contract is
// api/openapi.yaml (single port, Bearer api_key auth, typed error
// envelope). Every method returns the RAW response body so --json mode
// can emit the server's JSON as-is; human renderers unmarshal separately.
type Client struct {
	Base   string // aihub base URL, e.g. http://localhost:8420
	APIKey string // aihub api_key (Bearer)
	HTTP   *http.Client
}

// NewClient builds a client with a default transport.
func NewClient(base, apiKey string) *Client {
	return &Client{Base: base, APIKey: apiKey, HTTP: &http.Client{}}
}

// do issues one request against the aihub face.
func (c *Client) do(method, path string, query url.Values, body any) ([]byte, error) {
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return execute(c.HTTP, method, u, body, map[string]string{
		"Authorization": "Bearer " + c.APIKey,
	})
}

// ---------------------------------------------------------------------------
// Request/response view structs (mirrors api/openapi.yaml schemas; only
// the fields the CLI reads are decoded — everything else stays in the raw
// body that --json mode passes through).
// ---------------------------------------------------------------------------

// CreateWorkItemRequest mirrors the row-1 create body.
type CreateWorkItemRequest struct {
	Project           string         `json:"project"`
	Goal              string         `json:"goal"`
	Content           *string        `json:"content,omitempty"`
	ParentID          *string        `json:"parent_id,omitempty"`
	Priority          string         `json:"priority,omitempty"`
	Milestone         *string        `json:"milestone,omitempty"`
	Labels            []string       `json:"labels,omitempty"`
	Attrs             map[string]any `json:"attrs,omitempty"`
	BundleName        *string        `json:"bundle_name,omitempty"`
	RepoInventory     []RepoDecl     `json:"repo_inventory,omitempty"`
	RepoInventoryBase []RepoBaseDecl `json:"repo_inventory_base,omitempty"`
}

// RepoDecl is one repo inventory declaration (02 §4.6).
type RepoDecl struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Subdir *string `json:"subdir,omitempty"`
}

// RepoBaseDecl freezes one repo's base commit at creation.
type RepoBaseDecl struct {
	Name string `json:"name"`
	Base string `json:"base"`
}

// DependencyCreateRequest mirrors the row-26 POST body.
type DependencyCreateRequest struct {
	BlockingWiID string `json:"blocking_wi_id"`
	Kind         string `json:"kind,omitempty"` // defaults to blocks server-side
}

// BudgetIncrementRequest mirrors the row-11 CAS body.
type BudgetIncrementRequest struct {
	Dimension  string `json:"dimension"`
	Amount     int64  `json:"amount"`
	Note       string `json:"note,omitempty"`
	BasedOnSeq int64  `json:"based_on_seq"`
}

// MemoryCreateRequest mirrors the row-22 POST body (not part of the 12
// verbs; provided for completeness of the memory face).
type MemoryCreateRequest struct {
	Project        string   `json:"project"`
	Type           string   `json:"type"`
	Title          *string  `json:"title,omitempty"`
	Visibility     string   `json:"visibility"`
	Content        string   `json:"content"`
	Tags           []string `json:"tags,omitempty"`
	Related        []string `json:"related,omitempty"`
	WorkItemID     *string  `json:"work_item_id,omitempty"`
	IdempotencyKey *string  `json:"idempotency_key,omitempty"`
}

// MachineRegisterRequest mirrors the row-21 heartbeat/registration body
// (api/openapi.yaml POST /api/v1/machines/heartbeat).
type MachineRegisterRequest struct {
	MachineID      string `json:"machine_id"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
}

// RegisterMachine performs the row-21 registration upsert (02 §4.10): an
// unknown machine_id registers (machine user + machines row + one-time
// api key in the response), a known one refreshes its heartbeat.
func (c *Client) RegisterMachine(req *MachineRegisterRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/machines/heartbeat", nil, req)
}

// UserCreateRequest mirrors M1.
type UserCreateRequest struct {
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind,omitempty"`
	Role        string `json:"role,omitempty"`
}

// UserPatchRequest mirrors the M1 PATCH body.
type UserPatchRequest struct {
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
}

// ApiKeyCreateRequest mirrors M2.
type ApiKeyCreateRequest struct {
	UserID    string  `json:"user_id"`
	ProjectID *string `json:"project_id"`
}

// ProjectCreateRequest mirrors M3.
type ProjectCreateRequest struct {
	Name    string `json:"name"`
	Visible *bool  `json:"visible,omitempty"`
}

// ProjectPatchRequest mirrors the M3 PATCH body: members upsert plus the
// physical remove_members list (implementation fix #18, api/openapi.yaml).
type ProjectPatchRequest struct {
	Members       *[]ProjectMember `json:"members,omitempty"`
	RemoveMembers *[]string        `json:"remove_members,omitempty"`
	Visible       *bool            `json:"visible,omitempty"`
}

// ProjectMember is one membership upsert row (role in
// viewer|writer|maintainer — the permission hard boundary's sole
// mechanical criterion, 02 §4.10).
type ProjectMember struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// RotateSecretRequest mirrors M5 (02 §6.6 two-mode rotation).
type RotateSecretRequest struct {
	Mode         string `json:"mode"` // grace | rollback
	GraceSeconds int64  `json:"grace_seconds,omitempty"`
}

// ---------------------------------------------------------------------------
// WI business face (02 §5.1 rows 1–6)
// ---------------------------------------------------------------------------

// CreateWorkItem creates a WI (row 1). Returns the full detail record.
func (c *Client) CreateWorkItem(req *CreateWorkItemRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/work-items", nil, req)
}

// ListWorkItems lists/filters WIs (row 2). filters keys: status, assignee,
// waiting_on, parent, milestone (unknown keys are rejected loudly here
// rather than 400'd by the server).
func (c *Client) ListWorkItems(project string, filters map[string]string) ([]byte, error) {
	q := url.Values{}
	q.Set("project", project)
	for _, k := range []string{"status", "assignee", "waiting_on", "parent", "milestone"} {
		if v, ok := filters[k]; ok && v != "" {
			q.Set(k, v)
		}
	}
	return c.do("GET", "/api/v1/work-items", q, nil)
}

// GetWorkItem reads the detail record (row 3, GET). include is the A9
// comma-separated multi-value: events, deps, children.
func (c *Client) GetWorkItem(id string, include []string) ([]byte, error) {
	q := url.Values{}
	if len(include) > 0 {
		q.Set("include", strings.Join(include, ","))
	}
	return c.do("GET", "/api/v1/work-items/"+url.PathEscape(id), q, nil)
}

// PatchWorkItem updates the closed PATCHable field set (row 3, PATCH).
// body must contain only keys from {goal, content, labels, priority,
// milestone, waiting_on, attrs} — sending an immutable key answers
// CORE-400 IMMUTABLE server-side.
func (c *Client) PatchWorkItem(id string, body map[string]any) ([]byte, error) {
	return c.do("PATCH", "/api/v1/work-items/"+url.PathEscape(id), nil, body)
}

// ReadyQueue reads the flat two-array ready queue (row 4, §5.1b A8).
func (c *Client) ReadyQueue(project string) ([]byte, error) {
	q := url.Values{"project": {project}}
	return c.do("GET", "/api/v1/ready-queue", q, nil)
}

// CancelWorkItem terminates a queued/paused/blocked WI (row 5).
func (c *Client) CancelWorkItem(id string) ([]byte, error) {
	return c.do("POST", "/api/v1/work-items/"+url.PathEscape(id)+"/cancel", nil, map[string]any{})
}

// GetProgress is the read-through progress face (row 6) — the fallback
// observe layer for WIs whose Capsule is not on this machine
// (04-local-runtime.md §5.1a layer two).
func (c *Client) GetProgress(id string) ([]byte, error) {
	return c.do("GET", "/api/v1/work-items/"+url.PathEscape(id)+"/progress", nil, nil)
}

// ---------------------------------------------------------------------------
// Dependency edges (row 26)
// ---------------------------------------------------------------------------

// AddDependency creates one edge; blockedID is the path {id} side (the
// dependent), req names the blocking side.
func (c *Client) AddDependency(blockedID string, req *DependencyCreateRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/work-items/"+url.PathEscape(blockedID)+"/dependencies", nil, req)
}

// ListDependencies returns the blocking/blocked_by lists.
func (c *Client) ListDependencies(id string) ([]byte, error) {
	return c.do("GET", "/api/v1/work-items/"+url.PathEscape(id)+"/dependencies", nil, nil)
}

// DeleteDependency removes one edge physically.
func (c *Client) DeleteDependency(blockedID, blockingID, kind string) ([]byte, error) {
	p := "/api/v1/work-items/" + url.PathEscape(blockedID) + "/dependencies/" +
		url.PathEscape(blockingID) + "/" + url.PathEscape(kind)
	return c.do("DELETE", p, nil, nil)
}

// ---------------------------------------------------------------------------
// Budget face (rows 11/12 — human plane, role-based, no generation token)
// ---------------------------------------------------------------------------

// GetBudget reads baseline + increments + effective ceilings + max_seq.
func (c *Client) GetBudget(id string) ([]byte, error) {
	return c.do("GET", "/api/v1/work-items/"+url.PathEscape(id)+"/budget", nil, nil)
}

// AddBudgetIncrement posts one CAS increment (row 11).
func (c *Client) AddBudgetIncrement(id string, req *BudgetIncrementRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/work-items/"+url.PathEscape(id)+"/budget-increments", nil, req)
}

// ---------------------------------------------------------------------------
// Memory face (rows 22–23; the recall verb uses search)
// ---------------------------------------------------------------------------

// SearchMemories is the real-time trigram FTS retrieval (row 23) — the
// only v1 search path.
func (c *Client) SearchMemories(project, q, memType string, tags []string, limit int) ([]byte, error) {
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
	return c.do("GET", "/api/v1/memories/search", v, nil)
}

// ---------------------------------------------------------------------------
// Management face (M1–M5; admin unless noted)
// ---------------------------------------------------------------------------

// CreateUser issues a user (M1, admin only).
func (c *Client) CreateUser(req *UserCreateRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/users", nil, req)
}

// ListUsers lists all users (M1, admin only).
func (c *Client) ListUsers() ([]byte, error) {
	return c.do("GET", "/api/v1/users", nil, nil)
}

// GetUser reads one user (M1).
func (c *Client) GetUser(id string) ([]byte, error) {
	return c.do("GET", "/api/v1/users/"+url.PathEscape(id), nil, nil)
}

// PatchUser updates display_name and/or role (M1).
func (c *Client) PatchUser(id string, req *UserPatchRequest) ([]byte, error) {
	return c.do("PATCH", "/api/v1/users/"+url.PathEscape(id), nil, req)
}

// CreateApiKey mints a key (M2; plaintext returned exactly once).
func (c *Client) CreateApiKey(req *ApiKeyCreateRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/api-keys", nil, req)
}

// ListApiKeys lists key metadata, never material (M2).
func (c *Client) ListApiKeys() ([]byte, error) {
	return c.do("GET", "/api/v1/api-keys", nil, nil)
}

// RevokeApiKey sets revoked_at (M2).
func (c *Client) RevokeApiKey(id string) ([]byte, error) {
	return c.do("DELETE", "/api/v1/api-keys/"+url.PathEscape(id), nil, nil)
}

// CreateProject creates a project (M3, admin only).
func (c *Client) CreateProject(req *ProjectCreateRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/projects", nil, req)
}

// ListProjects lists visible projects (M3, any authenticated caller).
func (c *Client) ListProjects() ([]byte, error) {
	return c.do("GET", "/api/v1/projects", nil, nil)
}

// GetProject reads one project by id or name (M3).
func (c *Client) GetProject(idOrName string) ([]byte, error) {
	return c.do("GET", "/api/v1/projects/"+url.PathEscape(idOrName), nil, nil)
}

// PatchProject updates a project and/or its membership rows (M3).
func (c *Client) PatchProject(idOrName string, req *ProjectPatchRequest) ([]byte, error) {
	return c.do("PATCH", "/api/v1/projects/"+url.PathEscape(idOrName), nil, req)
}

// Backup triggers the on-demand snapshot (M4, admin only).
func (c *Client) Backup() ([]byte, error) {
	return c.do("POST", "/api/v1/admin/backup", nil, map[string]any{})
}

// RotateSecret rotates the service secret (M5, admin only; two modes
// grace/rollback, 02 §6.6).
func (c *Client) RotateSecret(req *RotateSecretRequest) ([]byte, error) {
	return c.do("POST", "/api/v1/service/rotate-secret", nil, req)
}
