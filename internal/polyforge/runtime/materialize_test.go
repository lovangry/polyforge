package runtime

// materialize_test: the bundle digest verification wired into
// materialize — a pinned digest mismatched by the copied tree refuses
// materialization (CAP-405), the name-derived stand-in skips
// verification, and a matched pin passes it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

// pristineBundle writes the demo bundle into a daemon-checkout workflows
// dir and answers the digest an honest pin of that content would carry
// (entries hand-derived here, independently of bundleEntries).
func pristineBundle(t *testing.T) (workflowsDir, pinned string) {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "demo-bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CRLF on disk: the digest input is the LF-normalized content
	if err := os.WriteFile(filepath.Join(bundle, "agents", "step.md"), []byte("do the thing\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfSum := sha256.Sum256([]byte("# demo\n"))
	stepSum := sha256.Sum256([]byte("do the thing\n"))
	pinned = workflow.Digest([]workflow.FileEntry{
		{Path: "workflow.md", Sha256: hex.EncodeToString(wfSum[:]), Bytes: 7},
		{Path: "agents/step.md", Sha256: hex.EncodeToString(stepSum[:]), Bytes: 13},
	})
	return dir, pinned
}

func sentinelDigest() string {
	sum := sha256.Sum256([]byte("bundle-name:demo-bundle"))
	return hex.EncodeToString(sum[:])
}

// A bundle whose checkout diverged from its explicit pin is refused
// materialization with CAP-405.
func TestMaterializeTamperedPinnedBundleRejected(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	// the checkout was tampered after the pin was recorded
	if err := os.WriteFile(filepath.Join(wfDir, "demo-bundle", "agents", "step.md"),
		[]byte("do the EVIL thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub()
	hub.digest = pinned
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir

	err := r.Start(context.Background(), "wi_dig1")
	if err == nil {
		t.Fatal("Start must refuse a tampered pinned bundle")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrCapCheckoutDiver {
		t.Fatalf("expected CAP-405, got %v", err)
	}
	if r.jobOf("wi_dig1") != nil {
		t.Fatal("a refused materialization must leave no live job")
	}
}

// A name-derived default digest (aihub's bundle-name stand-in) never
// verifies: materialization proceeds even against diverged content.
func TestMaterializeNameDerivedDigestSkipsVerification(t *testing.T) {
	setGitIdentity(t)
	wfDir, _ := pristineBundle(t)
	if err := os.WriteFile(filepath.Join(wfDir, "demo-bundle", "workflow.md"),
		[]byte("# tampered too\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub()
	hub.digest = sentinelDigest()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir

	if err := r.Start(context.Background(), "wi_dig2"); err != nil {
		t.Fatalf("Start must skip verification for a name-derived digest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(testCapsuleDir(r, "wi_dig2"), capsule.DirWorkflows, "demo-bundle", "workflow.md")); err != nil {
		t.Fatalf("bundle must be materialized: %v", err)
	}
	if err := r.Stop(context.Background(), "wi_dig2", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// An explicit pin that matches the copied tree passes verification.
func TestMaterializePinnedBundleVerified(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	hub := newFakeHub()
	hub.digest = pinned
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.WorkflowsDir = wfDir

	if err := r.Start(context.Background(), "wi_dig3"); err != nil {
		t.Fatalf("Start must pass a matching pin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(testCapsuleDir(r, "wi_dig3"), capsule.DirWorkflows, "demo-bundle", "workflow.md")); err != nil {
		t.Fatalf("bundle must be materialized: %v", err)
	}
	if err := r.Stop(context.Background(), "wi_dig3", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
