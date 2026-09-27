package capsule

// Lifecycle tests for the Daemon-side capsule (03 §2/§3.1/§6.1): create,
// reuse, quarantine, capsule.json fencing, layout.

import (
	"os"
	"path/filepath"
	"testing"
)

func newCapsule(t *testing.T, gen int64) *Capsule {
	t.Helper()
	c, _, err := Create(t.TempDir(), "wi_test01", gen, "m-01", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateLayout(t *testing.T) {
	c := newCapsule(t, 3)
	for _, d := range c.layoutDirs() {
		if fi, err := os.Stat(filepath.Join(c.Dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("layout dir %s missing", d)
		}
	}
	// .pi/agents must exist even when empty (03 §4.5 layer 3).
	if fi, err := os.Stat(filepath.Join(c.Dir, ".pi", "agents")); err != nil || !fi.IsDir() {
		t.Fatal(".pi/agents must always exist")
	}
	idn, err := readIdentity(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if idn.Status != StatusInitializing || idn.Generation != 3 || idn.CapsuleID != "cap_wi_test01" {
		t.Fatalf("identity: %+v", idn)
	}
}

func TestCreateReuseAndMarkReady(t *testing.T) {
	ws := t.TempDir()
	c1, _, err := Create(ws, "wi_test01", 3, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.MarkReady(); err != nil {
		t.Fatal(err)
	}
	// Idempotent retry: same generation, ready → direct reuse (03 §6.1 2a).
	c2, res2, err := Create(ws, "wi_test01", 3, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Reused || c2.Dir != c1.Dir {
		t.Fatalf("expected reuse, got %+v", res2)
	}
	// New generation: quarantine + fresh directory (03 §6.1 2d).
	c3, res3, err := Create(ws, "wi_test01", 4, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if res3.Reused || res3.Quarantined == "" {
		t.Fatalf("expected quarantine, got %+v", res3)
	}
	if c3.Dir != c1.Dir {
		t.Fatal("new capsule must use the canonical dir path")
	}
	if idn, _ := readIdentity(c3.Dir); idn.Generation != 4 || idn.Status != StatusInitializing {
		t.Fatalf("new identity: %+v", idn)
	}
	// The quarantined copy is preserved under _quarantine.
	if _, err := os.Stat(res3.Quarantined); err != nil {
		t.Fatalf("quarantine dir missing: %v", err)
	}
}

func TestCreateInitializingHuskQuarantined(t *testing.T) {
	ws := t.TempDir()
	// Crash between Create and MarkReady leaves status=initializing; the
	// next Create for the SAME generation must quarantine, not reuse.
	if _, _, err := Create(ws, "wi_test01", 3, "m-01", ""); err != nil {
		t.Fatal(err)
	}
	_, res, err := Create(ws, "wi_test01", 3, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused || res.Quarantined == "" {
		t.Fatalf("initializing husk must be quarantined: %+v", res)
	}
}

func TestCreateFencedWhenLocalGenerationNewer(t *testing.T) {
	ws := t.TempDir()
	c, _, err := Create(ws, "wi_test01", 5, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReady(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Create(ws, "wi_test01", 4, "m-01", ""); err == nil || err.(*Error).Code != ErrCapsuleFenced {
		t.Fatalf("expected CAP-409, got %v", err)
	}
}

func TestValidateGeneration(t *testing.T) {
	c := newCapsule(t, 3)
	if err := c.ValidateGeneration(3); err != nil {
		t.Fatal(err)
	}
	err := c.ValidateGeneration(4)
	if err == nil || err.(*Error).Code != ErrCapsuleFenced {
		t.Fatalf("expected CAP-409, got %v", err)
	}
}

func TestPathValidation(t *testing.T) {
	c := newCapsule(t, 3)
	if _, err := c.Path("repos/x/a.go"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "/abs", "a\\b", "runtime-state/../x"} {
		if _, err := c.Path(bad); err == nil {
			t.Errorf("path %q must be rejected", bad)
		}
	}
}
