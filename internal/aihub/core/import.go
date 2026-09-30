package core

// import.go: the migration-only work-item import path. The public create
// (WIManager.Create) deliberately cannot produce a terminal record — a
// fresh ticket is always queued, ids are always server-minted and
// closed_at is always server time. Migrating a finished pilot's record
// stock therefore needs a second, strictly narrower path that CAN carry
// id/status/closed_at, kept as far from the ordinary face as possible:
//
//   - statuses are restricted to the record set (wrapped/failed/canceled/
//     blocked); a queued/running/paused answer would make this a general
//     purpose create-with-chosen-id and could inject claimable live
//     state behind the state machine's back;
//   - the caller is the admin face only (server mounts it under
//     /api/v1/admin/import-work-item), never the business face;
//   - execution history never enters: no owner_generations row, no
//     dependencies, no claim idempotency — one audit event records that
//     the row arrived by import, not by migration of history;
//   - the id must match the wi_ domain but its exact shape is the old
//     world's (old ids migrate verbatim; NewID's length is not imposed).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// wiIDPattern pins the import id domain: "wi_" + identifier body. Path
// hostile bodies are rejected; the exact length NewID produces is NOT
// required of old-world stock.
var wiIDPattern = regexp.MustCompile(`^wi_[A-Za-z0-9_-]+$`)

// importableStatuses is the closed set the import path accepts: terminal
// records (wrapped/failed/canceled) plus blocked — a blocked ticket is a
// record of work that stopped without a verdict and v1 legitimately
// carries it as blocked. Everything else (queued/running/paused) is live
// state only the state machine may produce.
var importableStatuses = map[string]bool{
	api.StatusWrapped: true, api.StatusFailed: true,
	api.StatusCanceled: true, api.StatusBlocked: true,
}

// ImportRequest is the POST /api/v1/admin/import-work-item body.
type ImportRequest struct {
	Project   string         `json:"project"` // target project name or id
	ID        string         `json:"id"`      // old-world id, kept verbatim
	Seq       *int64         `json:"seq"`     // old display seq; nil = auto
	Goal      string         `json:"goal"`
	Content   *string        `json:"content"`
	Status    string         `json:"status"`
	Priority  string         `json:"priority"`
	Milestone *string        `json:"milestone"`
	Labels    []string       `json:"labels"`
	Attrs     map[string]any `json:"attrs"`
	Reporter  string         `json:"reporter"` // TARGET user id (the tool resolves old→new)
	Assignee  *string        `json:"assignee"`
	WaitingOn *string        `json:"waiting_on"`
	// Bundle reference triple, carried like Create does (02 §4.4/§4.5).
	BundleName     *string         `json:"bundle_name"`
	BundleDigest   *string         `json:"bundle_digest"`
	BudgetBaseline json.RawMessage `json:"workflow_budget_baseline"`
	CreatedAt      string          `json:"created_at"` // RFC3339, old world's
	UpdatedAt      string          `json:"updated_at"`
	ClosedAt       *string         `json:"closed_at"`
}

// ImportOutcome says what the import did.
type ImportOutcome string

const (
	ImportDone    ImportOutcome = "imported"
	ImportSkipped ImportOutcome = "skipped-existing"
)

// ImportResult is the import answer.
type ImportResult struct {
	Outcome  ImportOutcome `json:"outcome"`
	WorkItem *api.WorkItem `json:"work_item"`
}

// errImportAlreadyExists reports a skipped re-import (idempotent re-run).
type errImportAlreadyExists struct{ id string }

func (e *errImportAlreadyExists) Error() string {
	return "work item " + e.id + " already exists"
}

// IsImportAlreadyExists reports whether err is the idempotent-skip signal.
func IsImportAlreadyExists(err error) bool {
	_, ok := err.(*errImportAlreadyExists)
	return ok
}

func parseImportTimestamp(s, field string) error {
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return Fail(api.ErrCoreBadInput, "IMPORT_BAD_TIMESTAMP",
			"%s: %q is not RFC3339", field, s)
	}
	return nil
}

// Import inserts one migration record. See the file comment for the face
// constraints. Same-id re-import skips (the caller counts it); seq
// collisions are refused, never silently re-assigned.
func (m *WIManager) Import(req ImportRequest) (*ImportResult, error) {
	req.Goal = strings.TrimSpace(req.Goal)
	if !wiIDPattern.MatchString(req.ID) {
		return nil, Fail(api.ErrCoreBadInput, "IMPORT_BAD_ID",
			"id %q must match %s", req.ID, wiIDPattern.String())
	}
	if req.Goal == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "goal required")
	}
	if len(req.Goal) > 500 {
		return nil, Fail(api.ErrCoreBadInput, "", "goal exceeds 500 characters")
	}
	if !importableStatuses[req.Status] {
		return nil, Fail(api.ErrCoreBadInput, "IMPORT_BAD_STATUS",
			"status %q is not an importable record status (wrapped|failed|canceled|blocked)", req.Status)
	}
	switch req.Status {
	case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
		if req.ClosedAt == nil || strings.TrimSpace(*req.ClosedAt) == "" {
			return nil, Fail(api.ErrCoreBadInput, "IMPORT_CLOSED_AT_REQUIRED",
				"status %s requires closed_at", req.Status)
		}
	default: // blocked: a non-terminal record carries no close time
		req.ClosedAt = nil
	}
	if req.ClosedAt != nil {
		if err := parseImportTimestamp(*req.ClosedAt, "closed_at"); err != nil {
			return nil, err
		}
	}
	if err := parseImportTimestamp(req.CreatedAt, "created_at"); err != nil {
		return nil, err
	}
	if err := parseImportTimestamp(req.UpdatedAt, "updated_at"); err != nil {
		return nil, err
	}
	if req.Priority == "" {
		req.Priority = api.PriorityNormal
	}
	if !validPriority(req.Priority) {
		return nil, Fail(api.ErrCoreBadInput, "", "bad priority %q", req.Priority)
	}
	if req.Labels == nil {
		req.Labels = []string{}
	}
	if len(req.Labels) > 20 {
		return nil, Fail(api.ErrCoreBadInput, "", "too many labels (max 20)")
	}
	if req.Attrs == nil {
		req.Attrs = map[string]any{}
	}
	if err := checkAttrs(req.Attrs, "attrs"); err != nil {
		return nil, err
	}
	if req.Reporter == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "reporter required")
	}

	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var projectID string
	err = tx.QueryRow(`SELECT id FROM projects WHERE name=?1 OR id=?1`, req.Project).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "project %q", req.Project)
	}
	if err != nil {
		return nil, err
	}
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE id=?1`, req.Reporter).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreBadInput, "IMPORT_BAD_REPORTER",
				"reporter %q is not a user (the tool must resolve old ids to target users)", req.Reporter)
		}
		return nil, err
	}
	// Idempotency: an existing id skips; the caller decides whether the
	// existing row matches the record (the tool samples it).
	var existing string
	err = tx.QueryRow(`SELECT id FROM work_items WHERE id=?1`, req.ID).Scan(&existing)
	if err == nil {
		return &ImportResult{Outcome: ImportSkipped}, &errImportAlreadyExists{id: req.ID}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Seq: preserved when given (old display seq keeps the old slug
	// numbers), auto when absent; a collision is refused, never bumped.
	seq := int64(0)
	if req.Seq != nil {
		if *req.Seq <= 0 {
			return nil, Fail(api.ErrCoreBadInput, "IMPORT_BAD_SEQ", "seq must be > 0")
		}
		var taken int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM work_items WHERE seq=?1`, *req.Seq).Scan(&taken); err != nil {
			return nil, err
		}
		if taken > 0 {
			return nil, Fail(api.ErrCoreConflict, "IMPORT_SEQ_TAKEN",
				"seq %d already exists", *req.Seq)
		}
		seq = *req.Seq
	} else {
		var maxSeq sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(seq) FROM work_items`).Scan(&maxSeq); err != nil {
			return nil, err
		}
		seq = maxSeq.Int64 + 1
	}
	// Bundle reference triple, mirroring Create's stand-in rule.
	var bundleName, bundleDigest any
	if req.BundleName != nil && *req.BundleName != "" {
		bundleName = *req.BundleName
		if req.BundleDigest != nil && *req.BundleDigest != "" {
			bundleDigest = *req.BundleDigest
		} else {
			bundleDigest = Sha256Hex("bundle-name:" + *req.BundleName)
		}
	}
	var budget any
	if len(req.BudgetBaseline) > 0 {
		budget = string(req.BudgetBaseline)
	}
	labels, _ := json.Marshal(req.Labels)
	attrs, _ := json.Marshal(req.Attrs)
	if _, err := tx.Exec(`INSERT INTO work_items(id, seq, project_id, goal, content, parent_id,
		status, priority, milestone, labels, attrs, reporter, assignee, waiting_on,
		bundle_name, bundle_digest, workflow_budget_baseline, created_at, updated_at, closed_at)
		VALUES (?1,?2,?3,?4,?5,NULL,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16,?17,?18,?19)`,
		req.ID, seq, projectID, req.Goal, nullableString(req.Content),
		req.Status, req.Priority, nullableString(req.Milestone), string(labels), string(attrs),
		req.Reporter, nullableString(req.Assignee), nullableString(req.WaitingOn),
		bundleName, bundleDigest, budget, req.CreatedAt, req.UpdatedAt, nullableString(req.ClosedAt)); err != nil {
		return nil, err
	}
	// One audit event: the row arrived by import. This is deliberately
	// NOT migrated history — the old world's wi_events stay behind.
	if err := appendEventTx(tx, req.ID, Now(), "", "work_item_imported", map[string]any{
		"imported_status": req.Status,
		"imported_seq":    seq,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	wi, err := m.Get(req.ID, GetOptions{})
	if err != nil {
		return nil, err
	}
	return &ImportResult{Outcome: ImportDone, WorkItem: wi}, nil
}
