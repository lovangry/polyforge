package test

import (
	"encoding/json"
	"strings"
	"syscall"
	"testing"
	"time"
)

// v1_s05: the agent is killed with SIGKILL. The observe lane detects the
// confirmed process exit, the recovery engine resumes from the surviving
// transcript (the discriminator: transcript readable → resume), the
// re-derived agent continues under the #N lineage id and finishes the
// work item. Exactly one recovery (the storm guard), one re-derivation,
// and the fenced lineage ids are asserted from the spawn journal.
func TestV1S05AgentKill9(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s05: agent killed mid-run",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		},
		[]map[string]any{{"do": "wrap"}}) // the crash continuation
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})

	// Let the first step land (its writes are the surviving state).
	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "first step never landed", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "step" && rec["phase"] == "done" {
				return true
			}
		}
		return false
	})

	// SIGKILL the agent — the cruelest death: no flush, no notice.
	pid := d.agentPIDOf(t, wi)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}

	// The observe lane detected the death and the runtime recovered.
	waitUntil(t, 30*time.Second, "no needs_recovery after the kill", func() bool {
		for _, e := range d.eventsOf(t, wi) {
			if e.Kind == "needs_recovery" {
				return true
			}
		}
		return false
	})
	// The recovery reason is the confirmed death.
	var reason string
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind == "needs_recovery" {
			var body struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(e.Body, &body)
			reason = body.Reason
		}
	}
	if reason != "agent_death" {
		t.Fatalf("recovery reason %q, want agent_death", reason)
	}

	// The re-derived agent finished the work item.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// Exactly one recovery signal (the storm guard) and one re-derivation.
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "needs_recovery"); n != 1 {
		t.Fatalf("needs_recovery fired %d times (storm guard broken): %v", n, kinds)
	}
	if n := countOf(kinds, "wi_agent_rederived"); n != 1 {
		t.Fatalf("re-derivation count %d, want 1: %v", n, kinds)
	}

	// The spawn journal: two derivations, the second a RESUME of the same
	// attempt with the #1 lineage id (the protocol fence).
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 2 {
		t.Fatalf("spawn journal has %d entries, want 2: %+v", len(spawns), spawns)
	}
	first, second := spawns[0], spawns[1]
	if first["resume"] != false || second["resume"] != true {
		t.Fatalf("second derivation was not a resume: %+v %+v", first, second)
	}
	if inst2, ok := second["instance"].(string); !ok || !strings.HasSuffix(inst2, "#1") {
		t.Fatalf("second derivation lineage id wrong: %+v", second)
	}

	// The resumed agent's transcript ride: the sealed chain carries the
	// session directory with the transcript the resume consumed.
	if !inst.bareHasPath(t, wi, "runtime-state/spawn-journal.jsonl") {
		t.Fatal("sealed chain misses the spawn journal")
	}
	// The whole run stayed inside ONE aihub generation.
	if item, err := inst.client.GetWorkItem(wi); err != nil || item.Status != "wrapped" {
		t.Fatalf("final state: %+v %v", item, err)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, e := range list {
		if e == s {
			n++
		}
	}
	return n
}
