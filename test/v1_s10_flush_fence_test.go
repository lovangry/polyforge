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

// v1_s10: the flush spawn_n fence (v1.5 B2). A zombie survivor of a
// superseded derivation (or a stale flush carrying an older counter)
// must never seal the live derivation's work: flush(wrap) with an OLD
// spawn_n answers DAEMON-409 STALE_SPAWN and never reaches the monitor;
// an ABSENT counter (0) is refused the same way, fail-closed; the
// CURRENT derivation's flush(wrap) succeeds and the job is undisturbed
// by the rejected ones. The dual-window half of the fence — a flush
// carrying the LATEST ANNOUNCED counter while the monitor's adoption
// still lags must be queued, not refused — is pinned per-derivation at
// the observe face (internal/polyforge/observe/flush_recovery_window_
// test.go); every recovery scenario here (s05/s06/s07/s16/s19) also
// exercises it live, since each re-derived agent's own first flush
// rides exactly that window.
func TestV1S10FlushSpawnFence(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s10: flush spawn_n fence",
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

	// The first derivation's counter is 1 (a freshly claimed capsule
	// seeds the counter from an empty spawn journal).
	oldSpawn := 0
	for _, j := range d.status(t).Jobs {
		if j.WorkItemID == wi {
			oldSpawn = j.SpawnN
		}
	}
	if oldSpawn == 0 {
		t.Fatalf("no spawn_n on the status job for %s", wi)
	}

	// Force one recovery so the counter advances: the OLD counter is the
	// one the zombie would still speak in, the CURRENT one is +1.
	if err := syscall.Kill(d.agentPIDOf(t, wi), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Wait for the ANNOUNCEMENT to be fully processed (monitor adoption),
	// not merely the spawn journal: the fence's supersession point is the
	// RTRecovered emission — before it, the old counter is genuinely
	// current (no successor announced) and a flush under it is
	// legitimate. Waiting on the journal alone races the announcement.
	waitForEvent(t, d, wi, "wi_agent_rederived", 30*time.Second)
	currentSpawn := oldSpawn + 1
	if got := d.observe(t, wi).SpawnN; got != currentSpawn {
		t.Fatalf("post-recovery derivation counter %d, want %d", got, currentSpawn)
	}

	// The stale flush: wrap in the OLD derivation's counter → DAEMON-409
	// STALE_SPAWN.
	_, err := d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, SpawnN: oldSpawn, Reason: protocol.FlushWrap,
		FinalStatus: "wrapped",
	})
	if err == nil {
		t.Fatal("stale flush(wrap) was accepted")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonConflict || !strings.Contains(ae.Detail, "STALE_SPAWN") {
		t.Fatalf("stale flush error wrong: %v", err)
	}

	// The absent counter (0) is refused fail-closed, exactly like a
	// mismatched one: a v1-era flush body (keyed on the deleted
	// attempt_id, no spawn_n) must never reach the monitor either.
	_, err = d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, SpawnN: 0, Reason: protocol.FlushWrap,
		FinalStatus: "wrapped",
	})
	if err == nil {
		t.Fatal("absent-counter flush(wrap) was accepted")
	}
	ae, ok = err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonConflict || !strings.Contains(ae.Detail, "STALE_SPAWN") {
		t.Fatalf("absent-counter flush error wrong: %v", err)
	}

	// The current derivation is undisturbed: the job still runs.
	if st := inst.wiStatus(t, wi); st != "running" {
		t.Fatalf("stale flush disturbed the job: status %q", st)
	}
	if d.agentPIDOf(t, wi) == 0 {
		t.Fatal("stale flush killed the agent")
	}

	// The CURRENT derivation's flush(wrap) seals the session's work.
	raw, err := d.client.Flush(wi, protocol.FlushRequest{
		WorkItem: wi, SpawnN: currentSpawn, Reason: protocol.FlushWrap,
		FinalStatus: "wrapped",
	})
	if err != nil {
		t.Fatalf("current-counter flush(wrap) rejected: %v", err)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(raw, &ack); err != nil || !ack.OK || !ack.Pushed {
		t.Fatalf("wrap flush ack wrong: %v (%s)", err, raw)
	}
	inst.waitWIStatus(t, wi, "wrapped", 30*time.Second)

	// And the fence held the whole time: one session, two derivations.
	if n := len(spawnJournal(t, d.capsuleDir(wi))); n != 2 {
		t.Fatalf("spawn journal has %d entries, want 2", n)
	}
	// The wrap cleared the session at aihub (m4: no holder on a
	// terminal work item).
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
	}
}
