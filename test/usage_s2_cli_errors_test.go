package test

// usage_s2_cli_errors_test.go: S2 — the CLI error paths a human actually
// hits, against the real binaries: wrong api key (typed envelope, human
// AND --json fidelity), unreachable server, unknown wi id, cancelling a
// RUNNING work item (must refuse per the state machine), the
// --waiting-on queue filter, config-file precedence (empty --config must
// load the DEFAULT path; a missing --config file is not an error), and
// the daemon-face-down behaviors (ops dial error; observe read-through
// fallback). Pins the doground fixes: no doubled error codes in either
// face's rendered details.

import (
	"strings"
	"testing"
)

func TestUsageS2CLIErrorPaths(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s2")
	m.start()
	wi := inst.wiCreate(t, "S2: error paths")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })

	// 1. Wrong api key: typed envelope, exit 1, no stack trace.
	errs := inst.pfExit(t, 1, "--api-key", "pfk_wrong_key", "queue")
	if !strings.Contains(errs, "CORE-401") || strings.Contains(errs, "goroutine") ||
		strings.Contains(errs, "CORE-401: CORE-401") {
		t.Fatalf("wrong key must render ONE typed CORE-401, no stack, no doubled code: %q", errs)
	}
	// 1b. --json fidelity: the raw wire envelope on stderr, verbatim.
	errs = inst.pfExit(t, 1, "--json", "--api-key", "pfk_wrong_key", "queue")
	if !strings.HasPrefix(strings.TrimSpace(errs), `{"error":{"code":"CORE-401"`) {
		t.Fatalf("--json error output must be the raw server envelope, got %q", errs)
	}

	// 2. Unreachable server: one clean line, exit 1.
	errs = inst.pfExit(t, 1, "--server", "http://127.0.0.1:1", "queue")
	if !strings.Contains(errs, "connection refused") || strings.Contains(errs, "goroutine") {
		t.Fatalf("unreachable server must be a one-line dial error: %q", errs)
	}

	// 3. Unknown wi id: typed CORE-404, and --json emits it verbatim.
	errs = inst.pfExit(t, 1, "file", "show", "wi_nope00000000")
	if !strings.Contains(errs, "CORE-404: work item wi_nope00000000") {
		t.Fatalf("unknown wi must render CORE-404, got %q", errs)
	}
	errs = inst.pfExit(t, 1, "--json", "file", "show", "wi_nope00000000")
	if !strings.Contains(errs, `"code":"CORE-404"`) {
		t.Fatalf("--json unknown wi must carry the typed code, got %q", errs)
	}

	// 4. Cancel a RUNNING work item from the CLI: the state machine
	// refuses (CORE-409, active generation).
	errs = inst.pfExit(t, 1, "cancel", wi)
	if !strings.Contains(errs, "CORE-409") || !strings.Contains(errs, "running") {
		t.Fatalf("cancel of a running WI must refuse with CORE-409 naming the status: %q", errs)
	}

	// 5. The --waiting-on queue filter: set a value, filter finds the WI
	// (assert on --json: the human table renders the slug, not the id).
	inst.pf(t, "file", "edit", wi, "--waiting-on", "owner")
	out := inst.pf(t, "--json", "queue", "--waiting-on", "owner")
	if !strings.Contains(out, wi) {
		t.Fatalf("--waiting-on owner must list the WI, got:\n%s", out)
	}
	out = inst.pf(t, "--json", "queue", "--waiting-on", "runtime:mac_s2")
	if strings.Contains(out, wi) {
		t.Fatalf("--waiting-on runtime filter must not match owner, got:\n%s", out)
	}

	// 6. Config precedence: an EMPTY --config loads the DEFAULT path
	// (the fix this round pins); a MISSING --config file is not an error
	// (defaults apply).
	if out := inst.pfExit(t, -1, "--config", "", "users", "list"); !strings.Contains(out, "usr_root") {
		t.Fatalf("empty --config must load the default config path, got:\n%s", out)
	}
	// A missing file under an EMPTY XDG home falls back to defaults: the
	// call still reaches the server through the flag overrides.
	hollow := t.TempDir()
	out = runPfWithXDG(t, inst, hollow, "--config", filepathJoin(hollow, "missing.toml"),
		"--api-key", inst.adminKey, "--server", inst.url, "users", "list")
	if !strings.Contains(out, "usr_root") {
		t.Fatalf("a missing --config file must apply defaults, not fail, got:\n%s", out)
	}

	// 7. Daemon face down: ops status reports the dial error; observe
	// falls back to the AIHub read-through (with a note on stderr).
	dead := filepathJoin(inst.socketDir, "dead.sock")
	errs = inst.pfExit(t, 1, "--socket", dead, "ops", "status")
	if !strings.Contains(errs, "dial unix") {
		t.Fatalf("dead socket must surface the dial error: %q", errs)
	}
	// Before any publish the read-through layer renders the
	// first-publish-pending empty-chain view instead of surfacing the
	// typed CORE-504 as a hard error (polyforge#36): exit 0, view on
	// stdout, fallback note on stderr.
	out = inst.pfExit(t, 0, "--socket", dead, "observe", wi)
	if !strings.Contains(out, "no published revision yet (claimed, first publish pending)") {
		t.Fatalf("claimed-unpublished observe must render the empty-chain view, got %q", out)
	}
	if _, noteIO := inst.pfIO("--socket", dead, "observe", wi); !strings.Contains(noteIO, "daemon not reachable locally") {
		t.Fatalf("daemon-down observe must still note the fallback on stderr: %q", noteIO)
	}
	// A working publish through the daemon face (the WI Agent's own
	// operation, 04 §5.1) gives the read-through layer something to show.
	udsBody(t, m.socket, "POST", "/jobs/"+wi+"/publish_request",
		map[string]any{"reason": "step_completed"}, nil, nil)
	stdout, stderrIO := inst.pfIO("--socket", dead, "observe", wi)
	if !strings.Contains(stderrIO, "daemon not reachable locally") || !strings.Contains(stdout, "current_step") {
		t.Fatalf("daemon-down observe after a publish: want note on stderr + published progress on stdout, got %q / %q", stderrIO, stdout)
	}

	// 8. Daemon-face errors: the answered/consumed pins. No open question
	// on this WI (the ask-mode agent's question is open — answer the
	// WRONG question id instead).
	errs = m.pfOnExit(1, "answer", wi, "--question", "q_wrong", "--text", "x", "--by", "usr_root")
	if !strings.Contains(errs, "DAEMON-409") || strings.Contains(errs, "(DAEMON-409)") {
		t.Fatalf("consumed-question answer must render DAEMON-409 without the doubled code: %q", errs)
	}

	// Wrap up the running WI so nothing leaks past cleanup.
	m.pfOn("wrap", wi)
}
