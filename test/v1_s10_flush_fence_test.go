package test

import (
	"encoding/json"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// v1_s10: the flush attempt-id fence. A zombie survivor of a superseded
// instance (or a stale flush from an older lineage id) must never seal
// the live attempt's work: flush(wrap) with an OLD instance id answers
// DAEMON-409 STALE_ATTEMPT and never reaches the monitor, while the
// CURRENT instance's flush(wrap) succeeds and the job is undisturbed by
// the rejected one.
func TestV1S10FlushAttemptFence(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s10: flush attempt-id fence",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		},
		[]map[string]any{{"do": "park"}})
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	t.Cleanup(func() { killOrphans(wi) })

	// Force one recovery so the lineage advances: the OLD instance id is
	// the bare attempt id, the CURRENT one is attempt#1.
	oldInstance := ""
	for _, j := range d.status(t).Jobs {
		if j.WorkItemID == wi {
			oldInstance = j.AttemptID
		}
	}
	if oldInstance == "" {
		t.Fatalf("no job for %s", wi)
	}
	if err := syscall.Kill(d.agentPIDOf(t, wi), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, "lineage never advanced to #1", func() bool {
		spawns := spawnJournal(t, d.capsuleDir(wi))
		return len(spawns) == 2
	})
	currentInstance := oldInstance + "#1"

	// The stale flush: wrap in the OLD instance's name → DAEMON-409.
	_, err := d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, AttemptID: oldInstance, Reason: protocol.FlushWrap,
		FinalStatus: "wrapped",
	})
	if err == nil {
		t.Fatal("stale flush(wrap) was accepted")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonConflict || !strings.Contains(ae.Detail, "STALE_ATTEMPT") {
		t.Fatalf("stale flush error wrong: %v", err)
	}

	// The current instance is undisturbed: the job still runs.
	if st := inst.wiStatus(t, wi); st != "running" {
		t.Fatalf("stale flush disturbed the job: status %q", st)
	}
	if d.agentPIDOf(t, wi) == 0 {
		t.Fatal("stale flush killed the agent")
	}

	// The CURRENT instance's flush(wrap) seals the attempt.
	raw, err := d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, AttemptID: currentInstance, Reason: protocol.FlushWrap,
		FinalStatus: "wrapped",
	})
	if err != nil {
		t.Fatalf("current-instance flush(wrap) rejected: %v", err)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(raw, &ack); err != nil || !ack.OK || !ack.Pushed {
		t.Fatalf("wrap flush ack wrong: %v (%s)", err, raw)
	}
	inst.waitWIStatus(t, wi, "wrapped", 30*time.Second)

	// And the fence held the whole time: exactly one generation was
	// sealed, one recovery, two derivations.
	if n := len(spawnJournal(t, d.capsuleDir(wi))); n != 2 {
		t.Fatalf("spawn journal has %d entries, want 2", n)
	}
}
