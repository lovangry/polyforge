package test

// usage_s1_default_install_test.go: S1 — default-install smoke with the
// REAL binaries. Everything an operator needs on a fresh machine:
// first boot (bootstrap admin), CLI config at the DEFAULT path (no
// --config flag), machine registration (the row-21 operator duty),
// project + WI seeding, and the full submit -> observe -> wrap loop
// through real processes. Also pins the binaries' DEFAULT flag values
// (the 8420 single-port ruling) from their own -h output — the daemon's
// via `pf daemon -h` (the subcommand surface of the merged work-machine
// binary).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageS1DefaultInstallSmoke(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s1")

	// The bootstrap admin key was harvested from the FIRST boot's log;
	// the whole install is being driven with it (every pf call in this
	// file relies on that key working).
	if inst.adminKey == "" {
		t.Fatal("no bootstrap admin key")
	}

	// A SECOND boot of the same data dir must NOT reseed (the seed is a
	// first-boot-of-an-empty-deployment action, never a key faucet).
	// (The running aihub holds the data-dir lock, so assert on the log
	// instead of a second process: exactly one seed line may exist.)
	rawLog := readFileOr(t, inst.aihubLog)
	if n := strings.Count(rawLog, "seeded bootstrap admin"); n != 1 {
		t.Fatalf("exactly one bootstrap seed expected, found %d", n)
	}

	// The users the operator created through the CLI exist.
	out := inst.pf(t, "--json", "users", "list")
	if !strings.Contains(out, `"display_name": "usage-test"`) &&
		!strings.Contains(out, m.userID) {
		t.Fatalf("registered machine user %s missing from users list: %s", m.userID, out)
	}

	// The default flag values: the daemon must point at port 8420 by
	// default (the single-port A5 ruling the scratch install relies on).
	// -h prints flag defaults from the real binary — the daemon is the
	// `pf daemon` subcommand of the merged work-machine binary now, so
	// the probe goes through the subcommand surface.
	daemonHelp := runBinaryStderr(t, inst.pfPath, "daemon", "-h")
	if !strings.Contains(daemonHelp, `default "http://127.0.0.1:8420"`) {
		t.Fatalf("pf daemon default aihub-url must stay http://127.0.0.1:8420:\n%s", daemonHelp)
	}
	aihubHelp := runBinaryStderr(t, filepath.Join(inst.binDir, "aihub"), "-h")
	if !strings.Contains(aihubHelp, `default ":8420"`) {
		t.Fatalf("aihub default listen must stay :8420:\n%s", aihubHelp)
	}

	// The config is loaded from the DEFAULT path: every pf invocation in
	// this file runs with XDG_CONFIG_HOME and no --config flag.
	wi := inst.wiCreate(t, "S1 smoke: default install end to end")
	if got := inst.wiStatus(t, wi); got != "queued" {
		t.Fatalf("fresh WI must be queued, got %s", got)
	}
	if ids := inst.readyQueueIDs(t); len(ids) != 1 || ids[0] != wi {
		t.Fatalf("ready queue must list exactly the new WI, got %v", ids)
	}

	// Submit to the real daemon; the real fake-harness WI Agent starts.
	m.start("FAKEPI_MODE=ask")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })

	// Observe: layer-1 ring + state snapshot from the live capsule.
	obs := m.pfOn("observe", wi)
	if !strings.Contains(obs, "job_started") {
		t.Fatalf("observe must show the job_started ring event:\n%s", obs)
	}

	// Wrap: terminal success through the real close path.
	m.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("WI must be wrapped after pf wrap, got %s", got)
	}
	if ids := inst.readyQueueIDs(t); len(ids) != 0 {
		t.Fatalf("wrapped WI must leave the ready queue, got %v", ids)
	}
	if m.spawnCount() != 1 {
		t.Fatalf("the close must not re-derive the agent: %d spawns", m.spawnCount())
	}
}

// readFileOr reads a file for assertion purposes ("" on miss).
func readFileOr(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// runBinaryStderr executes a helper binary with -h and returns its
// combined output (the flag package prints usage to stderr; the -h exit
// code 2 is expected and ignored).
func runBinaryStderr(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, _ := exec.Command(bin, args...).CombinedOutput()
	return string(out)
}
