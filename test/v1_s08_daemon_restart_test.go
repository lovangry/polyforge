package test

import (
	"path/filepath"
	"testing"
	"time"
)

// v1_s08: daemon crash with a running work item. The daemon process is
// SIGKILLed (the WI agent deliberately survives — it parks on stdin
// EOF). The restarted daemon runs the §十一 reconciliation against
// aihub's own verdict (running), routes the job through the recovery
// engine, kills the SURVIVING old agent tree, resumes from the
// transcript and finishes the work item. No orphan of the old daemon
// survives the reconciliation.
func TestV1S08DaemonRestart(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s08: daemon crash mid-run",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		},
		[]map[string]any{{"do": "wrap"}})
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "first step never landed", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "step" && rec["phase"] == "done" {
				return true
			}
		}
		return false
	})
	t.Cleanup(func() { killOrphans(wi) })

	// Crash the daemon hard. The agent survives (parked on EOF).
	oldPID := d.agentPIDOf(t, wi)
	d.crash()
	waitUntil(t, 10*time.Second, "agent did not survive the daemon crash", func() bool {
		return len(orphanPIDs(wi)) > 0
	})

	// Restart the SAME daemon (same socket, data dir, workspace). The
	// startup reconciliation recovers the running work item.
	d2 := &v1Daemon{
		t: t, inst: inst, sock: d.sock, dataDir: d.dataDir, workspace: d.workspace,
		machineID: d.machineID, logPath: filepath.Join(t.TempDir(), "daemon2.log"),
	}
	t.Cleanup(func() { d2.stop() })
	d2.start()

	// The reconciliation resumed the job: a live agent again, and the
	// SURVIVOR of the crash was collected (no orphan of the old tree).
	waitUntil(t, 30*time.Second, "restarted daemon never re-derived the agent", func() bool {
		return d2.agentPIDOf(t, wi) > 0
	})
	waitUntil(t, 30*time.Second, "the crash survivor was never collected", func() bool {
		return len(orphanPIDs(wi)) <= 1 // the new derivation may hold the marker briefly
	})
	// The re-derivation is a RESUME of the same attempt (the transcript
	// survived the crash in the capsule).
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 2 {
		t.Fatalf("spawn journal has %d entries after restart, want 2", len(spawns))
	}
	if spawns[1]["resume"] != true {
		t.Fatalf("restart recovery was not a resume: %+v", spawns[1])
	}
	if pid2, ok := spawns[1]["pid"].(float64); !ok || int(pid2) == oldPID {
		t.Fatalf("restart re-derived the same pid %v", spawns[1]["pid"])
	}

	// The resumed agent (scripted by resume_actions) finished the work
	// item inside the SAME generation.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// And no orphan of the old daemon's tree survived the whole cycle.
	waitUntil(t, 10*time.Second, "orphan agents survived the restart cycle", func() bool {
		return len(orphanPIDs(wi)) == 0
	})

	// The reattached monitor watched the new derivation to its end.
	kinds := d2.eventKinds(t, wi)
	if !contains(kinds, "job_started") || !contains(kinds, "job_stopped") {
		t.Fatalf("restarted daemon's events miss the lifecycle: %v", kinds)
	}
}
