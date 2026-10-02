package observe

// view.go: the JobView projection served by GET /jobs/{id}/observe —
// TWO layers over one response (04 §5.1a):
//
//   - the execution.json DIRECT-READ layer: the capsule's
//     runtime-state/execution.json is re-read from disk at request time.
//     It is the single execution-state authority (INV-1); the monitor's
//     in-memory copy is only a poll cache and is never served as state.
//   - the events layer: the per-WI ring mirrored from the events file.
//
// The pending-question projection across recovery is specified once, in
// answers.go (the relay's single authority); this projection only keeps
// it visible.

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// JobView renders the two-layer projection for this work item. A missing
// or unreadable execution.json degrades to the monitor-known facts
// (status from the monitor state machine, no step detail) — observe
// stays available exactly when the operator needs it most.
func (m *Monitor) JobView() *protocol.JobView {
	m.stateMu.Lock()
	pending := m.pending
	sealing, wrapped, blocked := m.sealing, m.wrapped, m.blocked
	spawn := m.spawnN
	fp := ""
	if m.sessionID != "" {
		fp = api.SessionFingerprint(m.sessionID) // public handle (B3)
	}
	m.stateMu.Unlock()

	view := &protocol.JobView{
		WorkItem:           m.wi.WorkItem,
		Project:            m.wi.Project,
		SpawnN:             spawn,
		SessionFingerprint: fp,
		BundleName:         m.wi.BundleName,
		Pending:            pending,
	}

	// layer 1: execution.json direct read (authoritative).
	var exec capsule.Execution
	if data, err := os.ReadFile(m.execPath); err == nil &&
		json.Unmarshal(data, &exec) == nil {
		view.CurrentStep = exec.CurrentStep
		view.Steps = map[string]*protocol.StepView{}
		for id, s := range exec.Steps {
			sv := &protocol.StepView{
				Status:    s.Status,
				Model:     s.Model,
				Attempts:  len(s.Attempts),
				StartedAt: s.StepStartedAt,
			}
			if len(s.Output) > 0 {
				if raw, err := json.Marshal(s.Output); err == nil {
					sv.Output = string(raw)
				}
			}
			view.Steps[id] = sv
		}
		if raw, err := json.Marshal(exec.BudgetConsumed); err == nil {
			view.Budget = raw
		}
	}

	// layer 2: events.
	ring := m.log.Since(m.wi.WorkItem)
	// Daemon-scope entries (wi="" — e.g. the drain degraded signal)
	// ride every live monitor's view: the drain fetch owns no job, and
	// the owner's ruling requires the degraded signal to be readable
	// from the observe projection. Chronological merge; daemon-scope
	// events are rare, so the sort stays trivial.
	ring = append(ring, m.log.Since("")...)
	sort.SliceStable(ring, func(i, j int) bool { return ring[i].At < ring[j].At })
	view.Events = make([]protocol.EventEntry, 0, len(ring))
	for _, e := range ring {
		view.Events = append(view.Events, e)
	}

	// status derivation, most specific first. A terminal derivation
	// (wrap acked or wrap-blocked) or a terminal workflow_status in the
	// authoritative file wins; then the seal-in-progress window; then the
	// needs_input exemption; otherwise the job is running.
	switch {
	case wrapped || blocked || isTerminalStatus(exec.WorkflowStatus):
		view.Status = protocol.JobDone
	case sealing:
		view.Status = protocol.JobSealing
	case pending != nil:
		view.Status = protocol.JobNeedsInput
	default:
		view.Status = protocol.JobRunning
	}
	return view
}

// PendingInput is one work item's owner-facing needs_input projection
// (D6): the pending question's id, step and ask moment, derived from the
// per-WI monitor's pending state — the /status face merges it over the
// runtime's own bookkeeping so an agent blocked on a human is never
// presented as merely "running".
type PendingInput struct {
	QuestionID string
	Step       string
	AskedAt    string
}

// pendingQuestion answers the monitor's current pending-question
// projection (stateMu-guarded; nil = none).
func (m *Monitor) pendingQuestion() *protocol.PendingView {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.pending
}

// lastProgressAt answers when this job's stream last proved AGENT
// progress (the F15c owner face's source; zero = no progress record
// yet — the caller renders "-"). The stream pointer is read under
// stateMu because recovery swaps it on the monitor goroutine while
// this runs on the HTTP/status face's goroutine; the heartbeat itself
// locks the stream's own mutex. UI-noise records never move this clock
// (stream.go's F15 whitelist).
func (m *Monitor) lastProgressAt() time.Time {
	m.stateMu.Lock()
	s := m.stream
	m.stateMu.Unlock()
	if s == nil {
		return time.Time{}
	}
	return s.heartbeat().LastProgressAt
}

// ProgressAge is one work item's owner-facing progress staleness (F15c):
// when the job's stream last proved agent progress. Zero = never.
type ProgressAge struct {
	LastProgressAt time.Time
}

// ProgressAges answers the progress staleness of every monitored work
// item (F15c): canonical work-item id → last agent-progress moment.
// Monitors are keyed by the canonical id the runtime announced; the
// caller merges by the same key (the ops status face renders it as the
// LAST-PROGRESS column — the pilot's 2.7-hour wedge had NO owner-
// visible surface while it ran).
func (o *Observe) ProgressAges() map[string]ProgressAge {
	o.mu.Lock()
	monitors := make([]*Monitor, 0, len(o.monitors))
	for _, m := range o.monitors {
		monitors = append(monitors, m)
	}
	o.mu.Unlock()
	out := map[string]ProgressAge{}
	for _, m := range monitors {
		out[m.wi.WorkItem] = ProgressAge{LastProgressAt: m.lastProgressAt()}
	}
	return out
}

// PendingInputs answers the needs_input state of every monitored work
// item (D6): canonical work-item id → pending question. Monitors are
// keyed by the canonical id the runtime announced; the caller merges by
// the same key.
func (o *Observe) PendingInputs() map[string]PendingInput {
	o.mu.Lock()
	monitors := make([]*Monitor, 0, len(o.monitors))
	for _, m := range o.monitors {
		monitors = append(monitors, m)
	}
	o.mu.Unlock()
	out := map[string]PendingInput{}
	for _, m := range monitors {
		if pv := m.pendingQuestion(); pv != nil {
			out[m.wi.WorkItem] = PendingInput{
				QuestionID: pv.ID, Step: pv.Step, AskedAt: pv.AskedAt,
			}
		}
	}
	return out
}

// isTerminalStatus reports whether execution.json's workflow_status is
// terminal (the zero value of the struct read is "" when the file was
// unreadable, which correctly lands in the not-terminal branch).
func isTerminalStatus(s string) bool {
	switch s {
	case "wrapped", "failed", "blocked":
		return true
	}
	return false
}
