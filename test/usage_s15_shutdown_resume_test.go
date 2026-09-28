package test

// usage_s15_shutdown_resume_test.go: S15 — graceful shutdown mid-work ->
// paused -> resume, the FULL chain against real processes. The fixture
// bundle s15-lifecycle (alpha -> beta) lets the scenario park the WI
// Agent while `beta` is in flight, then:
//
//   - `pf ops shutdown` lands the bounded convergence (the polyforge#32
//     path): the daemon closes the job through the one 04 §6.2 A-path
//     close — boundary notice, final flush over the quiesced agent,
//     end-generation(pause) with the daemon-generated flush evidence —
//     and the process exits 0 within a bounded window;
//   - the WI lands paused with the flush evidence chain INTACT: the
//     generation ledger carries gen-1 end_reason=pause (an end-generation
//     with a broken chain is CORE-425-refused and never lands), and the
//     read-through face serves the final published HEAD (alpha done,
//     beta still in_progress);
//   - `pf resume` on a SECOND machine of the same install is a fresh
//     claim: generation 2, a NEW session id, a full site reconstruction
//     from the published HEAD — and the new agent re-derives and resumes
//     the SAME step: exactly one new beta dispatch, ZERO new alpha
//     dispatches (the completed step is never re-run), the flow
//     completes and wraps on the resumed generation.

import (
	"strings"
	"testing"
	"time"
)

func TestUsageS15ShutdownMidStepPauseResume(t *testing.T) {
	inst := newFaultInstall(t)
	mA := inst.newUsageMachine(t, "mac_s15a")
	// No release file: the hold before beta parks forever — exactly the
	// "step in flight when the operator shuts the machine down" shape.
	mA.start(mA.faultDriveEnv("FAKEPI_HOLD_BEFORE=beta")...)

	wi := inst.wiCreateBundle(t, "S15: graceful shutdown mid-step, resume continues the same step", "s15-lifecycle")
	mA.pfOn("submit", wi)

	// Mid-flight: alpha completed and published, beta started and parked.
	exec := waitCapsuleExec(t, mA, wi, "beta to be in flight", func(e map[string]any) bool {
		return e["current_step"] == "beta"
	})
	if capsuleStep(t, exec, "alpha")["status"] != "done" {
		t.Fatalf("alpha must be done before the shutdown, got %v", exec["steps"])
	}
	if capsuleStep(t, exec, "beta")["status"] != "in_progress" {
		t.Fatalf("beta must be in flight when the shutdown lands, got %v", exec["steps"])
	}
	// The gen-1 dispatch ledger snapshot (the not-re-run baseline).
	disp1 := journalDispatches(t, mA, wi)
	alphaDispatches := len(dispatchesOfStep(disp1, "alpha"))
	betaDispatches := len(dispatchesOfStep(disp1, "beta"))
	if alphaDispatches != 1 || betaDispatches != 0 {
		t.Fatalf("baseline: one alpha dispatch, beta not yet dispatched, got %v", disp1)
	}
	sess1 := mA.spawnSession(1)

	// Graceful shutdown WITH the step in flight: bounded convergence.
	shutdownAt := time.Now()
	if out := mA.pfOn("ops", "shutdown"); !strings.Contains(out, "graceful shutdown requested") {
		t.Fatalf("ops shutdown must confirm, got:\n%s", out)
	}
	waitUntilUse(t, "the daemon process to exit", func() bool { return daemonExitedUsage(mA.cmd) })
	daemonAssertExit0Usage(t, mA.cmd)
	if elapsed := time.Since(shutdownAt); elapsed > 15*time.Second {
		t.Fatalf("the shutdown must converge in a bounded window, took %s", elapsed)
	}

	// The WI lands paused server-side with the flush evidence chain
	// intact: the generation ledger row exists (the Core accepted the
	// evidence — a broken chain is CORE-425-refused), the capsule is
	// sealed read-only, and the published HEAD still serves the
	// mid-flight state through the read-through face.
	waitUntilUse(t, "the WI to land paused", func() bool { return inst.wiStatus(t, wi) == "paused" })
	if gens := aihubGenRows(t, inst, wi); gens[1] != "pause" {
		t.Fatalf("generation 1 must be recorded ended with reason pause, got %v", gens)
	}
	if sealed, serr := statPathUsage(capsuleDirOf(mA, wi)); serr != nil || sealed.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the paused capsule must be sealed read-only, got %v err=%v", sealed, serr)
	}
	obs := inst.pf(t, "observe", "--read-through", wi)
	for _, want := range []string{`"current_step": "beta"`, `"status": "in_progress"`} {
		if !strings.Contains(obs, want) {
			t.Fatalf("the published HEAD must carry the mid-flight state (%s):\n%s", want, obs)
		}
	}

	// Resume on a SECOND machine of the same install: a fresh claim —
	// generation 2, a NEW session id, a full site reconstruction.
	mB := inst.newUsageMachine(t, "mac_s15b")
	mB.start(mB.faultDriveEnv()...)
	if out := mB.pfOn("resume", wi); !strings.Contains(out, "resumed") {
		t.Fatalf("pf resume must confirm the fresh claim, got:\n%s", out)
	}
	waitUntilUse(t, "the generation-2 agent on the second machine", func() bool { return mB.spawnCount() == 1 })
	sess2 := mB.spawnSession(1)
	if sess1 == sess2 {
		t.Fatalf("resume must derive a NEW session id, got %s twice", sess1)
	}
	if got := inst.wiStatus(t, wi); got != "running" {
		t.Fatalf("the resumed WI must be running, got %s", got)
	}

	// The resumed site was rebuilt from the published HEAD: the completed
	// step carried over as done and was NEVER re-dispatched; the SAME
	// in-flight step resumed exactly once.
	exec = waitCapsuleExec(t, mB, wi, "the resumed flow to complete", func(e map[string]any) bool {
		return e["workflow_status"] == "completed"
	})
	if capsuleStep(t, exec, "alpha")["status"] != "done" {
		t.Fatalf("the completed step must stay done after the resume, got %v", exec["steps"])
	}
	disp2 := journalDispatches(t, mB, wi)
	if got := len(dispatchesOfStep(disp2, "alpha")); got != alphaDispatches {
		t.Fatalf("the completed step must never be re-run: %d alpha dispatches after the resume (was %d)",
			got, alphaDispatches)
	}
	beta2 := dispatchesOfStep(disp2, "beta")
	if len(beta2) != 1 {
		t.Fatalf("the resumed generation must re-dispatch exactly the in-flight step, got %v", beta2)
	}
	if beta2[0]["status_at_dispatch"] != "in_progress" {
		t.Fatalf("the resume must continue the SAME in-flight step in place, got %v", beta2[0])
	}

	// The resumed generation closes cleanly: wrap lands, and the ledger
	// records generation 2 ended with reason wrap.
	mB.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the resumed WI must wrap, got %s", got)
	}
	if gens := aihubGenRows(t, inst, wi); gens[1] != "pause" || gens[2] != "wrap" {
		t.Fatalf("the generation ledger must tell the whole story, got %v", gens)
	}
}
