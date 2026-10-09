package daemon

// workspace_flag_test.go pins the required-workspace launch surface
// (#103): -workspace PATH (env mirror POLYFORGE_WORKSPACE) is REQUIRED —
// no flag and no env refuses to start with an error naming the flag plus
// one example; the flag wins over the env (A1); the legacy
// -workspace-root flag is deleted outright (unknown-flag parse failure)
// and the deleted POLYFORGE_WORKSPACE_ROOT env is ignored silently — it
// neither satisfies the requirement nor perturbs the new surface.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestResolveWorkspaceRequiredError(t *testing.T) {
	_, err := resolveWorkspace("", func(string) string { return "" })
	if err == nil {
		t.Fatal("no flag and no env must refuse to start")
	}
	for _, want := range []string{"-workspace", "POLYFORGE_WORKSPACE", "pf daemon -workspace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error %q must name/mention %q", err, want)
		}
	}
}

func TestResolveWorkspaceEnvOnly(t *testing.T) {
	ws, err := resolveWorkspace("", func(key string) string {
		if key == "POLYFORGE_WORKSPACE" {
			return "/srv/pf/ws"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("env-only workspace must resolve: %v", err)
	}
	if ws != "/srv/pf/ws" {
		t.Fatalf("workspace = %q, want the env value", ws)
	}
}

func TestResolveWorkspaceFlagBeatsEnv(t *testing.T) {
	ws, err := resolveWorkspace("/flag/ws", func(key string) string {
		if key == "POLYFORGE_WORKSPACE" {
			return "/env/ws"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("flag+env must resolve: %v", err)
	}
	if ws != "/flag/ws" {
		t.Fatalf("workspace = %q, want the flag value (flag beats env)", ws)
	}
}

func TestResolveWorkspaceLegacyEnvIgnored(t *testing.T) {
	var queried []string
	getenv := func(key string) string {
		queried = append(queried, key)
		if key == "POLYFORGE_WORKSPACE_ROOT" {
			return "/legacy/ws"
		}
		return ""
	}
	// The deleted env alone does NOT satisfy the requirement.
	if _, err := resolveWorkspace("", getenv); err == nil {
		t.Fatal("POLYFORGE_WORKSPACE_ROOT alone must not satisfy the requirement")
	}
	// ... and the resolver never even consults it (ignored silently —
	// no error, no warning, no effect on the new surface).
	ws, err := resolveWorkspace("/new/ws", getenv)
	if err != nil {
		t.Fatalf("new surface with legacy env set must resolve: %v", err)
	}
	if ws != "/new/ws" {
		t.Fatalf("workspace = %q, want the new-surface value untouched by the legacy env", ws)
	}
	for _, k := range queried {
		if k == "POLYFORGE_WORKSPACE_ROOT" {
			t.Fatalf("resolver queried the deleted env %q — it must be ignored outright", k)
		}
	}
}

// TestMainUnknownWorkspaceRootFlag pins the outright deletion: passing
// the removed flag is an unknown-flag parse failure (exit 2), the same
// failure class as any unrecognized flag, and the daemon never starts.
func TestMainUnknownWorkspaceRootFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := Main([]string{"-workspace-root", "/tmp/pf-legacy-ws"}, nil, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (unknown flag)", code)
	}
	if !strings.Contains(stderr.String(), "workspace-root") {
		t.Errorf("stderr %q must name the unknown flag", stderr.String())
	}
}

// TestMainRefusesWithoutWorkspace pins the end-to-end refusal: a launch
// with neither -workspace nor POLYFORGE_WORKSPACE exits non-zero with an
// error naming the flag and one example invocation.
func TestMainRefusesWithoutWorkspace(t *testing.T) {
	t.Setenv("POLYFORGE_WORKSPACE", "")
	// Config isolation: the in-process Main reads the default config
	// location; a scratch XDG dir keeps an operator's config file out of
	// the refusal's causal chain (this machine is a live polyforge
	// install whose cli.toml is not this test's subject).
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stderr bytes.Buffer
	code := Main([]string{"-socket", "/tmp/pf-unused.sock"}, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refused: no workspace selected)", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "-workspace") || !strings.Contains(out, "example") {
		t.Errorf("refusal output %q must name the -workspace flag and an example", out)
	}
	if strings.Contains(out, "POLYFORGE_WORKSPACE_ROOT") {
		t.Errorf("refusal output %q must not resurrect the deleted env name as an option", out)
	}
}

// resolveWorkspace never wraps its refusal: Main prefixes "pf daemon: "
// exactly once when printing it.
func TestResolveWorkspaceErrorUnprefixed(t *testing.T) {
	_, err := resolveWorkspace("", func(string) string { return "" })
	if err == nil || errors.Unwrap(err) != nil {
		t.Fatalf("refusal must be a plain error, got %v", err)
	}
	if strings.HasPrefix(err.Error(), "pf daemon:") {
		t.Fatalf("refusal error carries the main.go prefix itself: %q (double prefix when printed)", err)
	}
}
