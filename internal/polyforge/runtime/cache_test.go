package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheLogCreatesLogDirectory(t *testing.T) {
	d := t.TempDir()
	cacheLog(d, "cache skipped")
	data, err := os.ReadFile(filepath.Join(d, "logs", "cache.log"))
	if err != nil || !strings.Contains(string(data), "cache skipped") {
		t.Fatalf("cache log was not created: %v %q", err, data)
	}
}

func TestCacheAbsentLeavesMaterializationUnchanged(t *testing.T) {
	d := t.TempDir()
	before, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	warmCapsuleCache(d, "project", map[string]string{"repo": "base"})
	after, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("absent cache changed materialization: before=%d after=%d", len(before), len(after))
	}
}

func TestWarmCacheHardlinksNodeModules(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst")
	if err := os.MkdirAll(filepath.Join(src, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "node_modules", "x"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if !hardlinkTree(filepath.Join(src, "node_modules"), filepath.Join(dst, "node_modules")) {
		t.Fatal("hardlinkTree failed")
	}
	a, _ := os.Stat(filepath.Join(src, "node_modules", "x"))
	b, _ := os.Stat(filepath.Join(dst, "node_modules", "x"))
	if !os.SameFile(a, b) {
		t.Fatal("node_modules file was copied instead of hardlinked")
	}
}

func TestCacheSkipsBaselineMismatch(t *testing.T) {
	d := t.TempDir()
	cur := filepath.Join(d, "current")
	repo := filepath.Join(cur, "repos", "repo")
	os.MkdirAll(filepath.Join(repo, ".polyforge"), 0755)
	os.MkdirAll(filepath.Join(d, "logs"), 0755)
	os.WriteFile(filepath.Join(repo, ".polyforge", "capsule-cache.conf"), []byte("node_modules = true\n"), 0644)
	writeCacheCapsule(t, d, "other", "project", "old", "cached")
	warmCapsuleCache(cur, "project", map[string]string{"repo": "new"})
	if _, err := os.Stat(filepath.Join(repo, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("mismatched cache was used: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(cur, "logs", "cache.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "no warmed capsule with matching baseline") {
		t.Fatalf("missing mismatch reason: %s", log)
	}
}

func TestCacheSelectsSameBaselineCapsule(t *testing.T) {
	d := t.TempDir()
	cur := filepath.Join(d, "current")
	repo := filepath.Join(cur, "repos", "repo")
	os.MkdirAll(filepath.Join(repo, ".polyforge"), 0755)
	os.WriteFile(filepath.Join(repo, ".polyforge", "capsule-cache.conf"), []byte("node_modules = true\n"), 0644)
	src := writeCacheCapsule(t, d, "other", "project", "same", "cached")
	warmCapsuleCache(cur, "project", map[string]string{"repo": "same"})
	a, err := os.Stat(filepath.Join(src, "repos", "repo", "node_modules", "x"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(repo, "node_modules", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("same-baseline cache was not hardlinked")
	}
}

func writeCacheCapsule(t *testing.T, root, name, project, baseline, content string) string {
	t.Helper()
	d := filepath.Join(root, name)
	repo := filepath.Join(d, "repos", "repo")
	if err := os.MkdirAll(filepath.Join(repo, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "capsule.json"), []byte(`{"project":"`+project+`","repos":{"repo":"`+baseline+`"}}`), 0644)
	os.WriteFile(filepath.Join(repo, "node_modules", "x"), []byte(content), 0644)
	os.WriteFile(filepath.Join(repo, ".polyforge-warmed"), []byte("warmed\n"), 0644)
	return d
}

func TestCacheRejectsMissingBuildPath(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst")
	os.MkdirAll(src, 0755)
	if copyBuildPath(src, dst, "missing/dist") {
		t.Fatal("missing build path unexpectedly copied")
	}
}

func TestCacheInstallFailureDoesNotBlock(t *testing.T) {
	d := t.TempDir()
	repo := filepath.Join(d, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	runCacheInstall(repo, "false", d)
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("install failure removed materialization: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, "logs", "cache.log")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallBreaksHardlinks(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst")
	if err := os.MkdirAll(filepath.Join(src, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "node_modules", "x"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if !hardlinkTree(filepath.Join(src, "node_modules"), filepath.Join(dst, "node_modules")) {
		t.Fatal("hardlinkTree failed")
	}
	os.MkdirAll(filepath.Join(d, "logs"), 0755)
	runCacheInstall(dst, "printf new > node_modules/x", d)
	b, _ := os.ReadFile(filepath.Join(src, "node_modules", "x"))
	if string(b) != "old" {
		t.Fatalf("install mutated hardlinked source: %q", b)
	}
}

func TestWarmCacheCopiesBuildPaths(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst")
	os.MkdirAll(filepath.Join(src, "packages", "a", "dist"), 0755)
	os.WriteFile(filepath.Join(src, "packages", "a", "dist", "x"), []byte("types"), 0644)
	if !copyBuildPath(src, dst, "packages/*/dist") {
		t.Fatal("copy failed")
	}
	b, e := os.ReadFile(filepath.Join(dst, "packages", "a", "dist", "x"))
	if e != nil || string(b) != "types" {
		t.Fatalf("copy=%q,%v", b, e)
	}
}

func TestHardlinkTreeFailureCleansDestination(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(src, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(cacheMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if hardlinkTree(src, dst) {
		t.Fatal("oversized tree unexpectedly linked")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("partial destination remains: %v", err)
	}
}

func TestCopyBuildPathFailureCleansDestination(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src", "dist")
	dst := filepath.Join(d, "dst")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(src, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(cacheMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if copyBuildPath(filepath.Join(d, "src"), dst, "dist") {
		t.Fatal("oversized path unexpectedly copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "dist")); !os.IsNotExist(err) {
		t.Fatalf("partial destination remains: %v", err)
	}
}
