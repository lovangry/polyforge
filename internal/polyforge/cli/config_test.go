package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv removes the PF_* overrides for a hermetic config test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PF_SERVER", "PF_API_KEY", "PF_DAEMON_SOCKET", "PF_USER_ID", "PF_PROJECT"} {
		t.Setenv(k, "")
	}
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
// precedence chain — flag > env > toml > default — plus the other
// fields at toml level.
func TestConfigPriorityMatrix(t *testing.T) {
	clearEnv(t)

	// Level 0: no file, no env → documented defaults.
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

	// Level 1: toml beats defaults.
	path := writeTempConfig(t, `
# pf CLI configuration (flat TOML subset)
server = "http://aihub.internal:8420"
api_key = "k_toml"
daemon_socket = "/tmp/pfd.sock"  # trailing comment
user_id = "usr_toml"
project = "polyforge"
`)
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(toml): %v", err)
	}
	if cfg.Server != "http://aihub.internal:8420" || cfg.APIKey != "k_toml" ||
		cfg.DaemonSocket != "/tmp/pfd.sock" || cfg.UserID != "usr_toml" || cfg.Project != "polyforge" {
		t.Fatalf("toml level: got %+v", cfg)
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
	if cfg.DaemonSocket != "/tmp/pfd.sock" || cfg.Project != "polyforge" {
		t.Errorf("non-overridden keys must stay from toml: got %+v", cfg)
	}

	// Level 3: flag beats env — applied by Run; verified end-to-end in
	// TestFlagBeatsEnvEndToEnd (verbs_test.go) since only a verb call
	// proves the resolved value reached the wire.
}

// TestConfigMalformedTomlIsAnError pins the loud-failure ruling.
func TestConfigMalformedTomlIsAnError(t *testing.T) {
	clearEnv(t)
	path := writeTempConfig(t, "server = \"http://x\nbroken line without equals\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("malformed toml must be an error")
	}
}
