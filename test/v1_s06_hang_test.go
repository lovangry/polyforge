package test

import (
	"testing"
	"time"
)

// v1_s06: the hang watchdog. The agent opens a turn and goes silent —
// no records, no probe ack. The monitor's ruled criterion (silence
// WHILE A TURN IS IN FLIGHT) arms, the liveness probe is sent and times
// out, and exactly one WINeedsRecovery(hang) fires; the recovery
// engine resumes from the transcript, the re-derived agent finishes and
// the work item wraps. The idle agent (s01's parked one) is never a
// hang — only the in-flight silence is.
//
// This scenario runs against the REAL `pf daemon` process: the detection
// windows are injected purely through the CLI's observe-window flags
// (#12: --hang-window / --probe-timeout), so the whole production path —
// the spawner's record tap, the daemon's observe binding, the control
// channel the liveness probe rides, the recovery engine's resume spawn —
// is the thing under test. The idle and answer-ack windows are left at
// their production defaults (no flag): only the hang lane may fire here.
func TestV1S06HangRecovery(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s06: hung agent (in-flight silence)",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "hang"},
		},
		[]map[string]any{{"do": "wrap"}})
	// Injected windows: 1s of in-flight silence arms the watchdog, the
	// probe gets 1s to prove liveness.
	d := newV1Daemon(t, inst, optObserveWindows(time.Second, 0, time.Second, 0))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "first step never landed", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "step" && rec["phase"] == "done" {
				return true
			}
		}
		return false
	})

	// The watchdog suspects, probes, and the probe times out (the fake
	// suppresses probe acks while hanging — journaled).
	suspect := waitForEvent(t, d, wi, "infra_incident", 30*time.Second)
	var sb struct {
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	}
	eventBody(t, suspect, &sb)
	if sb.Kind != "hang_suspected" {
		t.Fatalf("first incident %q, want hang_suspected", sb.Kind)
	}
	// The probe really reached the agent and was really ignored.
	waitUntil(t, 15*time.Second, "the liveness probe never reached the hung agent", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "control" && rec["method"] == "pf/probe" && rec["suppressed"] == true {
				return true
			}
		}
		return false
	})

	// Exactly one hang recovery fires and its reason names the hang.
	rec := waitForEvent(t, d, wi, "needs_recovery", 30*time.Second)
	var body struct {
		Reason string `json:"reason"`
	}
	eventBody(t, rec, &body)
	if body.Reason != "hang" {
		t.Fatalf("recovery reason %q, want hang", body.Reason)
	}

	// The recovery engine resumed and the resumed agent finished.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 2 || spawns[1]["resume"] != true {
		t.Fatalf("hang recovery was not a resume: %+v", spawns)
	}

	// The whole episode fired ONE recovery (the recovered agent's new
	// stream resets the detection surfaces).
	var recoveries int
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "needs_recovery" {
			recoveries++
		}
	}
	if recoveries != 1 {
		t.Fatalf("needs_recovery fired %d times, want 1", recoveries)
	}
}
