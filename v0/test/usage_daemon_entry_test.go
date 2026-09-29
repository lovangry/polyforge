package test

// usage_daemon_entry_test.go: the `pf daemon` subcommand's ENTRY and EXIT
// surface, pinned at process level against the real binary:
//   - RH-6/B-2: a stray positional is REJECTED with the usage exit 2 —
//     Go's flag package stops parsing at the first non-flag argument, so
//     `pf daemon stray --workspace-root X` would otherwise silently
//     swallow the flag and start on the DEFAULT workspace; everything
//     after a bare `--` lands in the same positional bucket and is
//     rejected the same way.
//   - RH-1/B-1: the signal-driven graceful shutdown (SIGTERM) ends the
//     process with exit 0 — a main fall-through to the CLI dispatcher
//     would print `unknown command "daemon"` and exit 2, which a
//     supervisor (systemd Restart=on-failure) would loop on.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestUsageDaemonEntryRejectsStrayPositionals(t *testing.T) {
	bins := usageBins(t)
	pf := filepath.Join(bins, "pf")

	// daemonEntryExit runs `pf daemon ...` and returns (exit code,
	// stderr). Rejection happens at flag-parse time — before any api-key
	// check or aihub contact — so no install is needed: a daemon that
	// accepted the stray positional would instead run on and reach the
	// api-key fatal (exit 1) or actually start, never exit 2 here.
	daemonEntryExit := func(args ...string) (int, string) {
		cmd := exec.Command(pf, append([]string{"daemon"}, args...)...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf daemon %v: %v", args, err)
		}
		return ee.ExitCode(), stderr.String()
	}

	t.Run("stray positional suppresses the later flags", func(t *testing.T) {
		code, stderr := daemonEntryExit("stray", "--workspace-root", "/must/not/start")
		if code != 2 {
			t.Fatalf("stray positional must exit 2 (usage error), got %d (stderr %q)", code, stderr)
		}
		for _, want := range []string{"unexpected argument", "stray", "Usage of pf daemon"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("the rejection must name %q:\n%s", want, stderr)
			}
		}
	})
	t.Run("args after -- are positionals too", func(t *testing.T) {
		code, stderr := daemonEntryExit("--", "trailing")
		if code != 2 {
			t.Fatalf("args after -- must exit 2 (usage error), got %d (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "Usage of pf daemon") {
			t.Fatalf("the rejection must print the usage:\n%s", stderr)
		}
	})
}

func TestUsageDaemonSignalShutdownExitZero(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s9c")
	m.start("FAKEPI_MODE=ask")

	// The signal-driven shutdown path: SIGTERM latches the same graceful
	// sequence the ops face requests. The process must end with the ZERO
	// exit status a supervisor reads as a clean stop (RH-1: main returns
	// immediately after runDaemon — the pre-fix fall-through printed
	// `unknown command "daemon"` and exited 2).
	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the daemon: %v", err)
	}
	waitUntilUse(t, "the daemon process to exit", func() bool { return daemonExitedUsage(m.cmd) })
	daemonAssertExit0Usage(t, m.cmd)

	log := m.daemonLog()
	for _, want := range []string{"shutdown sequence starting", "bye"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the daemon log must show %q:\n%s", want, log)
		}
	}
}
