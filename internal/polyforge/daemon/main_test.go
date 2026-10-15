package daemon

// main_test.go: #171 additions plus the TestMain env isolation. The
// harness-root tests pin the polyforge#171 launch surface: the former
// --harness-sessions-root fails the boot at flag parsing (unknown flag —
// the loud failure, never a silent path move), the --harness-root > env
// resolution order, and the pass-through into the assembled config (the
// runtime derives the three harness dirs from it; the default stays the
// data dir).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/mapping"
	"github.com/silgrid/polyforge/internal/testenv"
)

// TestMain isolates HOME/XDG/POLYFORGE_* so no test can touch the real install.
func TestMain(m *testing.M) { testenv.Main(m) }

// TestOldSessionsRootFlagRefuses: the pre-#171 flag name is not parsed —
// an old deployment fails loudly at flag parsing (exit 2), never silently
// moves its harness state under the renamed semantics.
func TestOldSessionsRootFlagRefuses(t *testing.T) {
	oldName := "--harness" + "-sessions" + "-root"
	var stderr bytes.Buffer
	code := Main([]string{oldName, filepath.Join(t.TempDir(), "sessions")}, nil, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (unknown-flag refusal), stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), oldName) {
		t.Fatalf("refusal %q does not name the old flag", stderr.String())
	}
}

func TestResolveHarnessRootOrder(t *testing.T) {
	if got := resolveHarnessRoot("/flag", "/env"); got != "/flag" {
		t.Fatalf("flag %q lost to env %q", got, "/flag")
	}
	if got := resolveHarnessRoot("", "/env"); got != "/env" {
		t.Fatalf("env fallback %q, want /env", got)
	}
	if got := resolveHarnessRoot("", ""); got != "" {
		t.Fatalf("no override must leave the default path, got %q", got)
	}
}

// TestHarnessRootPassThrough: the assembled daemon config carries the
// flag/env override (and derives the default from the data dir when no
// override is set) — the runtime's three harness dirs are derived off it.
func TestHarnessRootPassThrough(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "cli.toml")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(ws, "capsules"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "server = \"http://127.0.0.1:1\"\n[workspace.\"" + ws + "\"]\nprojects = [\"e2e\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := mapping.Build([]mapping.Entry{{Workspace: ws, Projects: []string{"e2e"}}})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "harness-root")
	boot := func(cfg Config) Config {
		cfg.Mapping = plan
		cfg.Hub = newFakeHub()
		cfg.SocketPath = filepath.Join(t.TempDir(), "s.sock")
		cfg.AgentStarter = fakeEngineStarter()
		d, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return d.cfg
	}
	got := boot(Config{DataDir: t.TempDir(), HarnessRoot: root})
	if got.HarnessRoot != root {
		t.Fatalf("the override did not pass through: %q != %q", got.HarnessRoot, root)
	}
	def := boot(Config{DataDir: t.TempDir()})
	if def.HarnessRoot != def.DataDir {
		t.Fatalf("the default harness root %q is not the data dir %q", def.HarnessRoot, def.DataDir)
	}
}
