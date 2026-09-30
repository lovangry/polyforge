package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// v1_s21: the F15 progress-semantics regression — the pilot's
// double-blind wedge (#46, two identical on-machine incidents) pinned as
// three shapes. A real WI agent dispatched a step subagent whose model
// call hung; the agent's turn stayed OPEN while the harness's stdout
// kept streaming extension-UI records continuously. Under the old
// "any record refreshes the heartbeat" rule both watchdogs were blind:
// the hang criterion (silence with a turn open) never armed because the
// noise kept lastRecordAt fresh, and the idle criterion (no record, no
// turn) never armed because the turn was open. The wedge lasted 2.7
// hours with zero events and no owner-visible surface.
//
// The F15 fix: only the turn-machinery record families (message
// family, toolResult family, lifecycle, response echo) prove PROGRESS;
// extension_ui_request and every unclassified record are NOISE and
// never refresh the watchdogs' clock. Plus the F15c owner face: the
// same progress silence lands a no_progress event and the ops status
// jobs table grows a LAST-PROGRESS column.
//
// The three shapes (all against the REAL `pf daemon`, windows injected
// through the CLI flags — hang 2s / idle 2s / probe 2s):
//
//   - shape A (the on-machine incident; RED before the fix): turn open
//   - continuous UI noise + no progress → hang probe → no ack → ONE
//     hang recovery; the resumed agent wraps.
//   - shape B (legal slow work): turn open + ordinary progress deltas
//     streaming → nothing fires, ever.
//   - shape C: no turn + no progress + UI noise → the idle path fires
//     exactly as it would without the noise (before the fix the noise
//     held the idle window open too).
func TestV1S21F15ProgressSemantics(t *testing.T) {
	inst := newV1Install(t)
	d := newV1Daemon(t, inst, optObserveWindows(2*time.Second, 2*time.Second, 2*time.Second, 0))
	d.start()

	// ------------------------------------------------------------------
	// shape A: the on-machine incident.

	wiA := inst.newWI(t, "s21-A: open turn + UI noise (double-blind hang)",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "open_turn"},
			{"do": "ui_noise", "ms": 50},
			{"do": "park"},
		},
		[]map[string]any{{"do": "wrap"}})
	d.pf(t, "submit", wiA)
	capA := d.capsuleDir(wiA)

	waitUntil(t, 30*time.Second, "shape A: first step never landed", func() bool {
		return journalHas(t, capA, "step", "done")
	})
	waitUntil(t, 15*time.Second, "shape A: the turn never opened / noise never started", func() bool {
		return journalKind(t, capA, "open_turn") && journalKind(t, capA, "ui_noise")
	})

	// The F15c owner face through the noise: LAST-PROGRESS goes stale
	// even though records keep arriving (the column is anchored on
	// progress, not on record freshness — this is the pin the pilot
	// lacked for 2.7 hours).
	waitUntil(t, 15*time.Second, "shape A: LAST-PROGRESS never went stale through the UI noise", func() bool {
		for _, j := range d.status(t).Jobs {
			if j.WorkItemID == wiA && j.LastProgressAgoSec != nil && *j.LastProgressAgoSec >= 1 {
				return true
			}
		}
		return false
	})

	// The hang watchdog arms THROUGH the noise: suspects, probes, the
	// probe reaches the stuck agent and is ignored (journaled).
	suspectA := waitForEvent(t, d, wiA, "infra_incident", 30*time.Second)
	var sbA struct {
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	}
	eventBody(t, suspectA, &sbA)
	if sbA.Kind != "hang_suspected" {
		t.Fatalf("shape A: first incident %q, want hang_suspected", sbA.Kind)
	}
	waitUntil(t, 15*time.Second, "shape A: the liveness probe never reached the stuck agent", func() bool {
		for _, rec := range fakeJournal(t, capA) {
			if rec["type"] == "control" && rec["method"] == "pf/probe" && rec["suppressed"] == true {
				return true
			}
		}
		return false
	})

	// Exactly one hang recovery; the no_progress owner event landed too.
	recA := waitForEvent(t, d, wiA, "needs_recovery", 30*time.Second)
	var bodyA struct {
		Reason string `json:"reason"`
	}
	eventBody(t, recA, &bodyA)
	if bodyA.Reason != "hang" {
		t.Fatalf("shape A: recovery reason %q, want hang", bodyA.Reason)
	}
	if evs := eventsOfKind(t, d, wiA, "needs_recovery"); len(evs) != 1 {
		t.Fatalf("shape A: needs_recovery fired %d times, want 1", len(evs))
	}
	waitForEvent(t, d, wiA, "no_progress", 30*time.Second)

	// The recovery engine resumed and the resumed agent finished.
	inst.waitWIStatus(t, wiA, "wrapped", 60*time.Second)
	if spawns := spawnJournal(t, capA); len(spawns) != 2 || spawns[1]["resume"] != true {
		t.Fatalf("shape A: hang recovery was not a resume: %+v", spawns)
	}
	killOrphans(wiA)

	// ------------------------------------------------------------------
	// shape B: legal slow work — a turn streaming progress deltas must
	// never arm any watchdog.

	wiB := inst.newWI(t, "s21-B: open turn + slow progress (no trigger)",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "open_turn"},
			{"do": "ui_noise", "kind": "progress", "ms": 100},
			{"do": "park"},
		}, nil)
	capB := d.capsuleDir(wiB)
	d.pf(t, "submit", wiB)
	t.Cleanup(func() { killOrphans(wiB) })

	waitUntil(t, 30*time.Second, "shape B: first step never landed", func() bool {
		return journalHas(t, capB, "step", "done")
	})
	waitUntil(t, 15*time.Second, "shape B: the progress stream never started", func() bool {
		for _, rec := range fakeJournal(t, capB) {
			if rec["type"] == "ui_noise" && rec["kind"] == "progress" {
				return true
			}
		}
		return false
	})

	// Four full hang/idle windows of a progressing turn: nothing fires,
	// no probe is sent, LAST-PROGRESS stays fresh.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range d.status(t).Jobs {
			if j.WorkItemID == wiB {
				if j.LastProgressAgoSec == nil || *j.LastProgressAgoSec > 1 {
					t.Fatalf("shape B: LAST-PROGRESS stale (%v) while progress streams", j.LastProgressAgoSec)
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, kind := range []string{"needs_recovery", "no_progress"} {
		if evs := eventsOfKind(t, d, wiB, kind); len(evs) != 0 {
			t.Fatalf("shape B: %s fired for a progressing turn: %s", kind, evs[0])
		}
	}
	for _, e := range d.eventsOf(t, wiB) {
		if e.Kind == "infra_incident" {
			var b struct {
				Kind string `json:"kind"`
			}
			eventBody(t, e, &b)
			if b.Kind == "hang_suspected" || b.Kind == "idle_suspect" {
				t.Fatalf("shape B: watchdog suspected a progressing turn: %s", b.Kind)
			}
		}
	}
	for _, rec := range fakeJournal(t, capB) {
		if rec["type"] == "control" && rec["method"] == "pf/probe" {
			t.Fatal("shape B: a liveness probe was sent while progress streamed")
		}
	}

	// ------------------------------------------------------------------
	// shape C: no turn, no progress, UI noise — the idle path fires
	// exactly as it would without the noise.

	wiC := inst.newWI(t, "s21-C: no turn, no progress, UI noise (idle)",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "ui_noise", "ms": 50},
			{"do": "park"},
		},
		[]map[string]any{{"do": "wrap"}})
	// The stuck-session shape must not ack liveness probes: rewrite the
	// plan with ack_probe=false (the harness's newWI helper carries no
	// plan-mode knobs).
	planC := map[string]any{
		"actions": []map[string]any{
			{"do": "step", "id": "code"},
			{"do": "ui_noise", "ms": 50},
			{"do": "park"},
		},
		"resume_actions": []map[string]any{{"do": "wrap"}},
		"ack_probe":      false,
	}
	blob, err := json.Marshal(planC)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.planDir, wiC+".json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	capC := d.capsuleDir(wiC)
	d.pf(t, "submit", wiC)
	t.Cleanup(func() { killOrphans(wiC) })

	waitUntil(t, 30*time.Second, "shape C: first step never landed", func() bool {
		return journalHas(t, capC, "step", "done")
	})
	waitUntil(t, 15*time.Second, "shape C: the noise never started", func() bool {
		return journalKind(t, capC, "ui_noise")
	})

	suspectC := waitForEvent(t, d, wiC, "infra_incident", 30*time.Second)
	var sbC struct {
		Kind string `json:"kind"`
	}
	eventBody(t, suspectC, &sbC)
	if sbC.Kind != "idle_suspect" {
		t.Fatalf("shape C: first incident %q, want idle_suspect", sbC.Kind)
	}
	recC := waitForEvent(t, d, wiC, "needs_recovery", 30*time.Second)
	var bodyC struct {
		Reason string `json:"reason"`
	}
	eventBody(t, recC, &bodyC)
	if bodyC.Reason != "idle" {
		t.Fatalf("shape C: recovery reason %q, want idle", bodyC.Reason)
	}
	if evs := eventsOfKind(t, d, wiC, "needs_recovery"); len(evs) != 1 {
		t.Fatalf("shape C: needs_recovery fired %d times, want 1", len(evs))
	}
	waitForEvent(t, d, wiC, "no_progress", 30*time.Second)
	inst.waitWIStatus(t, wiC, "wrapped", 60*time.Second)
	if spawns := spawnJournal(t, capC); len(spawns) != 2 || spawns[1]["resume"] != true {
		t.Fatalf("shape C: idle recovery was not a resume: %+v", spawns)
	}
}

// journalHas reports whether the fake agent's journal carries one step
// record with the given phase.
func journalHas(t *testing.T, capsuleDir string, typ, phase string) bool {
	t.Helper()
	for _, rec := range fakeJournal(t, capsuleDir) {
		if rec["type"] == typ && rec["phase"] == phase {
			return true
		}
	}
	return false
}

// journalKind reports whether the fake agent's journal carries one
// record of the given type.
func journalKind(t *testing.T, capsuleDir, typ string) bool {
	t.Helper()
	for _, rec := range fakeJournal(t, capsuleDir) {
		if rec["type"] == typ {
			return true
		}
	}
	return false
}

// eventsOfKind reads the daemon's durable events.log entries of one
// work item, filtered by kind.
func eventsOfKind(t *testing.T, d *v1Daemon, wi, kind string) []string {
	t.Helper()
	var out []string
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == kind {
			out = append(out, string(e.Body))
		}
	}
	return out
}
