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

// TestSealReadOnlySealsWholeScene is the FE-8 regression: sealing used to
// chmod only the capsule ROOT, leaving the preserved scene's contents
// writable. 03 §2.1's “只读现场” is the whole preserved scene: directories
// 0555, regular files 0444, symlinks untouched (chmod follows links).
func TestSealReadOnlySealsWholeScene(t *testing.T) {
	c := newCapsule(t, 3)
	// Nested content: files, a subdirectory, and a symlink.
	if err := os.MkdirAll(filepath.Join(c.Dir, "artifacts", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(c.Dir, "artifacts", "f.md"),
		filepath.Join(c.Dir, "artifacts", "sub", "g.md"),
	} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("f.md", filepath.Join(c.Dir, "artifacts", "link")); err != nil {
		t.Fatal(err)
	}
	if err := c.SealReadOnly(); err != nil {
		t.Fatal(err)
	}
	// B02 scope note: the mode bits are the assertion — this environment
	// runs tests as root, which bypasses DAC enforcement, so the bits (not
	// an EACCES probe) are what the test can hold; for the non-root Daemon
	// the bits ARE the anti-mistake EACCES guard.
	for _, d := range []string{c.Dir, filepath.Join(c.Dir, "artifacts"), filepath.Join(c.Dir, "artifacts", "sub")} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o555 {
			t.Errorf("sealed directory %s must be 0555, got %o", d, fi.Mode().Perm())
		}
	}
	for _, f := range []string{filepath.Join(c.Dir, "artifacts", "f.md"), filepath.Join(c.Dir, "artifacts", "sub", "g.md")} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o444 {
			t.Errorf("sealed file %s must be 0444, got %o", f, fi.Mode().Perm())
		}
	}
	// The symlink itself is untouched (never chmod'd through the link);
	// its TARGET was sealed by the walk's own visit.
	li, err := os.Lstat(filepath.Join(c.Dir, "artifacts", "link"))
	if err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink must survive sealing as a symlink: %v", err)
	}
	// Restore writability so t.TempDir's cleanup can remove the scene.
	filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}

// TestCreateQuarantinesDirWithoutIdentity is the FE-9 regression: a capsule
// directory whose capsule.json is MISSING (crashed creation, tampering) is
// a husk — 03 §6.1 step 2b/3 wants it quarantined, never reused in place
// (§2.4: the new generation always starts from a clean directory).
func TestCreateQuarantinesDirWithoutIdentity(t *testing.T) {
	ws := t.TempDir()
	// A husk: the directory exists, the identity file does not.
	dir := filepath.Join(ws, "capsules", "wi_test01")
	if err := os.MkdirAll(filepath.Join(dir, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repos", "junk.txt"), []byte("husk residue"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, res, err := Create(ws, "wi_test01", 3, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused || res.Quarantined == "" {
		t.Fatalf("a directory without capsule.json must be quarantined, not reused: %+v", res)
	}
	// The residue was preserved under _quarantine (audit scene), not reused.
	raw, rerr := os.ReadFile(filepath.Join(res.Quarantined, "repos", "junk.txt"))
	if rerr != nil || string(raw) != "husk residue" {
		t.Fatalf("the quarantined husk must be preserved: %q %v", raw, rerr)
	}
	// The fresh directory carries a clean identity and none of the residue.
	idn, ierr := readIdentity(c.Dir)
	if ierr != nil || idn.Generation != 3 || idn.Status != StatusInitializing {
		t.Fatalf("fresh identity: %v %+v", ierr, idn)
	}
	if _, serr := os.Stat(filepath.Join(c.Dir, "repos", "junk.txt")); !os.IsNotExist(serr) {
		t.Fatal("the fresh capsule must not inherit husk residue")
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
