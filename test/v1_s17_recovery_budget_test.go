package test

import (
	"syscall"
	"testing"
	"time"
)

// v1_s17: the recovery budget runs OUT, driver/envelope edition. The live
// step process is SIGKILLed, the recovery engine starts its ladder, and
// every re-dispatch fails AT BIRTH: the fail_spawn_resume mode makes each
// resume derivation exit before reading one byte of stdin (journaled as
// spawn_fail), so the dispatch fails — the driver surfaces the
// envelope-less exit, the runtime re-enters the recovery ladder, and the
// entry-rate storm guard (two re-entries inside the 60s stability window)
// bounded-fails the work item — RTFinished(blocked), the aihub side
// blocked with waiting_on=owner and NO closed_at (blocked is
// resumable-by-owner, not closed), the job dropped from the daemon's live
// set. It is the e2e half of the semantics runtime/recover_test.go pins
// in-process; here the ladder runs through the REAL daemon, the real
// spawner and real step processes.
//
// PIPE-CAPACITY CONTRACT RE-VERIFIED AGAINST DRIVER SPAWNS. The driver
// assembles each dispatch's prompt from the bundle's step definition (a
// few KB), so the prompt write always completes into the stdin pipe
// buffer. The must-fail
// ladder is deterministic anyway, and for a stronger reason: a derivation
// that dies at birth can NEVER write its result envelope, so EVERY
// ordering of the birth-exit race fails the dispatch (envelope absence),
// which is the same failed-recovery input the pipe-capacity shape used to
// buy. The fail_spawn_* flags stay in the plan so the scenario's shape is
// unchanged now that #106 has removed the legacy lane.
func TestV1S17RecoveryBudgetBoundedFail(t *testing.T) {
	inst := newV1Install(t)
	actions := []map[string]any{
		// Ordinal 1 is the healthy original: it burns a 2.5s delay (the
		// kill window) before its envelope write.
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "ms": 2500},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}
	wi := inst.newWI(t, "s17: every re-derivation fails at birth", actions, nil)
	// ONE plan for the whole run: the original derivation executes the
	// actions above, and every derivation the recovery ladder mints
	// afterwards dies at birth (resume AND fresh re-derivations alike).
	writePlanExtra(t, inst, wi, actions, nil, map[string]any{
		"fail_spawn_resume": true,
		"fail_spawn_fresh":  true,
	})

	d := newV1Daemon(t, inst, optDriverEngine())
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// The original derivation is healthy (live, mid-delay), then killed.
	capDir := d.capsuleDir(wi)
	waitSpawnCount(t, d, wi, 1, 30*time.Second)
	pid := latestSpawnPID(t, d, wi)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}

	// The ladder: two failed resume re-derivations, then the storm guard
	// bounded-fails the WI. The terminal state is BLOCKED.
	inst.waitWIStatus(t, wi, "blocked", 120*time.Second)

	// blocked is waiting-on-owner, NOT closed: the owner can still
	// resume it (that is the whole difference from a terminal failure).
	item, err := inst.client.GetWorkItem(wi)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	if item.WaitingOn == nil || *item.WaitingOn != "owner" {
		t.Fatalf("blocked work item waiting_on = %v, want \"owner\"", item.WaitingOn)
	}
	if item.ClosedAt != nil {
		t.Fatalf("blocked work item must not be closed, got closed_at %q", *item.ClosedAt)
	}

	// T5 (M2): the bounded fail CLEARED the session row — blocked with a
	// live session would be revivable by a restart and the storm guard
	// would be a lie. The cleared row keeps the work item OUT of the
	// claimable set: the CAS gate answers the typed NOT_CLAIMABLE refusal
	// on the wire.
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("blocked work item still names session %q (M2: bounded fail must clear the session)", sess.Fingerprint)
	}
	refuseClaim(t, inst, wi, "NOT_CLAIMABLE")

	// The daemon dropped the job: no relaunch storm, nothing live.
	waitUntil(t, 30*time.Second, "the bounded-failed job never left the daemon's live set", func() bool {
		for _, j := range d.status(t).Jobs {
			if j.WorkItemID == wi {
				return false
			}
		}
		return true
	})

	// THREE recovery signals for the ladder (the kill plus one per failed
	// dispatch); exactly TWO went live — the driver restarts (the bridge
	// handle comes up each time; it is the STEP dispatch inside that dies
	// at birth) — and the third re-entry hit the storm guard and
	// bounded-failed without a third restart.
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "needs_recovery"); n != 3 {
		t.Fatalf("needs_recovery fired %d times, want 3 (kill + two failed dispatches): %v", n, kinds)
	}
	if n := countOf(kinds, "wi_agent_rederived"); n != 2 {
		t.Fatalf("re-derivation count %d, want 2 (the two live driver restarts): %v", n, kinds)
	}

	// The spawn journal pins the ladder shape. Failed derivations ARE
	// journaled — the parent writes the identity line before the prompt —
	// so the file carries the original plus the two birth-death resumes,
	// in ladder order, counters advancing (the driver's step ordinals
	// advance per durable step_start, and the journal is the fence's
	// source of truth).
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 3 {
		t.Fatalf("spawn journal has %d entries, want 3 (original + two failed resumes): %+v", len(spawns), spawns)
	}
	wantResume := []bool{false, true, true}
	for i, want := range wantResume {
		if got, _ := spawns[i]["resume"].(bool); got != want {
			t.Fatalf("spawn %d resume=%v, want %v (ladder shape %+v)", i, got, want, spawns)
		}
		if n, _ := spawns[i]["spawn_n"].(float64); int(n) != i+1 {
			t.Fatalf("spawn %d carries spawn_n %v, want %d", i, spawns[i]["spawn_n"], i+1)
		}
	}
	// The agent side agrees it died at birth every time: two must-fail
	// resume births and no second live "spawn" line.
	failResume, live := 0, 0
	for _, r := range fakeJournal(t, capDir) {
		switch r["type"] {
		case "spawn_fail":
			if resumed, _ := r["resume"].(bool); resumed {
				failResume++
			}
		case "spawn":
			live++
		}
	}
	if failResume != 2 || live != 1 {
		t.Fatalf("must-fail births: %d resume, %d live derivations, want 2 + 1", failResume, live)
	}

	// The interrupted step is still in_progress on the durable face (the
	// blocked workflow's resume input — the owner's §6.4 move).
	x := readExecution(t, capDir)
	if x.WorkflowStatus != "running" || x.Steps["grill"].Status != "in_progress" {
		t.Fatalf("the durable face moved during the ladder: status=%s grill=%s",
			x.WorkflowStatus, x.Steps["grill"].Status)
	}
}
