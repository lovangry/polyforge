package test

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"
)

// v1_s05: the agent is killed with SIGKILL. The observe lane detects the
// confirmed process exit, the recovery engine resumes from the surviving
// transcript (the discriminator: transcript readable → resume), the
// re-derived agent continues under the NEXT derivation counter — the
// v1.5 B2 rule: the ONLY identity change across a recovery is
// spawn_n++ — and finishes the work item inside the SAME session
// (T4: the session fingerprint never moves on same-machine recovery).
// Exactly one recovery (the storm guard), one re-derivation, and the
// derivation counters are asserted from the spawn journal.
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

	// The session fingerprint BEFORE the kill — T4's identity contract.
	sessBefore := inst.sessionOf(t, wi)
	if sessBefore.Fingerprint == "" {
		t.Fatal("running work item carries no session")
	}

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

	// T4 (B2): the recovery re-derived under a NEW counter but the SAME
	// session — same-machine recovery never changes session ownership.
	if sessAfter := inst.sessionOf(t, wi); sessAfter.Fingerprint != sessBefore.Fingerprint {
		t.Fatalf("same-machine recovery changed the session: %q -> %q",
			sessBefore.Fingerprint, sessAfter.Fingerprint)
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

	// The spawn journal: two derivations, the second a RESUME whose counter
	// advanced (spawn_n 1 → 2 — the B2 fence key; the #1-suffixed
	// spawn_token value is the LOCAL display token the runtime keeps for
	// the transcript path only, never a protocol identity).
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 2 {
		t.Fatalf("spawn journal has %d entries, want 2: %+v", len(spawns), spawns)
	}
	first, second := spawns[0], spawns[1]
	if first["resume"] != false || second["resume"] != true {
		t.Fatalf("second derivation was not a resume: %+v %+v", first, second)
	}
	if n1, _ := first["spawn_n"].(float64); n1 != 1 {
		t.Fatalf("first derivation counter wrong: %+v", first)
	}
	if n2, _ := second["spawn_n"].(float64); n2 != 2 {
		t.Fatalf("second derivation counter wrong: %+v", second)
	}

	// The resumed agent's transcript ride: the sealed chain carries the
	// session directory with the transcript the resume consumed.
	if !inst.bareHasPath(t, wi, "runtime-state/spawn-journal.jsonl") {
		t.Fatal("sealed chain misses the spawn journal")
	}
	// The whole run stayed inside ONE hub session — and the wrap cleared
	// it (T1's terminal half).
	if item, err := inst.client.GetWorkItem(wi); err != nil || item.Status != "wrapped" {
		t.Fatalf("final state: %+v %v", item, err)
	}
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
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
