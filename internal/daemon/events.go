package daemon

// events.go: the per-job in-memory observe ring (04 §5.1a): at most 500
// events per job, monotonically numbered (the number IS the cursor), never
// persisted — a live view only; the execution-state authority stays the
// capsule's runtime-state/execution.json (INV-1 / INV-D1). No SSE, no
// push, no long connections: observers poll GET /jobs/{wi}/observe?after=N.

import "sync"

// Ring capacity (04 §5.1a).
const ObserveRingCap = 500

// The nine coarse event types (04 §5.1a, closed set).
const (
	EvJobStarted       = "job_started"
	EvWIAgentRederived = "wi_agent_rederived"
	EvStepStarted      = "step_started"
	EvStepCompleted    = "step_completed"
	EvNeedsInput       = "needs_input"
	EvAnswered         = "answered"
	EvPublished        = "published"
	EvJobStopped       = "job_stopped"
	EvSyncFailed       = "sync_failed"
)

// Event is one ring entry.
type Event struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// ObserveResponse is the fixed three-part body (04 §5.1a):
// {events, state_snapshot, next_cursor}, plus compacted=true when the
// cursor fell out of the ring (or the ring is empty after a restart) and
// only the snapshot can rebuild the observer's view.
type ObserveResponse struct {
	Events        []Event        `json:"events"`
	StateSnapshot map[string]any `json:"state_snapshot"`
	NextCursor    int64          `json:"next_cursor"`
	Compacted     bool           `json:"compacted"`
}

// Ring is one job's event ring.
type Ring struct {
	mu     sync.Mutex
	events []Event
	next   int64
}

// Append adds an event and returns its sequence number.
func (r *Ring) Append(typ string, payload map[string]any) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	ev := Event{Seq: r.next, Type: typ, Payload: payload}
	r.events = append(r.events, ev)
	if len(r.events) > ObserveRingCap {
		r.events = r.events[len(r.events)-ObserveRingCap:]
	}
	return ev.Seq
}

// Observe returns events strictly after `after` plus the snapshot rendered
// by the caller. Compacted reports that `after` predates the ring's oldest
// retained event (observer must rebuild from the snapshot and continue
// from next_cursor).
func (r *Ring) Observe(after int64, snapshot func() map[string]any) *ObserveResponse {
	r.mu.Lock()
	var out []Event
	oldest := int64(0)
	if len(r.events) > 0 {
		oldest = r.events[0].Seq
	}
	compacted := after+1 < oldest || (len(r.events) == 0 && after > 0)
	highest := after
	for _, e := range r.events {
		if e.Seq > after {
			out = append(out, e)
			if e.Seq > highest {
				highest = e.Seq
			}
		}
	}
	r.mu.Unlock()
	resp := &ObserveResponse{Events: out, NextCursor: highest, Compacted: compacted}
	if snapshot != nil {
		resp.StateSnapshot = snapshot()
	}
	if resp.Events == nil {
		resp.Events = []Event{}
	}
	return resp
}

// Latest returns the newest sequence number (0 when empty).
func (r *Ring) Latest() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}
