// Package api defines the shared business contract between the aihub server
// and the pf client: work items, projects, users, sessions, memories and
// the typed error-code registry (session model and routes:
// docs/design/polyforge-vnext/reference.md §2–§5).
//
// v1 deltas vs v0 (removed from the tree; in git history):
//   - SYNC-* and revision-chain endpoints are deleted (git IS the chain).
//   - New GIT-* error codes for the git-http-backend proxy surface.
//   - DAEMON-* codes retained; publish_request is deleted.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// WorkItem is the server-side work item record (reference.md §2). Field semantics are
// inherited from the v0 contract; JSON tags are wire-stable.
type WorkItem struct {
	ID          string         `json:"id"`
	Slug        string         `json:"slug"`
	Seq         int64          `json:"seq"`
	ProjectID   string         `json:"project_id"`
	ProjectName string         `json:"project"`
	Goal        string         `json:"goal"`
	Content     *string        `json:"content"`
	ParentID    *string        `json:"parent_id"`
	Status      string         `json:"status"`
	Priority    string         `json:"priority"`
	Milestone   *string        `json:"milestone"`
	Labels      []string       `json:"labels"`
	Attrs       map[string]any `json:"attrs"`
	Reporter    string         `json:"reporter"`
	Assignee    *string        `json:"assignee"`
	WaitingOn   *string        `json:"waiting_on"`
	BundleName  *string        `json:"bundle_name"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	ClosedAt    *string        `json:"closed_at"`
}

// DisplayName answers the human-facing name of a work item: its slug
// ("<project>#<seq>"), or the id when no slug is known. Machine-side
// references (API paths, capsule.json, state.json keys) keep the id.
func DisplayName(slug, id string) string {
	if slug != "" {
		return slug
	}
	return id
}

// PathName answers the path- and ref-safe readable name of a work item,
// "<project>-<seq>": the slug with its "#" (a zsh EXTENDED_GLOB
// metacharacter and the URL fragment separator) replaced by "-". It names
// the capsule directory and the delivery branch
// polyforge/<project>-<seq>. Project names never
// contain "#", so the seq is always the part after the last "-". The id
// is the answer when no slug is known, or when the slug is not of the
// "<project>#<seq>" shape (it never becomes a path or ref component
// unchecked).
func PathName(slug, id string) string {
	if slugShape.MatchString(slug) {
		return strings.Replace(slug, "#", "-", 1)
	}
	return id
}

// slugShape is the slug a path or ref may be built from: an aihub project
// name (core/admin.go's charset) and a decimal seq.
var slugShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}#[0-9]+$`)

// WI statuses (reference.md §2.1).
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

// Project (reference.md §4.1).
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
	Role   string `json:"role"` // maintainer|writer|viewer
}

// User (reference.md §4.1).
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
	ID         string   `json:"id"`
	Project    string   `json:"project"`
	Type       string   `json:"type"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags"`
	Visibility string   `json:"visibility"` // admin|private|project|team|public
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	Related    []string `json:"related_memory_ids"`
	WorkItemID string   `json:"work_item_id,omitempty"`
}

// ListResponse is the envelope for list endpoints.
type ListResponse[T any] struct {
	Items  []T     `json:"items"`
	Total  int     `json:"total"`
	Cursor *string `json:"cursor,omitempty"`
}

// ---------------------------------------------------------------------------
// Session model wire types (docs/design/polyforge-vnext/reference.md §3).

// SessionClaim is the v1.5 claim/attach response envelope. The
// session_id is a SECRET minted by the daemon; it is never echoed here —
// only its public fingerprint travels (B3 identity/credential split).
type SessionClaim struct {
	WorkItemID         string        `json:"work_item_id"`
	SessionFingerprint string        `json:"session_fingerprint"`
	MachineID          string        `json:"machine_id"`
	BundleName         string        `json:"bundle_name"`
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
// client-minted secret, journaled durably BEFORE this call (B1); it is
// also the replay key (a repeat by the holding session answers the same
// claim). aihub does not read idempotency_key.
type SessionClaimRequest struct {
	WorkItemID     string `json:"work_item_id"`
	SessionID      string `json:"session_id"`
	MachineID      string `json:"machine_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ForceEndSessionRequest is the admin/maintainer force-end body (reason
// required; it lands in the session_superseded audit event).
type ForceEndSessionRequest struct {
	Reason string `json:"reason"`
}

// ResumeBlockedRequest is the resume-blocked door's body. AcceptLostWork
// is the human's explicit acceptance that the work the sealing machine
// never pushed is lost: the door then clears the blocked seal (the
// recorded head and machine, see SessionClaim.SealedHead) so a claim on
// any machine continues from the chain aihub holds.
type ResumeBlockedRequest struct {
	AcceptLostWork bool `json:"accept_lost_work,omitempty"`
}

// SessionAttachRequest resumes a paused WI under the SAME session (M3:
// same-machine restart resume; clean_pause WIs refuse attach).
type SessionAttachRequest struct {
	WorkItemID string `json:"work_item_id"`
	SessionID  string `json:"session_id"`
	MachineID  string `json:"machine_id"`
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
	Status    string `json:"status"` // wrapped|failed|blocked|paused
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
