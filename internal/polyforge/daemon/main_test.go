package daemon

// main_test.go: #171 additions plus the TestMain env isolation. The
// harness-root tests pin the polyforge#171 launch surface: the former
// --harness-sessions-root fails the boot at flag parsing (unknown flag —
// the loud failure, never a silent path move) and the --harness-root
// pass-through into the assembled config (the
// runtime derives the three harness dirs from it; the default stays the
// data dir).

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
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

// TestHarnessRootPassThrough: the assembled daemon config carries the
// --harness-root override (and derives the default from the data dir when no
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

// TestApplyDaemonTablePrecedence (decision 2026-10-16 G2): an explicit
// command-line flag > the cli.toml [daemon] table > the built-in default.
func TestApplyDaemonTablePrecedence(t *testing.T) {
	table := cli.DaemonSettings{StepTimeout: 45 * time.Minute, StallWindow: -time.Second,
		HarnessRoot: "/t/root", OperatorAgentDir: "/t/op", MaxConcurrentJobs: 6}
	parse := func(args ...string) (time.Duration, time.Duration, string, string, int) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		st := fs.Duration("step-timeout", 0, "")
		sw := fs.Duration("stall-window", 0, "")
		hr := fs.String("harness-root", "", "")
		op := fs.String("operator-agent-dir", "", "")
		mx := fs.Int("max-concurrent-jobs", 0, "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		applyDaemonTable(fs, table, st, sw, hr, op, mx)
		return *st, *sw, *hr, *op, *mx
	}
	st, sw, hr, op, mx := parse()
	if st != 45*time.Minute || sw != -time.Second || hr != "/t/root" || op != "/t/op" || mx != 6 {
		t.Fatalf("table not applied: %v %v %q %q %d", st, sw, hr, op, mx)
	}
	st, sw, hr, op, mx = parse("-step-timeout", "5m", "-stall-window", "0", "-harness-root", "/f/root",
		"-operator-agent-dir", "/f/op", "-max-concurrent-jobs", "2")
	if st != 5*time.Minute || sw != 0 || hr != "/f/root" || op != "/f/op" || mx != 2 {
		t.Fatalf("explicit flags must beat the table: %v %v %q %q %d", st, sw, hr, op, mx)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	st0, sw0 := fs.Duration("step-timeout", 0, ""), fs.Duration("stall-window", 0, "")
	hr0, op0, mx0 := fs.String("harness-root", "", ""), fs.String("operator-agent-dir", "", ""), fs.Int("max-concurrent-jobs", 0, "")
	applyDaemonTable(fs, cli.DaemonSettings{}, st0, sw0, hr0, op0, mx0)
	if *st0 != 0 || *sw0 != 0 || *hr0 != "" || *op0 != "" || *mx0 != 0 {
		t.Fatal("an empty table must leave the built-in defaults")
	}
}
