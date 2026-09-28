package test

// usage_s9_ops_test.go: S9 — the ops face through the CLI against the
// real daemon: status, mode query/set (batch), drain enter/abort, and —
// the load-bearing pin — `pf ops shutdown` with a REMAINING live job:
// the graceful close must actually LAND server-side (the WI becomes
// paused) before the daemon process exits. Before the polyforge#17 fix
// this exact drill left the WI running forever: the daemon face answered
// and ran the close in the background, the process main ran a SECOND
// shutdown sequence that skipped the job via the one-close guard, and
// the process exited mid-close. A second variant kills the WI Agent
// first: the shutdown then closes over the dead-writers shortcut
// (05 §2.7 condition ①) and still lands the pause.

import (
	"strings"
	"testing"
)

func TestUsageS9ModeOpsAndShutdown(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s9")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S9: ops face and graceful shutdown")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })

	// ops status: machine, mode, queue, the job row.
	out := m.pfOn("ops", "status")
	for _, want := range []string{"mac_s9", "interactive", wi} {
		if !strings.Contains(out, want) {
			t.Fatalf("ops status must show %s:\n%s", want, out)
		}
	}

	// Mode query + set (batch), then drain enter/abort — the transitions
	// ride the state machine and answer with the new mode.
	if out := m.pfOn("ops", "mode"); !strings.Contains(out, "interactive") {
		t.Fatalf("mode query must show interactive, got:\n%s", out)
	}
	if out := m.pfOn("ops", "mode", "set", "--max-concurrent-jobs", "2", "--mode", "batch"); !strings.Contains(out, `"mode": "batch"`) {
		t.Fatalf("mode set must answer the new mode, got:\n%s", out)
	}
	if out := m.pfOn("ops", "mode", "drain"); !strings.Contains(out, `"mode": "drain"`) {
		t.Fatalf("drain must answer drain, got:\n%s", out)
	}
	if out := m.pfOn("ops", "mode", "abort-drain"); !strings.Contains(out, `"mode": "interactive"`) {
		t.Fatalf("abort-drain must answer interactive, got:\n%s", out)
	}

	// Graceful shutdown WITH a remaining live job: the close lands
	// server-side (paused) BEFORE the process exits.
	out = m.pfOn("ops", "shutdown")
	if !strings.Contains(out, "graceful shutdown requested") {
		t.Fatalf("ops shutdown must confirm, got:\n%s", out)
	}
	waitUntilUse(t, "the daemon process to exit", func() bool { return daemonExitedUsage(m.cmd) })
	// B-1/RH-1 pin: the ops-face shutdown path must end the process with
	// exit 0, not fall through to the CLI dispatcher (exit 2).
	daemonAssertExit0Usage(t, m.cmd)
	waitUntilUse(t, "the remaining job to close as paused", func() bool { return inst.wiStatus(t, wi) == "paused" })
	if !strings.Contains(m.daemonLog(), "shutdown sequence starting") {
		t.Fatalf("the daemon log must show the graceful sequence:\n%s", m.daemonLog())
	}
}

func TestUsageS9ShutdownWithDeadAgent(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s9b")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S9b: shutdown while the WI Agent is already dead")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })
	killAgentUsage(t, m.agentPIDs()[0])

	// The shutdown closes over the dead agent via the process-group-dead
	// shortcut (05 §2.7): boundary notice is undeliverable but the group
	// is provably empty, so the final flush runs and the pause lands.
	m.pfOn("ops", "shutdown")
	waitUntilUse(t, "the daemon process to exit", func() bool { return daemonExitedUsage(m.cmd) })
	// B-1/RH-1 pin: the dead-writers-shortcut shutdown path must also end
	// the process with exit 0.
	daemonAssertExit0Usage(t, m.cmd)
	waitUntilUse(t, "the job to close as paused", func() bool { return inst.wiStatus(t, wi) == "paused" })
}
