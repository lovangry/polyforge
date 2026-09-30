package test

// v1_s18: the push-backpressure TIME trigger. The real daemon rides a
// 30min BackpressureAfter, which no e2e can wait out — but the inproc
// rig assembles the SAME production components (runtime, observe,
// DefaultSpawner, the real UDS routes) with an injectable observe
// config, so the window shrinks to seconds here: BackpressureAfter=3s
// and BackpressureSteps raised far out of reach, so the time bound is
// what fires. The pushes themselves stay REAL: GitRemoteBase points at
// the REAL aihub process's git face, and the outage is that process
// being genuinely stopped mid-run.
//
// Same assertion depth as s17, per trigger: the push_backpressure event
// carries reason "time"; the agent survives the whole episode under the
// same non-zero PID with no recovery/respawn signal and no new attempt
// instance; after the aihub returns the chain catches up (the bare repo
// reaches the outage-written local commits) and a fresh published event
// lands.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestV1S18BackpressureTime(t *testing.T) {
	inst := newV1Install(t)
	// The plan keeps producing versions through the (short) outage
	// window, then parks — the agent stays alive; degradation never
	// kills and the scenario closes the WI itself.
	actions := []map[string]any{{"do": "step", "id": "warm"}}
	for i := 0; i < 8; i++ {
		actions = append(actions,
			map[string]any{"do": "step", "id": fmt.Sprintf("outage-%02d", i)},
			map[string]any{"do": "sleep", "ms": 1000},
		)
	}
	actions = append(actions, map[string]any{"do": "park"})
	wi := inst.newWI(t, "s18: push backpressure (time trigger, inproc rig)",
		actions, nil)
	// The injected ladder: the time window is 3s, the steps threshold
	// is far out of reach (1000), so the time bound fires first.
	rig := newInprocRig(t, inst, 0, 0, optBackpressure(3*time.Second, 1000))
	t.Cleanup(func() { killOrphans(wi) })
	rig.start(wi)

	agentPID := func() int {
		ji, ok := rig.rt.Info(wi)
		if !ok || ji.Agent == nil {
			return 0
		}
		return ji.AgentPID
	}
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return agentPID() > 0
	})
	pid := agentPID()

	// The push path is proven healthy before the outage: at least one
	// version reached the aihub's bare repo over the real git face.
	rig.waitEvent(t, wi, "published", 30*time.Second)

	// The outage: stop the REAL aihub. The next push fails (the
	// push_failed ledger opens the episode), and 3s after the last
	// successful push the degradation event fires with reason "time".
	inst.stopAihub()
	rig.waitEvent(t, wi, "push_failed", 30*time.Second)
	bp := rig.waitEvent(t, wi, "push_backpressure", 30*time.Second)
	var body struct {
		Reason       string `json:"reason"`
		FailedPushes int    `json:"failed_pushes"`
	}
	if err := json.Unmarshal(bp.Body, &body); err != nil {
		t.Fatalf("push_backpressure body: %v (%s)", err, bp.Body)
	}
	if body.Reason != "time" {
		t.Fatalf("backpressure reason %q, want time (body %s)", body.Reason, bp.Body)
	}
	if body.FailedPushes < 1 {
		t.Fatalf("failed_pushes %d, want >= 1 (the episode is open)", body.FailedPushes)
	}

	// Degradation never kills the agent: same non-zero PID at the
	// moment the event is observed, and the episode shows none of the
	// recovery/respawn signals nor a new attempt instance.
	if now := agentPID(); now == 0 || now != pid {
		t.Fatalf("agent pid changed across the degradation: was %d, now %d", pid, now)
	}
	eventKinds := func() []string {
		var kinds []string
		for _, e := range rig.eventsOf(t, wi) {
			kinds = append(kinds, e.Kind)
		}
		return kinds
	}
	kinds := eventKinds()
	for _, bad := range []string{"wi_agent_rederived", "needs_recovery", "job_stopped"} {
		if n := countOf(kinds, bad); n != 0 {
			t.Fatalf("%s fired %d times during the outage: %v", bad, n, kinds)
		}
	}
	if n := countOf(kinds, "job_started"); n != 1 {
		t.Fatalf("job_started fired %d times, want 1 (no new attempt instance): %v", n, kinds)
	}

	// The catch-up baseline: what the agent produced during the outage
	// lives as local commits in the capsule's chain. Bring the aihub
	// back and demand the server-side bare repo reach that history plus
	// a fresh published event.
	local := capsuleCommitCount(t, rig.dataDir, wi)
	if local == 0 {
		t.Fatal("no local commits after the outage episode")
	}
	publishedBefore := countOf(kinds, "published")
	inst.restartAihub()
	inst.waitBareCommits(t, wi, local, 60*time.Second)
	waitUntil(t, 60*time.Second, "no published event after the recovery push", func() bool {
		return countOf(eventKinds(), "published") > publishedBefore
	})

	// The agent is STILL the same live process after the recovery.
	if now := agentPID(); now == 0 || now != pid {
		t.Fatalf("agent pid changed across the recovery: was %d, now %d", pid, now)
	}
}
