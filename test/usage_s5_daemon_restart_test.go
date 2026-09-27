package test

// usage_s5_daemon_restart_test.go: S5 — kill -9 of the REAL pf-daemon
// process mid-run with the WI Agent alive (both real OS processes), then
// a restart of the real binary. Pins, against real processes:
//   - the agent SURVIVES the daemon's kill -9 (it is its own process
//     group, 04 §6.1a Setpgid);
//   - the restarted daemon re-attaches the LIVE agent: exactly one agent,
//     no respawn (04 §7.1 step 3), state.json loads, observe answers;
//   - a close on the adopted agent is honestly fenced (the generation
//     token lived only in the killed process's memory, INV-D1/D8): the
//     typed DAEMON-403 stale_generation verdict, scene quarantined —
//     the same convergence the in-process kill9 drill pins;
//   - the operator converges via the CLI: submit --takeover (gen 3) then
//     wrap.

import (
	"path/filepath"
	"strings"
	"testing"
)

// filepathGlob is filepath.Glob, re-exposed for the scenario files.
func filepathGlob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

func TestUsageS5DaemonRestartLiveAgent(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s5")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S5: daemon kill -9 with a live agent")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })
	agentPID := m.agentPIDs()[0]

	// kill -9 the daemon process; the agent (own process group) survives.
	m.kill9()
	waitUntilUse(t, "the orphaned agent to settle", func() bool { return processAliveUsage(agentPID) })
	if m.spawnCount() != 1 {
		t.Fatalf("kill -9 must not touch the agent: %d spawns", m.spawnCount())
	}

	// Restart the REAL daemon over the same home/socket: crash recovery
	// (04 §7.1) loads state.json and ADOPTS the live agent — never a
	// respawn (exactly one spawn in the journal).
	m.start("FAKEPI_MODE=ask")
	if got := m.spawnCount(); got != 1 {
		t.Fatalf("the live agent must be ADOPTED, not re-derived: %d spawns", got)
	}
	if !processAliveUsage(agentPID) {
		t.Fatal("the adopted agent process must still be the original one")
	}
	// The job is exposed and observe answers (layer-1 with the adopted
	// runtime; the ring restarted empty, the snapshot serves execution.json).
	out := m.pfOn("ops", "status")
	if !strings.Contains(out, wi) {
		t.Fatalf("the recovered daemon must expose the job:\n%s", out)
	}
	obs := m.pfOn("observe", wi)
	if !strings.Contains(obs, "current_step") {
		t.Fatalf("observe must answer after the restart:\n%s", obs)
	}

	// A close on the adopted agent is fenced: the generation token was
	// lost with the killed process (INV-D1/D8 — state.json deliberately
	// records no execution secret). Typed DAEMON-403 stale_generation,
	// scene quarantined, job dropped.
	errs := m.pfOnExit(1, "wrap", wi)
	if !strings.Contains(errs, "DAEMON-403") || !strings.Contains(errs, "stale_generation") {
		t.Fatalf("the post-crash close must be fenced typed, got %q", errs)
	}
	if strings.Contains(m.pfOn("ops", "status"), wi) {
		t.Fatal("the fenced close must drop the local job")
	}
	// The quarantined scene: moved under _quarantine/<wi>/ with the durable
	// marker naming the cause (04 §7.3: preserved for audit, never deleted).
	qmarker := ""
	if hits, _ := filepathGlob(m.home + "/workspace/capsules/_quarantine/" + wi + "/*/logs/quarantine.json"); len(hits) == 1 {
		qmarker = readFileOr(t, hits[0])
	}
	if !strings.Contains(qmarker, "stale_generation") {
		t.Fatalf("the fenced scene must be quarantined with a stale_generation marker, log:\n%s", m.daemonLog())
	}

	// Operator convergence through the CLI. Force-takeover is admin-only
	// (02 §6.2): the machine's plain writer key gets the typed CORE-403
	// AT SUBMIT TIME (the polyforge#17 F7 fix — before it this failure was
	// swallowed and the submit answer lied "accepted").
	errs = m.pfOnExit(1, "submit", wi, "--takeover")
	if !strings.Contains(errs, "CORE-403") || !strings.Contains(errs, "force-takeover requires admin") {
		t.Fatalf("takeover with a writer machine key must surface the typed CORE-403, got %q", errs)
	}
	// The operator's designed path: elevate the machine user via M1.
	inst.pf(t, "users", "patch", m.userID, "--role", "admin")
	m.pfOn("submit", wi, "--takeover")
	waitUntilUse(t, "the gen-3 agent", func() bool { return m.spawnCount() == 2 })
	if got := inst.wiStatus(t, wi); got != "running" {
		t.Fatalf("the takeover must re-open the WI, got %s", got)
	}
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the drill must converge to wrapped, got %s", got)
	}
}

// processAliveUsage is the real-process liveness probe (signal 0).
func processAliveUsage(pid int) bool {
	return procSignal0(pid) == nil
}
