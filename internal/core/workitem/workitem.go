// Package workitem implements the WI state machine, creation transaction
// (with workflow frontmatter validation + digest pinning + repo inventory
// freeze), queries (list/detail/include/ready-queue), dependency edges
// (§4.2a domain logic), budget increments (CAS) and cancel. Design authority:
// docs/design/polyforge-vnext/02-aihub-core.md §2.4/§2.5/§4.2/§4.2a/§4.4–§4.7/§5.1b/§5.1c/§6.5/§6.7.
package workitem

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/silgrid/polyforge/internal/core/store"
	wfdigest "github.com/silgrid/polyforge/internal/workflow/digest"
	wfschema "github.com/silgrid/polyforge/internal/workflow/schema"
)

// APIError mirrors owner.APIError (kept local to avoid an owner dependency).
type APIError struct {
	Code   string
	Reason string
	Detail string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func errf(code, reason, format string, args ...any) *APIError {
	return &APIError{Code: code, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// WorkItem is the row shape returned to the HTTP face.
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

// RepoEntry is one wi_repo_inventory row (with frozen base).
type RepoEntry struct {
	Position int64   `json:"position"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Subdir   *string `json:"subdir"`
	Base     string  `json:"base"`
}

// Manager is the work-item domain manager.
type Manager struct {
	db           *store.DB
	workflowsDir string
}

func NewManager(db *store.DB, workflowsDir string) *Manager {
	return &Manager{db: db, workflowsDir: workflowsDir}
}

// ---------------------------------------------------------------------------
// Creation (02 §4.5/§4.6, transactional)

// CreateRequest is the POST /api/v1/work-items body.
type CreateRequest struct {
	Project           string         `json:"project"`
	Goal              string         `json:"goal"`
	Content           *string        `json:"content"`
	ParentID          *string        `json:"parent_id"`
	Priority          string         `json:"priority"`
	Milestone         *string        `json:"milestone"`
	Labels            []string       `json:"labels"`
	Attrs             map[string]any `json:"attrs"`
	Reporter          string         `json:"-"`
	BundleName        string         `json:"bundle_name"`
	RepoInventory     []RepoDecl     `json:"repo_inventory"`
	RepoInventoryBase []RepoBaseDecl `json:"repo_inventory_base"`
}

// RepoDecl is the declared (name,url,subdir) inventory entry.
type RepoDecl struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Subdir *string `json:"subdir"`
}

// RepoBaseDecl pairs a repo name with its frozen base commit.
type RepoBaseDecl struct {
	Name string `json:"name"`
	Base string `json:"base"`
}

// Create runs the full creation transaction (02 §4.5):
// registration vs execution ticket split, parent constraints, frontmatter
// validation + digest pinning (execution tickets only), inventory freeze,
// all in one SQLite transaction.
func (m *Manager) Create(req CreateRequest) (*Detail, error) {
	if strings.TrimSpace(req.Goal) == "" {
		return nil, errf("CORE-400", "", "goal required")
	}
	// R2C-F11: 02 §4.2 pins goal "≤500 字符" — CHARACTERS, not UTF-8 bytes.
	// Counting bytes (len(string)) rejected multilingual goals early (200
	// CJK chars = 600 bytes) even though the doc's own limit is 500
	// characters. Rune-counting applies the documented limit consistently
	// to every script; the PATCH path below shares the same rule.
	if utf8.RuneCountInString(req.Goal) > 500 {
		return nil, errf("CORE-400", "", "goal exceeds 500 characters")
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}
	switch req.Priority {
	case "low", "normal", "high", "urgent":
	default:
		return nil, errf("CORE-400", "", "bad priority %q", req.Priority)
	}
	if len(req.Labels) > 20 {
		return nil, errf("CORE-400", "", "labels exceed 20 entries")
	}
	if err := ValidateAttrs(req.Attrs); err != nil {
		return nil, errAsAPI(err)
	}

	// Project resolution.
	var projectID string
	if err := m.db.QueryRow(`SELECT id FROM projects WHERE name=?1`, req.Project).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("CORE-404", "", "project %q", req.Project)
		}
		return nil, err
	}

	// Parent (epic-lite) constraints — all write-path enforced (02 §4.2a).
	var parentBundle sql.NullString
	var parentParent sql.NullString
	var parentProject string
	if req.ParentID != nil && *req.ParentID != "" {
		err := m.db.QueryRow(`SELECT bundle_name, parent_id, project_id FROM work_items WHERE id=?1`, *req.ParentID).
			Scan(&parentBundle, &parentParent, &parentProject)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("CORE-404", "", "parent %s", *req.ParentID)
		}
		if err != nil {
			return nil, err
		}
		if parentParent.Valid && parentParent.String != "" {
			return nil, errf("CORE-400", "PARENT_DEPTH", "epic depth is limited to 1 level")
		}
		if parentBundle.Valid && parentBundle.String != "" {
			return nil, errf("CORE-400", "PARENT_NOT_REGISTRY", "parent must be a registration-only ticket")
		}
		if parentProject != projectID {
			return nil, errf("CORE-400", "PARENT_PROJECT_MISMATCH", "parent belongs to a different project")
		}
	}

	// Inventory validation (02 §4.6): 1..8 entries, unique names, base set
	// matching in count and names.
	baseByName := map[string]string{}
	for _, b := range req.RepoInventoryBase {
		if _, dup := baseByName[b.Name]; dup {
			return nil, errf("CORE-400", "INVENTORY_MISMATCH", "duplicate base entry for %q", b.Name)
		}
		baseByName[b.Name] = b.Base
	}
	seenNames := map[string]bool{}
	for _, r := range req.RepoInventory {
		if seenNames[r.Name] {
			return nil, errf("CORE-400", "INVENTORY_MISMATCH", "duplicate inventory name %q", r.Name)
		}
		seenNames[r.Name] = true
	}
	if len(req.RepoInventory) > 8 {
		return nil, errf("CORE-400", "", "repo_inventory allows 1..8 entries")
	}
	if len(baseByName) != len(req.RepoInventory) {
		return nil, errf("CORE-400", "INVENTORY_MISMATCH", "repo_inventory_base count differs from repo_inventory")
	}
	for _, r := range req.RepoInventory {
		base, ok := baseByName[r.Name]
		if !ok {
			return nil, errf("CORE-400", "INVENTORY_MISMATCH", "repo_inventory_base missing %q", r.Name)
		}
		if base == "" {
			return nil, errf("CORE-400", "INVENTORY_BASE_MISSING", "base missing for %q", r.Name)
		}
	}

	// Execution ticket: workflow frontmatter validation + digest pinning
	// (server-internal step, 02 §4.5); registration ticket skips it.
	var bundleDigest, budgetBaseline string
	fm := (*wfschema.Frontmatter)(nil)
	if req.BundleName != "" {
		bundleDir := filepath.Join(m.workflowsDir, req.BundleName)
		var err error
		fm, err = wfschema.ValidateWorkflowFrontmatter(bundleDir, req.BundleName)
		if err != nil {
			return nil, errAsAPI(err)
		}
		if err := wfschema.CheckAgentsExist(bundleDir, fm); err != nil {
			return nil, errAsAPI(err)
		}
		d, err := wfdigest.ComputeBundleDigest(bundleDir)
		if err != nil {
			return nil, errAsAPI(err)
		}
		bundleDigest = d
		bl, err := budgetBaselineJSON(fm)
		if err != nil {
			return nil, errAsAPI(err)
		}
		budgetBaseline = bl
		m.warnDrift(req.BundleName, d)
	} else if len(req.RepoInventory) == 0 && req.ParentID == nil {
		// plain registration ticket
	} else if len(req.RepoInventory) > 0 {
		// inventory on a registration ticket is legal (repo-backed registry)
	}

	// One transaction: seq allocation, row insert, inventory freeze.
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM work_items`).Scan(&maxSeq); err != nil {
		return nil, err
	}
	id, err := store.NewID("wi_")
	if err != nil {
		return nil, err
	}
	now := store.Now()
	labels, _ := json.Marshal(req.Labels)
	if req.Labels == nil {
		labels = []byte("[]")
	}
	attrs, err := json.Marshal(req.Attrs)
	if err != nil {
		return nil, errf("CORE-400", "", "attrs not JSON-serializable: %v", err)
	}
	if req.Attrs == nil {
		attrs = []byte("{}")
	}
	var bundleAny, digestAny, baselineAny any
	if req.BundleName != "" {
		bundleAny, digestAny, baselineAny = req.BundleName, bundleDigest, budgetBaseline
	}
	var parentAny any
	if req.ParentID != nil && *req.ParentID != "" {
		parentAny = *req.ParentID
	}
	var contentAny any
	if req.Content != nil {
		contentAny = *req.Content
	}
	var milestoneAny any
	if req.Milestone != nil {
		milestoneAny = *req.Milestone
	}
	if _, err := tx.Exec(`INSERT INTO work_items
		(id, seq, project_id, goal, content, parent_id, status, priority, milestone,
		 labels, attrs, reporter, bundle_name, bundle_digest, workflow_budget_baseline,
		 created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,'queued',?7,?8,?9,?10,?11,?12,?13,?14,?15,?15)`,
		id, maxSeq.Int64+1, projectID, req.Goal, contentAny, parentAny, req.Priority,
		milestoneAny, string(labels), string(attrs), req.Reporter,
		bundleAny, digestAny, baselineAny, now); err != nil {
		return nil, err
	}
	for i, r := range req.RepoInventory {
		var subdirAny any
		if r.Subdir != nil {
			subdirAny = *r.Subdir
		}
		if _, err := tx.Exec(`INSERT INTO wi_repo_inventory(work_item_id, position, name, url, subdir, base)
			VALUES (?1,?2,?3,?4,?5,?6)`, id, i+1, r.Name, r.URL, subdirAny, baseByName[r.Name]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.Get(id, GetOptions{})
}

// budgetBaselineJSON transcribes the workflow-level budget into the
// three-scalar baseline stored on the row (02 §4.4:
// {"requests","tokens","wall_minutes"}).
func budgetBaselineJSON(fm *wfschema.Frontmatter) (string, error) {
	bl := map[string]any{"requests": nil, "tokens": nil, "wall_minutes": nil}
	if fm.Budget != nil {
		if fm.Budget.Requests != nil {
			bl["requests"] = *fm.Budget.Requests
		}
		if fm.Budget.Tokens != nil {
			bl["tokens"] = *fm.Budget.Tokens
		}
		if fm.Budget.WallMinutes != nil {
			bl["wall_minutes"] = *fm.Budget.WallMinutes
		}
	}
	b, err := json.Marshal(bl)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// warnDrift reports a bundle digest drift: same bundle_name referenced by
// non-terminal WIs with a different digest (02 §4.4 drift alert — monitoring
// signal only; nothing already stored is rewritten, no new event type).
func (m *Manager) warnDrift(bundleName, digest string) {
	rows, err := m.db.Query(`SELECT DISTINCT bundle_digest FROM work_items
		WHERE bundle_name=?1 AND bundle_digest IS NOT NULL AND bundle_digest <> ?2
		AND status NOT IN ('wrapped','failed','cancelled')`, bundleName, digest)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var old string
		if err := rows.Scan(&old); err == nil {
			fmt.Fprintf(os.Stderr, "aihub: BUNDLE_DRIFT bundle=%s current=%s referenced=%s\n", bundleName, digest, old)
		}
	}
}

// errAsAPI converts workflow schema rule errors to CORE-400 with rule detail.
func errAsAPI(err error) *APIError {
	var re *wfschema.RuleError
	if errors.As(err, &re) {
		return &APIError{Code: "CORE-400", Reason: "FRONTMATTER_INVALID", Detail: re.Msg + " (rule_id=" + re.RuleID + ")"}
	}
	if ae, ok := err.(*APIError); ok {
		return ae
	}
	if msg := err.Error(); strings.HasPrefix(msg, "CORE-") {
		// Error strings produced in this package carry their code inline
		// (e.g. the INV-2c guard); recover the code token.
		rest := strings.TrimPrefix(msg, "CORE-")
		code := "CORE-" + rest[:strings.IndexAny(rest, " :")]
		return &APIError{Code: code, Detail: msg}
	}
	return &APIError{Code: "CORE-500", Detail: err.Error()}
}

// ---------------------------------------------------------------------------
// Read paths

// GetOptions selects the include= additions (02 §5.1c).
type GetOptions struct {
	Events   bool
	Deps     bool
	Children bool
}

// Detail is the full GET response (base record + repo_inventory + includes).
type Detail struct {
	*WorkItem
	RepoInventory []RepoEntry     `json:"repo_inventory"`
	Events        []Event         `json:"events,omitempty"`
	Deps          *Deps           `json:"deps,omitempty"`
	Children      *ChildrenRollup `json:"children,omitempty"`
}

// Event is one wi_events row.
type Event struct {
	ID        int64           `json:"id"`
	CreatedAt string          `json:"created_at"`
	Actor     *string         `json:"actor"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

// dbHandle is satisfied by both *store.DB (which embeds *sql.DB) and
// *sql.Tx; the read/edge helpers below take it so the same code runs either
// directly (list/queue paths) or inside a caller-held transaction. With the
// store's single connection (INV-2a) a helper must NEVER touch m.db while a
// transaction is open - that would ask the pool for a second connection that
// never comes (self-deadlock).
type dbHandle interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// Deps is the two-list dependency include.
type Deps struct {
	Blocking  []Edge `json:"blocking"`
	BlockedBy []Edge `json:"blocked_by"`
}

// Edge is one dependency edge entry.
type Edge struct {
	WiID string `json:"wi_id"`
	Kind string `json:"kind"`
}

// ChildrenRollup is the children include + derived rollup (query-time, never stored).
type ChildrenRollup struct {
	Children []WorkItem `json:"children"`
	Rollup   Rollup     `json:"rollup"`
}

// Rollup counts direct children by status bucket (02 §5.1c).
type Rollup struct {
	Total   int `json:"total"`
	Wrapped int `json:"wrapped"`
	Running int `json:"running"`
	Blocked int `json:"blocked"`
	Queued  int `json:"queued"`
}

func (m *Manager) Get(wiID string, opts GetOptions) (*Detail, error) {
	// FA-8 (02 §5.1c): the include assembly and the base-record read run in
	// ONE read transaction so the assembled detail is a single consistent
	// snapshot - base row, inventory, events, deps and children cannot tear
	// against a concurrent write. Read-only: the deferred rollback merely
	// releases the snapshot.
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return m.getTx(tx, wiID, opts)
}

func (m *Manager) getTx(q dbHandle, wiID string, opts GetOptions) (*Detail, error) {
	row, err := m.loadWI(q, wiID)
	if err != nil {
		return nil, err
	}
	d := &Detail{WorkItem: row}
	d.RepoInventory, err = m.inventory(q, wiID)
	if err != nil {
		return nil, err
	}
	if opts.Events {
		ev, err := m.events(q, wiID)
		if err != nil {
			return nil, err
		}
		d.Events = ev
	}
	if opts.Deps {
		dep, err := m.deps(q, wiID)
		if err != nil {
			return nil, err
		}
		d.Deps = dep
	}
	if opts.Children {
		kids, err := m.children(q, wiID)
		if err != nil {
			return nil, err
		}
		d.Children = kids
	}
	return d, nil
}

func (m *Manager) loadWI(q dbHandle, wiID string) (*WorkItem, error) {
	var w WorkItem
	var content, parentID, milestone, assignee, waitingOn, bundleName, bundleDigest, baseline, closedAt sql.NullString
	var labels, attrs string
	err := q.QueryRow(`SELECT w.id, w.seq, w.project_id, p.name, w.goal, w.content, w.parent_id,
		w.status, w.priority, w.milestone, w.labels, w.attrs, w.reporter, w.assignee, w.waiting_on,
		w.bundle_name, w.bundle_digest, w.workflow_budget_baseline, w.created_at, w.updated_at, w.closed_at
		FROM work_items w JOIN projects p ON p.id=w.project_id WHERE w.id=?1`, wiID).
		Scan(&w.ID, &w.Seq, &w.ProjectID, &w.ProjectName, &w.Goal, &content, &parentID,
			&w.Status, &w.Priority, &milestone, &labels, &attrs, &w.Reporter, &assignee, &waitingOn,
			&bundleName, &bundleDigest, &baseline, &w.CreatedAt, &w.UpdatedAt, &closedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("CORE-404", "", "work item %s", wiID)
	}
	if err != nil {
		return nil, err
	}
	w.Slug = fmt.Sprintf("%s#%d", w.ProjectName, w.Seq)
	nullable(&content, &w.Content)
	nullable(&parentID, &w.ParentID)
	nullable(&milestone, &w.Milestone)
	nullable(&assignee, &w.Assignee)
	// R2C-F10 read-path defense: 02 §2.4's empty waiting_on value is
	// stored as NULL; a legacy '' row (written before the PATCH write-path
	// normalization) reads as no-waiting here so the ready-queue's
	// “waiting_on non-NULL = blocked signal” test (§5.1b) cannot mis-sort a
	// queued executable ticket on old data.
	if waitingOn.Valid && waitingOn.String != "" {
		w.WaitingOn = &waitingOn.String
	}
	nullable(&bundleName, &w.BundleName)
	nullable(&bundleDigest, &w.BundleDigest)
	if baseline.Valid && baseline.String != "" {
		w.BudgetBaseline = json.RawMessage(baseline.String)
	} else {
		w.BudgetBaseline = json.RawMessage("null")
	}
	nullable(&closedAt, &w.ClosedAt)
	_ = json.Unmarshal([]byte(labels), &w.Labels)
	if w.Labels == nil {
		w.Labels = []string{}
	}
	_ = json.Unmarshal([]byte(attrs), &w.Attrs)
	if w.Attrs == nil {
		w.Attrs = map[string]any{}
	}
	return &w, nil
}

func nullable(src *sql.NullString, dst **string) {
	if src.Valid {
		*dst = &src.String
	}
}

// Inventory returns the frozen repo inventory (base included).
func (m *Manager) Inventory(wiID string) ([]RepoEntry, error) {
	return m.inventory(m.db, wiID)
}

func (m *Manager) inventory(q dbHandle, wiID string) ([]RepoEntry, error) {
	rows, err := q.Query(`SELECT position, name, url, subdir, base FROM wi_repo_inventory
		WHERE work_item_id=?1 ORDER BY position`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoEntry
	for rows.Next() {
		var e RepoEntry
		var subdir sql.NullString
		if err := rows.Scan(&e.Position, &e.Name, &e.URL, &subdir, &e.Base); err != nil {
			return nil, err
		}
		if subdir.Valid {
			e.Subdir = &subdir.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Events returns the WI event stream ordered by id.
func (m *Manager) Events(wiID string) ([]Event, error) {
	return m.events(m.db, wiID)
}

func (m *Manager) events(q dbHandle, wiID string) ([]Event, error) {
	rows, err := q.Query(`SELECT id, created_at, actor, event_type, payload FROM wi_events
		WHERE work_item_id=?1 ORDER BY id`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var actor sql.NullString
		// The driver hands TEXT back as string; *json.RawMessage is not a
		// scannable destination for it, so take the payload as []byte and
		// wrap. (First exercised by the FA-8 snapshot test - the
		// include=events path was previously untested at this layer.)
		var payload []byte
		if err := rows.Scan(&e.ID, &e.CreatedAt, &actor, &e.EventType, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		if actor.Valid {
			e.Actor = &actor.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Dependencies returns both edge lists (§5.1c include=deps).
func (m *Manager) Dependencies(wiID string) (*Deps, error) {
	return m.deps(m.db, wiID)
}

func (m *Manager) deps(q dbHandle, wiID string) (*Deps, error) {
	blocking, err := m.edges(q, `SELECT blocked_wi_id, kind FROM wi_dependencies WHERE blocking_wi_id=?1`, wiID)
	if err != nil {
		return nil, err
	}
	blockedBy, err := m.edges(q, `SELECT blocking_wi_id, kind FROM wi_dependencies WHERE blocked_wi_id=?1`, wiID)
	if err != nil {
		return nil, err
	}
	return &Deps{Blocking: blocking, BlockedBy: blockedBy}, nil
}

func (m *Manager) edges(q dbHandle, query string, args ...any) ([]Edge, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.WiID, &e.Kind); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (m *Manager) children(q dbHandle, wiID string) (*ChildrenRollup, error) {
	rows, err := q.Query(`SELECT id FROM work_items WHERE parent_id=?1 ORDER BY seq`, wiID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := &ChildrenRollup{Children: []WorkItem{}}
	for _, id := range ids {
		w, err := m.loadWI(q, id)
		if err != nil {
			return nil, err
		}
		out.Children = append(out.Children, *w)
		out.Rollup.Total++
		switch w.Status {
		case "wrapped":
			out.Rollup.Wrapped++
		case "running":
			out.Rollup.Running++
		case "blocked":
			out.Rollup.Blocked++
		case "queued":
			out.Rollup.Queued++
		}
	}
	return out, nil
}

// AppendEvent validates and appends one wi_events row (INV-2c double check).
func (m *Manager) AppendEvent(wiID, actor, eventType string, payload map[string]any) error {
	if !AllowedEventTypes(eventType) {
		return errf("CORE-400", "UNKNOWN_EVENT_TYPE", "%q", eventType)
	}
	if err := ValidateEvent(eventType, payload); err != nil {
		return errAsAPI(err)
	}
	var actorAny any
	if actor != "" {
		actorAny = actor
	}
	_, err := m.db.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1,?2,?3,?4,?5)`, wiID, store.Now(), actorAny, eventType, EncodePayload(payload))
	return err
}

// ---------------------------------------------------------------------------
// Patch / Cancel / List / ReadyQueue

// patchable is the closed PATCH field set (02 §5.1 row 3). Workflow three
// columns and parent_id are immutable (INV-2e / §4.2a); status/seq/reporter/
// project are not PATCHable either.
var patchable = map[string]bool{
	"goal": true, "content": true, "labels": true, "priority": true,
	"milestone": true, "waiting_on": true, "attrs": true,
}

// Patch applies a partial update. fields keys must ⊆ patchable; attrs passes
// the INV-2c blacklist.
//
// R3C-F3 (02 §2.5): the status read below and the field validations run on
// the shared connection BEFORE the applying transaction opens, so an
// end-generation(block) can commit `blocked` + waiting_on in that window —
// the unconditional UPDATE would then clear waiting_on on a blocked row,
// leaving a blocked WI with no waiting object (dissolving the §2.5
// invariant the R2C-F10 guard exists to protect). The status-dependent
// refusals are therefore re-evaluated INSIDE the transaction (patchTx),
// which holds the store's single connection (INV-2a) so nothing can
// interleave past that point, and the UPDATE itself is guarded for the
// clearing case.
func (m *Manager) Patch(wiID string, fields map[string]json.RawMessage) (*Detail, error) {
	var status string
	if err := m.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("CORE-404", "", "work item %s", wiID)
		}
		return nil, err
	}
	switch status {
	case "wrapped", "failed", "cancelled":
		return nil, errf("CORE-410", "", "terminal work item")
	}
	for k := range fields {
		if !patchable[k] {
			if k == "bundle_name" || k == "bundle_digest" || k == "workflow_budget_baseline" || k == "parent_id" {
				return nil, errf("CORE-400", "IMMUTABLE", "%s is immutable after creation (INV-2e)", k)
			}
			return nil, errf("CORE-400", "", "field %q is not patchable", k)
		}
	}
	sets := []string{}
	args := []any{}
	// clearing marks a patch that sets waiting_on to the NULL "no
	// waiting" value (02 §2.4) — the one field whose legality depends on
	// the live status (R2C-F10/R3C-F3).
	clearing := false
	add := func(col string, val any) {
		sets = append(sets, fmt.Sprintf("%s=?%d", col, len(args)+1))
		args = append(args, val)
	}
	if v, ok := fields["goal"]; ok {
		var goal string
		if err := json.Unmarshal(v, &goal); err != nil || goal == "" {
			return nil, errf("CORE-400", "", "goal must be a non-empty string")
		}
		// FA-9: creation enforces goal <= 500 chars (02 §4.2); PATCH carries
		// the same column limit so the closed invariant cannot be widened
		// through the update path. R2C-F11: "chars" means CHARACTERS per
		// 02 §4.2 (rune count), the same rule the creation path applies —
		// a byte-count here would make multilingual PATCH goals fail under
		// a limit their creation already passed.
		if utf8.RuneCountInString(goal) > 500 {
			return nil, errf("CORE-400", "", "goal exceeds 500 characters")
		}
		add("goal", goal)
	}
	if v, ok := fields["content"]; ok {
		var content *string
		if err := json.Unmarshal(v, &content); err != nil {
			return nil, errf("CORE-400", "", "content must be a string or null")
		}
		if content == nil {
			sets = append(sets, "content=NULL")
		} else {
			add("content", *content)
		}
	}
	if v, ok := fields["labels"]; ok {
		var labels []string
		if err := json.Unmarshal(v, &labels); err != nil {
			return nil, errf("CORE-400", "", "labels must be an array of strings")
		}
		if len(labels) > 20 {
			return nil, errf("CORE-400", "", "labels exceed 20 entries")
		}
		b, _ := json.Marshal(labels)
		add("labels", string(b))
	}
	if v, ok := fields["priority"]; ok {
		var p string
		if err := json.Unmarshal(v, &p); err != nil {
			return nil, errf("CORE-400", "", "priority must be a string")
		}
		switch p {
		case "low", "normal", "high", "urgent":
		default:
			return nil, errf("CORE-400", "", "bad priority %q", p)
		}
		add("priority", p)
	}
	if v, ok := fields["milestone"]; ok {
		var ms *string
		if err := json.Unmarshal(v, &ms); err != nil {
			return nil, errf("CORE-400", "", "milestone must be a string or null")
		}
		if ms == nil {
			sets = append(sets, "milestone=NULL")
		} else {
			add("milestone", *ms)
		}
	}
	if v, ok := fields["waiting_on"]; ok {
		var wo *string
		if err := json.Unmarshal(v, &wo); err != nil {
			return nil, errf("CORE-400", "", "waiting_on must be a string or null")
		}
		if wo == nil || *wo == "" {
			// R2C-F10: 02 §2.4 pins waiting_on's empty value as "no waiting"
			// and “waiting_on 非空是「被阻塞」的可见信号” — the stored form of
			// the empty value is NULL (the column's own nullable semantics),
			// so an explicit "" is normalized to NULL on write. Storing ''
			// non-NULL made a queued EXECUTION ticket read as waiting and land
			// in the ready-queue's blocked list (§5.1b) even though nothing
			// blocks it.
			//
			// 02 §2.5 additionally defines `blocked` BY its non-empty
			// waiting_on (“blocked 是有明确等待对象（waiting_on 非空）”);
			// clearing waiting_on on a blocked WI would strand a blocked row
			// with no waiting object, dissolving the documented paused/blocked
			// distinction. The recovery path out of blocked is `unblock` (a
			// fresh claim, §2.5/§6.2), never a PATCH — so the clear is refused;
			// re-pointing the waiting object to another non-empty domain value
			// stays legal.
			if status == "blocked" {
				// Precheck only — the authoritative re-evaluation runs inside
				// the applying transaction (R3C-F3), on a status a concurrent
				// end-generation(block) cannot slip past.
				return nil, errf("CORE-400", "BLOCKED_NEEDS_WAITING_ON",
					"waiting_on cannot be cleared while the work item is blocked (02 §2.5: blocked implies a non-empty waiting_on); unblock (a fresh claim) is the recovery path")
			}
			sets = append(sets, "waiting_on=NULL")
			clearing = true
		} else {
			// FA-7: 02 §2.4 pins a closed waiting_on value domain; a PATCH
			// must not widen it with arbitrary non-empty strings.
			if err := validateWaitingOn(*wo); err != nil {
				return nil, err
			}
			add("waiting_on", *wo)
		}
	}
	if v, ok := fields["attrs"]; ok {
		var attrs map[string]any
		if err := json.Unmarshal(v, &attrs); err != nil {
			return nil, errf("CORE-400", "", "attrs must be an object")
		}
		if err := ValidateAttrs(attrs); err != nil {
			return nil, errAsAPI(err)
		}
		b, _ := json.Marshal(attrs)
		add("attrs", string(b))
	}
	if len(sets) == 0 {
		return m.Get(wiID, GetOptions{})
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := m.patchTx(tx, wiID, sets, args, clearing); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.Get(wiID, GetOptions{})
}

// patchTx applies the validated SET list inside the caller's open
// transaction (the transactional half, mirroring cancelTx/claimNew so the
// R3C-F3 interleave is white-box testable). sets/args carry the field
// assignments WITHOUT the updated_at/id tail; clearing marks a patch that
// sets waiting_on to the NULL "no waiting" value.
//
// The status is re-read on the transaction's own snapshot and the two
// status-dependent refusals — terminal WI, and clearing waiting_on on a
// blocked WI (R2C-F10, 02 §2.5) — are re-evaluated there. The UPDATE is
// additionally guarded for the clearing case (WHERE ... AND
// (status<>'blocked' OR waiting_on IS NULL)) so the §2.5 invariant holds
// by the write itself, not only by the read: 0 rows affected is refused
// with nothing written.
func (m *Manager) patchTx(tx *sql.Tx, wiID string, sets []string, args []any, clearing bool) error {
	var status string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errf("CORE-404", "", "work item %s", wiID)
		}
		return err
	}
	switch status {
	case "wrapped", "failed", "cancelled":
		// A terminalization (cancel/end-generation wrap|fail) won the race
		// the outer read lost: the patch must not touch a terminal row.
		return errf("CORE-410", "", "terminal work item")
	}
	if clearing && status == "blocked" {
		return errf("CORE-400", "BLOCKED_NEEDS_WAITING_ON",
			"waiting_on cannot be cleared while the work item is blocked (02 §2.5: blocked implies a non-empty waiting_on); unblock (a fresh claim) is the recovery path")
	}
	args = append(args, store.Now())
	sets = append(sets, fmt.Sprintf("updated_at=?%d", len(args)))
	args = append(args, wiID)
	query := `UPDATE work_items SET ` + strings.Join(sets, ", ") + fmt.Sprintf(` WHERE id=?%d`, len(args))
	if clearing {
		// The invariant guard rides the write: a blocked row with a waiting
		// object can never be flipped to a cleared waiting_on, even if some
		// future store shape ever lets a write land inside this transaction.
		query += ` AND (status<>'blocked' OR waiting_on IS NULL)`
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if clearing {
		if n, _ := res.RowsAffected(); n == 0 {
			return errf("CORE-400", "BLOCKED_NEEDS_WAITING_ON",
				"waiting_on cannot be cleared while the work item is blocked (02 §2.5: blocked implies a non-empty waiting_on); unblock (a fresh claim) is the recovery path")
		}
	}
	return nil
}

// Cancel terminates a non-active WI (02 §6.5). Authorization is resolved by
// the caller (reporter / project maintainer / admin).
func (m *Manager) Cancel(wiID, actor string) (*Detail, error) {
	var status string
	if err := m.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("CORE-404", "", "work item %s", wiID)
		}
		return nil, err
	}
	switch status {
	case "queued", "paused", "blocked":
	case "wrapped", "failed", "cancelled":
		// fix #15: 02 §5.1 row 5 pins CORE-410 for an already-terminal WI,
		// distinct from CORE-409 for a status that is simply not one of the
		// three cancellable ones (i.e. running).
		return nil, errf("CORE-410", "", "work item %s is already terminal (%s)", wiID, status)
	default:
		return nil, errf("CORE-409", "", "cancel requires a non-active status (queued/paused/blocked), got %s", status)
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := m.cancelTx(tx, wiID, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.Get(wiID, GetOptions{})
}

// cancelTx performs the cancellation writes inside the caller's open
// transaction. FA-6 (02 §2.5/§6.5): the outer status check runs on the
// shared connection BEFORE the transaction opens, so a claim can commit in
// between and the WI may already be running with an active generation by
// the time this transaction starts; cancelling it then would violate §2.3
// ("cancel never ends an active generation row") and orphan the live
// generation. The status is therefore re-checked IN the transaction (which
// holds the single connection, so nothing can interleave past this point)
// and the UPDATE is guarded: 0 rows affected means the race was lost and
// the typed CORE-409/CORE-410 is returned with nothing written.
func (m *Manager) cancelTx(tx *sql.Tx, wiID, actor string) error {
	var status string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errf("CORE-404", "", "work item %s", wiID)
		}
		return err
	}
	switch status {
	case "queued", "paused", "blocked":
	case "wrapped", "failed", "cancelled":
		return errf("CORE-410", "", "work item %s is already terminal (%s)", wiID, status)
	default:
		return errf("CORE-409", "", "cancel requires a non-active status (queued/paused/blocked), got %s", status)
	}
	// Defense in depth: none of the three cancellable statuses may carry an
	// active generation (02 §2.3); refuse rather than cancel around one.
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wiID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return errf("CORE-409", "", "work item %s has an active generation; end it (pause/block) before cancelling", wiID)
	}
	now := store.Now()
	res, err := tx.Exec(`UPDATE work_items SET status='cancelled', closed_at=?1, updated_at=?1
		WHERE id=?2 AND status IN ('queued','paused','blocked')`, now, wiID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errf("CORE-409", "", "work item %s is no longer cancellable (state changed concurrently)", wiID)
	}
	if _, err := tx.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1,?2,?3,'cancelled','{}')`, wiID, now, actor); err != nil {
		return err
	}
	return nil
}

// ListFilters are the GET /work-items query params (02 §5.1 row 2).
type ListFilters struct {
	Project   string
	Status    string
	Assignee  string
	WaitingOn string
	Parent    string
	Milestone string
}

// List returns WIs matching the filters (visibility pruning is the server's
// job, §4.10).
func (m *Manager) List(f ListFilters) ([]WorkItem, error) {
	q := `SELECT w.id FROM work_items w JOIN projects p ON p.id = w.project_id WHERE 1=1`
	args := []any{}
	if f.Project != "" {
		args = append(args, f.Project)
		q += fmt.Sprintf(` AND p.name=?%d`, len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		q += fmt.Sprintf(` AND w.status=?%d`, len(args))
	}
	if f.Assignee != "" {
		args = append(args, f.Assignee)
		q += fmt.Sprintf(` AND w.assignee=?%d`, len(args))
	}
	if f.WaitingOn != "" {
		args = append(args, f.WaitingOn)
		q += fmt.Sprintf(` AND w.waiting_on=?%d`, len(args))
	}
	if f.Parent != "" {
		args = append(args, f.Parent)
		q += fmt.Sprintf(` AND w.parent_id=?%d`, len(args))
	}
	if f.Milestone != "" {
		args = append(args, f.Milestone)
		q += fmt.Sprintf(` AND w.milestone=?%d`, len(args))
	}
	// fix #12: no LIMIT here — v1 has no pagination, and the ready-queue path
	// (which reuses List) must see every queued/blocked WI, not just the most
	// recent 500.
	q += ` ORDER BY w.created_at DESC, w.seq DESC`
	rows, err := m.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []WorkItem{}
	for _, id := range ids {
		w, err := m.loadWI(m.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, nil
}

// ReadyQueueEntry is one queue entry (02 §5.1b).
type ReadyQueueEntry struct {
	ID         string   `json:"id"`
	Slug       string   `json:"slug"`
	Goal       string   `json:"goal"`
	Priority   string   `json:"priority"`
	Status     string   `json:"status"`
	BundleName *string  `json:"bundle_name"`
	WaitingOn  *string  `json:"waiting_on,omitempty"`
	BlockedBy  []string `json:"blocked_by,omitempty"`
	CreatedAt  string   `json:"created_at"`
	Labels     []string `json:"labels"`
}

// ReadyQueue is the flat two-array response (02 §5.1b).
type ReadyQueue struct {
	Project    string            `json:"project"`
	Executable []ReadyQueueEntry `json:"executable"`
	Blocked    []ReadyQueueEntry `json:"blocked"`
}

var priorityRank = map[string]int{"urgent": 0, "high": 1, "normal": 2, "low": 3}

// ReadyQueue computes {executable, blocked} (02 §5.1b): executable excludes
// registration tickets, terminal/running/paused/blocked statuses and
// anything with an unfinished blocks edge; blocked collects blocked/paused
// plus dependency-waiting queued tickets; both arrays sorted priority→created.
func (m *Manager) ReadyQueue(projectName string) (*ReadyQueue, error) {
	var projectID string
	if err := m.db.QueryRow(`SELECT id FROM projects WHERE name=?1`, projectName).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("CORE-404", "", "project %q", projectName)
		}
		return nil, err
	}
	wis, err := m.List(ListFilters{Project: projectName})
	if err != nil {
		return nil, err
	}
	out := &ReadyQueue{Project: projectName, Executable: []ReadyQueueEntry{}, Blocked: []ReadyQueueEntry{}}
	for i := range wis {
		w := &wis[i]
		entry := ReadyQueueEntry{
			ID: w.ID, Slug: w.Slug, Goal: w.Goal, Priority: w.Priority,
			Status: w.Status, BundleName: w.BundleName, WaitingOn: w.WaitingOn,
			CreatedAt: w.CreatedAt, Labels: w.Labels,
		}
		unfinished, err := m.unfinishedBlockers(m.db, w.ID)
		if err != nil {
			return nil, err
		}
		entry.BlockedBy = unfinished
		switch {
		case w.Status == "queued" && w.BundleName != nil && *w.BundleName != "" &&
			w.WaitingOn == nil && len(unfinished) == 0:
			out.Executable = append(out.Executable, entry)
		case w.Status == "blocked" || w.Status == "paused":
			out.Blocked = append(out.Blocked, entry)
		case w.Status == "queued":
			// fix #12: a plain queued registration ticket (bundle NULL, no
			// waiting_on, no unfinished blocker) belongs in NEITHER array (02
			// §5.1b) — it is not executable content and nothing is blocking it,
			// it simply has no workflow to run. Anything actually waiting on
			// something (waiting_on set, or an unfinished blocker) still lands
			// in blocked, execution ticket or not.
			if w.WaitingOn != nil || len(unfinished) > 0 {
				out.Blocked = append(out.Blocked, entry)
			}
		}
	}
	sort.SliceStable(out.Executable, func(i, j int) bool {
		return queueLess(out.Executable[i], out.Executable[j])
	})
	sort.SliceStable(out.Blocked, func(i, j int) bool {
		return queueLess(out.Blocked[i], out.Blocked[j])
	})
	return out, nil
}

func queueLess(a, b ReadyQueueEntry) bool {
	ra, rb := priorityRank[a.Priority], priorityRank[b.Priority]
	if ra != rb {
		return ra < rb
	}
	return a.CreatedAt < b.CreatedAt
}

// unfinishedBlockers returns the ids of blocking WIs that are not terminal.
func (m *Manager) unfinishedBlockers(q dbHandle, wiID string) ([]string, error) {
	rows, err := q.Query(`SELECT d.blocking_wi_id FROM wi_dependencies d
		JOIN work_items b ON b.id = d.blocking_wi_id
		WHERE d.blocked_wi_id=?1 AND d.kind='blocks'
		AND b.status NOT IN ('wrapped','failed','cancelled')`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// validateWaitingOn enforces the closed waiting_on value domain of 02
// §2.4: "" | owner | runtime:<machine_id> | dependency:<wi_id> | gate.
// (Kept local: owner cannot import workitem, so the closed domain is
// spelled out in both packages, like hasUnfinishedBlockers there.)
func validateWaitingOn(wo string) error {
	switch {
	case wo == "" || wo == "owner" || wo == "gate":
		return nil
	case strings.HasPrefix(wo, "runtime:"):
		if strings.TrimPrefix(wo, "runtime:") == "" {
			return errf("CORE-400", "", "waiting_on runtime: requires a machine_id (02 §2.4)")
		}
		return nil
	case strings.HasPrefix(wo, "dependency:"):
		if strings.TrimPrefix(wo, "dependency:") == "" {
			return errf("CORE-400", "", "waiting_on dependency: requires a wi_id (02 §2.4)")
		}
		return nil
	}
	return errf("CORE-400", "", "waiting_on %q is outside the §2.4 value domain (owner|runtime:<machine_id>|dependency:<wi_id>|gate|empty)", wo)
}

// Dependency edges (02 §4.2a domain logic; endpoint surface pending — see
// work-mode note: the canonical 25-row inventory in §5.1 has no edge route).

// AddDependency enforces all §4.2a write-path constraints and the insert in
// ONE transaction. FA-5 (02 §4.2a: every write-path constraint is enforced
// inside the create/delete-edge transaction): the reachability check, the
// insert and the waiting_on materialization previously ran as independent
// statements on the shared connection, so two concurrent AddDependency
// calls could BOTH pass their cycle checks before either insert landed and
// then both commit - closing a cycle the check exists to refuse. With the
// check inside the transaction (which holds the store's single connection,
// INV-2a), a concurrent add cannot interleave between the check and the
// insert.
func (m *Manager) AddDependency(blocking, blocked, kind string) error {
	switch kind {
	case "blocks", "supersedes", "related":
	default:
		return errf("CORE-400", "", "kind must be blocks|supersedes|related")
	}
	if blocking == blocked {
		return errf("CORE-400", "", "edge endpoints must differ")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var bp, dp string
	if err := tx.QueryRow(`SELECT project_id FROM work_items WHERE id=?1`, blocking).Scan(&bp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errf("CORE-404", "", "work item %s", blocking)
		}
		return err
	}
	if err := tx.QueryRow(`SELECT project_id FROM work_items WHERE id=?1`, blocked).Scan(&dp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errf("CORE-404", "", "work item %s", blocked)
		}
		return err
	}
	if bp != dp {
		return errf("CORE-400", "", "dependency edges must stay within one project")
	}
	// Epic parent/child exclusion (any kind).
	var p1, p2 sql.NullString
	_ = tx.QueryRow(`SELECT parent_id FROM work_items WHERE id=?1`, blocking).Scan(&p1)
	_ = tx.QueryRow(`SELECT parent_id FROM work_items WHERE id=?1`, blocked).Scan(&p2)
	if (p1.Valid && p1.String == blocked) || (p2.Valid && p2.String == blocking) {
		return errf("CORE-400", "PARENT_CHILD_DEPENDENCY", "edges may not cross the epic parent/child relation")
	}
	if kind != "related" {
		cyclic, err := m.reaches(tx, blocked, blocking, kind)
		if err != nil {
			return err
		}
		if cyclic {
			return errf("CORE-409", "DEPENDENCY_CYCLE", "%s edge %s->%s would close a cycle", kind, blocked, blocking)
		}
	}
	if _, err := tx.Exec(`INSERT INTO wi_dependencies(blocking_wi_id, blocked_wi_id, kind, created_at)
		VALUES (?1,?2,?3,?4)`, blocking, blocked, kind, store.Now()); err != nil {
		return err
	}
	// blocks edges materialize the waiting_on signal (§2.4/§4.2a).
	if kind == "blocks" {
		if _, err := tx.Exec(`UPDATE work_items SET waiting_on=?1, updated_at=?2 WHERE id=?3 AND status='queued'`,
			"dependency:"+blocking, store.Now(), blocked); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveDependency physically deletes the edge (§4.2a) and re-derives
// waiting_on for the previously blocked WI in the SAME transaction (FA-5:
// the delete and the re-derivation must not tear - a concurrent re-add of
// another blocks edge between the two statements would otherwise be
// overwritten by a stale waiting_on recomputation).
func (m *Manager) RemoveDependency(blocking, blocked, kind string) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM wi_dependencies WHERE blocking_wi_id=?1 AND blocked_wi_id=?2 AND kind=?3`,
		blocking, blocked, kind)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errf("CORE-404", "", "no such edge")
	}
	if kind == "blocks" {
		left, err := m.unfinishedBlockers(tx, blocked)
		if err != nil {
			return err
		}
		var wo any
		if len(left) > 0 {
			wo = "dependency:" + left[0]
		}
		if _, err := tx.Exec(`UPDATE work_items SET waiting_on=?1, updated_at=?2 WHERE id=?3 AND status='queued'`,
			wo, store.Now(), blocked); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// reaches reports whether src can reach dst following edges of kind, reading
// through the caller's handle so the cycle check shares the add/remove
// transaction (FA-5).
func (m *Manager) reaches(q dbHandle, src, dst, kind string) (bool, error) {
	if src == dst {
		return true, nil
	}
	seen := map[string]bool{}
	stack := []string{src}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		rows, err := q.Query(`SELECT blocked_wi_id FROM wi_dependencies WHERE blocking_wi_id=?1 AND kind=?2`, cur, kind)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var next string
			if err := rows.Scan(&next); err != nil {
				rows.Close()
				return false, err
			}
			if next == dst {
				rows.Close()
				return true, nil
			}
			stack = append(stack, next)
		}
		rows.Close()
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Budget (02 §4.5a/§6.7)

// Budget is the GET .../budget response (row 12).
type Budget struct {
	Baseline   map[string]*int64 `json:"baseline"`
	Increments []BudgetIncrement `json:"increments"`
	Effective  map[string]*int64 `json:"effective"`
	MaxSeq     int64             `json:"max_seq"`
}

// BudgetIncrement is one workflow_budget_increments row.
type BudgetIncrement struct {
	Dimension string  `json:"dimension"`
	Seq       int64   `json:"seq"`
	Amount    int64   `json:"amount"`
	GrantedBy string  `json:"granted_by"`
	GrantedAt string  `json:"granted_at"`
	Note      *string `json:"note"`
}

// GetBudget returns baseline + increments + per-dimension effective ceilings
// + the global max seq (CAS read source, §6.7).
func (m *Manager) GetBudget(wiID string) (*Budget, error) {
	var baseline sql.NullString
	err := m.db.QueryRow(`SELECT workflow_budget_baseline FROM work_items WHERE id=?1`, wiID).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("CORE-404", "", "work item %s", wiID)
	}
	if err != nil {
		return nil, err
	}
	out := &Budget{
		Baseline:   map[string]*int64{"requests": nil, "tokens": nil, "wall_minutes": nil},
		Effective:  map[string]*int64{"requests": nil, "tokens": nil, "wall_minutes": nil},
		Increments: []BudgetIncrement{},
		MaxSeq:     0,
	}
	if baseline.Valid && baseline.String != "" {
		if err := json.Unmarshal([]byte(baseline.String), &out.Baseline); err != nil {
			return nil, err
		}
	}
	rows, err := m.db.Query(`SELECT dimension, seq, amount, granted_by, granted_at, note
		FROM workflow_budget_increments WHERE work_item_id=?1 ORDER BY seq`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sums := map[string]int64{}
	for rows.Next() {
		var inc BudgetIncrement
		var note sql.NullString
		if err := rows.Scan(&inc.Dimension, &inc.Seq, &inc.Amount, &inc.GrantedBy, &inc.GrantedAt, &note); err != nil {
			return nil, err
		}
		if note.Valid {
			inc.Note = &note.String
		}
		out.Increments = append(out.Increments, inc)
		out.MaxSeq = inc.Seq
		sums[inc.Dimension] += inc.Amount
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, dim := range []string{"requests", "tokens", "wall_minutes"} {
		if base := out.Baseline[dim]; base != nil {
			v := *base + sums[dim]
			out.Effective[dim] = &v
		} else if sums[dim] > 0 {
			v := sums[dim]
			out.Effective[dim] = &v
		}
	}
	return out, nil
}

// AddBudgetIncrement applies the §6.7 CAS: based_on_seq must equal the WI's
// current global max seq; the winner inserts seq=based_on_seq+1 in-transaction.
func (m *Manager) AddBudgetIncrement(wiID, dimension string, amount int64, note string, basedOnSeq int64, grantedBy string) (*Budget, error) {
	switch dimension {
	case "requests", "tokens", "wall_minutes":
	default:
		return nil, errf("CORE-400", "", "dimension must be requests|tokens|wall_minutes")
	}
	if amount <= 0 {
		return nil, errf("CORE-400", "", "amount must be > 0")
	}
	if _, err := m.loadWI(m.db, wiID); err != nil {
		return nil, err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM workflow_budget_increments WHERE work_item_id=?1`, wiID).Scan(&maxSeq); err != nil {
		return nil, err
	}
	if maxSeq.Int64 != basedOnSeq {
		return nil, errf("CORE-409", "CAS_CONFLICT", "current max seq is %d", maxSeq.Int64)
	}
	var noteAny any
	if note != "" {
		noteAny = note
	}
	if _, err := tx.Exec(`INSERT INTO workflow_budget_increments(work_item_id, dimension, seq, amount, granted_by, granted_at, note)
		VALUES (?1,?2,?3,?4,?5,?6,?7)`, wiID, dimension, basedOnSeq+1, amount, grantedBy, store.Now(), noteAny); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"seq": basedOnSeq + 1, "dimension": dimension, "amount": amount})
	if _, err := tx.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1,?2,?3,'budget_incremented',?4)`, wiID, store.Now(), grantedBy, string(payload)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.GetBudget(wiID)
}
