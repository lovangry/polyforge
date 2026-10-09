package test

import (
	"testing"
	"time"
)

// v1_s21: the F15 progress-semantics regression, driver/envelope edition.
// The pilot's double-blind wedge (#46) was pinned by three shapes on the
// WI-Agent record stream; the driver-era faces re-express the fix's heart
// — the PROGRESS classification at the owner face — on the driver's step
// processes, whose records reach the monitor through the same production
// record tap:
//
//   - shape A (the on-machine incident's classification half): a step
//     process streaming extension_ui_request records (UI noise — NEVER
//     progress) while its workflow parks on an open question:
//     LAST-PROGRESS goes STALE through the noise.
//   - shape B (legal slow work): message_update deltas (the progress
//     family) keep LAST-PROGRESS FRESH.
//   - the parked question is designed stillness: with the question open
//     the watchdogs are exempt (the #99 pending-question exemption), so
//     neither shape fires a recovery, a probe or an incident — asserted
//     explicitly in both shapes.
//
// RETIRED SUB-ASSERTIONS (Phase-4 ticket polyforge#106): the legacy
// shape C (no turn + no progress + UI noise → the IDLE recovery fires
// exactly as without the noise) needed an agent that suppresses liveness
// probes while staying alive — a WI-Agent RPC control-channel shape. The
// driver bridge acks probes for every LIVE driver and the parked
// question exempts the idle path entirely, so the idle-recovery limb has
// no driver counterpart; it retires with this comment (deletion is
// #106's), not silently.
func TestV1S21F15ProgressSemantics(t *testing.T) {
	inst := newV1Install(t)
	d := newV1Daemon(t, inst, optDriverEngine(), optObserveWindows(2*time.Second, 2*time.Second, 2*time.Second, 0))
	d.start()

	// ------------------------------------------------------------------
	// shape A: UI noise never proves progress — LAST-PROGRESS goes stale
	// through it.

	wiA := inst.newWI(t, "s21-A: UI noise never proves progress (stale reads stale)",
		[]map[string]any{
			// The envelope action opens a stdout turn and streams
			// extension_ui_request records for 4s BEFORE the envelope
			// write: the step process stays alive and the UI records keep
			// flowing while LAST-PROGRESS (anchored on PROGRESS records
			// only) goes stale.
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": "q_s21a",
				"question":  "s21-A: park here (the test answers after the staleness pin).",
				"open_turn": true, "noise": "ui", "noise_ms": 50, "ms": 4000},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d.pf(t, "submit", wiA)
	capA := d.capsuleDir(wiA)
	// The boot turn is the stream's progress anchor; the UI noise that
	// follows never refreshes it. The staleness pin lands WHILE the
	// process is still alive and streaming (the 4s delay): LAST-PROGRESS
	// reads >= 2s while UI records keep arriving (the column is anchored
	// on PROGRESS records, not on record freshness — the pin the pilot
	// lacked for 2.7 hours).
	waitUntil(t, 15*time.Second, "shape A: LAST-PROGRESS never went stale through the UI noise", func() bool {
		for _, j := range d.status(t).Jobs {
			if j.WorkItemID == wiA && j.LastProgressAgoSec != nil && *j.LastProgressAgoSec >= 2 {
				return true
			}
		}
		return false
	})
	staleAt := time.Now()
	_ = staleAt
	waitQuestion(t, d, wiA, "q_s21a", 30*time.Second)
	// Designed stillness: NO recovery ever fires — not through the
	// streaming window, not through the park. (The pre-park window may
	// legitimately draw the hang_suspected incident and the F15c
	// no_progress owner event — the turn is in flight and the progress
	// clock is stale THROUGH the noise, which is the F15 classification
	// working as designed — but the probe proves liveness and the
	// recovery never comes; the parked question then exempts the
	// watchdogs entirely.)
	if n := countOf(d.eventKinds(t, wiA), "needs_recovery"); n != 0 {
		t.Fatalf("shape A: needs_recovery fired %d times against UI noise + a parked question: %v",
			n, d.eventKinds(t, wiA))
	}
	for _, e := range d.eventsOf(t, wiA) {
		if e.Kind == "infra_incident" {
			var b struct {
				Kind string `json:"kind"`
			}
			eventBody(t, e, &b)
			switch b.Kind {
			case "hang_suspected", "hang_cleared":
				// the suspicion through the noise, and its clearance when
				// the parked question exempted the criterion — the F15
				// classification at work; never a recovery.
			default:
				t.Fatalf("shape A: unexpected incident kind %q", b.Kind)
			}
		}
	}
	for _, rec := range fakeJournal(t, capA) {
		if rec["type"] == "control" && rec["method"] == "pf/probe" {
			t.Fatal("shape A: a liveness probe was sent against a parked question")
		}
	}
	// The owner answers; the workflow completes and the terminal wiring
	// wraps it (the noise's stillness never degraded the run).
	answerQuestion(t, d, wiA, "q_s21a", "Shape A done.")
	waitWorkflowStatus(t, d, wiA, "completed", 120*time.Second)
	inst.waitWIStatus(t, wiA, "wrapped", 60*time.Second)
	killOrphans(wiA)

	// ------------------------------------------------------------------
	// shape B: legal slow work — progress-family deltas keep
	// LAST-PROGRESS fresh.

	wiB := inst.newWI(t, "s21-B: progress deltas prove progress (fresh stays fresh)",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": "q_s21b",
				"question":  "s21-B: park here.",
				"open_turn": true, "noise": "progress", "noise_ms": 50, "ms": 6000},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	capB := d.capsuleDir(wiB)
	d.pf(t, "submit", wiB)
	t.Cleanup(func() { killOrphans(wiB) })

	// While the process is alive and the message_update deltas stream
	// (the 6s pre-write window), LAST-PROGRESS stays fresh — the deltas
	// ARE the progress family. Four hang windows' worth of streaming:
	// nothing fires.
	waitUntil(t, 15*time.Second, "shape B: the progress stream never started", func() bool {
		for _, rec := range fakeJournal(t, capB) {
			if rec["type"] == "ui_noise" && rec["kind"] == "progress" {
				return true
			}
		}
		return false
	})
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range d.status(t).Jobs {
			if j.WorkItemID == wiB {
				if j.LastProgressAgoSec != nil && *j.LastProgressAgoSec > 2 {
					t.Fatalf("shape B: LAST-PROGRESS stale (%vs) while progress deltas stream", *j.LastProgressAgoSec)
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, kind := range []string{"needs_recovery", "no_progress"} {
		if evs := eventsOfKind(t, d, wiB, kind); len(evs) != 0 {
			t.Fatalf("shape B: %s fired for a progressing stream: %s", kind, evs[0])
		}
	}
	for _, e := range d.eventsOf(t, wiB) {
		if e.Kind == "infra_incident" {
			var b struct {
				Kind string `json:"kind"`
			}
			eventBody(t, e, &b)
			t.Fatalf("shape B: a watchdog incident fired for a progressing stream: %s", b.Kind)
		}
	}
	for _, rec := range fakeJournal(t, capB) {
		if rec["type"] == "control" && rec["method"] == "pf/probe" {
			t.Fatal("shape B: a liveness probe was sent while progress streamed")
		}
	}
	// The envelope lands when the streaming window ends; the workflow
	// parks, the owner answers, the run completes (the stillness never
	// degraded anything).
	waitQuestion(t, d, wiB, "q_s21b", 30*time.Second)
	answerQuestion(t, d, wiB, "q_s21b", "Shape B done.")
	waitWorkflowStatus(t, d, wiB, "completed", 120*time.Second)
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
