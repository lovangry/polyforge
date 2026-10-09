package runtime

// legacy_state_failclosed_test.go: pins the deploy-time fail-closed probe
// (polyforge#106) — an in-flight WI whose capsule state is LEGACY-ONLY
// (an execution.json the driver never wrote: no driver journal stamp,
// i.e. no bundle_digest) is refused outright on the recovery path. The
// refusal names the WI and the missing driver journal, offers no
// migration path (no re-derivation, no conversion), and closes the WI
// blocked for its owner. A capsule the driver stamped recovers normally.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func writeLegacyExecution(t *testing.T, capDir, workflowStatus string) {
	t.Helper()
	dir := filepath.Join(capDir, "runtime-state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The legacy WI-Agent shape: workflow_status and steps only — no
	// bundle_digest (the driver stamps every execution.json it owns).
	blob := `{"work_item_id":"wi","current_step":"grill","workflow_status":"` +
		workflowStatus + `","steps":{"grill":{"status":"in_progress"}}}`
	if err := os.WriteFile(filepath.Join(dir, "execution.json"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInFlightLegacyOnlyStateFailsClosed(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_legacy"
	j := seedJob(r, wi)
	writeLegacyExecution(t, j.capsuleDir, "running")
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	// No re-derivation: the engine starter is never reached.
	if n := sp.callCount(); n != 1 { // installOldAgent's own call only
		t.Fatalf("the legacy-only WI must not be re-derived, got %d spawns", n)
	}
	// Fail closed: bounded fail — blocked at the hub, job dropped.
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("expected RTFinished(blocked), got %+v", msg)
	}
	if !strings.Contains(msg.Detail, "LEGACY-ONLY") || !strings.Contains(msg.Detail, wi) {
		t.Fatalf("the refusal must name the WI and the legacy-only state, got %q", msg.Detail)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != api.StatusBlocked {
		t.Fatalf("the close must be blocked, got %+v", hub.lastEnd())
	}
	if r.jobOf(wi) != nil {
		t.Fatal("the legacy-only WI must leave the live set")
	}
}

func TestDriverStampedStateRecoversNormally(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_driver"
	j := seedJob(r, wi)
	dir := filepath.Join(j.capsuleDir, "runtime-state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The driver's own shape: the bundle_digest stamp is present.
	blob := `{"work_item_id":"` + wi + `","current_step":"grill","workflow_status":"running","bundle_digest":"deadbeef","steps":{}}`
	if err := os.WriteFile(filepath.Join(dir, "execution.json"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 {
		t.Fatalf("a driver-stamped WI must re-derive, got %d spawns", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func TestMissingExecutionStateRecoversNormally(t *testing.T) {
	// No execution.json yet: the driver initializes it — not legacy state.
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_fresh"
	j := seedJob(r, wi)
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 {
		t.Fatalf("a pre-init capsule must re-derive, got %d spawns", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}
