package test

// v1_s17: the push-backpressure STEPS trigger, end to end with the REAL
// daemon riding PRODUCTION defaults. The daemon CLI exposes no
// backpressure flags (no injection seam exists, none is added), so the
// scenario lets the real ladder run itself out: 5 consecutive failed
// pushes spaced by the 10s PushRetryBackoff = ~40-50s to the event.
//
// The outage is a REAL stopped aihub process; the push path is the real
// git-over-HTTP lane. Mid-run the aihub is stopped; every observe push
// now fails against the dead TCP face and the monitor counts the
// failures. The degradation event (reason "steps", failed_pushes >= 5)
// must land while the agent -- the WHOLE point of the degradation lane --
// keeps running under the SAME pid with none of the recovery/respawn
// signals firing. After the aihub returns, the chain catches up: the
// versions written during the outage (local commits a failed push
// leaves behind) reach the server-side bare repo and a fresh published
// event lands.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestV1S17BackpressureSteps(t *testing.T) {
	inst := newV1Install(t)
	// The plan keeps producing versions through the outage window:
	// steps spaced by sleeps (~22x2.5s = ~55s), longer than the 5x10s
	// failed-push ladder needs, then parks (the agent stays alive --
	// degradation never kills, and the WI is closed by the scenario,
	// not by the plan).
	actions := []map[string]any{{"do": "step", "id": "warm"}}
	for i := 0; i < 22; i++ {
		actions = append(actions,
			map[string]any{"do": "step", "id": fmt.Sprintf("outage-%02d", i)},
			map[string]any{"do": "sleep", "ms": 2500},
		)
	}
	actions = append(actions, map[string]any{"do": "park"})
	wi := inst.newWI(t, "s17: push backpressure (steps trigger, aihub outage)",
		actions, nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	pid := d.agentPIDOf(t, wi)

	// The push path is proven healthy before the outage: at least one
	// version reached the aihub's bare repo.
	waitForEvent(t, d, wi, "published", 30*time.Second)

	// The outage: stop the REAL aihub mid-run. Pushes fail (one
	// push_failed per retry), the ladder climbs on the production 10s
	// backoff, and after 5 consecutive failures the degradation event
	// fires with reason "steps".
	inst.stopAihub()
	waitForEvent(t, d, wi, "push_failed", 30*time.Second)
	bp := waitForEvent(t, d, wi, "push_backpressure", 150*time.Second)
	var body struct {
		Reason       string `json:"reason"`
		FailedPushes int    `json:"failed_pushes"`
	}
	if err := json.Unmarshal(bp.Body, &body); err != nil {
		t.Fatalf("push_backpressure body: %v (%s)", err, bp.Body)
	}
	if body.Reason != "steps" {
		t.Fatalf("backpressure reason %q, want steps (body %s)", body.Reason, bp.Body)
	}
	if body.FailedPushes < 5 {
		t.Fatalf("failed_pushes %d, want >= 5 (the production default threshold)", body.FailedPushes)
	}

	// Degradation never kills the agent: same non-zero pid at the
	// moment the event is observed, and the episode shows none of the
	// recovery/respawn signals nor a new attempt instance.
	if now := d.agentPIDOf(t, wi); now == 0 || now != pid {
		t.Fatalf("agent pid changed across the degradation: was %d, now %d", pid, now)
	}
	kinds := d.eventKinds(t, wi)
	for _, bad := range []string{"wi_agent_rederived", "needs_recovery", "job_stopped"} {
		if n := countOf(kinds, bad); n != 0 {
			t.Fatalf("%s fired %d times during the outage: %v", bad, n, kinds)
		}
	}
	if n := countOf(kinds, "job_started"); n != 1 {
		t.Fatalf("job_started fired %d times, want 1 (no new attempt instance): %v", n, kinds)
	}
	if n := countOf(kinds, "push_failed"); n < 5 {
		t.Fatalf("push_failed fired %d times during the outage, want >= 5: %v", n, kinds)
	}

	// The catch-up baseline: what the agent produced during the outage
	// lives as local commits in the capsule's chain. Bring the aihub
	// back and demand the server-side bare repo reach that history plus
	// a fresh published event -- the failed pushes were never lost
	// work.
	local := capsuleCommitCount(t, d.dataDir, wi)
	if local == 0 {
		t.Fatal("no local commits after the outage episode")
	}
	publishedBefore := countOf(kinds, "published")
	inst.restartAihub()
	inst.waitBareCommits(t, wi, local, 60*time.Second)
	waitUntil(t, 60*time.Second, "no published event after the recovery push", func() bool {
		return countOf(d.eventKinds(t, wi), "published") > publishedBefore
	})

	// The agent is STILL the same live process after the recovery.
	if now := d.agentPIDOf(t, wi); now == 0 || now != pid {
		t.Fatalf("agent pid changed across the recovery: was %d, now %d", pid, now)
	}
}
