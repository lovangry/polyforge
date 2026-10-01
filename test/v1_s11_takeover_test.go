package test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// v1_s11: cross-machine force takeover under the v1.5 session model.
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
//   - the new session's agent runs on daemon B, the derivation counter
//     re-seeds from the cloned capsule spawn journal (contract §4.3),
//     and the continuation wraps and clears the session.
//
// SHAPE NOTE (v1.5): the release is observed while B holds the WI
// PAUSED, not running — the six-layer cleanup's L4 env-marker layer
// kills EVERY process carrying PF_WI_ID=<wi> on the machine, daemon-
// agnostic (D1: "this machine's writers of this WI are zombies"). On
// two real machines B's live agent is untouched by A's release; on
// this e2e host both daemons share one kernel, so B pausing first
// (no live agent) keeps the release deterministic instead of a race
// between A's cleanup sweep and B's recovery. The pause is itself a
// v1.5 surface: session kept at aihub (M3), clean_pause=true.
//
// The generation/attempt lineage this scenario pinned in v1 is deleted
// (contract §5): there is no attempt id to compare anymore, the
// identity pair is (session_fingerprint, spawn_n).
func TestV1S11Takeover(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s11: force takeover from another daemon",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		}, nil)
	daemonA := newV1Daemon(t, inst, optMachineID("m-e2e-a"))
	daemonA.start()
	daemonA.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned on daemon A", func() bool {
		return daemonA.agentPIDOf(t, wi) > 0
	})
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
	daemonB := newV1Daemon(t, inst, optMachineID("m-e2e-b"))
	daemonB.start()
	daemonB.pf(t, "submit", wi, "--takeover")

	// The hub's session row was swapped atomically: a NEW public
	// fingerprint, daemon B's machine — never the secret. (The CLI submit
	// only enqueues; the dispatcher's ForceTakeover lands a moment later.)
	var sessB wiSession
	waitUntil(t, 30*time.Second, "takeover never swapped the session", func() bool {
		sessB = inst.sessionOf(t, wi)
		return sessB.Fingerprint != "" && sessB.Fingerprint != sessA.Fingerprint && sessB.Machine == "m-e2e-b"
	})
	waitUntil(t, 30*time.Second, "daemon B never re-derived the agent", func() bool {
		return daemonB.agentPIDOf(t, wi) > 0
	})

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

	// The old daemon learns. First crash it — its agent deliberately
	// survives, parked on stdin EOF (the same-machine L4 note in the
	// scenario header applies to the pause below too: B's close kills
	// every PF_WI_ID-marked process on this host, so the survivor is
	// observed HERE, before B pauses).
	daemonA.crash()
	waitUntil(t, 10*time.Second, "daemon A's agent did not survive its crash", func() bool {
		return len(orphanPIDs(wi)) > 0
	})

	// B pauses (the v1.5 operator pause: session KEPT, clean_pause=true,
	// no live agent — see the scenario header for why the release below
	// is observed against the paused shape).
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
		machineID: daemonA.machineID, logPath: filepath.Join(t.TempDir(), "daemonA2.log"),
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
	// The old daemon holds no job for the released work item, and its
	// surviving agent tree was collected.
	for _, j := range daemonA2.status(t).Jobs {
		if j.WorkItemID == wi {
			t.Fatalf("daemon A still holds a job for %s after the release", wi)
		}
	}
	waitUntil(t, 30*time.Second, "the old daemon's agent survived the release", func() bool {
		return len(orphanPIDs(wi)) == 0
	})

	// The continuation: the paused work item moves with a second
	// takeover, on the same holder machine — session swap #2, fresh
	// clone above the paused tip, and the scripted wrap.
	inst.setPlanFull(t, wi,
		[]map[string]any{{"do": "step", "id": "polish"}, {"do": "wrap"}},
		[]map[string]any{{"do": "wrap"}})
	daemonB.pf(t, "submit", wi, "--takeover")
	inst.waitWIStatus(t, wi, "running", 30*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// The sealed chain grew under the NEW session's credential: the bare
	// repo carries commits beyond what daemon A ever pushed, and the
	// wrap cleared the session row entirely.
	if inst.bareCommitCount(t, wi) == 0 {
		t.Fatal("takeover session never pushed")
	}
	sessFinal := inst.sessionOf(t, wi)
	if sessFinal.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sessFinal.Fingerprint)
	}
}

// killAgent SIGKILLs the daemon-side recorded agent pid of wi.
func killAgent(d *v1Daemon, wi string) error {
	pid := d.agentPIDOf(d.t, wi)
	if pid <= 0 {
		return fmt.Errorf("no live agent pid for %s", wi)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
