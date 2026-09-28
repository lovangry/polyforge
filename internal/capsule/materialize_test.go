package capsule

// materialize_test.go: SK-5/CR-2 regressions — materialization is atomic
// AS A SET. A kill between the copies must leave the next agent seeing
// either complete-old (crash while staging: live targets untouched, old
// completion marker intact) or an UNTRUSTED torn state (crash after the
// marker removal: no marker, so the consumer re-materializes) — never a
// half-new snapshot read as if it were whole. Partial/staging dirs left
// by a killed run must be cleaned by the next materialization.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// buildBundle writes a workflows/<bundle> checkout with workflow.md and
// agents/code.md carrying the given bodies and returns the checkout root
// plus the matching bundle digest.
func buildBundle(t *testing.T, workflowBody, agentBody string) (string, string) {
	t.Helper()
	wfd := filepath.Join(t.TempDir(), "workflows")
	dir := filepath.Join(wfd, "demo")
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workflow.md"), []byte(workflowBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "code.md"), []byte(agentBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := digest.ComputeBundleDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return wfd, sum
}

func readCapsuleFile(t *testing.T, c *Capsule, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(raw)
}

func stagingPath(c *Capsule) string {
	return filepath.Join(c.Dir, filepath.FromSlash(materializeStagingDir))
}

// ident builds the expected identity matching buildBundle + Materialize's
// recorded marker, for the trust-gate calls.
func ident(sum string, adapters ...string) MaterializationIdentity {
	if len(adapters) == 0 {
		adapters = []string{AdapterPi}
	}
	return MaterializationIdentity{BundleName: "demo", BundleDigest: sum, Adapters: adapters}
}

// withCrashHook installs a ONE-SHOT hook that aborts Materialize the first
// time the named phase fires (simulating a kill at exactly that point).
func withCrashHook(t *testing.T, phase string) {
	t.Helper()
	fired := false
	materializeCrashHook = func(p string) error {
		if p == phase && !fired {
			fired = true
			return errf("CAP-500", "simulated kill at %s", p)
		}
		return nil
	}
	t.Cleanup(func() { materializeCrashHook = nil })
}

// TestMaterializeCompletionMarker: a successful materialization writes the
// completion marker (under sync/, excluded from the sync surface) and the
// identity-validated trust gate reports true; a bare capsule without any
// materialization reports false.
func TestMaterializeCompletionMarker(t *testing.T) {
	wfd, sum := buildBundle(t, "---\nname: demo\n---\nbody-v1\n", "---\nname: code\n---\nv1\n")
	c := newCapsule(t, 3)
	if c.MaterializationComplete(ident(sum)) {
		t.Fatal("a capsule that never materialized must not report a complete snapshot")
	}
	if _, err := c.Materialize(wfd, "demo", sum, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	if !c.MaterializationComplete(ident(sum)) {
		t.Fatal("a successful materialization must be observable as complete")
	}
	if _, err := os.Stat(filepath.Join(c.Dir, filepath.FromSlash(RelMaterializeMarker))); err != nil {
		t.Fatalf("the completion marker must exist at %s: %v", RelMaterializeMarker, err)
	}
	if _, err := os.Stat(stagingPath(c)); !os.IsNotExist(err) {
		t.Fatalf("the staging tree must be cleaned after success, got err=%v", err)
	}
}

// TestMaterializeCrashDuringStagingKeepsCompleteOldBytes: a kill between
// the STAGED copies leaves the live targets untouched and the old marker
// intact — the on-disk BYTES are still the previous complete snapshot
// (asserted below, the tear never touches the live tree while staging).
// The trust gate however reports NOT complete: the leftover staging tree
// proves an interrupted materialization run, and the fix-round-2 gate
// fails conservative on staging residue — the consumer re-materializes
// (the next run cleans the husk and lands complete-new) instead of
// trusting bytes whose provenance is ambiguous.
func TestMaterializeCrashDuringStagingKeepsCompleteOldBytes(t *testing.T) {
	wfd1, sum1 := buildBundle(t, "---\nname: demo\n---\nbody-v1\n", "---\nname: code\n---\nagent-v1\n")
	wfd2, sum2 := buildBundle(t, "---\nname: demo\n---\nbody-v2\n", "---\nname: code\n---\nagent-v2\n")
	c := newCapsule(t, 3)
	if _, err := c.Materialize(wfd1, "demo", sum1, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	// Kill after the first staged copy of the NEW snapshot.
	withCrashHook(t, "copied:workflow.md")
	if _, err := c.Materialize(wfd2, "demo", sum2, []string{AdapterPi}); err == nil {
		t.Fatal("the simulated kill must abort the materialization")
	}
	// The staging husk marks the run interrupted: the gate refuses trust
	// (fix round 2's no-staging-residue rule) even though the live bytes
	// are untouched complete-old — re-materialize, never trust ambiguous
	// provenance.
	if c.MaterializationComplete(ident(sum1)) {
		t.Fatal("a staging husk (interrupted run) must leave the snapshot untrusted by the gate")
	}
	if got := readCapsuleFile(t, c, "workflow.md"); got != "---\nname: demo\n---\nbody-v1\n" {
		t.Fatalf("workflow.md must still be complete-old, got %q", got)
	}
	if got := readCapsuleFile(t, c, ".pi/agents/code.md"); got != "---\nname: code\n---\nagent-v1\n" {
		t.Fatalf("the agent definition must still be complete-old, got %q", got)
	}
	// The next (successful) run cleans the stale staging tree and lands
	// complete-new.
	if _, err := c.Materialize(wfd2, "demo", sum2, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	if !c.MaterializationComplete(ident(sum2)) {
		t.Fatal("the repairing run must end complete")
	}
	if got := readCapsuleFile(t, c, "workflow.md"); got != "---\nname: demo\n---\nbody-v2\n" {
		t.Fatalf("workflow.md must now be complete-new, got %q", got)
	}
	if _, err := os.Stat(stagingPath(c)); !os.IsNotExist(err) {
		t.Fatal("the stale staging tree must be cleaned by the next materialization")
	}
}

// TestMaterializeCrashMidSwapIsNeverTrusted: a kill in the middle of the
// swap (after the marker removal, after some renames) leaves genuinely
// mixed bytes on disk — but they are NEVER trusted: the marker is absent,
// MaterializationComplete reports false, and the next materialization
// repairs the whole set (partial state replaced wholesale, staging
// cleaned).
func TestMaterializeCrashMidSwapIsNeverTrusted(t *testing.T) {
	wfd1, sum1 := buildBundle(t, "---\nname: demo\n---\nbody-v1\n", "---\nname: code\n---\nagent-v1\n")
	wfd2, sum2 := buildBundle(t, "---\nname: demo\n---\nbody-v2\n", "---\nname: code\n---\nagent-v2\n")
	c := newCapsule(t, 3)
	if _, err := c.Materialize(wfd1, "demo", sum1, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	// Kill after workflow.md was swapped in but before the agents dir.
	withCrashHook(t, "swapped:workflow.md")
	if _, err := c.Materialize(wfd2, "demo", sum2, []string{AdapterPi}); err == nil {
		t.Fatal("the simulated kill must abort the materialization")
	}
	if c.MaterializationComplete(ident(sum1)) {
		t.Fatal("a torn snapshot (mixed swap, no marker) must NEVER be reported complete")
	}
	// The tear is real: workflow.md is new, agents are old. That mix is
	// exactly what the old code let the next agent trust.
	if got := readCapsuleFile(t, c, "workflow.md"); got != "---\nname: demo\n---\nbody-v2\n" {
		t.Fatalf("setup: workflow.md should carry the swapped-in new body, got %q", got)
	}
	if got := readCapsuleFile(t, c, ".pi/agents/code.md"); got != "---\nname: code\n---\nagent-v1\n" {
		t.Fatalf("setup: the agent definition should still be old, got %q", got)
	}
	// Repair: the next run replaces the whole set with complete-new.
	if _, err := c.Materialize(wfd2, "demo", sum2, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	if !c.MaterializationComplete(ident(sum2)) {
		t.Fatal("the repairing materialization must end complete")
	}
	if got := readCapsuleFile(t, c, "workflow.md"); got != "---\nname: demo\n---\nbody-v2\n" {
		t.Fatalf("workflow.md must be complete-new, got %q", got)
	}
	if got := readCapsuleFile(t, c, ".pi/agents/code.md"); got != "---\nname: code\n---\nagent-v2\n" {
		t.Fatalf("the agent definition must be complete-new, got %q", got)
	}
	if _, err := os.Stat(stagingPath(c)); !os.IsNotExist(err) {
		t.Fatal("partial staging dirs must be cleaned by the repairing materialization")
	}
}

// TestMaterializeCrashAfterMarkerRemovalBeforeAnySwap: the untrust point is
// BEFORE the first rename — a kill right after the marker removal (all live
// targets still complete-old bytes, but no marker) is correctly reported
// untrusted, because those bytes were produced by two different runs.
func TestMaterializeCrashAfterMarkerRemovalBeforeAnySwap(t *testing.T) {
	wfd1, sum1 := buildBundle(t, "---\nname: demo\n---\nbody-v1\n", "---\nname: code\n---\nagent-v1\n")
	wfd2, sum2 := buildBundle(t, "---\nname: demo\n---\nbody-v2\n", "---\nname: code\n---\nagent-v2\n")
	c := newCapsule(t, 3)
	if _, err := c.Materialize(wfd1, "demo", sum1, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	withCrashHook(t, "marker-removed")
	if _, err := c.Materialize(wfd2, "demo", sum2, []string{AdapterPi}); err == nil {
		t.Fatal("the simulated kill must abort the materialization")
	}
	if c.MaterializationComplete(ident(sum1)) {
		t.Fatal("after the marker removal the snapshot must be untrusted, whatever bytes are on disk")
	}
	if _, err := c.Materialize(wfd2, "demo", sum2, []string{AdapterPi}); err != nil {
		t.Fatalf("the next run must repair: %v", err)
	}
	if !c.MaterializationComplete(ident(sum2)) {
		t.Fatal("the repairing materialization must end complete")
	}
}

// TestMaterializeCleansTornLeftovers: a staging tree and marker-less
// partial state left on disk by a killed run never block a later
// materialization — the entry reconcile removes the staging tree and the
// fresh run overwrites every live target.
func TestMaterializeCleansTornLeftovers(t *testing.T) {
	wfd, sum := buildBundle(t, "---\nname: demo\n---\nbody\n", "---\nname: code\n---\nagent\n")
	c := newCapsule(t, 3)
	// Fabricate the residue of a torn run: staging garbage + live targets
	// with no completion marker.
	if err := os.MkdirAll(filepath.Join(stagingPath(c), ".pi", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingPath(c), "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "workflow.md"), []byte("torn"), 0o444); err != nil {
		t.Fatal(err)
	}
	if c.MaterializationComplete(ident(sum, AdapterPi, AdapterClaude)) {
		t.Fatal("fabricated partial state must not be trusted")
	}
	if _, err := c.Materialize(wfd, "demo", sum, []string{AdapterPi, AdapterClaude}); err != nil {
		t.Fatal(err)
	}
	if !c.MaterializationComplete(ident(sum, AdapterPi, AdapterClaude)) {
		t.Fatal("the fresh materialization must end complete")
	}
	if got := readCapsuleFile(t, c, "workflow.md"); got != "---\nname: demo\n---\nbody\n" {
		t.Fatalf("the torn live target must be replaced, got %q", got)
	}
	if _, err := os.Stat(stagingPath(c)); !os.IsNotExist(err) {
		t.Fatal("the leftover staging tree must be cleaned")
	}
	if _, err := os.Stat(filepath.Join(c.Dir, ".claude", "agents", "code.md")); err != nil {
		t.Fatalf("claude adapter dirs must materialize on demand: %v", err)
	}
}

// TestMaterializationCompleteIdentityValidation (F2-4): the trust gate
// validates the marker's recorded identity against the EXPECTED one and
// the target set's existence — a parseable marker alone (the old gate)
// proves nothing.
func TestMaterializationCompleteIdentityValidation(t *testing.T) {
	wfd, sum := buildBundle(t, "---\nname: demo\n---\nbody\n", "---\nname: code\n---\nagent\n")
	c := newCapsule(t, 3)
	if _, err := c.Materialize(wfd, "demo", sum, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	// Valid identity: complete.
	if !c.MaterializationComplete(ident(sum)) {
		t.Fatal("a valid materialization with the matching identity must be complete")
	}
	// Wrong digest: not complete — the snapshot is not the pinned one.
	if c.MaterializationComplete(ident("sha256:deadbeef")) {
		t.Fatal("a wrong bundle_digest expectation must not validate the marker")
	}
	// Wrong bundle name: not complete.
	wrongName := ident(sum)
	wrongName.BundleName = "other-bundle"
	if c.MaterializationComplete(wrongName) {
		t.Fatal("a wrong bundle_name expectation must not validate the marker")
	}
	// Wrong adapter set: not complete.
	if c.MaterializationComplete(ident(sum, AdapterPi, AdapterClaude)) {
		t.Fatal("a wrong adapter-set expectation must not validate the marker")
	}
	// Empty identity: fail closed, never trust a marker blindly.
	if c.MaterializationComplete(MaterializationIdentity{}) {
		t.Fatal("an empty expectation must fail closed")
	}
	// Missing agent file: not complete — the marker records the target
	// set, and one deleted file invalidates the snapshot.
	if err := os.Remove(filepath.Join(c.Dir, ".pi", "agents", "code.md")); err != nil {
		t.Fatal(err)
	}
	if c.MaterializationComplete(ident(sum)) {
		t.Fatal("a missing recorded target (agent definition deleted) must not validate as complete")
	}
}
