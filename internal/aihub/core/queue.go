package core

// Ready queue: the executable segment (items) the daemon's auto-pull
// consumes, plus running / paused visibility segments. Ordering inside
// every segment: priority → created_at.

import (
	"database/sql"
	"errors"
	"sort"

	"github.com/silgrid/polyforge/internal/common/api"
)

// ReadyQueue is the response shape of GET /api/v1/projects/{p}/ready-queue.
// pending_bundles is the w140 additive segment: registration tickets
// (no bundle) that are childless, queued, unblocked and not waiting —
// the 待选 bundle candidates a client may bind a workflow onto.
type ReadyQueue struct {
	Project        string   `json:"project"`
	Items          []WIView `json:"items"` // executable: queued, unblocked, not a registration ticket
	Running        []WIView `json:"running"`
	Paused         []WIView `json:"paused"`
	PendingBundles []WIView `json:"pending_bundles"` // w140: 待选 bundle (additive; old clients ignore it)
}

var priorityRank = map[string]int{
	api.PriorityUrgent: 0, api.PriorityHigh: 1, api.PriorityNormal: 2, api.PriorityLow: 3,
}

func queueLess(a, b *WIView) bool {
	ra, rb := priorityRank[a.Priority], priorityRank[b.Priority]
	if ra != rb {
		return ra < rb
	}
	return a.CreatedAt < b.CreatedAt
}

// ReadyQueue builds the queue for a project (name or id). A WI is
// executable when: status queued, waiting_on empty, no unfinished
// `blocks` dependency edge (only a wrapped blocker releases, #161), and every repo its inventory names is still
// one of the project's repos (reference.md §2.1; else its claim would be refused
// INVENTORY_REPO_GONE on every drain wave).
func (m *WIManager) ReadyQueue(project string) (*ReadyQueue, error) {
	var projectID, projectName string
	err := m.db.QueryRow(`SELECT id, name FROM projects WHERE name=?1 OR id=?1`, project).
		Scan(&projectID, &projectName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "project %q", project)
		}
		return nil, err
	}
	projectRepos, err := projectReposTx(m.db, projectID)
	if err != nil {
		return nil, err
	}
	inProject := map[string]bool{}
	for _, pr := range projectRepos {
		inProject[pr.Name] = true
	}
	q := &ReadyQueue{
		Project:        projectName,
		Items:          []WIView{},
		Running:        []WIView{},
		Paused:         []WIView{},
		PendingBundles: []WIView{},
	}
	// One read of every candidate: the WorkItem, its public session block
	// (m4), the child count (an epic never queues, w140), the unfinished
	// blockers flag (#161) and the frozen inventory (reference.md §2.1).
	rows, err := m.db.Query(wiSelect+`, w.session_fingerprint, w.session_machine, w.session_started_at,
		w.clean_pause, (SELECT COUNT(*) FROM work_items c WHERE c.parent_id=w.id),
		EXISTS(SELECT 1 FROM wi_dependencies d JOIN work_items b ON b.id=d.blocking_wi_id
			WHERE d.blocked_wi_id=w.id AND d.kind='blocks' AND b.status <> 'wrapped'),
		w.repo_inventory`+wiFrom+`
		WHERE w.project_id=? AND w.status IN ('queued','running','paused') ORDER BY w.rowid`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type cand struct {
		view     WIView
		children int
		blocked  bool
		inv      string
	}
	var cands []cand
	var ids []string
	for rows.Next() {
		var c cand
		var fp, machine, started, inv sql.NullString
		wi, err := scanWorkItem(rows.Scan, &fp, &machine, &started, &c.view.CleanPause,
			&c.children, &c.blocked, &inv)
		if err != nil {
			return nil, err
		}
		c.view.WorkItem = *wi
		c.view.SessionFingerprint, c.view.SessionMachine, c.view.SessionStartedAt =
			fp.String, machine.String, started.String
		c.inv = inv.String
		cands = append(cands, c)
		ids = append(ids, wi.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// One grouped derived-epic read (w140): every queue entry carries the
	// additive derived block (parents show their child census).
	derived, err := m.db.derivedBatch(ids)
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		c.view.Derived = derived[c.view.ID]
		waiting := c.view.WaitingOn != nil && *c.view.WaitingOn != ""
		switch c.view.Status {
		case api.StatusRunning:
			q.Running = append(q.Running, c.view)
		case api.StatusPaused:
			q.Paused = append(q.Paused, c.view)
		case api.StatusQueued:
			if c.view.BundleName == nil || *c.view.BundleName == "" {
				// w140: the 待选 bundle segment (additive): a childless
				// registration ticket that would execute if it had a
				// bundle — the same exclusions as the executable segment
				// (waiting_on and unfinished blocks edges). An epic
				// (has children) never queues: it is neither executable
				// (no bundle, #17) nor a pending-bundle candidate.
				if c.children == 0 && !waiting && !c.blocked {
					q.PendingBundles = append(q.PendingBundles, c.view)
				}
				continue // registration tickets never execute (#17)
			}
			if waiting || c.blocked {
				continue // blocked-visible: not executable
			}
			gone, err := inventoryRepoGone(c.inv, inProject)
			if err != nil {
				return nil, err
			}
			if !gone { // reference.md §2.1: a declared repo left the project
				q.Items = append(q.Items, c.view)
			}
		}
	}
	for _, list := range [][]WIView{q.Items, q.Running, q.Paused, q.PendingBundles} {
		sort.SliceStable(list, func(i, j int) bool { return queueLess(&list[i], &list[j]) })
	}
	return q, nil
}
