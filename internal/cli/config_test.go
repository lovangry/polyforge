package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	// No file, no env: documented defaults apply.
	t.Setenv("PF_SERVER", "")
	t.Setenv("PF_API_KEY", "")
	t.Setenv("PF_DAEMON_SOCKET", "")
	t.Setenv("PF_USER_ID", "")
	t.Setenv("PF_PROJECT", "")
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if cfg.Server != defaultServer {
		t.Errorf("Server = %q, want %q", cfg.Server, defaultServer)
	}
	if cfg.DaemonSocket != defaultDaemonSocket {
		t.Errorf("DaemonSocket = %q, want %q", cfg.DaemonSocket, defaultDaemonSocket)
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.APIKey)
	}
}

func TestLoadConfigFile(t *testing.T) {
	clearEnv(t)
	path := writeTemp(t, `
# pf CLI configuration (flat TOML subset)
server = "http://aihub.internal:8420"
api_key = "k_test"
daemon_socket = "/tmp/pfd.sock"  # trailing comment
user_id = "usr_1"
project = "polyforge"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "http://aihub.internal:8420" {
		t.Errorf("Server = %q", cfg.Server)
	}
	if cfg.APIKey != "k_test" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
	if cfg.DaemonSocket != "/tmp/pfd.sock" {
		t.Errorf("DaemonSocket = %q", cfg.DaemonSocket)
	}
	if cfg.UserID != "usr_1" || cfg.Project != "polyforge" {
		t.Errorf("UserID/Project = %q/%q", cfg.UserID, cfg.Project)
	}
}

func TestLoadConfigSingleQuotedAndEmpty(t *testing.T) {
	clearEnv(t)
	path := writeTemp(t, "server = 'http://x:1'\napi_key = \"\"\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "http://x:1" || cfg.APIKey != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigMalformed(t *testing.T) {
	clearEnv(t)
	for _, bad := range []string{
		"server http://x\n",                // no '='
		"= \"x\"\n",                        // empty key
		"server = bareword\n",              // unquoted value
		"server = \"unterminated\n",        // quote mismatch
		"server = \"a\"\nserver = \"b\"\n", // duplicate key
	} {
		_, err := LoadConfig(writeTemp(t, bad))
		if err == nil {
			t.Errorf("malformed config %q must error", bad)
		}
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	path := writeTemp(t, "server = \"http://from-file:8420\"\napi_key = \"filekey\"\n")
	t.Setenv("PF_SERVER", "http://from-env:8420")
	t.Setenv("PF_API_KEY", "envkey")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "http://from-env:8420" {
		t.Errorf("env must beat file: Server = %q", cfg.Server)
	}
	if cfg.APIKey != "envkey" {
		t.Errorf("env must beat file: APIKey = %q", cfg.APIKey)
	}
}

func TestParseFlatTOMLRejectsTableHeader(t *testing.T) {
	if _, err := parseFlatTOML("[table]\nserver = \"x\"\n"); err == nil {
		t.Error("table headers are out of the documented subset and must be rejected")
	}
	if _, err := parseFlatTOML("x = \"a\\nb\"\n"); err == nil {
		t.Error("escape sequences must be rejected")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	p := DefaultConfigPath()
	if !strings.HasSuffix(p, "polyforge/cli.toml") {
		t.Errorf("DefaultConfigPath = %q, want .../polyforge/cli.toml", p)
	}
}

// clearEnv removes the override variables for hermetic tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PF_SERVER", "PF_API_KEY", "PF_DAEMON_SOCKET", "PF_USER_ID", "PF_PROJECT", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
}
