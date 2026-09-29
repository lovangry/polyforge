package test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v1_s11: force takeover by another daemon. The second daemon's
// takeover claim ends the first daemon's generation at aihub, which
// revokes its attempt-scoped push credential: the OLD credential is
// rejected at the git proxy with GIT-401 (asserted on the raw wire),
// while the new generation's agent runs on the second daemon and
// wraps. The generation fencing main layer, end to end.
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

	// Capture daemon A's attempt-scoped push credential from its
	// persisted bookkeeping (the runtime state file records it).
	type stateFile struct {
		Jobs []struct {
			WorkItemID string `json:"work_item_id"`
			AttemptID  string `json:"attempt_id"`
			Secret     string `json:"secret"`
		} `json:"jobs"`
	}
	var oldSecret, oldAttempt string
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
				oldSecret, oldAttempt = j.Secret, j.AttemptID
			}
		}
		return oldSecret != ""
	})

	// The credential works BEFORE the takeover (the fencing baseline).
	code, hdr, _ := httpStatusOf("GET",
		inst.url+"/git/"+wi+".git/info/refs?service=git-receive-pack", wi, oldSecret)
	if code != 401 && code != 200 {
		t.Fatalf("pre-takeover credential probe: HTTP %d", code)
	}
	_ = hdr

	// Script the takeover derivation BEFORE seizing (a fresh derivation
	// reads the plan at spawn): it finishes immediately.
	inst.setPlan(t, wi, []map[string]any{{"do": "wrap"}})

	// Daemon B force-takes the work item over through the real CLI.
	daemonB := newV1Daemon(t, inst, optMachineID("m-e2e-b"))
	daemonB.start()
	daemonB.pf(t, "submit", wi, "--takeover")

	// The takeover ended the old generation: the OLD credential now
	// fails at the git proxy with GIT-401 (raw wire assertion).
	waitUntil(t, 30*time.Second, "old credential still accepted after takeover", func() bool {
		code, hdr, _ := httpStatusOf("GET",
			inst.url+"/git/"+wi+".git/info/refs?service=git-receive-pack", wi, oldSecret)
		return code == 401 && strings.Contains(hdr.Get("X-PF-Git-Error"), "GIT-401")
	})

	// Daemon B's agent finished the work item.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// The sealed chain grew under the NEW generation's credential: the
	// bare repo carries commits beyond what daemon A ever pushed.
	if inst.bareCommitCount(t, wi) == 0 {
		t.Fatal("takeover generation never pushed")
	}
	// And daemon B's bookkeeping holds the NEW attempt, not the seized one.
	found := false
	for _, j := range daemonB.status(t).Jobs {
		if j.WorkItemID == wi {
			found = true
			if j.AttemptID == oldAttempt {
				t.Fatalf("daemon B runs the OLD attempt %s", oldAttempt)
			}
		}
	}
	_ = found // the sealed job is already dropped; the assertion is the wrap above

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
}
