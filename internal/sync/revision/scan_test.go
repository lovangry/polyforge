package revision

// Scan tests over a synthetic capsule directory: sync-root coverage,
// exclusion coverage (SYNC-INV-10), git-aware repo enumeration,
// gitignore handling.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeCapsuleTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanCapsuleRootsAndExclusions(t *testing.T) {
	dir := t.TempDir()
	writeCapsuleTree(t, dir, map[string]string{
		"runtime-state/execution.json":         `{"version":1}`,
		"artifacts/spec.md":                    "spec",
		"logs/run.txt":                         "log",
		"harness/pi/session.json":              `{"session_id":"s1"}`,
		"harness/pi/session-data/whatever.bin": "opaque",
		".pi/agents/code.md":                   "agentdef",
		"workflow.md":                          "wfsnapshot",
		"sync/dirty.json":                      "{}",
		"capsule.json":                         "{}",
	})
	snap, err := ScanCapsule(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"runtime-state/execution.json", "artifacts/spec.md", "logs/run.txt", "harness/pi/session.json",
	} {
		if _, ok := snap.Files[want]; !ok {
			t.Errorf("missing synced path %s", want)
		}
	}
	for _, banned := range []string{
		"harness/pi/session-data/whatever.bin", ".pi/agents/code.md", "workflow.md",
		"sync/dirty.json", "capsule.json",
	} {
		if _, ok := snap.Files[banned]; ok {
			t.Errorf("excluded path %s was scanned", banned)
		}
	}
}

func TestScanCapsuleGitRepo(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repos", "r1")
	os.MkdirAll(repo, 0o755)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("t"), 0o644)
	os.WriteFile(filepath.Join(repo, "ignored.bin"), []byte("build"), 0o644)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.bin\n"), 0o644)
	run("add", "tracked.txt", ".gitignore")
	run("commit", "-q", "-m", "c")
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("u"), 0o644)

	snap, err := ScanCapsule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files["repos/r1/tracked.txt"]; !ok {
		t.Error("tracked file missing from scan")
	}
	if _, ok := snap.Files["repos/r1/untracked.txt"]; !ok {
		t.Error("untracked (not ignored) file missing from scan")
	}
	if _, ok := snap.Files["repos/r1/.gitignore"]; !ok {
		t.Error(".gitignore must sync (tracked)")
	}
	if _, ok := snap.Files["repos/r1/ignored.bin"]; ok {
		t.Error("gitignored file must NOT sync (05 §4.1)")
	}
	if _, ok := snap.Files["repos/r1/.git/config"]; ok {
		t.Error(".git/ must never sync")
	}
}

func TestScanCapsuleSymlink(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755)
	os.WriteFile(filepath.Join(dir, "artifacts", "real.txt"), []byte("x"), 0o644)
	if err := os.Symlink("real.txt", filepath.Join(dir, "artifacts", "link.txt")); err != nil {
		t.Fatal(err)
	}
	snap, err := ScanCapsule(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, ok := snap.Files["artifacts/link.txt"]
	if !ok || !fi.IsSymlink || fi.Target != "real.txt" {
		t.Fatalf("symlink not scanned: %+v ok=%v", fi, ok)
	}
}

func TestWorktreeDigestNoRepos(t *testing.T) {
	d, err := WorktreeDigest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d != "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945" {
		t.Fatalf("no-repo worktree digest = %s", d)
	}
}
