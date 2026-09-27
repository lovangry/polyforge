package daemon

// swapreconcile_test.go: R3N-F4 regression at the DAEMON level — the
// startup path (New) sweeps the capsules root for swap markers left by a
// previous daemon life that crashed between the two renames of a capsule
// tree swap, and completes or rolls them back deterministically BEFORE any
// capsule.Create could lay a fresh husk over the gap. Stranded
// staging/trash trees from a crash DURING the copy phase (before the
// marker existed) are removed as garbage.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/sync/recover"
)

// TestDaemonStartupSweepReconcilesInterruptedSwap: the capsules root holds
// (a) a mid-swap marker whose staged tree is complete and whose live path
// is absent, and (b) a stray staging tree from an interrupted copy phase
// with no marker at all. A daemon start over that root must complete (a)
// from the marker and remove (b) as garbage.
func TestDaemonStartupSweepReconcilesInterruptedSwap(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	capsRoot := filepath.Join(ws, "capsules")
	if err := os.MkdirAll(capsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// (a) mid-swap residue for wi_sw01: live absent, staged = the fully
	// built replacement, trash = the old surface, marker present.
	live := filepath.Join(capsRoot, "wi_sw01")
	staged := filepath.Join(capsRoot, "wi_sw01.recover-staging-crash")
	trash := filepath.Join(capsRoot, "wi_sw01.recover-trash-crash")
	os.MkdirAll(staged, 0o755)
	os.WriteFile(filepath.Join(staged, "new.txt"), []byte("new surface"), 0o644)
	os.MkdirAll(trash, 0o755)
	os.WriteFile(filepath.Join(trash, "old.txt"), []byte("old surface"), 0o644)
	marker := filepath.Join(capsRoot, "wi_sw01.recover-swap.json")
	blob := "{\"live\": \"" + live + "\", \"staged\": \"" + staged + "\", \"trash\": \"" + trash + "\"}"
	if err := os.WriteFile(marker, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	// (b) garbage from a copy-phase crash: a staging tree with NO marker.
	stray := filepath.Join(capsRoot, "wi_sw02.recover-staging-stray")
	os.MkdirAll(stray, 0o755)
	os.WriteFile(filepath.Join(stray, "half-copied.txt"), []byte("partial"), 0o644)

	cfg := Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: ws,
		SkillsDir:     filepath.Join(dir, "skills"),
		Adapter:       "pi", Version: "sweep-test",
	}
	d, err := New(cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	_ = d

	// (a) completed: the staged tree is the live capsule, trash and marker
	// are gone.
	if raw, rerr := os.ReadFile(filepath.Join(live, "new.txt")); rerr != nil || string(raw) != "new surface" {
		t.Fatalf("the interrupted swap must be COMPLETED at startup: %q %v", raw, rerr)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatal("the trash tree must be removed after the completed swap")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the swap marker must be cleared after the completed swap")
	}
	// (b) removed as garbage.
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("a marker-less stranded staging tree must be swept away at startup")
	}

	// The empty capsules root: recover.ReconcileInterruptedSwaps is
	// idempotent on a clean root.
	if err := recover.ReconcileInterruptedSwaps(capsRoot); err != nil {
		t.Fatalf("a clean sweep must be a no-op: %v", err)
	}
}
