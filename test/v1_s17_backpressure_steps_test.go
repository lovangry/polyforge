package test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// v1_s17: the push-backpressure STEPS trigger, end to end with the REAL
// daemon riding PRODUCTION defaults, driver/envelope edition. The daemon
// CLI exposes no backpressure flags (no injection seam exists, none is
// added), so the scenario lets the real ladder run itself out: 5
// consecutive failed pushes spaced by the 10s PushRetryBackoff = ~40-50s
// to the event.
//
// The version stream that drives the ladder is the DRIVER's own write
// face: each owner answer is a transition (the D9 archive-and-clear write
// plus the re-dispatch's step_start), so the parked question/answer cycle
// keeps execution.json's Version climbing through the outage window —
// every bump re-arms the observe push round. The outage is a REAL stopped
// aihub process; the push path is the real git-over-HTTP lane. The
// degradation event (reason "steps", failed_pushes >= 5) must land while
// the driver keeps working — no recovery/respawn signal fires. After the
// aihub returns, the chain catches up (the versions written during the
// outage reach the server-side bare repo) and a fresh published event
// lands.
func TestV1S17BackpressureSteps(t *testing.T) {
	inst := newV1Install(t)
	// The plan: a long parked question/answer ladder (the version stream),
	// then the completion tail. Each answer consumes exactly one ordinal:
	// answer k re-dispatches grill under ordinal k+1, whose envelope parks
	// on question k+1.
	const outageRounds = 30
	actions := []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": "q_s17bp_0",
			"question": "s17: the version-stream ladder parks here."},
	}
	// outageRounds outage questions (q_1..q_outageRounds) plus one spare
	// catch-up question (q_outageRounds+1) answered after the aihub is
	// back, before the completion tail.
	for i := 1; i <= outageRounds+1; i++ {
		actions = append(actions, map[string]any{
			"do": "envelope", "id": "grill", "outcome": "needs_input",
			"question_id": fmt.Sprintf("q_s17bp_%d", i),
			"question":    fmt.Sprintf("s17: outage round %d.", i),
		})
	}
	actions = append(actions,
		map[string]any{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		map[string]any{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		map[string]any{"do": "envelope", "id": "verification", "outcome": "completed"},
		map[string]any{"do": "envelope", "id": "release", "outcome": "completed"},
	)
	wi := inst.newWI(t, "s17: push backpressure (steps trigger, aihub outage)", actions, nil)
	d := newV1Daemon(t, inst, optUseDriver())
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// round is the index of the CURRENTLY OPEN question of the ladder.
	round := 0
	advance := func(bound time.Duration) {
		answerQuestion(t, d, wi, fmt.Sprintf("q_s17bp_%d", round), fmt.Sprintf("round %d", round))
		round++
		waitQuestion(t, d, wi, fmt.Sprintf("q_s17bp_%d", round), bound)
	}
	waitQuestion(t, d, wi, fmt.Sprintf("q_s17bp_%d", round), 30*time.Second) // q_s17bp_0 parked

	// The push path is proven healthy before the outage: at least one
	// version reached the aihub's bare repo.
	waitForEvent(t, d, wi, "published", 30*time.Second)

	// The outage: stop the REAL aihub mid-run. Pushes fail (one
	// push_failed per retry), the ladder climbs on the production 10s
	// backoff, and after 5 consecutive failures the degradation event
	// fires with reason "steps".
	inst.stopAihub()
	// The outage's first boundary is the next answer's transition (the
	// parked version's wave already landed): one round re-arms the push
	// round against the dead aihub and the failure ladder begins.
	advance(30 * time.Second)
	waitForEvent(t, d, wi, "push_failed", 60*time.Second)
	// The same-error retry cap writes its own reason="same_error" event at
	// the same failure count (the versions the answers keep producing
	// re-arm the round each boundary, so the failure ladder itself is
	// unaffected) — wait for the STEPS event specifically, not whichever
	// degradation event lands first. Each answer round keeps the driver
	// writing versions, feeding the ladder; the bound covers the full
	// production ladder several times over.
	deadline := time.Now().Add(150 * time.Second)
	var body struct {
		Reason       string `json:"reason"`
		FailedPushes int    `json:"failed_pushes"`
	}
	for {
		for _, e := range d.eventsOf(t, wi) {
			if e.Kind != "push_backpressure" {
				continue
			}
			body.Reason = ""
			_ = json.Unmarshal(e.Body, &body)
			if body.Reason == "steps" {
				break
			}
		}
		if body.Reason == "steps" || time.Now().After(deadline) {
			break
		}
		// Keep the version stream alive through the outage window.
		if questionIDOf(statusQuiet(d), wi) != "" && round < outageRounds {
			advance(30 * time.Second)
			continue
		}
		time.Sleep(200 * time.Millisecond)
	}
	if body.Reason != "steps" {
		t.Fatalf("no steps-reason backpressure event in bound (last reason %q)", body.Reason)
	}
	if body.FailedPushes < 5 {
		t.Fatalf("failed_pushes %d, want >= 5 (the production default threshold)", body.FailedPushes)
	}

	// Degradation never kills the driver: no recovery/respawn signal, no
	// new attempt instance, and the work item is still running.
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

	// The catch-up baseline: what the driver wrote during the outage lives
	// as local commits in the capsule's chain. Bring the aihub back,
	// produce one more boundary, and demand the server-side bare repo
	// reach that history plus a fresh published event — the failed pushes
	// were never lost work.
	local := capsuleCommitCount(t, d.dataDir, wi)
	if local == 0 {
		t.Fatal("no local commits after the outage episode")
	}
	publishedBefore := countOf(kinds, "published")
	inst.restartAihub()
	advance(30 * time.Second) // the catch-up boundary
	inst.waitBareCommits(t, wi, local, 60*time.Second)
	waitUntil(t, 60*time.Second, "no published event after the recovery push", func() bool {
		return countOf(d.eventKinds(t, wi), "published") > publishedBefore
	})

	// The final answer completes the workflow; the terminal wiring seals
	// it with the aihub back up.
	answerQuestion(t, d, wi, fmt.Sprintf("q_s17bp_%d", round), "the last answer")
	waitWorkflowStatus(t, d, wi, "completed", 120*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
	x := readExecution(t, d.capsuleDir(wi))
	for _, step := range []string{"grill", "fix", "verification", "release"} {
		if st := x.Steps[step]; st == nil || st.Status != "done" {
			t.Fatalf("step %s: %+v, want done", step, st)
		}
	}
	// Still exactly the one attempt instance across the whole episode.
	if n := countOf(d.eventKinds(t, wi), "needs_recovery"); n != 1 {
		t.Fatalf("needs_recovery fired %d times, want 1 (the completion signal): %v", n, d.eventKinds(t, wi))
	}
}
