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
	if err := InitLocal(o); err != nil {
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

func TestRemotePublished(t *testing.T) {
	setIdentity(t)
	o, barePath := testOpts(t, "wi_pub")
	if p, err := RemotePublished(o); err != nil || p {
		t.Fatalf("absent repo: published=%v err=%v, want false,nil", p, err)
	}
	mkBare(t, barePath, true)
	if p, err := RemotePublished(o); err != nil || p {
		t.Fatalf("empty repo: published=%v err=%v, want false,nil", p, err)
	}
	mkCapsule(t, o)
	writeDirty(t, o, "f.txt", "x\n")
	if _, _, _, err := CommitPush(o, "x"); err != nil {
		t.Fatal(err)
	}
	if p, err := RemotePublished(o); err != nil || !p {
		t.Fatalf("published repo: published=%v err=%v, want true,nil", p, err)
	}
	o.RemoteURL = "http://127.0.0.1:1/x.git"
	if _, err := RemotePublished(o); err == nil {
		t.Fatal("transport failure must be an error, not 'nothing published'")
	}
}
