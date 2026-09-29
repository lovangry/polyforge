package test

// usage_s7_takeover_test.go: S7 — takeover between two REAL daemons: two
// `pf daemon` processes with different machine identities, homes and
// sockets on one box (two "machines"). Machine A runs the WI and
// publishes a working revision; machine B force-takes-over from its own
// CLI; B's daemon rebuilds its capsule via the row-19 replay (the
// published chain, including A's working revision); B wraps. A's next
// local write attempt is fenced and its scene quarantined.

import (
	"strings"
	"testing"
)

func TestUsageS7CrossMachineTakeover(t *testing.T) {
	inst := newUsageInstall(t)
	mA := inst.newUsageMachine(t, "mac_s7a")
	mB := inst.newUsageMachine(t, "mac_s7b")
	mA.start("FAKEPI_MODE=ask")

	// A runs the WI and publishes a working revision (the WI Agent's own
	// operation, through A's daemon face).
	wi := inst.wiCreate(t, "S7: cross-machine takeover drill")
	mA.pfOn("submit", wi)
	waitUntilUse(t, "A's WI Agent", func() bool { return mA.spawnCount() == 1 })
	code, raw := udsBody(t, mA.socket, "POST", "/jobs/"+wi+"/publish_request",
		map[string]any{"reason": "step_completed"}, nil, nil)
	if code != 200 || !strings.Contains(string(raw), `"published":true`) {
		t.Fatalf("A's working publish failed: %d %s", code, raw)
	}

	// B is a DIFFERENT machine: its own identity, home, workspace, socket.
	// Takeover is admin-only; with B's plain writer key the refusal is
	// typed AT SUBMIT (the F7 fix), not a silent acceptance.
	mB.start("FAKEPI_MODE=ask")
	errs := mB.pfOnExit(1, "submit", wi, "--takeover")
	if !strings.Contains(errs, "force-takeover requires admin") {
		t.Fatalf("B's non-admin takeover must surface the typed refusal, got %q", errs)
	}
	inst.pf(t, "users", "patch", mB.userID, "--role", "admin")

	// B takes over via its own CLI: generation 2 on machine B.
	mB.pfOn("submit", wi, "--takeover")
	waitUntilUse(t, "B's rebuilt scene + agent", func() bool { return mB.spawnCount() == 1 })
	if got := inst.wiStatus(t, wi); got != "running" {
		t.Fatalf("the takeover must leave the WI running on B, got %s", got)
	}
	// B's capsule was rebuilt by the row-19 replay: A's PUBLISHED work is
	// in it (the recovered execution state).
	exec := readCapsuleExecution(t, mB, wi)
	if exec["work_item_id"] != wi {
		t.Fatalf("B's rebuilt capsule must carry the recovered execution, got %v", exec)
	}

	// A (dispossessed) is fenced on its next local write: the pause is
	// refused typed, A's scene quarantined, A's job dropped.
	errs = mA.pfOnExit(1, "pause", wi)
	if !strings.Contains(errs, "stale_generation") {
		t.Fatalf("A's post-takeover write must be fenced, got %q", errs)
	}
	if strings.Contains(mA.pfOn("ops", "status"), wi) {
		t.Fatal("A's fenced scene must drop the local job")
	}

	// B wraps; the generation story ends terminal.
	mB.pfOn("wrap", wi)
	if got := inst.wiStatus(t, wi); got != "wrapped" {
		t.Fatalf("the drill must converge to wrapped, got %s", got)
	}
}
