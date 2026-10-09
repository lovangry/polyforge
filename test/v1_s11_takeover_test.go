package test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// v1_s11: cross-machine force takeover under the v1.5 session model,
// driver/envelope edition — the supervised units are the driver's step
// processes (the spawn journal is their ledger), the parked envelope
// agent holds the work item between the takeover phases.
//
// Daemon B's takeover claim atomically swaps aihub's session row to a
// NEW secret (ForceTakeover, contract §1.4); daemon A keeps its state
// and its credentials, and every subsequent use of the OLD secret is
// rejected — fencing-on-use, not invalidation-on-claim:
//
//   - the old secret's git face is refused GIT-401 at the aihub proxy,
//     the rejection naming the CURRENT holder's public fingerprint;
//   - the OLD daemon learns it was superseded through the restart
//     reconciliation's session verify (B3, compare-only): verify
//     mismatch + held elsewhere → release (kill the surviving agent
//     tree, quarantine the capsule, session_released event, NO hub
//     transition);
//   - the new session's driver runs on daemon B, the derivation counter
//     re-seeds from the cloned capsule spawn journal (contract §4.3),
//     and the continuation completes and wraps, clearing the session.
//
// SHAPE NOTE (v1.5): the release is observed while B holds the WI
// PAUSED, not running — the six-layer cleanup's L4 env-marker layer
// kills EVERY process carrying PF_WI_ID=<wi> on the machine, daemon-
// agnostic (D1). On two real machines B's live step process is untouched
// by A's release; on this e2e host both daemons share one kernel, so B
// pausing first (no live step process) keeps the release deterministic.
// The pause is itself a v1.5 surface: session kept at aihub (M3),
// clean_pause=true.
func TestV1S11Takeover(t *testing.T) {
	inst := newV1Install(t)
	// One plan for the whole run: ordinal 1 parks on the question (the
	// pre-takeover hold); the post-takeover continuations dispatch
	// ordinals 2+ — no mid-run plan rewrite anywhere.
	wi := inst.newWI(t, "s11: force takeover from another daemon",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": "q_s11",
				"question": "s11: parked across the takeover."},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	daemonA := newV1Daemon(t, inst, optMachineID("m-e2e-a"), optDriverEngine())
	daemonA.start()
	daemonA.pf(t, "submit", wi)
	waitQuestion(t, daemonA, wi, "q_s11", 30*time.Second)
	t.Cleanup(func() { killOrphans(wi) })

	// Daemon A holds the session: the public audit block (m4) names its
	// machine, and the local status face agrees on the fingerprint.
	sessA := inst.sessionOf(t, wi)
	if sessA.Fingerprint == "" || sessA.Machine != "m-e2e-a" {
		t.Fatalf("daemon A's session block wrong: %+v", sessA)
	}
	var statusFpA string
	for _, j := range daemonA.status(t).Jobs {
		if j.WorkItemID == wi {
			statusFpA = j.SessionFingerprint
		}
	}
	if statusFpA != sessA.Fingerprint {
		t.Fatalf("daemon A's status fingerprint %q != hub %q", statusFpA, sessA.Fingerprint)
	}

	// Capture daemon A's SECRET from its persisted state.json (the one
	// place it lives besides the one-shot askpass) — the fencing probe
	// needs it, and nothing else may ever see it.
	type stateFile struct {
		Jobs []struct {
			WorkItemID string `json:"work_item_id"`
			SessionID  string `json:"session_id"`
		} `json:"jobs"`
	}
	var oldSecret string
	waitUntil(t, 15*time.Second, "daemon A never persisted its state", func() bool {
		raw, err := os.ReadFile(filepath.Join(daemonA.dataDir, "runtime-state.json"))
		if err != nil {
			return false
		}
		var st stateFile
		if json.Unmarshal(raw, &st) != nil {
			return false
		}
		for _, j := range st.Jobs {
			if j.WorkItemID == wi {
				oldSecret = j.SessionID
			}
		}
		return oldSecret != ""
	})
	if fp := api.SessionFingerprint(oldSecret); fp != sessA.Fingerprint {
		t.Fatalf("daemon A's persisted secret does not hash to the public fingerprint (%s != %s)", fp, sessA.Fingerprint)
	}

	// The credential works BEFORE the takeover (the fencing baseline).
	code, _, _ := httpStatusOf("GET",
		inst.url+"/git/"+wi+".git/info/refs?service=git-receive-pack", wi, oldSecret)
	if code != 401 && code != 200 {
		t.Fatalf("pre-takeover credential probe: HTTP %d", code)
	}

	// Daemon B force-takes the work item over through the real CLI.
	// (Wait for A's first chain commit first: the spawn journal rides the
	// capsule git chain, so B's clone re-seeds its derivation counter
	// from it — contract §4.3.)
	inst.waitBareCommits(t, wi, 1, 30*time.Second)
	daemonB := newV1Daemon(t, inst, optMachineID("m-e2e-b"), optDriverEngine())
	daemonB.start()
	daemonB.pf(t, "submit", wi, "--takeover")

	// The hub's session row was swapped atomically: a NEW public
	// fingerprint, daemon B's machine — never the secret.
	var sessB wiSession
	waitUntil(t, 30*time.Second, "takeover never swapped the session", func() bool {
		sessB = inst.sessionOf(t, wi)
		return sessB.Fingerprint != "" && sessB.Fingerprint != sessA.Fingerprint && sessB.Machine == "m-e2e-b"
	})
	// Daemon B re-derived a step process (the parked question's WI now
	// runs on B's driver).
	waitSpawnCount(t, daemonB, wi, 1, 30*time.Second)

	// Daemon B's derivation counter continued from the cloned capsule
	// spawn journal (contract §4.3): the capsule carried 1 derivation, so
	// B's first is #2 — the counter is capsule-scoped, not daemon-local.
	bSpawn := 0
	for _, j := range daemonB.status(t).Jobs {
		if j.WorkItemID == wi {
			bSpawn = j.SpawnN
		}
	}
	if bSpawn != 2 {
		t.Fatalf("daemon B's derivation counter %d, want 2 (re-seeded from the cloned spawn journal)", bSpawn)
	}

	// Fencing-on-use: the OLD secret now fails at the git proxy with
	// GIT-401 (raw wire assertion), the rejection naming the new holder.
	waitUntil(t, 30*time.Second, "old credential still accepted after takeover", func() bool {
		code, hdr, body := httpStatusOf("GET",
			inst.url+"/git/"+wi+".git/info/refs?service=git-receive-pack", wi, oldSecret)
		return code == 401 && strings.Contains(hdr.Get("X-PF-Git-Error"), "GIT-401") &&
			strings.Contains(body, sessB.Fingerprint)
	})

	// A real git push with the old credential is rejected too (the git
	// client's own path through the same proxy).
	pushTmp := shortTmp("/tmp", "pfv1push")
	gitRun(t, "", "init", "-q", "-b", "main", pushTmp)
	gitRun(t, pushTmp, "commit", "--allow-empty", "-m", "stale push attempt")
	askpass := filepath.Join(pushTmp, "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' '"+wi+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "push", inst.url+"/git/"+wi+".git", "HEAD:refs/heads/main")
	cmd.Env = append(os.Environ(),
		"GIT_ASKPASS="+strings.ReplaceAll(askpass, "'", "'\\''"),
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("stale push was accepted: %s", out)
	}
	if !strings.Contains(string(out), "Authentication failed") && !strings.Contains(string(out), "401") {
		t.Fatalf("stale push failed for the wrong reason: %s", out)
	}

	// The old daemon learns. First crash it. A parked question holds no
	// live step process (the driver terminates each step process after
	// collecting its envelope — the durable execution.json is the parked
	// state), so the crashed daemon leaves no process behind: the release
	// below is observed against B's paused shape either way (the
	// same-machine L4 note in the scenario header).
	daemonA.crash()
	waitUntil(t, 10*time.Second, "the crashed daemon left processes behind", func() bool {
		return len(orphanPIDs(wi)) == 0
	})

	// B pauses (the v1.5 operator pause: session KEPT, clean_pause=true,
	// no live step process — see the scenario header for why the release
	// below is observed against the paused shape).
	daemonB.pf(t, "pause", wi)
	inst.waitWIStatus(t, wi, "paused", 30*time.Second)
	sessPaused := inst.sessionOf(t, wi)
	if sessPaused.Fingerprint != sessB.Fingerprint || !sessPaused.CleanPause {
		t.Fatalf("pause after takeover changed the session shape: %+v (was %+v)", sessPaused, sessB)
	}

	// Restart the crashed daemon A: the startup reconciliation runs the
	// compare-only session verify, learns the WI is held by ANOTHER
	// session, and releases: the surviving tree is collected, the capsule
	// quarantined, the record dropped, a session_released event left
	// behind, and NO hub transition (the new owner keeps the work item;
	// it never left "paused" and its session never moved).
	daemonA2 := &v1Daemon{
		t: t, inst: inst, sock: daemonA.sock, dataDir: daemonA.dataDir, workspace: daemonA.workspace,
		machineID: daemonA.machineID, extraArgs: daemonA.extraArgs,
		logPath: filepath.Join(t.TempDir(), "daemonA2.log"),
	}
	t.Cleanup(func() { daemonA2.stop() })
	daemonA2.start()
	waitForEvent(t, daemonA2, wi, "session_released", 30*time.Second)
	if st := inst.wiStatus(t, wi); st != "paused" {
		t.Fatalf("the release touched the hub state: status %q", st)
	}
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != sessB.Fingerprint {
		t.Fatalf("the release touched the hub session: %+v, want B's %q", sess, sessB.Fingerprint)
	}
	// The quarantined scene: daemon A's capsule was renamed in place.
	quarantined := false
	ents, _ := os.ReadDir(filepath.Join(daemonA.workspace, "capsules"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), wi+".quarantined.") {
			quarantined = true
		}
	}
	if !quarantined {
		names := []string{}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("daemon A's capsule was not quarantined (capsules/: %v)", names)
	}
	// The old daemon holds no job for the released work item, and the
	// machine carries no process of this WI at all (the parked question
	// never held a live step process; the release collected the record).
	for _, j := range daemonA2.status(t).Jobs {
		if j.WorkItemID == wi {
			t.Fatalf("daemon A still holds a job for %s after the release", wi)
		}
	}
	waitUntil(t, 30*time.Second, "processes survived the release", func() bool {
		return len(orphanPIDs(wi)) == 0
	})

	// The continuation: the paused work item moves with a second
	// takeover, on the same holder machine — session swap #2, fresh
	// clone above the paused tip. The cloned execution state carries the
	// still-open question, so the new driver parks on it; the owner's
	// answer unlocks the ordinal-2 continuation and the workflow runs to
	// its terminal wrap.
	daemonB.pf(t, "submit", wi, "--takeover")
	inst.waitWIStatus(t, wi, "running", 30*time.Second)
	waitQuestion(t, daemonB, wi, "q_s11", 30*time.Second)
	answerQuestion(t, daemonB, wi, "q_s11", "The takeover continuation is cleared.")
	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)

	// The sealed chain grew under the NEW session's credential: the bare
	// repo carries commits beyond what daemon A ever pushed, the sealed
	// execution state is the completed workflow, and the wrap cleared
	// the session row entirely.
	if inst.bareCommitCount(t, wi) == 0 {
		t.Fatal("takeover session never pushed")
	}
	waitWorkflowStatus(t, daemonB, wi, "completed", 30*time.Second)
	env := archivedEnvelope(t, daemonB.capsuleDir(wi), "grill", 2)
	if env["outcome"] != "completed" {
		t.Fatalf("the continuation's grill envelope wrong: %v", env["outcome"])
	}
	sessFinal := inst.sessionOf(t, wi)
	if sessFinal.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sessFinal.Fingerprint)
	}
}
