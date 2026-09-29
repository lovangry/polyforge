package core

// Ready queue: the LCRS segments the drain-mode daemon consumes.
// Simplified per the v1 lane scope: items / needs_human_session /
// unclassified (the three executable segments, split by the WI's
// requires_human_session classification) + running / paused visibility
// segments. Ordering inside every segment: priority → created_at.

import (
	"database/sql"
	"errors"

	"github.com/silgrid/polyforge/internal/common/api"
)

// ReadyQueue is the response shape of GET /api/v1/projects/{p}/ready-queue.
type ReadyQueue struct {
	Project           string         `json:"project"`
	Items             []api.WorkItem `json:"items"`               // executable, no human session needed
	NeedsHumanSession []api.WorkItem `json:"needs_human_session"` // executable, needs a human
	Unknown           []api.WorkItem `json:"unclassified"`        // executable, classification missing
	Running           []api.WorkItem `json:"running"`
	Paused            []api.WorkItem `json:"paused"`
}

// rhs classification of a queued WI: true / false / missing.
type rhsClass int

const (
	rhsMissing rhsClass = iota
	rhsTrue
	rhsFalse
)

func classifyRHS(wi *api.WorkItem) rhsClass {
	v, ok := wi.Attrs["requires_human_session"]
	if !ok {
		return rhsMissing
	}
	b, isBool := v.(bool)
	if !isBool {
		return rhsMissing
	}
	if b {
		return rhsTrue
	}
	return rhsFalse
}

var priorityRank = map[string]int{
	api.PriorityUrgent: 0, api.PriorityHigh: 1, api.PriorityNormal: 2, api.PriorityLow: 3,
}

func queueLess(a, b *api.WorkItem) bool {
	ra, rb := priorityRank[a.Priority], priorityRank[b.Priority]
	if ra != rb {
		return ra < rb
	}
	return a.CreatedAt < b.CreatedAt
}

// ReadyQueue builds the queue for a project (name or id). A WI is
// executable when: status queued, waiting_on empty, and no unfinished
// `blocks` dependency edge.
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
	q := &ReadyQueue{
		Project:           projectName,
		Items:             []api.WorkItem{},
		NeedsHumanSession: []api.WorkItem{},
		Unknown:           []api.WorkItem{},
		Running:           []api.WorkItem{},
		Paused:            []api.WorkItem{},
	}
	// Loaded per candidate below: bundle_name decides execution vs
	// registration ticket (02 §5.1b: the executable segments always exclude
	// registration tickets — they cannot be claimed, #17).
	rows, err := m.db.Query(`SELECT id, COALESCE(waiting_on,''), bundle_name IS NULL FROM work_items
		WHERE project_id=? AND status IN ('queued','running','paused')`, projectID)
	if err != nil {
		return nil, err
	}
	type cand struct {
		wi           *api.WorkItem
		waitingOn    string
		registration bool
	}
	var cands []cand
	var ids []string
	for rows.Next() {
		var id, waiting string
		var registration bool
		if err := rows.Scan(&id, &waiting, &registration); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		cands = append(cands, cand{waitingOn: waiting, registration: registration})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range cands {
		wi, err := m.load(ids[i])
		if err != nil {
			return nil, err
		}
		cands[i].wi = wi
	}
	sortBy := func(list *[]api.WorkItem) {
		// stable priority→created_at ordering
		for i := 1; i < len(*list); i++ {
			for j := i; j > 0 && queueLess(&(*list)[j], &(*list)[j-1]); j-- {
				(*list)[j], (*list)[j-1] = (*list)[j-1], (*list)[j]
			}
		}
	}
	for _, c := range cands {
		switch c.wi.Status {
		case api.StatusRunning:
			q.Running = append(q.Running, *c.wi)
		case api.StatusPaused:
			q.Paused = append(q.Paused, *c.wi)
		case api.StatusQueued:
			if c.registration {
				continue // registration tickets never execute (#17)
			}
			if c.waitingOn != "" {
				continue // blocked-visible: not executable
			}
			blocked, err := m.hasUnfinishedBlockers(c.wi.ID)
			if err != nil {
				return nil, err
			}
			if blocked {
				continue
			}
			switch classifyRHS(c.wi) {
			case rhsTrue:
				q.NeedsHumanSession = append(q.NeedsHumanSession, *c.wi)
			case rhsFalse:
				q.Items = append(q.Items, *c.wi)
			default:
				q.Unknown = append(q.Unknown, *c.wi)
			}
		}
	}
	sortBy(&q.Items)
	sortBy(&q.NeedsHumanSession)
	sortBy(&q.Unknown)
	sortBy(&q.Running)
	sortBy(&q.Paused)
	return q, nil
}
