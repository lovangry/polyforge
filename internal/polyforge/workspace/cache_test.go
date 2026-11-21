package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheConfigAbsentNoIO(t *testing.T) {
	d := t.TempDir()
	c, e := LoadCacheConfig(d)
	if e != nil || c != nil {
		t.Fatalf("absent config = %#v,%v", c, e)
	}
}
func TestCacheConfigRejectsAbsoluteBuildPath(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, ".polyforge")
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "capsule-cache.conf"), []byte("build_paths = /tmp/dist\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadCacheConfig(d); err == nil || c != nil {
		t.Fatalf("absolute build path accepted: %#v, %v", c, err)
	}
}

func TestCacheConfigParsesDeclaration(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, ".polyforge")
	os.MkdirAll(p, 0755)
	os.WriteFile(filepath.Join(p, "capsule-cache.conf"), []byte("build_paths = packages/*/dist, .eslintcache\nnode_modules = true\ninstall = pnpm install --offline\n"), 0644)
	c, e := LoadCacheConfig(d)
	if e != nil || len(c.BuildPaths) != 2 || !c.NodeModules || c.Install != "pnpm install --offline" {
		t.Fatalf("config=%#v err=%v", c, e)
	}
}
