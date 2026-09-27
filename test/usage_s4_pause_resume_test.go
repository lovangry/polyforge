package test

// usage_s4_pause_resume_test.go: S4 — pause/resume through the CLI against
// the real daemon: pf pause closes the generation server-side (the WI is
// paused, the capsule sealed read-only, the ready queue untouched by
// paused items), observe keeps working DURING the pause (layer-1 is gone
// locally; the daemon's layer-2 read-through serves the published HEAD),
// and pf resume is a fresh claim: generation 2, a NEW session id, a new
// agent process.

import (
	"os"
	"strings"
	"testing"
)

func TestUsageS4PauseResumeCLI(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s4")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S4: pause and resume via the CLI")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })

	// Pause through the CLI: server-side paused + capsule sealed.
	out := m.pfOn("pause", wi)
	if !strings.Contains(out, "pause requested") {
		t.Fatalf("pf pause must confirm, got:\n%s", out)
	}
	waitUntilUse(t, "the WI to be paused server-side", func() bool { return inst.wiStatus(t, wi) == "paused" })
	// The sealed capsule: read-only (03 §6.1 close).
	if info, err := statPathUsage(m.home + "/workspace/capsules/" + wi); err != nil || !info.IsDir() || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the paused capsule must be sealed read-only, got %v err=%v", info, err)
	}
	// Queue evolution: a paused WI is NOT executable.
	for _, id := range inst.readyQueueIDs(t) {
		if id == wi {
			t.Fatal("a paused WI must not be executable in the ready queue")
		}
	}

	// Observe DURING the pause: the local layer-1 job is gone; the
	// daemon answers with the layer-2 read-through of the published HEAD.
	obs := m.pfOn("observe", wi)
	if !strings.Contains(obs, "current_step") {
		t.Fatalf("observe during pause must serve the published progress:\n%s", obs)
	}

	// Resume through the CLI: fresh claim, generation 2, NEW session.
	sess1 := m.spawnSession(1)
	out = m.pfOn("resume", wi)
	if !strings.Contains(out, "resumed") {
		t.Fatalf("pf resume must confirm a fresh claim, got:\n%s", out)
	}
	waitUntilUse(t, "the generation-2 agent", func() bool { return m.spawnCount() == 2 })
	sess2 := m.spawnSession(2)
	if sess1 == sess2 {
		t.Fatalf("resume must derive a NEW session id, got %s twice", sess1)
	}
	if got := inst.wiStatus(t, wi); got != "running" {
		t.Fatalf("resumed WI must be running, got %s", got)
	}
	// Layer-1 is alive again: the observe shows the gen-2 job ring.
	obs = m.pfOn("observe", wi)
	if !strings.Contains(obs, `"generation":2`) {
		t.Fatalf("observe after resume must show the generation-2 job_started:\n%s", obs)
	}

	// Wrap up.
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("WI must wrap, got %s", got)
	}
}

// statPathUsage stats a path for the sealed-capsule assertion.
func statPathUsage(path string) (os.FileInfo, error) { return os.Stat(path) }
