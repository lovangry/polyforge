package test

// usage_s6_dead_agent_restart_test.go: S6 — daemon restart with a DEAD
// agent, real processes: kill -9 the agent AND the daemon, restart, and
// pin the 04 §7.1 step-4 machinery: the residual process group is
// collected (proven dead), the agent is re-derived with the SAME session
// id (the only session-reuse case, 03 §4.4), and the unpublished local
// changes in the capsule are intact. The post-crash close is fenced
// (generation token lost with the process, INV-D1/D8) — the honest typed
// verdict — and the drill converges via CLI takeover + wrap.

import (
	"strings"
	"testing"
)

func TestUsageS6RestartDeadAgent(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s6")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S6: restart with a dead agent")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })
	// Unpublished local work: never sent through a publish.
	unpub := m.home + "/workspace/capsules/" + wi + "/artifacts/unpub.md"
	if err := writeFileUsage(unpub, "unpublished change\n"); err != nil {
		t.Fatal(err)
	}

	// Kill BOTH real processes with SIGKILL (a hard crash of the whole
	// machine's execution plane).
	m.kill9()
	killAgentUsage(t, m.agentPIDs()[0])

	// Restart: crash recovery collects the (already dead) residual group
	// and re-derives the agent with the SAME session id.
	sess1 := m.spawnSession(1)
	m.start("FAKEPI_MODE=ask")
	waitUntilUse(t, "the re-derived agent", func() bool { return m.spawnCount() == 2 })
	sess2 := m.spawnSession(2)
	if sess1 != sess2 {
		t.Fatalf("the re-derivation must reuse the session id: %s vs %s", sess1, sess2)
	}
	// The unpublished changes survived the crash + re-derivation (the
	// capsule is opened, never rebuilt, in the same-generation path).
	if got := readFileOr(t, unpub); got != "unpublished change\n" {
		t.Fatalf("unpublished work must be intact, got %q", got)
	}
	// The observe stream shows the honest rederivation event.
	if obs := m.pfOn("observe", wi); !strings.Contains(obs, "wi_agent_rederived") {
		t.Fatalf("observe must show wi_agent_rederived:\n%s", obs)
	}

	// The post-crash close is fenced (token lost with the process):
	// typed DAEMON-403 stale_generation, scene quarantined — and the
	// unpublished work is PRESERVED in the quarantine for audit.
	errs := m.pfOnExit(1, "wrap", wi)
	if !strings.Contains(errs, "stale_generation") {
		t.Fatalf("the post-crash close must be fenced, got %q", errs)
	}
	qkept := false
	if hits, _ := filepathGlob(m.home + "/workspace/capsules/_quarantine/" + wi + "/*/artifacts/unpub.md"); len(hits) == 1 {
		qkept = readFileOr(t, hits[0]) == "unpublished change\n"
	}
	if !qkept {
		t.Fatal("the quarantined scene must keep the unpublished work")
	}

	// Convergence: elevate (takeover is admin), takeover, wrap.
	inst.pf(t, "users", "patch", m.userID, "--role", "admin")
	m.pfOn("submit", wi, "--takeover")
	waitUntilUse(t, "the post-takeover agent", func() bool { return m.spawnCount() == 3 })
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the drill must converge to wrapped, got %s", got)
	}
}
