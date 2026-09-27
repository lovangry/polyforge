package test

// usage_s8_wrap_fail_test.go: S8 — wrap-with-fail through the CLI against
// the real daemon: the run ends with reason fail; the WI is terminal
// failed; the ready queue no longer offers it; no re-claim is possible —
// submit refuses up front (the polyforge#17 F5 fix: CLI precheck for the
// typed terminal refusal, daemon-side precheck as the belt) and a cancel
// of the terminal WI is refused by the state machine.

import (
	"strings"
	"testing"
)

func TestUsageS8WrapWithFail(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s8")
	m.start("FAKEPI_MODE=ask")

	wi := inst.wiCreate(t, "S8: wrap with --fail")
	m.pfOn("submit", wi)
	waitUntilUse(t, "the WI Agent to be derived", func() bool { return m.spawnCount() == 1 })

	// End the run with reason fail.
	out := m.pfOn("wrap", wi, "--fail")
	if !strings.Contains(out, "fail requested") {
		t.Fatalf("pf wrap --fail must confirm, got:\n%s", out)
	}
	waitUntilUse(t, "the WI to be failed", func() bool { return inst.wiStatus(t, wi) == "failed" })

	// Queue evolution: a failed WI is terminal — not executable.
	for _, id := range inst.readyQueueIDs(t) {
		if id == wi {
			t.Fatal("a failed WI must not be executable")
		}
	}

	// No re-claim: the CLI submit precheck refuses with the typed
	// terminal verdict (F5) — never a silent "accepted".
	errs := inst.pfExit(t, 1, "submit", wi)
	if !strings.Contains(errs, "terminal (failed)") {
		t.Fatalf("submit of a failed WI must refuse up front, got %q", errs)
	}
	// The daemon-side precheck refuses even a direct raw claim on the UDS
	// face (belt to the CLI's braces; the machine face does not trust the
	// caller): DAEMON-409 naming the terminal status.
	code, raw := udsBody(t, m.socket, "POST", "/jobs/"+wi+"/claim", map[string]any{"priority": "normal"}, nil, nil)
	if code != 409 || !strings.Contains(string(raw), "DAEMON-409") || !strings.Contains(string(raw), "failed") {
		t.Fatalf("the daemon's own precheck must refuse the terminal WI: %d %s", code, raw)
	}
	// An unknown id is refused typed at submit too (no silent acceptance).
	errs = inst.pfExit(t, 1, "submit", "wi_nope00000000")
	if !strings.Contains(errs, "CORE-404") {
		t.Fatalf("submit of an unknown WI must refuse CORE-404, got %q", errs)
	}
	// Cancel of the terminal WI: the state machine refuses (CORE-410).
	errs = inst.pfExit(t, 1, "cancel", wi)
	if !strings.Contains(errs, "CORE-410") {
		t.Fatalf("cancel of a failed WI must refuse, got %q", errs)
	}
}
