package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/mapping"
)

// clearEnv removes the PF_* overrides for a hermetic config test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PF_SERVER", "PF_API_KEY", "PF_DAEMON_SOCKET", "PF_USER_ID", "PF_CHAT_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir()) // no ambient ~/.polyforge / ~/.config leaks in
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConfigPriorityMatrix walks one field (Server) down the whole
// precedence chain — flag > env > toml > default — plus the other flat
// fields at toml level. There is no `project` key anywhere in the chain
// (polyforge#113): the mapping lives in [workspace] sections.
func TestConfigPriorityMatrix(t *testing.T) {
	clearEnv(t)

	// Level 0: no file, no env → documented defaults, EMPTY mapping.
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if cfg.Server != defaultServer {
		t.Errorf("default Server = %q, want %q", cfg.Server, defaultServer)
	}
	if cfg.DaemonSocket != defaultDaemonSocket {
		t.Errorf("default DaemonSocket = %q, want %q", cfg.DaemonSocket, defaultDaemonSocket)
	}
	if cfg.Mapping == nil || !cfg.Mapping.Empty() {
		t.Errorf("missing file must yield an empty (non-nil) mapping, got %+v", cfg.Mapping)
	}

	// Level 1: toml beats defaults.
	path := writeTempConfig(t, `
# pf CLI configuration (flat TOML subset + [workspace] sections)
server = "http://aihub.internal:8420"
api_key = "k_toml"
daemon_socket = "/tmp/pfd.sock"  # trailing comment
user_id = "usr_toml"

[workspace."`+t.TempDir()+`"]
projects = ["polyforge", "ieops"]
`)
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(toml): %v", err)
	}
	if cfg.Server != "http://aihub.internal:8420" || cfg.APIKey != "k_toml" ||
		cfg.DaemonSocket != "/tmp/pfd.sock" || cfg.UserID != "usr_toml" {
		t.Fatalf("toml level: got %+v", cfg)
	}
	if got := cfg.Mapping.Projects(); len(got) != 2 || got[0] != "polyforge" || got[1] != "ieops" {
		t.Fatalf("toml mapping projects = %v", got)
	}

	// Level 2: env beats toml.
	t.Setenv("PF_SERVER", "http://env:8420")
	t.Setenv("PF_API_KEY", "k_env")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(env): %v", err)
	}
	if cfg.Server != "http://env:8420" || cfg.APIKey != "k_env" {
		t.Errorf("env must beat toml: got %+v", cfg)
	}
	// Untouched keys still come from the file.
	if cfg.DaemonSocket != "/tmp/pfd.sock" {
		t.Errorf("non-overridden keys must stay from toml: got %+v", cfg)
	}

	// Level 3: flag beats env — applied by Run; verified end-to-end in
	// TestFlagBeatsEnvEndToEnd (verbs_test.go) since only a verb call
	// proves the resolved value reached the wire.
}

// TestConfigMappingTildeAndAbsoluteKeys: the mapping loads with ~
// expanded and absolute keys literal — the same view any reader gets
// through LoadConfig (the daemon reads the same function).
func TestConfigMappingTildeAndAbsoluteKeys(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	wsAbs := t.TempDir()
	path := writeTempConfig(t, `
server = "http://x:1"

[workspace."~/w113home"]
projects = ["e2e"]

[workspace."`+wsAbs+`"]
projects = ["ieops"]
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wantTilde := filepath.Join(home, "w113home")
	got := cfg.Mapping.Workspaces()
	if len(got) != 2 || got[0] != wantTilde || got[1] != wsAbs {
		t.Fatalf("workspaces = %v, want [%s %s]", got, wantTilde, wsAbs)
	}
	for proj, wantWS := range map[string]string{"e2e": wantTilde, "ieops": wsAbs} {
		ws, err := cfg.Mapping.WorkspaceFor(proj)
		if err != nil || ws != wantWS {
			t.Fatalf("WorkspaceFor(%q) = %q, %v", proj, ws, err)
		}
	}
}

// TestConfigOldShapeRefusedWithMigrationGuidance: the retired flat
// `project` key refuses EVERY reader (the loader is the shared choke
// point) with the three guidance tokens the contract pins.
func TestConfigOldShapeRefusedWithMigrationGuidance(t *testing.T) {
	clearEnv(t)
	path := writeTempConfig(t, "server = \"http://x:1\"\napi_key = \"k\"\nproject = \"e2e\"\n")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("the retired flat project key must refuse loading")
	}
	for _, tok := range []string{"project", "[workspace", "doctor"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("refusal %q misses the %q guidance token", err, tok)
		}
	}
}

// TestConfigMalformedMappingRejected: the loud-parse posture over the
// extended grammar — each malformed shape is a line-named error.
func TestConfigMalformedMappingRejected(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name string
		toml string
		want string
	}{
		{"bare workspace table", "[workspace]\nprojects = [\"a\"]\n", `workspace`},
		{"unquoted section key", "[workspace.~/ws]\nprojects = [\"a\"]\n", "QUOTED"},
		{"unknown section", "[other]\nkey = \"v\"\n", "other"},
		{"non-array projects", "[workspace.\"/tmp/x\"]\nprojects = \"a\"\n", "array of quoted strings"},
		{"unquoted array element", "[workspace.\"/tmp/x\"]\nprojects = [a]\n", "quoted strings"},
		{"non-string array element", "[workspace.\"/tmp/x\"]\nprojects = [1]\n", "quoted strings"},
		{"top-level projects key", "projects = [\"a\"]\n", "[workspace"},
		{"other key in section", "[workspace.\"/tmp/x\"]\ngoal = \"nope\"\n", "projects"},
		{"duplicate section", "[workspace.\"/tmp/x\"]\nprojects = [\"a\"]\n[workspace.\"/tmp/x\"]\nprojects = [\"b\"]\n", "duplicate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempConfig(t, c.toml)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("malformed mapping must be a load error (%s)", c.name)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("error %q must be line-named", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q misses %q", err, c.want)
			}
		})
	}
}

// TestConfigMappingValidationRefusals surface the typed mapping errors
// through the loader (the daemon's S04/S05 bars ride the same path).
func TestConfigMappingValidationRefusals(t *testing.T) {
	clearEnv(t)
	wsA := t.TempDir()
	wsB := t.TempDir()
	t.Run("ambiguous inverse", func(t *testing.T) {
		path := writeTempConfig(t,
			"[workspace.\""+wsA+"\"]\nprojects = [\"ieops\"]\n[workspace.\""+wsB+"\"]\nprojects = [\"ieops\"]\n")
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("ambiguous inverse must refuse loading")
		}
		var ae *mapping.AmbiguityError
		if !errorsAs(err, &ae) {
			t.Fatalf("want the typed AmbiguityError, got %T: %v", err, err)
		}
		for _, tok := range []string{"ieops", wsA, wsB} {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("error %q misses %q", err, tok)
			}
		}
	})
	t.Run("duplicate in one list", func(t *testing.T) {
		path := writeTempConfig(t, "[workspace.\""+wsA+"\"]\nprojects = [\"e2e\", \"e2e\"]\n")
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("duplicate project must refuse loading")
		}
		var de *mapping.DuplicateError
		if !errorsAs(err, &de) {
			t.Fatalf("want the typed DuplicateError, got %T: %v", err, err)
		}
		for _, tok := range []string{"e2e", wsA} {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("error %q misses %q", err, tok)
			}
		}
	})
	t.Run("relative key", func(t *testing.T) {
		path := writeTempConfig(t, "[workspace.\"rel/ws\"]\nprojects = [\"e2e\"]\n")
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "rel/ws") {
			t.Fatalf("relative key must refuse naming it, got %v", err)
		}
	})
	t.Run("trailing separator", func(t *testing.T) {
		path := writeTempConfig(t, "[workspace.\""+wsA+"/\"]\nprojects = [\"e2e\"]\n")
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "trailing") {
			t.Fatalf("trailing separator must refuse loudly, got %v", err)
		}
	})
}

// TestConfigLegacyFallbackDropped: ~/.polyforge/config.toml is not read
// when the XDG cli.toml is absent (polyforge#113) — the command proceeds
// on the documented defaults, never the legacy file's values.
func TestConfigLegacyFallbackDropped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(home, "xdg-config")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(home, ".polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "server = \"http://127.0.0.1:59999\"\napi_key = \"k_legacy\"\nproject = \"e2e\"\n"
	if err := os.WriteFile(filepath.Join(home, ".polyforge", "config.toml"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("a missing XDG file is not an error: %v", err)
	}
	if cfg.Server != defaultServer || cfg.APIKey != "" {
		t.Fatalf("legacy file leaked in: %+v", cfg)
	}
	if !cfg.Mapping.Empty() {
		t.Fatalf("legacy mapping leaked in: %+v", cfg.Mapping)
	}
	if got := DefaultConfigPath(); got != filepath.Join(xdg, "polyforge", "cli.toml") {
		t.Fatalf("DefaultConfigPath = %q, want the XDG location", got)
	}
}

// TestConfigMalformedTomlIsAnError pins the loud-failure ruling.
func TestConfigMalformedTomlIsAnError(t *testing.T) {
	clearEnv(t)
	path := writeTempConfig(t, "server = \"http://x\nbroken line without equals\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("malformed toml must be an error")
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// The watch summarizer is gone: its [watch.*] sections and watch_* keys
// are tolerated and ignored (an installed cli.toml keeps loading); any
// other unknown section still refuses.
func TestConfigRetiredWatchConfigIsIgnored(t *testing.T) {
	clearEnv(t)
	ws := t.TempDir()
	base := "[workspace.\"" + ws + "\"]\nprojects = [\"e2e\"]\n"
	body := base + "\n[watch.summarize_model.pi]\nmodel = \"prov/pi-m\"\n\nwatch_poll = \"7s\"\nharness_sessions_root = \"/x\"\n" +
		"\n[watch.summarize_model.claude]\nmodel = \"prov/cl\"\n"
	cfg, err := LoadConfig(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("retired watch config must stay loadable: %v", err)
	}
	if len(cfg.Mapping.Workspaces()) != 1 {
		t.Fatalf("the mapping must survive: %v", cfg.Mapping.Workspaces())
	}
	if _, err := LoadConfig(writeTempConfig(t, base+"\n[other.thing]\nx = \"1\"\n")); err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Fatalf("an unknown section must still refuse: %v", err)
	}
}
