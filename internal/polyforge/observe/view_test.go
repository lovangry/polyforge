package observe

// view_test.go: the JobView projection (two layers — execution.json
// direct read + events ring) and the events-file JSONL shape.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// armQuestion records an open pending question through execution.json
// (the driver's own write; the monitor's poll projects it).
func armQuestion(t *testing.T, m *Monitor, qid string) {
	t.Helper()
	writePendingQuestion(t, m, qid, "pick one", "code")
}

func TestJobViewProjectionFromExecutionFile(t *testing.T) {
	h := newFakeHandle("att_7")
	m, _ := newTestMonitor(t, h, nil)
	m.writeExec(t, 3, "code")

	// Arm a pending question so the status derivation sees needs_input.
	armQuestion(t, m, "q_view")

	soon(t, time.Second, "execution.json polled", func() bool {
		v := m.JobView()
		return v.CurrentStep == "code"
	})

	v := m.JobView()
	if v.WorkItem != "wi_test" || v.Project != "proj" || v.BundleName != "bundle-a" {
		t.Fatalf("wi identity: %+v", v)
	}
	if v.SpawnN != 1 {
		t.Fatalf("spawn: %d", v.SpawnN)
	}
	if v.Status != protocol.JobNeedsInput {
		t.Fatalf("status: got %s, want %s (open pending question)", v.Status, protocol.JobNeedsInput)
	}
	if v.Pending == nil || v.Pending.ID != "q_view" {
		t.Fatalf("pending projection: %+v", v.Pending)
	}
	sv, ok := v.Steps["code"]
	if !ok {
		t.Fatal("steps map must carry the execution.json step")
	}
	if sv.Status != "in_progress" || sv.Attempts != 0 {
		t.Fatalf("step view: %+v", sv)
	}
	if len(v.Budget) == 0 {
		t.Fatal("budget must be projected from budget_consumed")
	}
	if len(v.Events) == 0 {
		t.Fatal("events layer must carry the ring entries")
	}
	// The events layer's entries are wire-shaped protocol.EventEntry.
	for _, e := range v.Events {
		if e.Kind == "" || e.At == "" {
			t.Fatalf("malformed event entry: %+v", e)
		}
	}

	// Consume the question: the direct-read layer is authoritative, and
	// the monitor reconciles its projection from it.
	m.stateMu.Lock()
	m.pending = nil
	m.stateMu.Unlock()
	if got := m.JobView().Status; got != protocol.JobRunning {
		t.Fatalf("status after clearing pending: %s", got)
	}
}

func TestJobViewDegradesWithoutExecutionFile(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	v := m.JobView()
	if v.Status != protocol.JobRunning {
		t.Fatalf("status: %s", v.Status)
	}
	if v.Steps != nil && len(v.Steps) > 0 {
		t.Fatal("no execution.json → no step detail, but the view stays available")
	}
}

func TestEventsFileShapeIsJSONLEventEntry(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q_file")

	// The durable record: every ring entry exists as ONE JSONL line in
	// the daemon data dir, parseable as protocol.EventEntry.
	soon(t, time.Second, "needs_input event persisted", func() bool {
		entries, err := ReadEventsFile(m.log.Path())
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.Kind == EvNeedsInput && e.WI == "wi_test" && e.At != "" {
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
		case EvNeedsInput:
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
		if err := m.log.Append("wi_ring", EvInfraIncident, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(m.log.Since("wi_ring")); got != RingCap {
		t.Fatalf("ring size: got %d, want %d", got, RingCap)
	}
}
