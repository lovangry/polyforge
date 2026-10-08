package test

import (
	"path/filepath"
	"testing"
	"time"
)

// v1_s09: graceful shutdown with a running driver workflow. The shutdown
// sequence latches shutting_down, notifies every live agent (the driver
// bridge carries no control channel — the notice is best-effort by
// contract), waits bounded for the agents' flush(shutdown) (a driver WI
// has no flush face: the wait degrades on timeout, exactly as designed —
// uncommitted state rides the recovery engine), stops the observe lane,
// terminates every agent tree (the driver handle's Terminate unwinds Run
// and its live step process with it), and the daemon PROCESS exits. The
// work item is NOT closed: aihub still shows running under the SAME
// session (M3), and a restarted daemon reconciles and resumes through the
// recovery engine. The stop_notice/flush(shutdown) journal-ordering faces
// of the legacy scenario are the WI-Agent RPC control channel — retired
// pending Phase-4 #106 (the driver-era teardown faces are the process
// exit, the orphan scan and the session record).
func TestV1S09GracefulShutdown(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s09: graceful shutdown with a running job",
		[]map[string]any{
			// Ordinal 1 holds mid-step (delayed envelope write) so the
			// shutdown interrupts a live driver with a live step process.
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "ms": 60000},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst, optUseDriver())
	d.start()
	d.pf(t, "submit", wi)
	waitSpawnCount(t, d, wi, 1, 30*time.Second)
	capDir := d.capsuleDir(wi)

	// The session fingerprint BEFORE the shutdown (T4's identity pair
	// with the one captured below).
	sessBefore := inst.sessionOf(t, wi)
	if sessBefore.Fingerprint == "" {
		t.Fatal("running work item carries no session")
	}

	// Graceful shutdown through the real CLI. The daemon answers, then
	// runs the bounded sequence and the process exits. (The per-job
	// flush(shutdown) wait degrades on timeout for a driver WI — the
	// sequence stays bounded by its budget; the exit bound here covers
	// that wait plus the teardown.)
	d.pf(t, "ops", "shutdown")
	if !d.exitedWithin(90 * time.Second) {
		t.Fatalf("daemon process did not exit after shutdown (log tail: %s)", d.logTail())
	}

	// Teardown completed: the driver's live step process was terminated
	// with the handle (no orphan survives the sequence — the env-marker
	// scan), and the driver goroutine unwound with the context.
	if pids := orphanPIDs(wi); len(pids) > 0 {
		t.Fatalf("orphan step processes survived the graceful shutdown: %v", pids)
	}

	// The work item is NOT closed server-side: shutdown leaves the
	// session row in place (status running, clean_pause=false — the
	// M3 marker the next start attaches to).
	if st := inst.wiStatus(t, wi); st != "running" {
		t.Fatalf("work item status after shutdown: %q, want running", st)
	}
	sessShutdown := inst.sessionOf(t, wi)
	if sessShutdown.Fingerprint != sessBefore.Fingerprint {
		t.Fatalf("shutdown changed the session: %q -> %q", sessBefore.Fingerprint, sessShutdown.Fingerprint)
	}
	if sessShutdown.CleanPause {
		t.Fatalf("shutdown must not set the operator-pause flag: %+v", sessShutdown)
	}
	// The interrupted step is durable: grill in_progress on the retained
	// capsule face (the recovery engine's re-dispatch input).
	if x := readExecution(t, capDir); x.Steps["grill"].Status != "in_progress" {
		t.Fatalf("the interrupted step is not in_progress on the retained face: %+v", x.Steps["grill"])
	}

	// A restarted daemon reconciles + resumes, and the workflow finishes.
	d2 := &v1Daemon{
		t: t, inst: inst, sock: d.sock, dataDir: d.dataDir, workspace: d.workspace,
		machineID: d.machineID, extraArgs: d.extraArgs,
		logPath: filepath.Join(t.TempDir(), "daemon2.log"),
	}
	t.Cleanup(func() { d2.stop() })
	d2.start()
	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)

	// T4: the restart attached to the SAME session the shutdown kept
	// (verify matches → recover-resume), and the wrap cleared it.
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
	}

	// The resume re-dispatched grill in place (a RESUME under the next
	// ordinal), then the tail ran fresh.
	spawns := waitSpawnCount(t, d2, wi, 5, 60*time.Second)
	if spawns[1]["resume"] != true {
		t.Fatalf("post-shutdown restart did not resume: %+v", spawns[1])
	}
	x := readExecution(t, capDir)
	for _, step := range []string{"grill", "fix", "verification", "release"} {
		if st := x.Steps[step]; st == nil || st.Status != "done" {
			t.Fatalf("step %s: %+v, want done", step, st)
		}
	}
}
