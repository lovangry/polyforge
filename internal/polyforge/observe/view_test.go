package observe

// view_test.go: the events-file JSONL shape.

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventsFileShapeIsJSONLEventEntry(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	// a driver event lands in the same durable log through the daemon's
	// binding (the driver's EventLogger IS EventLog.Append)
	if err := m.log.Append("wi_test", "needs_input", map[string]any{"question_id": "q_file", "step": "code"}); err != nil {
		t.Fatal(err)
	}

	// The durable record: every ring entry exists as ONE JSONL line in
	// the daemon data dir, parseable as protocol.EventEntry.
	soon(t, time.Second, "needs_input event persisted", func() bool {
		entries, err := ReadEventsFile(m.log.Path())
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.Kind == "needs_input" && e.WI == "wi_test" && e.At != "" {
				return true
			}
		}
		return false
	})
	entries, err := ReadEventsFile(m.log.Path())
	if err != nil {
		t.Fatal(err)
	}
	var started bool
	for _, e := range entries {
		switch e.Kind {
		case EvJobStarted:
			started = true
		case "needs_input":
			var b struct {
				QuestionID string `json:"question_id"`
			}
			if err := json.Unmarshal(e.Body, &b); err != nil || b.QuestionID != "q_file" {
				t.Fatalf("needs_input body: %s %v", e.Body, err)
			}
		}
		if e.WI != "wi_test" {
			t.Fatalf("event must carry its work item: %+v", e)
		}
	}
	if !started {
		t.Fatal("job_started must be the first ring/file entry")
	}
}

// The ring is bounded: 500 entries per WI, oldest dropped.
func TestEventRingBounded(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	for i := 0; i < RingCap+50; i++ {
		if err := m.log.Append("wi_ring", "t_event", nil); err != nil {
			t.Fatal(err)
		}
	}
	// The ring-bound pin goes through a cursor inside the ring window
	// (the ring answers): EventLog.Since was deleted with the audit's
	// dead-code sweep, and a seq floor of 0 would serve the durable
	// files instead once the ring has rotated.
	if got := len(m.log.SinceAfter("wi_ring", 50)); got != RingCap {
		t.Fatalf("ring size: got %d, want %d", got, RingCap)
	}
}
