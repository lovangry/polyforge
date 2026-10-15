package core

// Work items: CRUD, WI state machine, dependencies and events.
//
// v1.5 state machine (statuses in common/api): queued → running (the
// session CAS claim), running → paused/blocked (session-end), paused →
// running (session-attach, M3), running/paused → queued (force-end
// by an admin/maintainer: the recovery door), running → wrapped/failed (session-end), queued/paused/
// blocked → canceled (cancel); the blocked exits are resume-blocked
// (continue) and cancel (abandon).
// Terminal states are read-only (CORE-410).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

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
	// The bundle reference freezes at FIRST CLAIM (the R2 window, w140):
	// while the WI is queued and never claimed, PATCH may set, rename or
	// clear bundle_name through the shared persist path (bundleRef);
	// after the first claim it is immutable again (INV-2e as amended).
	BundleName *string `json:"bundle_name"`
	// Repo inventory (02 §4.6): the project repos this WI works in,
	// [{name}] only, frozen at create into work_items.repo_inventory and
	// served by the claim (URLs resolved from the project then) in place
	// of the project's full repo list.
	RepoInventory RepoInventoryInput `json:"repo_inventory"`
	// RepoInventoryBase is the removed second array of the pre-v2.5.0
	// shape. It is decoded only to be refused (INVENTORY_BASE_UNSUPPORTED):
	// a frozen base is not supported and must not be silently dropped.
	RepoInventoryBase json.RawMessage `json:"repo_inventory_base"`
	// After (polyforge#161) names the work items (id or slug) this one
	// runs after: one `blocks` edge per entry, written INSIDE the create
	// transaction under the AddDependency rules (same project, blocker
	// must be an execution ticket). Any bad entry refuses the whole
	// create — nothing is written.
	After []string `json:"after"`
}

// maxAfter caps the create-time blockers of one work item.
const maxAfter = 20

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
	if utf8.RuneCountInString(req.Goal) > api.MaxGoalRunes {
		return nil, Fail(api.ErrCoreBadInput, "", "goal exceeds %d characters", api.MaxGoalRunes)
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

	// Create-time blockers (polyforge#161): resolved and validated before
	// anything is written.
	if len(req.After) > maxAfter {
		return nil, Fail(api.ErrCoreBadInput, "", "too many after entries (max %d)", maxAfter)
	}
	var blockers []string
	seenBlocker := map[string]bool{}
	for _, ref := range req.After {
		bid, err := resolveWith(tx, strings.TrimSpace(ref))
		if err != nil {
			return nil, err
		}
		if err := checkBlockerTx(tx, bid, ref, projectID); err != nil {
			return nil, err
		}
		if !seenBlocker[bid] {
			seenBlocker[bid] = true
			blockers = append(blockers, bid)
		}
	}

	// Repo inventory (02 §4.6): resolved INSIDE the create transaction
	// against the project's repos, so a refused create persists nothing.
	if err := refuseRepoInventoryBase(req.RepoInventoryBase); err != nil {
		return nil, err
	}
	projectRepos, err := projectReposTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	inv, err := resolveRepoInventory(req.RepoInventory, projectRepos)
	if err != nil {
		return nil, err
	}
	invJSON, err := repoInventoryJSON(inv)
	if err != nil {
		return nil, err
	}

	// Display seq: PER-PROJECT monotonic (<project>#<seq>). The legacy
	// library numbers WIs per project (aiterm#1 and defswitch#1 both
	// exist) and slugs have always resolved by (project, seq); the seq-fix
	// owner ruling makes the numbering match — MAX(seq) scoped to this
	// project, not the whole table.
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM work_items WHERE project_id=?1`, projectID).Scan(&maxSeq); err != nil {
		return nil, err
	}
	seq := maxSeq.Int64 + 1

	// Bundle reference (02 §4.4/§4.5): a non-empty bundle_name makes an
	// execution ticket. The name resolves through the ONE shared persist
	// path (bundleRef — the same helper the PATCH entry uses): a nil/empty
	// name resolves to NULL. Full §4.5 fixation is deleted with the digest
	// chain (polyforge#149): the reference is the name, and the bundle's
	// truth is the materialized checkout.
	bundleName := bundleRef(req.BundleName)

	id, err := NewID("wi_")
	if err != nil {
		return nil, err
	}
	now := Now()
	labels, _ := json.Marshal(req.Labels)
	attrs, _ := json.Marshal(req.Attrs)
	if _, err := tx.Exec(`INSERT INTO work_items(id, seq, project_id, goal, content, parent_id,
		status, priority, milestone, labels, attrs, reporter, bundle_name, repo_inventory, created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?15)`,
		id, seq, projectID, req.Goal, nullableString(req.Content), nullableString(req.ParentID),
		api.StatusQueued, req.Priority, nullableString(req.Milestone), string(labels), string(attrs), req.Reporter,
		bundleName, invJSON, now); err != nil {
		return nil, err
	}
	if err := appendEventTx(tx, id, now, req.Reporter, "work_item_filed", nil); err != nil {
		return nil, err
	}
	// The new work item has no outgoing edge yet, so no cycle is possible.
	for _, bid := range blockers {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO wi_dependencies(blocking_wi_id, blocked_wi_id, kind, created_at)
			VALUES (?1,?2,'blocks',?3)`, bid, id, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m.Get(id, GetOptions{})
}

// checkBlockerTx enforces the blocker-side rules of a `blocks` edge
// (polyforge#161): the blocker exists, lives in projectID (dependency
// edges never cross projects) and is an execution ticket — a ticket with
// no bundle (registration ticket or epic) can never wrap, so an edge
// onto it would wait forever. ref is the caller's spelling for errors.
func checkBlockerTx(q queryRower, blockingID, ref, projectID string) error {
	var proj string
	var bundle sql.NullString
	err := q.QueryRow(`SELECT project_id, bundle_name FROM work_items WHERE id=?1`, blockingID).Scan(&proj, &bundle)
	if errors.Is(err, sql.ErrNoRows) {
		return Fail(api.ErrCoreNotFound, "", "work item %s", ref)
	}
	if err != nil {
		return err
	}
	if proj != projectID {
		return Fail(api.ErrCoreBadInput, "", "dependency edges cannot cross projects")
	}
	if !bundle.Valid || bundle.String == "" {
		return Fail(api.ErrCoreBadInput, "BLOCKER_NOT_EXECUTABLE",
			"blocker %s must be an execution ticket — bind a bundle first (a registration ticket or epic never wraps)", ref)
	}
	return nil
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Resolve maps an id or a "<project>#<seq>" slug to the work-item id.
func (m *WIManager) Resolve(idOrSlug string) (string, error) {
	return resolveWith(m.db, idOrSlug)
}

// resolveWith is Resolve on a given reader (the create transaction must
// resolve on its own connection: the store is single-connection).
func resolveWith(q queryRower, idOrSlug string) (string, error) {
	if strings.HasPrefix(idOrSlug, "wi_") {
		return idOrSlug, nil
	}
	i := strings.LastIndexByte(idOrSlug, '#')
	if i < 0 {
		return "", Fail(api.ErrCoreNotFound, "", "work item %s", idOrSlug)
	}
	project, seqStr := idOrSlug[:i], idOrSlug[i+1:]
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		return "", Fail(api.ErrCoreNotFound, "", "work item %s", idOrSlug)
	}
	var id string
	err = q.QueryRow(`SELECT w.id FROM work_items w JOIN projects p ON p.id=w.project_id
		WHERE p.name=?1 AND w.seq=?2`, project, seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Fail(api.ErrCoreNotFound, "", "work item %s", idOrSlug)
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
	var content, parent, milestone, assignee, waitingOn, bundle, closed sql.NullString
	var labels, attrs string
	err := m.db.QueryRow(`SELECT w.id, w.seq, w.project_id, p.name, w.goal, w.content, w.parent_id,
		w.status, w.priority, w.milestone, w.labels, w.attrs, w.reporter, w.assignee, w.waiting_on,
		w.bundle_name, w.created_at, w.updated_at, w.closed_at
		FROM work_items w JOIN projects p ON p.id=w.project_id WHERE w.id=?1`, id).
		Scan(&wi.ID, &wi.Seq, &wi.ProjectID, &wi.ProjectName, &wi.Goal, &content, &parent,
			&wi.Status, &wi.Priority, &milestone, &labels, &attrs, &wi.Reporter, &assignee, &waitingOn,
			&bundle, &wi.CreatedAt, &wi.UpdatedAt, &closed)
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
		var payload string
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Actor, &e.Type, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
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

// bundleRef is the ONE bundle-reference persist path, shared by create
// and PATCH (spec: both entries MUST hit the same code path). A nil/empty
// name resolves to NULL: an unbind clears the reference.
func bundleRef(name *string) (bundleName any) {
	if name == nil || *name == "" {
		return nil
	}
	return *name
}

// patchableFields (02 §5.1 row 3). Workflow refs and parent are immutable.
// clean_pause rides the whitelist per the v1.5 contract §1.5 AMEND
// ("pf resume clears the flag via WI PATCH (whitelist gained
// clean_pause)") — the operator resume face; the update half below was
// wired at merge, the whitelist entry was missed there (found by the
// Lane D e2e s03 pause/resume scenario).
//
// bundle_name rides the R2 window (w140): it is patchable exactly
// while the WI is queued and never claimed — the window gate below
// refuses it everywhere else.
var patchableFields = map[string]bool{
	"goal": true, "content": true, "labels": true, "priority": true,
	"milestone": true, "waiting_on": true, "clean_pause": true,
	"bundle_name": true,
}

// bundlePatch is a parsed PATCH bundle_name field. Name nil = the
// JSON-null unbind; Name non-empty = bind or rename.
type bundlePatch struct {
	Name *string
}

// binding reports whether the patch binds a non-NULL bundle (the
// symmetric child rule applies only to binding, not to unbinding; the
// parser already rejected an empty name).
func (b *bundlePatch) binding() bool { return b != nil && b.Name != nil }

// parseBundlePatch extracts the bundle name from a PATCH body. Absent →
// nil (not a bundle patch); malformed shapes → typed CORE-400. A PATCH
// body carrying a digest key is refused by the patchable-fields gate
// below (the key is not patchable).
func parseBundlePatch(fields map[string]json.RawMessage) (*bundlePatch, error) {
	raw, hasName := fields["bundle_name"]
	if !hasName {
		return nil, nil
	}
	var bp bundlePatch
	if json.Unmarshal(raw, &bp.Name) != nil {
		return nil, Fail(api.ErrCoreBadInput, "", "bundle_name must be a string or null")
	}
	if bp.Name != nil && *bp.Name == "" {
		return nil, Fail(api.ErrCoreBadInput, "",
			"bundle_name must be a non-empty string or null (null unbinds)")
	}
	return &bp, nil
}

// refuseBundleWithChildrenTx is the symmetric children-side rule (w140,
// spec R-对称-1): a ticket WITH children must never GAIN a bundle —
// children count in any status (terminal included). The PATCH bind entry
// runs it inside its transaction and re-validates it in the guarded
// write; the create entry cannot violate it (a created row is childless
// by construction) but runs the mirror rule instead: a bundle-bound
// ticket is never a parent (PARENT_NOT_REGISTRY, below).
func refuseBundleWithChildrenTx(q queryRower, wiID string) error {
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_items WHERE parent_id=?1`, wiID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return Fail(api.ErrCoreBadInput, "HAS_CHILDREN_NO_BUNDLE",
			"work item has %d child ticket(s); a ticket with children cannot bind a workflow bundle", n)
	}
	return nil
}

// diagnoseR2Window is the R2 bundle-PATCH gate's error face (w140). The
// window is status==queued AND never claimed; "never claimed" uses the
// claimTx judgment (spec Assumption 4): no live session row AND no
// session_claimed event — a force-end clears the session row but the
// claim event remains, and the freeze stays. waiting_on is deliberately
// NOT consulted: a waiting ticket inside the window still accepts the
// PATCH (whether it executes is a queue-segment question, not an
// immutability one).
func diagnoseR2Window(q queryRower, wiID string) error {
	var status string
	var session, fp sql.NullString
	err := q.QueryRow(`SELECT status, session_hash, session_fingerprint FROM work_items WHERE id=?1`, wiID).
		Scan(&status, &session, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
	}
	if err != nil {
		return err
	}
	if isTerminal(status) {
		return Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", status)
	}
	if session.Valid && session.String != "" {
		return Fail(api.ErrCoreBadInput, "IMMUTABLE",
			"bundle_name froze at first claim (session %s holds the work item)", fp.String)
	}
	if status != api.StatusQueued {
		return Fail(api.ErrCoreBadInput, "IMMUTABLE",
			"bundle_name is only patchable while queued and never claimed (R2 window); the work item is %s", status)
	}
	var claims int
	if err := q.QueryRow(`SELECT COUNT(*) FROM wi_events
		WHERE work_item_id=?1 AND event_type='session_claimed'`, wiID).Scan(&claims); err != nil {
		return err
	}
	if claims > 0 {
		return Fail(api.ErrCoreBadInput, "IMMUTABLE",
			"bundle_name froze at first claim (a session_claimed event exists)")
	}
	return nil
}

// refuseUnbindWhileBlockingTx refuses unbinding the bundle of a work item
// that is still the blocker of a `blocks` edge (polyforge#161): without a
// bundle it can never wrap, so its dependents would wait forever.
// Rebinding to another bundle stays allowed.
func refuseUnbindWhileBlockingTx(tx *sql.Tx, wiID string) error {
	rows, err := tx.Query(`SELECT p.name, w.seq FROM wi_dependencies d
		JOIN work_items w ON w.id = d.blocked_wi_id
		JOIN projects p ON p.id = w.project_id
		WHERE d.blocking_wi_id=?1 AND d.kind='blocks' ORDER BY w.seq`, wiID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var dependents []string
	for rows.Next() {
		var project string
		var seq int64
		if err := rows.Scan(&project, &seq); err != nil {
			return err
		}
		dependents = append(dependents, project+"#"+strconv.FormatInt(seq, 10))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(dependents) > 0 {
		return Fail(api.ErrCoreBadInput, "BLOCKER_STILL_BLOCKS",
			"cannot unbind the bundle: this work item still blocks %s — remove the edges first (`pf file dep rm`)",
			strings.Join(dependents, ", "))
	}
	return nil
}

// Patch updates the mutable field set. fields maps field name → raw JSON.
func (m *WIManager) Patch(idOrSlug string, fields map[string]json.RawMessage) (*api.WorkItem, error) {
	// Bundle fields (w140): parsed up front (pure — no DB access); the R2
	// window gate and the CAS write run inside the transaction later on.
	// Concurrency with Claim needs no extra arbitration: the single-writer
	// connection serializes the two transactions and each one's CAS keeps
	// the end state consistent — a claim first freezes the bundle (this
	// write then finds a claimed row and is refused IMMUTABLE), a PATCH
	// first is simply the bundle the later claim freezes.
	bp, err := parseBundlePatch(fields)
	if err != nil {
		return nil, err
	}
	id, err := m.Resolve(idOrSlug)
	if err != nil {
		return nil, err
	}
	// The repo inventory (02 §4.6) is frozen at create: no UPDATE path.
	if _, ok := fields["repo_inventory"]; ok {
		return nil, Fail(api.ErrCoreBadInput, "IMMUTABLE",
			"repo_inventory is frozen at create (02 §4.6, no UPDATE path); cancel and re-create the work item to change it")
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
	for _, field := range []string{"goal", "content", "labels", "priority", "milestone", "waiting_on", "clean_pause"} {
		raw, ok := fields[field]
		if !ok {
			continue
		}
		if field == "clean_pause" {
			// Merge amend: the operator resume face. clean_pause=false
			// re-enables the holder daemon's restart auto-attach; a pause
			// sets it through the atomic SessionEnd, not PATCH.
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return nil, Fail(api.ErrCoreBadInput, "", "clean_pause must be a boolean")
			}
			sets = append(sets, "clean_pause=?")
			args = append(args, v)
			continue
		}
		switch field {
		case "goal":
			var v string
			if json.Unmarshal(raw, &v) != nil || strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > api.MaxGoalRunes {
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
	if bp != nil {
		// R2 window: fast-fail diagnosis on this snapshot; the guarded
		// write below re-validates the same gates CAS-style (0 rows = the
		// race was lost, re-diagnosed for the precise wire error).
		if err := diagnoseR2Window(tx, id); err != nil {
			return nil, err
		}
		if bp.binding() {
			// Symmetric child rule, inside the same transaction (w140).
			if err := refuseBundleWithChildrenTx(tx, id); err != nil {
				return nil, err
			}
		} else if err := refuseUnbindWhileBlockingTx(tx, id); err != nil {
			// polyforge#161: an unbound blocker could never wrap.
			return nil, err
		}
		name := bundleRef(bp.Name)
		sets = append(sets, "bundle_name=?")
		args = append(args, name)
	}
	args = append(args, id)
	q := `UPDATE work_items SET ` + strings.Join(sets, ",") + ` WHERE id=?`
	if bp != nil {
		// claimTx-style CAS guard (w140): only a queued, never-claimed row
		// may take a bundle write, so a concurrent Claim makes THIS write
		// a 0-row no-op instead of interleaving with it.
		q += ` AND status='queued' AND session_hash IS NULL
			AND (SELECT COUNT(*) FROM wi_events e
			     WHERE e.work_item_id=work_items.id AND e.event_type='session_claimed')=0`
		if bp.binding() {
			// The symmetric child rule rides the same guarded write: a
			// child that landed concurrently makes THIS write a 0-row
			// no-op (bind-bundle vs new-child: exactly one wins, w140).
			q += ` AND (SELECT COUNT(*) FROM work_items c WHERE c.parent_id=work_items.id)=0`
		}
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if bp != nil {
			if derr := diagnoseR2Window(tx, id); derr != nil {
				return nil, derr
			}
			if bp.binding() {
				if derr := refuseBundleWithChildrenTx(tx, id); derr != nil {
					return nil, derr
				}
			}
			return nil, Fail(api.ErrCoreConflict, "RACE_LOST", "state changed concurrently")
		}
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

// validWaitingOn accepts the two LIVE values (the rewind-fix retention:
// 'owner' and 'dependency:<slug>') plus the empty clear. The dead
// branches ('gate' and 'runtime:*') are deleted (polyforge#149).
func validWaitingOn(wo string) bool {
	switch {
	case wo == "" || wo == "owner":
		return true
	case strings.HasPrefix(wo, "dependency:") && len(wo) > len("dependency:"):
		return true
	}
	return false
}

// Cancel terminates a non-active WI (queued/paused/blocked); a running WI
// must first end its session.
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
		return nil, Fail(api.ErrCoreConflict, "", "running work item: end its session (pause/block/force-end) before cancelling")
	}
	now := Now()
	// Guarded: only a cancelable row flips; 0 rows = concurrent transition.
	// The session row is cleared with the status: a terminal WI must never
	// stay "held" (M2's clearing rule applies to every terminal transition).
	res, err := tx.Exec(`UPDATE work_items SET status=?, closed_at=?, updated_at=?,
		waiting_on=NULL, session_hash=NULL, session_fingerprint=NULL, session_machine=NULL,
		session_started_at=NULL, clean_pause=0
		WHERE id=? AND status IN (?,?,?)`,
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

// ResumeBlocked is the §6.4 owner resume verdict's hub door (polyforge#116):
// the door the owner's `pf resume` exercises on a workflow parked blocked on
// an honest incomplete result. It is NOT a pool return for a
// generic re-claim: it marks the WI re-claimable for the owner's own
// immediate re-establishment of execution (the daemon claims it back and the
// claim/start path resumes the parked capsule in place). Gate:
// status==blocked AND the session row is EMPTY — the M2 invariant
// (a blocked WI still carrying a session would orphan a
// holder). Effect: status→queued, waiting_on=NULL, event wi_resumed; the
// claim gate (unchanged: queued only) then admits the WI like any queued
// ticket. Authorization (server layer, v2-converge §7): the WI's reporter,
// a project maintainer, or an admin — the same gate as cancel.
func (m *WIManager) ResumeBlocked(idOrSlug string, actor string) (*api.WorkItem, error) {
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
	var session sql.NullString
	if err := tx.QueryRow(`SELECT status, session_hash FROM work_items WHERE id=?1`, id).
		Scan(&status, &session); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "work item %s", id)
		}
		return nil, err
	}
	if isTerminal(status) {
		return nil, Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", status)
	}
	if status != api.StatusBlocked {
		return nil, Fail(api.ErrCoreConflict, "NOT_BLOCKED",
			"resume requires a blocked work item (got %s)", status)
	}
	if session.Valid && session.String != "" {
		// M2's invariant, asserted in the gate: blocked = blocked status
		// PLUS a cleared session. A session that survived the verdict is
		// a half-settled close — refuse rather than orphan a live holder.
		return nil, Fail(api.ErrCoreConflict, "SESSION_HELD",
			"a blocked work item must carry no session (M2 cleared it); refuse rather than orphan the holder")
	}
	now := Now()
	// Guarded: only a blocked row flips; 0 rows = concurrent transition.
	res, err := tx.Exec(`UPDATE work_items SET status=?, waiting_on=NULL, updated_at=?
		WHERE id=? AND status=?`, api.StatusQueued, now, id, api.StatusBlocked)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreConflict, "", "state changed concurrently")
	}
	if err := appendEventTx(tx, id, now, actor, "wi_resumed",
		map[string]any{"from": api.StatusBlocked, "to": api.StatusQueued}); err != nil {
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
	// Limit caps the page. 0 = the production default (500, the queue
	// views). Explicit values are clamped to maxListLimit.
	Limit int
}

// maxListLimit clamps an explicit list page.
const maxListLimit = 1000

const defaultListLimit = 500

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
	q += ` ORDER BY w.created_at DESC LIMIT ?`
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	args = append(args, limit)
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

// Edge is one dependency edge seen from one side: the OTHER work item's
// id plus (additive, polyforge#161) its slug and current status, so a
// client renders "blocked by polyforge#164 (running)" without a second
// read.
type Edge struct {
	WI     string `json:"wi_id"`
	Kind   string `json:"kind"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

func (m *WIManager) Dependencies(wiID string) (*Deps, error) {
	out := &Deps{Blocking: []Edge{}, BlockedBy: []Edge{}}
	rows, err := m.db.Query(`SELECT d.blocking_wi_id, d.blocked_wi_id, d.kind, p.name, o.seq, o.status
		FROM wi_dependencies d
		JOIN work_items o ON o.id = CASE WHEN d.blocking_wi_id=?1 THEN d.blocked_wi_id ELSE d.blocking_wi_id END
		JOIN projects p ON p.id = o.project_id
		WHERE d.blocking_wi_id=?1 OR d.blocked_wi_id=?1
		ORDER BY o.seq, d.kind`, wiID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var blocking, blocked, kind, project, status string
		var seq int64
		if err := rows.Scan(&blocking, &blocked, &kind, &project, &seq, &status); err != nil {
			return nil, err
		}
		slug := project + "#" + strconv.FormatInt(seq, 10)
		if blocking == wiID {
			out.Blocking = append(out.Blocking, Edge{WI: blocked, Kind: kind, Slug: slug, Status: status})
		} else {
			out.BlockedBy = append(out.BlockedBy, Edge{WI: blocking, Kind: kind, Slug: slug, Status: status})
		}
	}
	return out, rows.Err()
}

// AddDependency creates a dependency edge with the v0 write-path rules:
// same-project hard boundary, no self edge, cycle detection for
// blocks/supersedes; a `blocks` blocker must be an execution ticket
// (polyforge#161). Adding an existing edge is idempotent.
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
	if kind == "blocks" {
		if err := checkBlockerTx(tx, blocking, blocking, projBlocked); err != nil {
			return err
		}
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
	// Re-adding an existing edge is a no-op (the caller gets the current
	// view), never a raw constraint failure.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO wi_dependencies(blocking_wi_id, blocked_wi_id, kind, created_at)
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
