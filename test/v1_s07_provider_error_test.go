package test

import (
	"testing"
	"time"
)

// v1_s07: provider errors ride the ordinary record stream
// (stopReason=error on the assistant message records — there is no
// type:"error" record). Two consecutive error turns arm the storm guard
// (chain ≥ 2 in stream order), the monitor fires exactly ONE
// WINeedsRecovery, the recovery engine resumes once, and the resumed
// agent finishes the work item. The re-derived old handle's exit (the
// recovery kills it) must NOT raise a second recovery — the
// "恢复一次而不是连杀" pin.
//
// This scenario runs through the in-process rig: the REAL `pf daemon`
// binary's record-stream binding is currently broken (daemon.New copies
// the runtime config into the spawner BEFORE setting RecordTap, so
// production-spawned agents' records never reach Observe — see the
// Real Bugs section of the lane report), and provider-error detection
// is exactly the face that blindness kills. The rig wires the same
// production components in the correct order; the scenario is the
// end-to-end pin of the detection machinery.
func TestV1S07ProviderErrorStorm(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s07: consecutive provider errors",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "provider_error", "count": 2},
			{"do": "park"},
		},
		[]map[string]any{{"do": "wrap"}})
	rig := newInprocRig(t, inst, 0, 0) // production detection windows
	t.Cleanup(func() { killOrphans(wi) })
	rig.start(wi)

	// The incidents are classified and journaled immediately; the
	// recovery decision fires after the second consecutive one.
	rec := rig.waitEvent(t, wi, "needs_recovery", 60*time.Second)
	var body struct {
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	eventBody(t, rec, &body)
	if body.Reason != "provider_error" {
		t.Fatalf("recovery reason %q, want provider_error", body.Reason)
	}

	// Both queued incidents surfaced as infra_incident events before the
	// recovery decision, classified as provider_error.
	var incidents []string
	for _, e := range rig.eventsOf(t, wi) {
		if e.Kind != "infra_incident" {
			continue
		}
		var ib struct {
			Kind string `json:"kind"`
		}
		eventBody(t, e, &ib)
		incidents = append(incidents, ib.Kind)
	}
	if len(incidents) < 2 {
		t.Fatalf("classified incidents %v, want ≥2", incidents)
	}
	for _, k := range incidents {
		if k != "provider_error" {
			t.Fatalf("incident kind %q, want provider_error", k)
		}
	}

	// The resumed agent finished.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// ONE recovery for the whole storm: the re-derived old handle's exit
	// never re-armed the detector (the storm guard), and the new agent
	// ran clean.
	var recoveries, rederives int
	for _, e := range rig.eventsOf(t, wi) {
		switch e.Kind {
		case "needs_recovery":
			recoveries++
		case "wi_agent_rederived":
			rederives++
		}
	}
	if recoveries != 1 {
		t.Fatalf("needs_recovery fired %d times, want 1", recoveries)
	}
	if rederives != 1 {
		t.Fatalf("re-derivation count %d, want 1", rederives)
	}
	// Two derivations only: the original + the one resume.
	if n := len(spawnJournal(t, rig.capsuleDir(wi))); n != 2 {
		t.Fatalf("spawn journal has %d entries, want 2", n)
	}
}
