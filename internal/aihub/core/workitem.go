package core

// Work items: CRUD, WI state machine, dependencies and events.
//
// v1 state machine (statuses in common/api): queued → running (claim),
// running → paused/blocked (end-generation), paused/blocked → running
// (claim intent=resume), running → wrapped/failed (end-generation),
// queued/paused/blocked → canceled (cancel). Terminal states are
// read-only (CORE-410).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// WIManager is the work-item state machine.
type WIManager struct {
	db *DB
}

func NewWIManager(db *DB) *WIManager { return &WIManager{db: db} }

// CreateRequest is the POST /api/v1/work-items body.
type CreateRequest struct {
	Project   string         `json:"project"` // project name or id
	Goal      string         `json:"goal"`
	Content   *string        `json:"content"`
	ParentID  *string        `json:"parent_id"`
	Priority  string         `json:"priority"`
	Milestone *string        `json:"milestone"`
	Labels    []string       `json:"labels"`
	Attrs     map[string]any `json:"attrs"`
	Reporter  string         `json:"reporter"` // user id (from auth)
	// BundleName non-empty marks an execution ticket; NULL leaves a pure
	// registration ticket (02 §4.4: not claimable, CORE-409 NOT_CLAIMABLE).
	// The bundle reference triple is immutable after the create
	// transaction (INV-2e); there is no UPDATE path.
	BundleName   *string `json:"bundle_name"`
	BundleDigest *string `json:"bundle_digest"` // optional client-supplied pin; defaulted below
}

// terminal statuses (common/api vocabulary, single-l "canceled").
func isTerminal(status string) bool {
	switch status {
	case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
		return true
	}
	return false
}

func validPriority(p string) bool {
	switch p {
	case api.PriorityLow, api.PriorityNormal, api.PriorityHigh, api.PriorityUrgent:
		return true
	}
	return false
}

// forbidden attrs keys (execution-state blacklist; 02 §5.1a, kept from v0
// business semantics): Core never persists workflow execution state.
var executionStateKeys = map[string]bool{
	"current_step": true, "step_id": true, "step_attempt_id": true,
	"attempt": true, "review_round": true, "review_rounds": true,
	"review_verdict": true, "progress": true, "workflow_status": true,
	"steps": true, "rewind_stats": true, "budget_consumed": true,
	"command_count": true, "step_start_refs": true, "pending_question": true,
}

// checkAttrs recursively rejects execution-state keys at any depth.
func checkAttrs(v any, path string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			p := path + "." + k
			if executionStateKeys[k] {
				return Fail(api.ErrCoreBadInput, "EXECUTION_STATE_REJECTED",
					"attrs key %s carries execution state; Core never persists it", p)
			}
			if err := checkAttrs(val, p); err != nil {
				return err
			}
		}
	case []any:
		for i, val := range t {
			if err := checkAttrs(val, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Create inserts a new queued work item and returns it.
func (m *WIManager) Create(req CreateRequest) (*api.WorkItem, error) {
	req.Goal = strings.TrimSpace(req.Goal)
	if req.Goal == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "goal required")
	}
	if len(req.Goal) > 500 {
		return nil, Fail(api.ErrCoreBadInput, "", "goal exceeds 500 characters")
	}
	if req.Priority == "" {
		req.Priority = api.PriorityNormal
	}
	if !validPriority(req.Priority) {
		return nil, Fail(api.ErrCoreBadInput, "", "bad priority %q", req.Priority)
	}
	if req.Attrs == nil {
		req.Attrs = map[string]any{}
	}
	if err := checkAttrs(req.Attrs, "attrs"); err != nil {
		return nil, err
	}
	if req.Labels == nil {
		req.Labels = []string{}
	}
	if len(req.Labels) > 20 {
		return nil, Fail(api.ErrCoreBadInput, "", "too many labels (max 20)")
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
	if req.Reporter == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "reporter required")
	}
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE id=?1`, req.Reporter).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreBadInput, "", "reporter %q is not a user", req.Reporter)
		}
		return nil, err
	}
	if req.ParentID != nil && *req.ParentID != "" {
		var parentProject, parentBundle sql.NullString
		err = tx.QueryRow(`SELECT project_id, bundle_name FROM work_items WHERE id=?1`, *req.ParentID).
			Scan(&parentProject, &parentBundle)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreBadInput, "PARENT_NOT_FOUND", "parent %q", *req.ParentID)
		}
		if err != nil {
			return nil, err
		}
		if parentProject.String != projectID {
			return nil, Fail(api.ErrCoreBadInput, "PARENT_PROJECT_MISMATCH", "parent belongs to another project")
		}
		if parentBundle.Valid && parentBundle.String != "" {
			return nil, Fail(api.ErrCoreBadInput, "PARENT_NOT_REGISTRY", "parent is an execution ticket, not a registry ticket")
		}
	} else {
		req.ParentID = nil
	}

	// Display seq: global monotonic (aihub#<seq>).
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM work_items`).Scan(&maxSeq); err != nil {
		return nil, err
	}
	seq := maxSeq.Int64 + 1

	// Bundle reference (02 §4.4/§4.5): a non-empty bundle_name makes an
	// execution ticket. Full §4.5 fixation (re-hashing the bundle checkout
	// server-side at create time) is not wired in this round; a client may
	// pin the digest explicitly via bundle_digest, and otherwise the
	// server stores a deterministic name-derived stand-in so the
	// reference triple is never half-empty. TODO(v1): §4.5 checkout hash.
	var bundleName, bundleDigest any
	if req.BundleName != nil && *req.BundleName != "" {
		bundleName = *req.BundleName
		if req.BundleDigest != nil && *req.BundleDigest != "" {
			bundleDigest = *req.BundleDigest
		} else {
			bundleDigest = Sha256Hex("bundle-name:" + *req.BundleName)
		}
	}

	id, err := NewID("wi_")
	if err != nil {
		return nil, err
	}
	now := Now()
	labels, _ := json.Marshal(req.Labels)
	attrs, _ := json.Marshal(req.Attrs)
	if _, err := tx.Exec(`INSERT INTO work_items(id, seq, project_id, goal, content, parent_id,
		status, priority, milestone, labels, attrs, reporter, bundle_name, bundle_digest, created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?15)`,
		id, seq, projectID, req.Goal, nullableString(req.Content), nullableString(req.ParentID),
		api.StatusQueued, req.Priority, nullableString(req.Milestone), string(labels), string(attrs), req.Reporter,
		bundleName, bundleDigest, now); err != nil {
		return nil, err
	}
	if err := appendEventTx(tx, id, now, req.Reporter, "work_item_filed", nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.Get(id, GetOptions{})
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Resolve maps an id or a "<project>#<seq>" slug to the work-item id.
func (m *WIManager) Resolve(idOrSlug string) (string, error) {
	if strings.HasPrefix(idOrSlug, "wi_") {
		return idOrSlug, nil
	}
	i := strings.LastIndexByte(idOrSlug, '#')
	if i < 0 {
		return "", Fail(api.ErrCoreNotFound, "", "work item %q", idOrSlug)
	}
	project, seqStr := idOrSlug[:i], idOrSlug[i+1:]
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		return "", Fail(api.ErrCoreNotFound, "", "work item %q", idOrSlug)
	}
	var id string
	err = m.db.QueryRow(`SELECT w.id FROM work_items w JOIN projects p ON p.id=w.project_id
		WHERE p.name=?1 AND w.seq=?2`, project, seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Fail(api.ErrCoreNotFound, "", "work item %q", idOrSlug)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetOptions controls detail assembly.
type GetOptions struct {
	IncludeEvents bool
	IncludeDeps   bool
}

// Get loads one work item (id or slug), with slug/assignee/project filled.
func (m *WIManager) Get(idOrSlug string, opts GetOptions) (*api.WorkItem, error) {
	id, err := m.Resolve(idOrSlug)
	if err != nil {
		return nil, err
	}
	wi, err := m.load(id)
	if err != nil {
		return nil, err
	}
	if opts.IncludeEvents {
		events, eerr := m.Events(id)
		if eerr != nil {
			return nil, eerr
		}
		wi.Attrs["events"] = events // presentation-only; not persisted
	}
	if opts.IncludeDeps {
		deps, derr := m.Dependencies(id)
		if derr != nil {
			return nil, derr
		}
		wi.Attrs["deps"] = deps
	}
	return wi, nil
}

func (m *WIManager) load(id string) (*api.WorkItem, error) {
	var wi api.WorkItem
	var content, parent, milestone, assignee, waitingOn, bundle, bundleDigest, budget,
		closed sql.NullString
	var labels, attrs string
	err := m.db.QueryRow(`SELECT w.id, w.seq, w.project_id, p.name, w.goal, w.content, w.parent_id,
		w.status, w.priority, w.milestone, w.labels, w.attrs, w.reporter, w.assignee, w.waiting_on,
		w.bundle_name, w.bundle_digest, w.workflow_budget_baseline, w.created_at, w.updated_at, w.closed_at
		FROM work_items w JOIN projects p ON p.id=w.project_id WHERE w.id=?1`, id).
		Scan(&wi.ID, &wi.Seq, &wi.ProjectID, &wi.ProjectName, &wi.Goal, &content, &parent,
			&wi.Status, &wi.Priority, &milestone, &labels, &attrs, &wi.Reporter, &assignee, &waitingOn,
			&bundle, &bundleDigest, &budget, &wi.CreatedAt, &wi.UpdatedAt, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "work item %s", id)
	}
	if err != nil {
		return nil, err
	}
	wi.Slug = wi.ProjectName + "#" + strconv.FormatInt(wi.Seq, 10)
	if content.Valid {
		c := content.String
		wi.Content = &c
	}
	if parent.Valid {
		p := parent.String
		wi.ParentID = &p
	}
	if milestone.Valid {
		mi := milestone.String
		wi.Milestone = &mi
	}
	if assignee.Valid {
		a := assignee.String
		wi.Assignee = &a
	}
	if waitingOn.Valid {
		w := waitingOn.String
		wi.WaitingOn = &w
	}
	if bundle.Valid {
		b := bundle.String
		wi.BundleName = &b
	}
	if bundleDigest.Valid {
		b := bundleDigest.String
		wi.BundleDigest = &b
	}
	if budget.Valid && budget.String != "" {
		wi.BudgetBaseline = json.RawMessage(budget.String)
	}
	if closed.Valid {
		c := closed.String
		wi.ClosedAt = &c
	}
	_ = json.Unmarshal([]byte(labels), &wi.Labels)
	if wi.Labels == nil {
		wi.Labels = []string{}
	}
	_ = json.Unmarshal([]byte(attrs), &wi.Attrs)
	if wi.Attrs == nil {
		wi.Attrs = map[string]any{}
	}
	return &wi, nil
}

// Event is one wi_events row.
type Event struct {
	ID        int64           `json:"id"`
	CreatedAt string          `json:"created_at"`
	Actor     string          `json:"actor,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func (m *WIManager) Events(wiID string) ([]Event, error) {
	rows, err := m.db.Query(`SELECT id, created_at, COALESCE(actor,''), event_type, payload
		FROM wi_events WHERE work_item_id=?1 ORDER BY id`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Actor, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func appendEventTx(tx *sql.Tx, wiID, at, actor, eventType string, payload map[string]any) error {
	var pb []byte
	if payload == nil {
		pb = []byte("{}")
	} else {
		pb, _ = json.Marshal(payload)
	}
	var actorAny any
	if actor != "" {
		actorAny = actor
	}
	_, err := tx.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1,?2,?3,?4,?5)`, wiID, at, actorAny, eventType, string(pb))
	return err
}

// Patchable fields (02 §5.1 row 3). Workflow refs and parent are immutable.
var patchableFields = map[string]bool{
	"goal": true, "content": true, "labels": true, "priority": true,
	"milestone": true, "waiting_on": true,
}

// Patch updates the mutable field set. fields maps field name → raw JSON.
func (m *WIManager) Patch(idOrSlug string, fields map[string]json.RawMessage) (*api.WorkItem, error) {
	id, err := m.Resolve(idOrSlug)
	if err != nil {
		return nil, err
	}
	for k := range fields {
		if !patchableFields[k] {
			return nil, Fail(api.ErrCoreBadInput, "IMMUTABLE", "field %q is not patchable", k)
		}
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "work item %s", id)
		}
		return nil, err
	}
	if isTerminal(status) {
		return nil, Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", status)
	}

	sets := []string{"updated_at=?"}
	args := []any{Now()}
	for _, field := range []string{"goal", "content", "labels", "priority", "milestone", "waiting_on"} {
		raw, ok := fields[field]
		if !ok {
			continue
		}
		switch field {
		case "goal":
			var v string
			if json.Unmarshal(raw, &v) != nil || strings.TrimSpace(v) == "" || len(v) > 500 {
				return nil, Fail(api.ErrCoreBadInput, "", "bad goal")
			}
			sets, args = append(sets, "goal=?"), append(args, v)
		case "content":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				return nil, Fail(api.ErrCoreBadInput, "", "bad content")
			}
			sets, args = append(sets, "content=?"), append(args, nullableString(v))
		case "labels":
			var v []string
			if json.Unmarshal(raw, &v) != nil || len(v) > 20 {
				return nil, Fail(api.ErrCoreBadInput, "", "bad labels")
			}
			if v == nil {
				v = []string{}
			}
			b, _ := json.Marshal(v)
			sets, args = append(sets, "labels=?"), append(args, string(b))
		case "priority":
			var v string
			if json.Unmarshal(raw, &v) != nil || !validPriority(v) {
				return nil, Fail(api.ErrCoreBadInput, "", "bad priority")
			}
			sets, args = append(sets, "priority=?"), append(args, v)
		case "milestone":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				return nil, Fail(api.ErrCoreBadInput, "", "bad milestone")
			}
			sets, args = append(sets, "milestone=?"), append(args, nullableString(v))
		case "waiting_on":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				return nil, Fail(api.ErrCoreBadInput, "", "bad waiting_on")
			}
			if v != nil && !validWaitingOn(*v) {
				return nil, Fail(api.ErrCoreBadInput, "", "waiting_on outside value domain")
			}
			sets, args = append(sets, "waiting_on=?"), append(args, nullableString(v))
		}
	}
	args = append(args, id)
	res, err := tx.Exec(`UPDATE work_items SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreNotFound, "", "work item %s", id)
	}
	if err := appendEventTx(tx, id, Now(), "", "wi_content_updated", nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.load(id)
}

func validWaitingOn(wo string) bool {
	switch {
	case wo == "" || wo == "owner" || wo == "gate":
		return true
	case strings.HasPrefix(wo, "runtime:") && len(wo) > len("runtime:"):
		return true
	case strings.HasPrefix(wo, "dependency:") && len(wo) > len("dependency:"):
		return true
	}
	return false
}

// Cancel terminates a non-active WI (queued/paused/blocked); a running WI
// must first end its generation.
func (m *WIManager) Cancel(idOrSlug string, actor string) (*api.WorkItem, error) {
	id, err := m.Resolve(idOrSlug)
	if err != nil {
		return nil, err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "work item %s", id)
		}
		return nil, err
	}
	if isTerminal(status) {
		return nil, Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is already terminal (%s)", status)
	}
	if status == api.StatusRunning {
		return nil, Fail(api.ErrCoreConflict, "", "running work item: end its generation (pause/block/takeover) before cancelling")
	}
	now := Now()
	// Guarded: only a cancelable row flips; 0 rows = concurrent transition.
	res, err := tx.Exec(`UPDATE work_items SET status=?, closed_at=?, updated_at=?,
		waiting_on=NULL WHERE id=? AND status IN (?,?,?)`,
		api.StatusCanceled, now, now, id, api.StatusQueued, api.StatusPaused, api.StatusBlocked)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreConflict, "", "state changed concurrently")
	}
	if err := appendEventTx(tx, id, now, actor, "work_item_canceled", nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.load(id)
}

// ListFilters is the list query.
type ListFilters struct {
	Project   string // project name or id; "" = all visible
	Status    string
	Assignee  string
	Priority  string
	Milestone string
}

// List returns work items matching the filters, newest first.
func (m *WIManager) List(f ListFilters) ([]api.WorkItem, error) {
	q := `SELECT w.id FROM work_items w JOIN projects p ON p.id=w.project_id WHERE 1=1`
	var args []any
	if f.Project != "" {
		q += ` AND (p.name=? OR p.id=?)`
		args = append(args, f.Project, f.Project)
	}
	if f.Status != "" {
		q += ` AND w.status=?`
		args = append(args, f.Status)
	}
	if f.Assignee != "" {
		q += ` AND w.assignee=?`
		args = append(args, f.Assignee)
	}
	if f.Priority != "" {
		q += ` AND w.priority=?`
		args = append(args, f.Priority)
	}
	if f.Milestone != "" {
		q += ` AND w.milestone=?`
		args = append(args, f.Milestone)
	}
	q += ` ORDER BY w.created_at DESC LIMIT 500`
	rows, err := m.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]api.WorkItem, 0, len(ids))
	for _, id := range ids {
		wi, err := m.load(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *wi)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Dependencies

// Deps is the two-sided dependency view.
type Deps struct {
	Blocking  []Edge `json:"blocking"`   // this WI blocks these
	BlockedBy []Edge `json:"blocked_by"` // these block this WI
}

type Edge struct {
	WI   string `json:"wi_id"`
	Kind string `json:"kind"`
}

func (m *WIManager) Dependencies(wiID string) (*Deps, error) {
	out := &Deps{Blocking: []Edge{}, BlockedBy: []Edge{}}
	rows, err := m.db.Query(`SELECT blocking_wi_id, blocked_wi_id, kind FROM wi_dependencies
		WHERE blocking_wi_id=?1 OR blocked_wi_id=?1`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var blocking, blocked, kind string
		if err := rows.Scan(&blocking, &blocked, &kind); err != nil {
			return nil, err
		}
		if blocking == wiID {
			out.Blocking = append(out.Blocking, Edge{WI: blocked, Kind: kind})
		} else {
			out.BlockedBy = append(out.BlockedBy, Edge{WI: blocking, Kind: kind})
		}
	}
	return out, rows.Err()
}

// AddDependency creates a dependency edge with the v0 write-path rules:
// same-project hard boundary, no self edge, cycle detection for
// blocks/supersedes.
func (m *WIManager) AddDependency(blocking, blocked, kind string) error {
	if kind == "" {
		kind = "blocks"
	}
	switch kind {
	case "blocks", "supersedes", "related":
	default:
		return Fail(api.ErrCoreBadInput, "", "bad kind %q", kind)
	}
	if blocking == blocked {
		return Fail(api.ErrCoreBadInput, "", "a work item cannot depend on itself")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projBlocking, projBlocked string
	err = tx.QueryRow(`SELECT project_id FROM work_items WHERE id=?1`, blocking).Scan(&projBlocking)
	if errors.Is(err, sql.ErrNoRows) {
		return Fail(api.ErrCoreNotFound, "", "work item %s", blocking)
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(`SELECT project_id FROM work_items WHERE id=?1`, blocked).Scan(&projBlocked)
	if errors.Is(err, sql.ErrNoRows) {
		return Fail(api.ErrCoreNotFound, "", "work item %s", blocked)
	}
	if err != nil {
		return err
	}
	if projBlocking != projBlocked {
		return Fail(api.ErrCoreBadInput, "", "dependency edges cannot cross projects")
	}
	if kind != "related" {
		reaches, err := reachesTx(tx, blocked, blocking, kind)
		if err != nil {
			return err
		}
		if reaches {
			return Fail(api.ErrCoreConflict, "DEPENDENCY_CYCLE", "edge would close a dependency cycle")
		}
	}
	if _, err := tx.Exec(`INSERT INTO wi_dependencies(blocking_wi_id, blocked_wi_id, kind, created_at)
		VALUES (?1,?2,?3,?4)`, blocking, blocked, kind, Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveDependency deletes an edge (physical delete).
func (m *WIManager) RemoveDependency(blocking, blocked, kind string) error {
	res, err := m.db.Exec(`DELETE FROM wi_dependencies WHERE blocking_wi_id=?1 AND blocked_wi_id=?2 AND kind=?3`,
		blocking, blocked, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Fail(api.ErrCoreNotFound, "", "no such dependency edge")
	}
	return nil
}

// reachesTx reports whether src can reach dst over `blocks` edges.
func reachesTx(tx *sql.Tx, src, dst, kind string) (bool, error) {
	seen := map[string]bool{}
	queue := []string{src}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == dst {
			return true, nil
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		rows, err := tx.Query(`SELECT blocked_wi_id FROM wi_dependencies WHERE blocking_wi_id=?1 AND kind=?2`,
			cur, kind)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var next string
			if err := rows.Scan(&next); err != nil {
				rows.Close()
				return false, err
			}
			queue = append(queue, next)
		}
		rows.Close()
	}
	return false, nil
}
