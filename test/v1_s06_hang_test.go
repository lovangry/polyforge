package test

import (
	"testing"
	"time"
)

// v1_s06: the unresponsive step process, driver/envelope edition. The
// driver-era hang face is the envelope absence: the grill dispatch burns
// its injected delay (4s — four hang windows' worth of silence) and then
// dies WITHOUT writing its result envelope. The driver surfaces the
// envelope-less exit, the recovery engine re-dispatches the step in place
// under the next ordinal, and the workflow completes and wraps. The
// record-stream watchdogs stay quiet through the silence (no turn is in
// flight — the driver lane's step processes answer the boot prompt and
// close their turn; the injected windows prove the suspicion-free
// stillness), so exactly ONE recovery fires for the whole episode.
//
// This scenario runs against the REAL `pf daemon` process: the detection
// windows are injected purely through the CLI's observe-window flags
// (#12), so the whole production path — the spawner, the daemon's observe
// binding, the recovery engine's resume re-dispatch — is the thing under
// test. The legacy open-turn/record-stream shapes are the WI-Agent RPC
// face (retired pending Phase-4 #106): the driver lane supervises the
// step PROCESS (envelope absence), not the record stream.
func TestV1S06HangRecovery(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s06: unresponsive step process (the delayed envelope never lands)",
		[]map[string]any{
			// Ordinal 1: 4s of silence, then die before the envelope write.
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true,
				"ms": 4000, "crash": "before"},
			// Ordinal 2: the recovery continuation, in place.
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	// Injected windows: 1s hang / 1s probe — far shorter than the delayed
	// write, so the whole silence window is covered by the watchdogs'
	// cadence and they still must not fire.
	d := newV1Daemon(t, inst, optObserveWindows(time.Second, 0, time.Second, 0))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	capDir := d.capsuleDir(wi)
	// The crash is journaled by the step process itself (the scripted
	// before-write death) and the recovery re-dispatches in place.
	waitUntil(t, 30*time.Second, "the before-write crash was never journaled", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "crash_before" && rec["step"] == "grill" {
				return true
			}
		}
		return false
	})

	// Exactly one hang-lane recovery fires and its reason is the confirmed
	// death (the envelope-less exit IS an agent death to the monitor).
	rec := waitForEvent(t, d, wi, "needs_recovery", 60*time.Second)
	var body struct {
		Reason string `json:"reason"`
	}
	eventBody(t, rec, &body)
	if body.Reason != "agent_death" {
		t.Fatalf("recovery reason %q, want agent_death", body.Reason)
	}

	// The recovery engine resumed and the resumed workflow finished; the
	// terminal wiring wrapped it.
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)
	spawns := waitSpawnCount(t, d, wi, 5, 60*time.Second)
	if len(spawns) != 5 || spawns[1]["resume"] != true {
		t.Fatalf("the recovery was not an in-place resume: %+v", spawns)
	}

	// The whole episode fired exactly TWO recovery signals: the crash and
	// the completion signal — never a watchdog false positive against the
	// silent-but-live step process.
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "needs_recovery"); n != 2 {
		t.Fatalf("needs_recovery fired %d times, want 2 (crash + completion): %v", n, kinds)
	}
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "infra_incident" {
			var b struct {
				Kind string `json:"kind"`
			}
			eventBody(t, e, &b)
			t.Fatalf("a watchdog incident fired against the silent step process: %s", b.Kind)
		}
	}

	// Teardown: the killed process is gone, the survivor count is zero
	// (every later step process was terminated by its own driver after
	// envelope collection).
	waitUntil(t, 15*time.Second, "step processes survived the episode", func() bool {
		return len(orphanPIDs(wi)) == 0
	})
}
