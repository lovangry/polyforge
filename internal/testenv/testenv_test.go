package testenv

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The real-gitconfig guard compares content fingerprints: creating,
// editing and deleting a guarded file must each change it.
func TestFingerprintDetectsEveryChange(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, ".gitconfig"), filepath.Join(dir, "git", "config")}
	absent := fingerprint(paths)
	if !bytes.Equal(absent, fingerprint(paths)) {
		t.Fatal("fingerprint is not stable")
	}
	if err := os.WriteFile(paths[0], []byte("[user]\n\tname = a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := fingerprint(paths)
	if bytes.Equal(absent, created) {
		t.Fatal("creating a guarded file went unnoticed")
	}
	if err := os.WriteFile(paths[0], []byte("[user]\n\tname = b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(created, fingerprint(paths)) {
		t.Fatal("editing a guarded file went unnoticed")
	}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(absent, fingerprint(paths)) {
		t.Fatal("restoring the absent state must restore the fingerprint")
	}
}

// Inside a Main-isolated binary the global git config is the scratch one
// with the test identity, never the operator's.
func TestMainIsolatesGitConfig(t *testing.T) {
	gc := os.Getenv("GIT_CONFIG_GLOBAL")
	if gc == "" || filepath.Dir(gc) != os.Getenv("HOME") {
		t.Fatalf("GIT_CONFIG_GLOBAL=%q is not the scratch home's (HOME=%q)", gc, os.Getenv("HOME"))
	}
	raw, err := os.ReadFile(gc)
	if err != nil || !bytes.Contains(raw, []byte(TestGitEmail)) {
		t.Fatalf("scratch config lacks the test identity: %q, %v", raw, err)
	}
	if os.Getenv("GIT_CONFIG_NOSYSTEM") != "1" {
		t.Fatal("the system git config must be off")
	}
}

func TestMain(m *testing.M) { Main(m) }
