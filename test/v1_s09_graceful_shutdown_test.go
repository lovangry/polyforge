package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// v1_s09: graceful shutdown with a running work item. The shutdown
// sequence latches shutting_down, delivers pf/stop_notice (the agent
// acks it — the correlation contract), waits for the agent's
// flush(shutdown) commit, stops the observe lane, tears down every
// agent tree, and the daemon PROCESS exits. The work item is NOT
// closed: aihub still shows running under the SAME session (M3 — the
// record keeps clean_pause=false, the "paused-with-session, will
// attach on restart" marker), and a restarted daemon reconciles
// (session verify matches) and resumes through the recovery engine.
// No orphan process survives (PF_WI_ID scan).
func TestV1S09GracefulShutdown(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s09: graceful shutdown with a running job",
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

	// The session fingerprint BEFORE the shutdown (T4's identity pair
	// with the one captured below).
	sessBefore := inst.sessionOf(t, wi)
	if sessBefore.Fingerprint == "" {
		t.Fatal("running work item carries no session")
	}

	// Graceful shutdown through the real CLI. The daemon answers, then
	// runs the bounded sequence and the process exits.
	d.pf(t, "ops", "shutdown")
	if !d.exitedWithin(30 * time.Second) {
		t.Fatalf("daemon process did not exit after shutdown (log tail: %s)", d.logTail())
	}

	// The agent saw the stop_notice and began its flush(shutdown) BEFORE
	// the teardown — the intent line is journaled synchronously in the
	// control loop, ahead of the ack, so it always survives the race
	// with the daemon's post-event teardown.
	var noticeAt, flushAt = -1, -1
	for i, rec := range fakeJournal(t, capDir) {
		if rec["type"] == "control" && rec["method"] == "pf/stop_notice" && noticeAt < 0 {
			noticeAt = i
		}
		if rec["type"] == "flush_begin" && rec["reason"] == "shutdown" && flushAt < 0 {
			flushAt = i
		}
		if rec["type"] == "flush" && rec["reason"] == "shutdown" && rec["ok"] != true {
			t.Fatalf("shutdown flush not acked ok: %+v", rec)
		}
	}
	if noticeAt < 0 || flushAt < 0 || flushAt < noticeAt {
		for i, rec := range fakeJournal(t, capDir) {
			t.Logf("journal[%d] = %v", i, rec)
		}
		raw, _ := os.ReadFile(filepath.Join(d.dataDir, "events.log"))
		t.Logf("RAW events:\n%s", raw)
		t.Logf("daemon log:\n%s", d.logTail())
		t.Fatalf("stop_notice/flush(shutdown) ordering wrong (notice %d, flush %d)", noticeAt, flushAt)
	}

	// The shutdown flush commit is recorded in the daemon's durable
	// events (the published event with the shutdown marker).
	shutdownPublished := false
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind != "published" {
			continue
		}
		var body struct {
			Shutdown bool `json:"shutdown"`
		}
		if json.Unmarshal(e.Body, &body) == nil && body.Shutdown {
			shutdownPublished = true
		}
	}
	if !shutdownPublished {
		t.Fatalf("no published{shutdown:true} event: %v", d.eventKinds(t, wi))
	}

	// No orphan agent survives the sequence (the env-marker scan).
	if pids := orphanPIDs(wi); len(pids) > 0 {
		t.Fatalf("orphan agents survived the graceful shutdown: %v", pids)
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

	// A restarted daemon reconciles + resumes, and the resumed agent
	// (resume_actions) finishes the work item.
	d2 := &v1Daemon{
		t: t, inst: inst, sock: d.sock, dataDir: d.dataDir, workspace: d.workspace,
		machineID: d.machineID, logPath: filepath.Join(t.TempDir(), "daemon2.log"),
	}
	t.Cleanup(func() { d2.stop() })
	d2.start()
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// T4: the restart attached to the SAME session the shutdown kept
	// (verify matches → recover-resume), and the wrap cleared it.
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
	}

	spawns := spawnJournal(t, capDir)
	if len(spawns) != 2 || spawns[1]["resume"] != true {
		t.Fatalf("post-shutdown restart did not resume: %+v", spawns)
	}
}
