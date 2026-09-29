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

func TestLoadConfigEmptyPathLoadsDefaultFile(t *testing.T) {
	// Regression (FC-2): an empty path means "the documented default",
	// not "no file": without --config the CLI must still honor
	// ~/.config/polyforge/cli.toml.
	clearEnv(t)
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	if err := os.MkdirAll(filepath.Join(base, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "polyforge", "cli.toml")
	if err := os.WriteFile(file, []byte(`server = "http://from-default-file:8420"`+"\n"+
		`api_key = "k_defaultfile"`+"\n"+
		`project = "fileproj"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\") must load the default file: %v", err)
	}
	if cfg.Server != "http://from-default-file:8420" {
		t.Errorf("Server = %q, want the default file's value", cfg.Server)
	}
	if cfg.APIKey != "k_defaultfile" {
		t.Errorf("APIKey = %q, want the default file's value", cfg.APIKey)
	}
	if cfg.Project != "fileproj" {
		t.Errorf("Project = %q, want the default file's value", cfg.Project)
	}
}

func TestLoadConfigEmptyPathMissingDefaultFileIsNotError(t *testing.T) {
	// An absent default file keeps the documented defaults (no error).
	clearEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("missing default file must not error: %v", err)
	}
	if cfg.Server != defaultServer || cfg.APIKey != "" {
		t.Errorf("cfg = %+v, want built-in defaults", cfg)
	}
}

func TestParseFlatTOMLHashInsideQuotedValue(t *testing.T) {
	// Regression (FC-8): `#` inside a quoted value is data (an api key
	// may contain one); comments are stripped only outside quotes.
	kv, err := parseFlatTOML("api_key = \"k#abc\"\n" +
		"server = 'http://x#y:1' # trailing comment\n" +
		"# full-line comment\n")
	if err != nil {
		t.Fatalf("values containing # must parse: %v", err)
	}
	if kv["api_key"] != "k#abc" {
		t.Errorf("api_key = %q, want k#abc", kv["api_key"])
	}
	if kv["server"] != "http://x#y:1" {
		t.Errorf("server = %q, want http://x#y:1 (trailing comment stripped)", kv["server"])
	}
}

// clearEnv removes the override variables for hermetic tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PF_SERVER", "PF_API_KEY", "PF_DAEMON_SOCKET", "PF_USER_ID", "PF_PROJECT", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
}
