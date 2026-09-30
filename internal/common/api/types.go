// Package api defines the shared business contract between the aihub server
// and the pf client: work items, projects, users, generations, memories and
// the typed error-code registry (v1 architecture: docs/design/polyforge-vnext/
// v1-architecture-spec.md + v1-spec-full.md §一, plus the 00–07 baseline docs
// for business semantics).
//
// v1 deltas vs v0 (frozen under /v0):
//   - SYNC-* and revision-chain endpoints are deleted (git IS the chain).
//   - New GIT-* error codes for the git-http-backend proxy surface.
//   - DAEMON-* codes retained; publish_request becomes flush(reason).
package api

import "encoding/json"

// WorkItem is the server-side work item record (02 §4). Field semantics are
// inherited from the v0 contract; JSON tags are wire-stable.
type WorkItem struct {
	ID             string          `json:"id"`
	Slug           string          `json:"slug"`
	Seq            int64           `json:"seq"`
	ProjectID      string          `json:"project_id"`
	ProjectName    string          `json:"project"`
	Goal           string          `json:"goal"`
	Content        *string         `json:"content"`
	ParentID       *string         `json:"parent_id"`
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
}

// WI statuses (02 §4.1).
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusPaused   = "paused"
	StatusBlocked  = "blocked"
	StatusWrapped  = "wrapped"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
)

// Priorities.
const (
	PriorityUrgent = "urgent"
	PriorityHigh   = "high"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// Project (02 §3).
type Project struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Repos        []ProjectRepo   `json:"repos"`
	Visible      bool            `json:"visible"`
	Members      []ProjectMember `json:"members"`
	Identifier   string          `json:"identifier"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type ProjectRepo struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

type ProjectMember struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"` // owner|maintainer|writer|viewer
}

// User (02 §2).
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"` // admin|writer
	UserType    string `json:"user_type"` // human|machine
	APITokens   []APIToken `json:"api_tokens"`
	CreatedAt   string `json:"created_at"`
}

type APIToken struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// Claim records a machine taking ownership of a work item (02 §6).
type Claim struct {
	WorkItemID  string `json:"work_item_id"`
	AttemptID   string `json:"attempt_id"`
	MachineID   string `json:"machine_id"`
	Owner       string `json:"owner"`
	Generation int64  `json:"generation"`
	BundleName  string `json:"bundle_name"`
	BundleDigest string `json:"bundle_digest"`
	ClaimedAt    string `json:"claimed_at"`
	Token       string `json:"token"`          // attempt-scoped push credential
	TokenExpires string `json:"token_expires"` // RFC3339
}

// PushCredential is the attempt-scoped git credential minted at claim time
// (v1 spec §六 Generation fencing, main layer). Takeover revokes it, which
// makes an old daemon's pushes fail at auth.
type PushCredential struct {
	AttemptID string `json:"attempt_id"`
	Username  string `json:"username"` // = work item id
	Secret    string `json:"secret"`
	ExpiresAt string `json:"expires_at"`
}

// Memory (06).
type Memory struct {
	ID            string          `json:"id"`
	Project       string          `json:"project"`
	Type          string          `json:"type"`
	Content       string          `json:"content"`
	ContextSnippet string         `json:"context_snippet"`
	Tags          []string        `json:"tags"`
	Visibility    string          `json:"visibility"` // admin|private|project|team|public
	BaseStrength  int             `json:"base_strength"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	Related       []string        `json:"related_memory_ids"`
	WorkItemID    string          `json:"work_item_id,omitempty"`
}

// ListResponse is the envelope for list endpoints.
type ListResponse[T any] struct {
	Items    []T     `json:"items"`
	Total    int     `json:"total"`
	Cursor   *string `json:"cursor,omitempty"`
}
