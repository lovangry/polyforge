package daemon

// workspace_flag_test.go pins the #113 launch surface at the daemon's
// real entry (Main): the retired workspace/project selection flags fail
// flag PARSING (exit 2 — deleted, never demoted), the retired selection
// environment variables refuse the boot BEFORE anything is loaded or
// served (exit 1, naming the variable), a config with zero [workspace]
// sections refuses startup naming the shape and `pf doctor`, and a
// mapping-bearing config proceeds past the launch guard. All retired
// names are assembled from fragments so this file's bytes never carry
// them contiguously (the greppable floor — S36's own convention).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envTok joins retired env-name fragments at runtime (flagTok lives in
// removed_knobs_test.go — same file set, one package).
func envTok(parts ...string) string { return strings.Join(parts, "") }

// TestRetiredSelectionFlagsFailAtParse: the deleted -workspace and
// --project flags (either dash spelling) are unknown-flag parse failures
// — the daemon never starts and the error names the flag.
func TestRetiredSelectionFlagsFailAtParse(t *testing.T) {
	cases := []struct {
		name string
		flag string
	}{
		{"retired workspace flag, single dash", flagTok("-", "work", "space")},
		{"retired workspace flag, double dash", flagTok("--", "work", "space")},
		{"retired project flag, single dash", flagTok("-", "proj", "ect")},
		{"retired project flag, double dash", flagTok("--", "proj", "ect")},
	}
	// Go's flag package normalizes the double-dash spelling in its
	// "not defined" message, so the named-token assertion uses the
	// single-dash form (a substring of the double-dash one).
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Main([]string{"--socket", "/nonexistent-probe.sock", c.flag, "/tmp/x"}, nil, &bytes.Buffer{}, &stderr)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (flag parse failure), stderr: %q", code, stderr.String())
			}
			named := flagTok("-", strings.TrimLeft(c.flag, "-"))
			if !strings.Contains(stderr.String(), named) {
				t.Fatalf("parse error %q does not name the retired flag %q", stderr.String(), named)
			}
		})
	}
}

// TestRetiredSelectionEnvRefusedBeforeAnythingLoads: either retired
// environment variable set (even over a fully valid mapping config)
// refuses the boot with an error naming the variable; the refusal is
// parse-first (a bad flag still wins with exit 2).
func TestRetiredSelectionEnvRefusedBeforeAnythingLoads(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	pfDir := filepath.Join(cfgDir, "polyforge")
	if err := os.MkdirAll(pfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	cfg := "server = \"http://127.0.0.1:1\"\n\n[workspace.\"" + ws + "\"]\nprojects = [\"proj\"]\n"
	if err := os.WriteFile(filepath.Join(pfDir, "cli.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("retired project variable", func(t *testing.T) {
		retired := envTok("PF_", "PROJECT")
		t.Setenv(retired, "ieops")
		var stderr bytes.Buffer
		code := Main([]string{"--socket", "/nonexistent-probe.sock"}, nil, &bytes.Buffer{}, &stderr)
		if code != 1 {
			t.Fatalf("exit = %d, want 1 (boot refusal), stderr: %q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), retired) {
			t.Fatalf("boot refusal %q does not name the retired variable %s", stderr.String(), retired)
		}
	})
	t.Run("retired workspace variable", func(t *testing.T) {
		retired := envTok("POLYFORGE_", "WORKSPACE")
		t.Setenv(retired, ws)
		var stderr bytes.Buffer
		code := Main([]string{"--socket", "/nonexistent-probe.sock"}, nil, &bytes.Buffer{}, &stderr)
		if code != 1 {
			t.Fatalf("exit = %d, want 1 (boot refusal), stderr: %q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), retired) {
			t.Fatalf("boot refusal %q does not name the retired variable %s", stderr.String(), retired)
		}
	})
	t.Run("parse failure still wins over the env guard", func(t *testing.T) {
		retired := envTok("PF_", "PROJECT")
		t.Setenv(retired, "ieops")
		var stderr bytes.Buffer
		code := Main([]string{"--socket", "/nonexistent-probe.sock", "--no-such-flag"}, nil, &bytes.Buffer{}, &stderr)
		if code != 2 {
			t.Fatalf("exit = %d, want 2 (flag parse before the env guard), stderr: %q", code, stderr.String())
		}
	})
}

// TestNoMappingRefusesAtMain: a config with zero [workspace] sections —
// the fresh-install template shape — refuses startup (exit 1) naming the
// [workspace] shape and `pf doctor`, before any socket is created.
func TestNoMappingRefusesAtMain(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	pfDir := filepath.Join(cfgDir, "polyforge")
	if err := os.MkdirAll(pfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pfDir, "cli.toml"),
		[]byte("server = \"http://127.0.0.1:1\"\napi_key = \"k\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := Main([]string{"--socket", filepath.Join(t.TempDir(), "d.sock")}, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no-mapping refusal), stderr: %q", code, stderr.String())
	}
	for _, tok := range []string{"[workspace", "doctor"} {
		if !strings.Contains(stderr.String(), tok) {
			t.Fatalf("refusal %q misses the %q guidance token", stderr.String(), tok)
		}
	}
}

// TestMappingConfiguredBootReachesWorkspaceValidation: a mapping-bearing
// config proceeds past the launch guard — with a MISSING workspace dir it
// fails in New's validation naming the workspace (proving the mapping was
// actually consumed, and the refusal names the mapped path).
func TestMappingConfiguredBootReachesWorkspaceValidation(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	pfDir := filepath.Join(cfgDir, "polyforge")
	if err := os.MkdirAll(pfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-ws")
	cfg := "server = \"http://127.0.0.1:1\"\n\n[workspace.\"" + missing + "\"]\nprojects = [\"proj\"]\n"
	if err := os.WriteFile(filepath.Join(pfDir, "cli.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := Main([]string{"--socket", filepath.Join(t.TempDir(), "d.sock")}, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (workspace validation failure), stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no-such-ws") {
		t.Fatalf("validation failure %q does not name the mapped workspace", stderr.String())
	}
}
