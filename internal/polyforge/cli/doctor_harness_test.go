package cli

// doctor_harness_test.go: polyforge#159 — the "harness claude" doctor
// check (the daemon's view of the Claude Code CLI, only when a step
// selects claude) and the cli.toml claude keys.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

func TestClaudeHarnessCheck(t *testing.T) {
	steps := []workspace.StepResolution{{Project: "p", Bundle: "bug-fix", Step: "review", Harness: "claude",
		HarnessFrom: "steps.p.review.harness", Served: []string{"claude-opus-5-5"}, ModelsFrom: "steps.p.review.models"}}
	ok := HarnessStatus{Bin: "claude", Found: true, Path: "/bin/claude", Version: "2.1.296", Verified: true,
		AuthTokenEnv: "set", AuthTokenToml: "unset", APIKeyEnv: "unset", BaseURLEnv: "unset", BackgroundTasks: "disabled"}
	cases := []struct {
		name   string
		steps  []workspace.StepResolution
		up     bool
		hs     map[string]HarnessStatus
		want   string
		needle string
	}{
		{"no claude step", nil, true, nil, "pass", "no step selects the claude harness"},
		{"healthy", steps, true, map[string]HarnessStatus{"claude": ok}, "pass", "p/bug-fix/review: claude (steps.p.review.harness), [claude-opus-5-5] (steps.p.review.models)"},
		{"daemon down", steps, false, nil, "skip", "daemon is unreachable"},
		{"older daemon", steps, true, map[string]HarnessStatus{}, "warn", "no harness status"},
		{"binary missing", steps, true, map[string]HarnessStatus{"claude": {Bin: "claude"}}, "fail", "does not resolve"},
		{"unverified version", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus { h := ok; h.Version, h.Verified = "9.9.9", false; return h }()}, "warn", "9.9.9 is not in the verified set"},
		{"no credential variable", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus { h := ok; h.AuthTokenEnv = "unset"; return h }()}, "warn", "settings.json `env` section"},
		{"base url set", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus { h := ok; h.BaseURLEnv = "set"; return h }()}, "warn", "ANTHROPIC_BASE_URL is set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, detail := claudeHarnessCheck(c.steps, "/wf", c.up, c.hs)
			if st != c.want || !strings.Contains(detail, c.needle) {
				t.Fatalf("got %s %q, want %s containing %q", st, detail, c.want, c.needle)
			}
		})
	}
}

func TestConfigClaudeKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.toml")
	if err := os.WriteFile(path, []byte("claude_bin = \"/opt/claude\"\nanthropic_auth_token = \"tok\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeBin != "/opt/claude" || cfg.AnthropicAuthToken != "tok" {
		t.Fatalf("cfg %+v", cfg)
	}
}
