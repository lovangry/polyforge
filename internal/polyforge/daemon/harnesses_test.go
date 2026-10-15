package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The /status harness view reports the claude binary, its version
// (cached per path+mtime) and set/unset only — never a credential value.
func TestHarnessStatusesNeverCarryValues(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	count := filepath.Join(dir, "count")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x >> "+count+"\necho '2.1.296 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	os.Unsetenv("ANTHROPIC_API_KEY")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-SENTINEL")
	d := &Daemon{cfg: Config{ClaudeBin: bin, PiBin: "/nonexistent/pi"}}
	hs := d.harnessStatuses()
	cl := hs["claude"]
	if !cl.Found || cl.Version != "2.1.296" || !cl.Verified || cl.AuthTokenEnv != "set" || cl.APIKeyEnv != "unset" {
		t.Fatalf("claude status %+v", cl)
	}
	if hs["pi"].Found {
		t.Fatal("a missing pi binary reported found")
	}
	blob, _ := json.Marshal(hs)
	if strings.Contains(string(blob), "SENTINEL") {
		t.Fatalf("status carries a credential value: %s", blob)
	}
	d.harnessStatuses() // cached: --version runs once per binary path+mtime
	if raw, _ := os.ReadFile(count); strings.Count(string(raw), "x") != 1 {
		t.Fatalf("--version ran %d times", strings.Count(string(raw), "x"))
	}
	d2 := &Daemon{cfg: Config{ClaudeBin: "/nonexistent/claude"}}
	if d2.harnessStatuses()["claude"].Found {
		t.Fatal("a missing claude binary reported found")
	}
}
