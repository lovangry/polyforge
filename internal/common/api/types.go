// Package api defines the shared business contract between the aihub server
// and the pf client: work items, projects, users, sessions, memories and
// the typed error-code registry (v1.5 session model:
// docs/design/polyforge-vnext/v1.5-api-contract.md, over the v1
// architecture baseline).
//
// v1 deltas vs v0 (removed from the tree; in git history):
//   - SYNC-* and revision-chain endpoints are deleted (git IS the chain).
//   - New GIT-* error codes for the git-http-backend proxy surface.
//   - DAEMON-* codes retained; publish_request is deleted.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

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
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Repos       []ProjectRepo   `json:"repos"`
	Visible     bool            `json:"visible"`
	Members     []ProjectMember `json:"members"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
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
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`      // admin|writer
	UserType    string     `json:"user_type"` // human|machine
	APITokens   []APIToken `json:"api_tokens"`
	CreatedAt   string     `json:"created_at"`
}

type APIToken struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// Memory (06).
type Memory struct {
	ID             string   `json:"id"`
	Project        string   `json:"project"`
	Type           string   `json:"type"`
	Content        string   `json:"content"`
	ContextSnippet string   `json:"context_snippet"`
	Tags           []string `json:"tags"`
	Visibility     string   `json:"visibility"` // admin|private|project|team|public
	BaseStrength   int      `json:"base_strength"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	Related        []string `json:"related_memory_ids"`
	WorkItemID     string   `json:"work_item_id,omitempty"`
}

// ListResponse is the envelope for list endpoints.
type ListResponse[T any] struct {
	Items  []T     `json:"items"`
	Total  int     `json:"total"`
	Cursor *string `json:"cursor,omitempty"`
}

// ---------------------------------------------------------------------------
// v1.5 session model wire types (docs/design/polyforge-vnext/v1.5-api-contract.md).

// SessionClaim is the v1.5 claim/attach response envelope. The
// session_id is a SECRET minted by the daemon; it is never echoed here —
// only its public fingerprint travels (B3 identity/credential split).
type SessionClaim struct {
	WorkItemID         string        `json:"work_item_id"`
	SessionFingerprint string        `json:"session_fingerprint"`
	MachineID          string        `json:"machine_id"`
	Owner              string        `json:"owner"`
	BundleName         string        `json:"bundle_name"`
	BundleDigest       string        `json:"bundle_digest"`
	ClaimedAt          string        `json:"claimed_at"`
	Repos              []ProjectRepo `json:"repos,omitempty"`
	// SealedHead/SealedMachine are the blocked verdict's recorded capsule
	// head and the machine that sealed it (v2-converge §7); empty unless
	// the WI was last closed blocked with a head. A claim by another
	// machine must find exactly this head in its clone or refuse.
	SealedHead    string `json:"sealed_head,omitempty"`
	SealedMachine string `json:"sealed_machine,omitempty"`
}

// SessionClaimResponse wraps SessionClaim plus the full WorkItem.
type SessionClaimResponse struct {
	Claim    SessionClaim `json:"claim"`
	WorkItem WorkItem     `json:"work_item"`
}

// SessionClaimRequest is the CAS claim body. session_id is the
// client-minted secret, journaled durably BEFORE this call (B1).
type SessionClaimRequest struct {
	WorkItemID     string `json:"work_item_id"`
	SessionID      string `json:"session_id"`
	MachineID      string `json:"machine_id"`
	Owner          string `json:"owner"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ForceEndSessionRequest is the admin/maintainer force-end body (reason
// required; it lands in the session_superseded audit event).
type ForceEndSessionRequest struct {
	Reason string `json:"reason"`
}

// SessionAttachRequest resumes a paused WI under the SAME session (M3:
// same-machine restart resume; clean_pause WIs refuse attach).
type SessionAttachRequest struct {
	WorkItemID string `json:"work_item_id"`
	SessionID  string `json:"session_id"`
	MachineID  string `json:"machine_id"`
	Owner      string `json:"owner"`
}

// SessionVerifyRequest is the compare-only body (B3: the hub stores only
// the hash and never returns the secret).
type SessionVerifyRequest struct {
	SessionID string `json:"session_id"`
}

// SessionVerifyResponse answers "is this session still the holder" plus
// the public audit fields a superseded daemon needs to self-diagnose.
type SessionVerifyResponse struct {
	WorkItemID         string `json:"work_item_id"`
	Matches            bool   `json:"matches"`
	Status             string `json:"status"`
	CurrentFingerprint string `json:"current_fingerprint,omitempty"`
	CleanPause         bool   `json:"clean_pause"`
}

// SessionEndRequest replaces end-generation: fenced on the session hash,
// terminal verdict sets the status and clears the session row entirely.
type SessionEndRequest struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // wrapped|failed|canceled|blocked|paused (canceled: the abandon of a repair_pending WI, ended by its holder daemon)
	Note      string `json:"note,omitempty"`
	// CleanPause rides the paused verdict only (merge amend: the atomic
	// pause transition — session KEPT, clean_pause set — is the M3 pause;
	// true = operator pause, restart must not auto-resume).
	CleanPause bool `json:"clean_pause,omitempty"`
	// HeadSHA rides the blocked verdict only (v2-converge §7): the capsule
	// revision-chain head the sealing daemon holds locally after its
	// best-effort commit+push. aihub records it (with the session's
	// machine) so a continue claimed by a DIFFERENT daemon can refuse a
	// clone whose head is not this one (the #116 stale-state hazard).
	HeadSHA string `json:"head_sha,omitempty"`
}

// SessionFingerprint derives the public handle from the secret:
// hex(sha256(session_id))[:16]. Both sides compute it identically; the hub
// stores the full hash and serves only the prefix on public surfaces.
func SessionFingerprint(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])[:16]
}

// SessionHashHex is the at-rest form: hex(sha256(session_id)).
func SessionHashHex(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Ship ledger (polyforge#123, P1b): the WI → repo → branch-top SHA
// accounting aihub keeps of what ship actually pushed, queryable per WI.
// ---------------------------------------------------------------------------

// ShipLedgerEntry is one repo's branch-top report: the POST body's entry.
// The SHA is what the daemon's ship bridge saw at the branch top it pushed
// (or re-used) — self-recorded accounting, not re-verified server-side.
type ShipLedgerEntry struct {
	Repo         string `json:"repo"`
	BranchTopSHA string `json:"branch_top_sha"`
}

// ShipLedgerRequest is the POST /api/v1/work-items/{id}/ship-ledger body:
// the delivery branch plus the repos pushed by one ship run.
type ShipLedgerRequest struct {
	Branch string            `json:"branch,omitempty"`
	Repos  []ShipLedgerEntry `json:"repos"`
}
