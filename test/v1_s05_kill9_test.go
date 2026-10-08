package test

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"
)

// v1_s05: the step process is killed with SIGKILL. The driver's step
// processes ARE the supervised units: the killed grill dispatch never
// writes its result envelope, the driver surfaces the envelope-less exit,
// and the recovery engine re-dispatches the step in place under the NEXT
// spawn ordinal — the v1.5 B2 rule: the ONLY identity change across a
// recovery is spawn_n++ — and the workflow completes and wraps inside the
// SAME session (T4: the session fingerprint never moves on same-machine
// recovery). Exactly one recovery for the kill (the storm guard) plus
// exactly the completion signal, and the derivation counters are asserted
// from the spawn journal.
func TestV1S05AgentKill9(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s05: agent killed mid-run",
		[]map[string]any{
			// Ordinal 1 delays its envelope write 2.5s: the deterministic
			// kill window (the envelope never lands).
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "ms": 2500},
			// Ordinal 2 is the crash continuation: grill again, in place.
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst, optUseDriver())
	d.start()
	d.pf(t, "submit", wi)
	waitSpawnCount(t, d, wi, 1, 30*time.Second)

	// The session fingerprint BEFORE the kill — T4's identity contract.
	sessBefore := inst.sessionOf(t, wi)
	if sessBefore.Fingerprint == "" {
		t.Fatal("running work item carries no session")
	}

	// SIGKILL the live step process — the cruelest death: no envelope, no
	// notice. (The status face carries no agent pid on the driver lane;
	// the spawn journal is the step-process ledger.)
	pid := latestSpawnPID(t, d, wi)
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

	// The re-dispatched workflow finished, and the terminal wiring wrapped.
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	// Exactly one recovery for the kill plus exactly the completion
	// signal (the storm guard; the wrap is the completed-exit translation).
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "needs_recovery"); n != 2 {
		t.Fatalf("needs_recovery fired %d times, want 2 (the kill + the completion signal): %v", n, kinds)
	}
	if n := countOf(kinds, "wi_agent_rederived"); n != 1 {
		t.Fatalf("re-derivation count %d, want 1: %v", n, kinds)
	}

	// The spawn journal: the kill consumed ordinal 1 and the recovery
	// re-dispatched grill in place as a RESUME under ordinal 2 (the B2
	// fence key), then the fresh dispatches of the tail.
	spawns := waitSpawnCount(t, d, wi, 5, 60*time.Second)
	if spawns[0]["resume"] != false || spawns[1]["resume"] != true {
		t.Fatalf("the recovery re-dispatch was not a resume: %+v %+v", spawns[0], spawns[1])
	}
	if n1, _ := spawns[0]["spawn_n"].(float64); n1 != 1 {
		t.Fatalf("first derivation counter wrong: %+v", spawns[0])
	}
	if n2, _ := spawns[1]["spawn_n"].(float64); n2 != 2 {
		t.Fatalf("second derivation counter wrong: %+v", spawns[1])
	}

	// The driver faces: the whole quick-fix chain done, grill's accepted
	// envelope is the CONTINUATION's (ordinal 2), and the sealed chain
	// carries the spawn journal.
	x := readExecution(t, d.capsuleDir(wi))
	for _, step := range []string{"grill", "fix", "verification", "release"} {
		if st := x.Steps[step]; st == nil || st.Status != "done" {
			t.Fatalf("step %s: %+v, want done", step, st)
		}
	}
	env2 := archivedEnvelope(t, d.capsuleDir(wi), "grill", 2)
	if env2["outcome"] != "completed" {
		t.Fatalf("grill's continuation envelope wrong: %v", env2["outcome"])
	}
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
