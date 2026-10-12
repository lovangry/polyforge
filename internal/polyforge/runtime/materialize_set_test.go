package runtime

// materialize_set_test.go: the F7 lane — the bundle materialization as
// one atomic set with a completion marker (03 §4.5 semantics): the full
// materialization shape (workflows/<bundle> + the steps/ → agents/ copy +
// marker targets), the trust gate's reuse rule (marker present
// and targets whole → untouched), the torn-snapshot rebuild (marker
// gone → re-materialize + marker rewritten), and the adoption of the
// pilot's hand-compensated scene (a manual marker that predates the
// product fix).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// startedWithBundle runs one full Start against the pristine demo bundle
// and answers the capsule dir + job of the live run.
func startedWithBundle(t *testing.T, wi string) (*Runtime, string, *job) {
	t.Helper()
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	hub := newFakeHub()
	hub.digest = pinned
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir
	if err := r.Start(context.Background(), wi); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Stop(context.Background(), wi, protocol.FinishWrapped)
	})
	j := r.jobOf(wi)
	if j == nil {
		t.Fatalf("no live job for %s", wi)
	}
	return r, testCapsuleDir(r, wi), j
}

func readMarker(t *testing.T, capDir string) materializeMarkerRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capDir, filepath.FromSlash(RelMaterializeMarker)))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m materializeMarkerRecord
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("marker unparsable: %v", err)
	}
	return m
}

// The full materialization: workflows/<bundle>/ tree, the bundle's steps/
// staged to the capsule's agents/, and a marker whose targets list covers
// every file on disk.
func TestMaterializeSetCompleteShape(t *testing.T) {
	_, capDir, _ := startedWithBundle(t, "wi_f7a")

	for _, rel := range []string{
		"workflows/demo-bundle/workflow.md",
		"workflows/demo-bundle/steps/step.md",
		"agents/step.md",
	} {
		if _, err := os.Stat(filepath.Join(capDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("materialized target missing: %s: %v", rel, err)
		}
	}
	// No harness discovery copy any more.
	if _, err := os.Stat(filepath.Join(capDir, ".pi", "agents")); !os.IsNotExist(err) {
		t.Fatalf(".pi/agents discovery copy must not be materialized: %v", err)
	}
	// The capsule copy is byte-identical to the bundle's step.
	auth, _ := os.ReadFile(filepath.Join(capDir, "agents", "step.md"))
	src, err := os.ReadFile(filepath.Join(capDir, "workflows", "demo-bundle", "steps", "step.md"))
	if err != nil || string(auth) != string(src) {
		t.Fatalf("agents/ copy diverged from the bundle's steps/ (%v)", err)
	}
	m := readMarker(t, capDir)
	if m.BundleName != "demo-bundle" || m.BundleDigest == "" {
		t.Fatalf("marker identity = %+v", m)
	}
	if len(m.Adapters) != 1 || m.Adapters[0] != "pi" {
		t.Fatalf("marker adapters = %v", m.Adapters)
	}
	want := map[string]bool{
		"workflows/demo-bundle/workflow.md":   false,
		"workflows/demo-bundle/steps/step.md": false,
		"agents/step.md":                      false,
	}
	if len(m.Targets) != len(want) {
		t.Fatalf("marker targets = %v", m.Targets)
	}
	for _, rel := range m.Targets {
		if _, ok := want[rel]; !ok {
			t.Fatalf("unexpected marker target %q", rel)
		}
		want[rel] = true
		if _, err := os.Stat(filepath.Join(capDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("marker target not on disk: %s: %v", rel, err)
		}
	}
	for rel, seen := range want {
		if !seen {
			t.Fatalf("marker target missing from the list: %s", rel)
		}
	}
	// sync/ is daemon-private: the capsule .gitignore excludes it, and
	// no staging residue survives a complete materialization.
	gi, err := os.ReadFile(filepath.Join(capDir, ".gitignore"))
	if err != nil || !containsLine(string(gi), "/sync/") {
		t.Fatalf(".gitignore must exclude /sync/: %v (%q)", err, gi)
	}
	if _, err := os.Stat(filepath.Join(capDir, "sync", "materialize-staging")); !os.IsNotExist(err) {
		t.Fatalf("staging husk survived: %v", err)
	}
}

func containsLine(s, line string) bool {
	for _, l := range splitLines(s) {
		if l == line {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// A trusted snapshot is left untouched: the gate (marker present and
// every recorded target whole) short-circuits the re-materialization, so
// neither the staged copies nor the marker churn.
func TestMaterializeSetTrustedSnapshotUntouched(t *testing.T) {
	r, capDir, j := startedWithBundle(t, "wi_f7b")
	before := readMarker(t, capDir)

	if err := r.materializeBundle(j); err != nil {
		t.Fatal(err)
	} // a trusted set is a no-op

	after := readMarker(t, capDir)
	if after.CompletedAt != before.CompletedAt {
		t.Fatalf("marker was rewritten on a trusted snapshot (%s → %s)",
			before.CompletedAt, after.CompletedAt)
	}
	if len(after.Targets) != len(before.Targets) {
		t.Fatalf("marker target set changed on a trusted snapshot")
	}
	if !r.materializationComplete(j) {
		t.Fatal("gate must validate its own materialization")
	}
}

// A torn snapshot (the marker removed — a crash between the marker drop
// and its rewrite) is re-materialized and the marker rewritten; a target
// lost after the marker (the gate's missing-file hole) triggers the same
// rebuild.
func TestMaterializeSetTornSnapshotRebuilt(t *testing.T) {
	r, capDir, j := startedWithBundle(t, "wi_f7c")

	// (a) torn: marker gone, one staged copy vandalized.
	os.Remove(filepath.Join(capDir, filepath.FromSlash(RelMaterializeMarker)))
	os.Remove(filepath.Join(capDir, "workflows", "demo-bundle", "workflow.md"))

	if err := r.materializeBundle(j); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(capDir, "workflows", "demo-bundle", "workflow.md")); err != nil {
		t.Fatalf("torn snapshot not rebuilt: %v", err)
	}
	m := readMarker(t, capDir)
	if m.BundleName != "demo-bundle" || len(m.Targets) == 0 {
		t.Fatalf("marker not rewritten after the rebuild: %+v", m)
	}
	if !r.materializationComplete(j) {
		t.Fatal("gate must validate the rebuilt snapshot")
	}

	// (b) missing target with the marker intact: the gate fails on the
	// missing file and the rebuild restores it.
	os.Remove(filepath.Join(capDir, "agents", "step.md"))
	if r.materializationComplete(j) {
		t.Fatal("gate validated a snapshot with a missing target")
	}
	if err := r.materializeBundle(j); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(capDir, "agents", "step.md")); err != nil {
		t.Fatalf("missing target not restored: %v", err)
	}
	if !r.materializationComplete(j) {
		t.Fatal("gate must validate the rebuilt snapshot (missing-target case)")
	}
}

// The pilot's hand-compensated scene is adopted smoothly: a manually
// written marker that predates the product fix (identity recorded, but
// without the adapter set the gate expects) fails the gate and the
// idempotent rebuild rewrites it — the hand-copied targets survive
// byte-identical either way.
func TestMaterializeSetAdoptsPilotCompensation(t *testing.T) {
	r, capDir, j := startedWithBundle(t, "wi_f7d")

	// Recreate the pilot's shape: marker dropped, hand-copied targets,
	// hand-written marker WITHOUT adapters (the pre-fix manual format).
	os.Remove(filepath.Join(capDir, filepath.FromSlash(RelMaterializeMarker)))
	manual, _ := json.Marshal(map[string]any{
		"bundle_name":   "demo-bundle",
		"bundle_digest": j.bundleDigest,
		"targets":       []string{"agents/step.md"},
		"completed_at":  "2026-10-01T00:00:00Z",
	})
	if err := os.WriteFile(filepath.Join(capDir, filepath.FromSlash(RelMaterializeMarker)), manual, 0o644); err != nil {
		t.Fatal(err)
	}
	if r.materializationComplete(j) {
		t.Fatal("a marker without the adapter set must not validate")
	}

	if err := r.materializeBundle(j); err != nil {
		t.Fatal(err)
	}

	m := readMarker(t, capDir)
	if len(m.Adapters) != 1 || m.Adapters[0] != "pi" {
		t.Fatalf("manual marker not rewritten with the product format: %+v", m)
	}
	kept, err := os.ReadFile(filepath.Join(capDir, "agents", "step.md"))
	if err != nil || len(kept) == 0 {
		t.Fatalf("hand-copied target lost in the adoption: %v", err)
	}
	if !r.materializationComplete(j) {
		t.Fatal("gate must validate the adopted scene")
	}
}

// The claim path re-materializes only what is missing: a journal-replayed
// start of the SAME session (the crashed-Start husk shape — B1 replay,
// same secret, so the capsule marker matches and the husk is reused)
// reuses the trusted snapshot instead of churning it.
func TestMaterializeSetClaimReplayReusesTrustedSnapshot(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	hub := newFakeHub()
	hub.digest = pinned
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir

	if err := r.Start(context.Background(), "wi_f7e"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	capDir := testCapsuleDir(r, "wi_f7e")
	before := readMarker(t, capDir)
	j := r.jobOf("wi_f7e")
	j.mu.Lock()
	session := j.sessionID
	j.mu.Unlock()

	// the crash shape: the tree dies, the job leaves the map, the
	// bookkeeping is wiped (the crash predates registration), and the
	// journal still names the unsettled start.
	r.killAgentTree(j)
	r.mu.Lock()
	delete(r.jobs, "wi_f7e")
	r.mu.Unlock()
	writeStateFile(t, r, nil)
	if err := r.noteClaiming("wi_f7e", session, randomKey()); err != nil {
		t.Fatal(err)
	}

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart (journal replay): %v", err)
	}
	t.Cleanup(func() {
		_ = r.Stop(context.Background(), "wi_f7e", protocol.FinishWrapped)
	})
	after := readMarker(t, capDir)
	if after.CompletedAt != before.CompletedAt {
		t.Fatalf("claim replay churned a trusted snapshot (%s → %s)",
			before.CompletedAt, after.CompletedAt)
	}
}

// A bundle with neither steps/ nor agents/ still gets its workflows/<bundle>
// tree copied and a valid completion marker (the latent bug: the workflow
// tree copy used to hang off the presence of a step directory).
func TestMaterializeBundleWithoutStepsStillCopiesWorkflowTree(t *testing.T) {
	setGitIdentity(t)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "demo-bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub()
	hub.digest = nameDerivedDigest("demo-bundle")
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = dir
	if err := r.Start(context.Background(), "wi_f7nos"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), "wi_f7nos", protocol.FinishWrapped) })
	capDir := testCapsuleDir(r, "wi_f7nos")
	if _, err := os.Stat(filepath.Join(capDir, "workflows", "demo-bundle", "workflow.md")); err != nil {
		t.Fatalf("workflow tree not materialized: %v", err)
	}
	m := readMarker(t, capDir)
	if len(m.Targets) != 1 || m.Targets[0] != "workflows/demo-bundle/workflow.md" {
		t.Fatalf("marker targets = %v", m.Targets)
	}
	if !r.materializationComplete(r.jobOf("wi_f7nos")) {
		t.Fatal("gate must validate a steps-less bundle's snapshot")
	}
}

// Recovery never re-pins: a workflows-dir source that moved past the claim
// pin leaves the job's pin and the capsule snapshot untouched (the
// relaunch uses the capsule's own copy), and a missing snapshot is a
// blocked finish, not a silent rebuild.
func TestRecoveryUsesClaimSnapshotNeverRepins(t *testing.T) {
	r, capDir, j := startedWithBundle(t, "wi_norepin")
	pinned := j.bundleDigest
	src := filepath.Join(r.cfg.WorkflowsDir, "demo-bundle", "workflow.md")
	if err := os.WriteFile(src, []byte("# evolved after claim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if j.bundleDigest != pinned {
		t.Fatalf("recovery re-pinned %s -> %s", pinned, j.bundleDigest)
	}
	got, _ := os.ReadFile(filepath.Join(capDir, "workflows", "demo-bundle", "workflow.md"))
	if string(got) == "# evolved after claim\n" {
		t.Fatal("recovery re-materialized from the moved source")
	}

	// A missing snapshot is not rebuilt: the WI blocks.
	if err := os.Remove(filepath.Join(capDir, filepath.FromSlash(RelMaterializeMarker))); err != nil {
		t.Fatal(err)
	}
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if _, err := os.Stat(filepath.Join(capDir, filepath.FromSlash(RelMaterializeMarker))); err == nil {
		t.Fatal("recovery rebuilt the snapshot instead of blocking")
	}
	if st, _ := j.snapshot(); st == StateRunning || st == StateRecovering {
		t.Fatalf("a missing snapshot must end the run, state %s", st)
	}
}
