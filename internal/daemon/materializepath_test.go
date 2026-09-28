package daemon

// materializepath_test.go: F2-3 regressions — materialization is wired into
// the PRODUCTION spawn path (03 §4.5/§7), not just the capsule library:
//
//   - a new claim materializes the workflow/agent snapshot BEFORE the WI
//     Agent is derived (asserted at the spawner: the capsule already carries
//     workflow.md + the completion marker when the derivation starts), and
//     the jobs[] record persists the bundle identity the crash-recovery
//     trust gate needs;
//   - a crash mid-materialize (fabricated torn snapshot: no marker, partial
//     targets, staging husk) is NEVER trusted by the re-derivation: the
//     snapshot is re-materialized wholesale before the new instance starts;
//   - a complete snapshot is TRUSTED by a same-generation re-derivation
//     (03 §7: no re-read of the checkout — the marker is untouched);
//   - a materialization that cannot complete fails CLOSED: the job is left
//     without an agent — human attention, never a silent respawn.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// capsuleAt returns the spawned job's capsule handle from the env.
func (e *denv) capsuleAt(t *testing.T) *capsule.Capsule {
	t.Helper()
	c, _, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// recordedIdentity reads the bundle identity the spawn persisted into the
// jobs[] record (the re-derivation gate's input).
func (e *denv) recordedIdentity(t *testing.T, adapter string) capsule.MaterializationIdentity {
	t.Helper()
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st.FindJob(e.wi)
	if j == nil {
		t.Fatal("jobs[] record missing")
	}
	if j.BundleName == "" || j.BundleDigest == "" {
		t.Fatalf("the jobs[] record must carry the bundle identity for the recovery gate: %+v", j)
	}
	return capsule.MaterializationIdentity{
		BundleName: j.BundleName, BundleDigest: j.BundleDigest,
		Adapters: []string{adapter},
	}
}

// TestClaimMaterializesBeforeSpawn (F2-3): the 04 §6.1 claim path goes
// through the materialization gate — a newly spawned capsule carries the
// workflow snapshot + completion marker BEFORE the agent derivation, and
// the persisted job record carries the bundle identity. Before the fix the
// path went from capsule create straight to spawn: no snapshot, no marker,
// no gate.
func TestClaimMaterializesBeforeSpawn(t *testing.T) {
	e := newDaemonEnv(t)
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.spawner.spawns) != 1 || !e.spawner.sawSnapshot[0] {
		t.Fatalf("the capsule must carry the materialized snapshot (workflow.md + marker) BEFORE the derivation, sawSnapshot=%v", e.spawner.sawSnapshot)
	}
	c := e.capsuleAt(t)
	for _, rel := range []string{"workflow.md", ".pi/agents/code.md", "sync/materialized.json"} {
		if _, err := os.Stat(filepath.Join(c.Dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("materialized target %s must exist after the spawn: %v", rel, err)
		}
	}
	if !c.MaterializationComplete(e.recordedIdentity(t, "pi")) {
		t.Fatal("the trust gate must report the fresh snapshot complete for the pinned identity")
	}
}

// TestCrashMidMaterializeRederivationRematerializes (F2-3): a crash in the
// middle of a materialization leaves a torn snapshot (no completion marker,
// partial targets, staging residue). The re-derivation path must see the
// gate refuse trust and re-materialize WHOLESALE before deriving the new
// instance — the re-derived agent never starts over a torn snapshot.
func TestCrashMidMaterializeRederivationRematerializes(t *testing.T) {
	e := newDaemonEnv(t)
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := e.recordedIdentity(t, "pi")
	c := e.capsuleAt(t)

	// The recorded agent DIES (04 §7.1 step 4: collect, then re-derive).
	if err := e.spawner.agents[0].Terminate(2 * time.Second); err != nil {
		t.Fatal(err)
	}

	// Fabricate the crash-mid-materialize aftermath on the SAME capsule:
	// marker gone, one agent file gone, staging husk left behind.
	if err := c.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(capsule.RelMaterializeMarker))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.Dir, ".pi", "agents", "code.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.Dir, "sync", "materialize-staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c.MaterializationComplete(id) {
		t.Fatal("setup: the torn state must not pass the gate")
	}

	// Restart over the same data dir: the dead-agent branch must refuse the
	// torn snapshot and re-materialize before the re-derivation.
	d2, err := New(e.d.cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	sp2 := d2.spawner.(*integSpawner)
	if len(sp2.spawns) != 1 {
		t.Fatalf("the dead agent must be re-derived exactly once, got %d", len(sp2.spawns))
	}
	if !sp2.sawSnapshot[0] {
		t.Fatal("the re-derivation must start only after the snapshot is complete — never over a torn one")
	}
	// The wholesale repair restored every target and ended complete.
	c2 := e.capsuleAt(t)
	for _, rel := range []string{"workflow.md", ".pi/agents/code.md"} {
		if _, err := os.Stat(filepath.Join(c2.Dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("the re-materialization must restore %s: %v", rel, err)
		}
	}
	if !c2.MaterializationComplete(id) {
		t.Fatal("the re-materialized snapshot must pass the trust gate")
	}
}

// TestRederivationTrustsCompleteSnapshot (F2-3, 03 §7): a same-generation
// re-derivation with a COMPLETE snapshot reuses it — no re-read of the
// checkout, the completion marker is left untouched.
func TestRederivationTrustsCompleteSnapshot(t *testing.T) {
	e := newDaemonEnv(t)
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := e.recordedIdentity(t, "pi")
	c := e.capsuleAt(t)
	markerBefore, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(capsule.RelMaterializeMarker)))
	if err != nil {
		t.Fatal(err)
	}
	if !c.MaterializationComplete(id) {
		t.Fatal("setup: the complete snapshot must pass the gate before the crash")
	}

	if err := e.spawner.agents[0].Terminate(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	d2, err := New(e.d.cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	sp2 := d2.spawner.(*integSpawner)
	if len(sp2.spawns) != 1 || !sp2.sawSnapshot[0] {
		t.Fatalf("the dead agent must be re-derived over the trusted snapshot, spawns=%d sawSnapshot=%v", len(sp2.spawns), sp2.sawSnapshot)
	}
	// Trusted, not re-materialized: the marker bytes are untouched.
	markerAfter, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(capsule.RelMaterializeMarker)))
	if err != nil {
		t.Fatal(err)
	}
	if string(markerBefore) != string(markerAfter) {
		t.Fatal("a complete snapshot must be REUSED by the same-generation re-derivation, not re-materialized")
	}
}

// TestRederivationFailsClosedWhenMaterializationCannotComplete (F2-3): when
// the snapshot is incomplete and the checkout cannot re-materialize it (the
// bundle vanished from the daemon's checkout), the re-derivation is DENIED:
// the job is left without an agent — human attention — and nothing is
// spawned over a snapshot nobody can vouch for.
func TestRederivationFailsClosedWhenMaterializationCannotComplete(t *testing.T) {
	e := newDaemonEnv(t)
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := e.capsuleAt(t)
	if err := c.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(capsule.RelMaterializeMarker))); err != nil {
		t.Fatal(err)
	}
	if err := e.spawner.agents[0].Terminate(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	// The daemon's checkout loses the bundle: materialization cannot
	// complete.
	if err := os.RemoveAll(filepath.Join(e.d.cfg.WorkflowsDir, "demo")); err != nil {
		t.Fatal(err)
	}

	d2, err := New(e.d.cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	sp2 := d2.spawner.(*integSpawner)
	if len(sp2.spawns) != 0 {
		t.Fatalf("a re-derivation over an unmaterializable snapshot must be DENIED, got %d spawns", len(sp2.spawns))
	}
	// The job stays in the runtime WITHOUT an agent: human attention.
	d2.mu.Lock()
	jr, ok := d2.jobs[e.wi]
	d2.mu.Unlock()
	if !ok || jr.agent != nil {
		t.Fatalf("the failed job must stay in the runtime without an agent (human attention), jr=%+v ok=%v", jr, ok)
	}
}
