package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExternalizeGitDir must refuse a pre-existing target git-dir (a stale one
// from a previous session): no silent reuse, no merge.
func TestExternalizeGitDirRefusesExistingTarget(t *testing.T) {
	setIdentity(t)
	o, _ := testOpts(t, "wi_stale_gd")
	if err := initLocal(t, o); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(o.GitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(o.GitDir, "STALE")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ExternalizeGitDir(o)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want a loud refusal, got %v", err)
	}
	if st, serr := os.Stat(filepath.Join(o.CapsuleDir, ".git")); serr != nil || !st.IsDir() {
		t.Fatal("the fresh clone's inner .git must be left untouched")
	}
	if _, serr := os.Stat(sentinel); serr != nil {
		t.Fatal("the existing target must be left untouched")
	}
}
