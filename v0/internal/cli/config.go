package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default aihub listen address (api/openapi.yaml servers: single port).
const defaultServer = "http://localhost:8420"

// Default daemon unix domain socket (04-local-runtime.md §5.1: the daemon
// listens on the local machine only, no network face).
const defaultDaemonSocket = "/run/polyforge.sock"

// Config holds the resolved CLI configuration.
type Config struct {
	// Server is the aihub base URL (e.g. http://localhost:8420).
	Server string
	// APIKey is the aihub api_key sent as a Bearer token.
	APIKey string
	// DaemonSocket is the local daemon unix domain socket path.
	DaemonSocket string
	// UserID is the optional local caller identity, used for
	// answer.answered_by (00-architecture.md §3.4a requires it to match the
	// authenticated identity).
	UserID string
	// Project is an optional default project for project-scoped verbs.
	Project string
}

// DefaultConfigPath returns the single documented config location:
// $XDG_CONFIG_HOME|~/.config / polyforge / cli.toml. ONE location is the
// ruling (see README "pf CLI"); there is no secondary lookup.
func DefaultConfigPath() string {
	base, err := os.UserConfigDir() // XDG_CONFIG_HOME or ~/.config
	if err != nil || base == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "polyforge/cli.toml"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "polyforge", "cli.toml")
}

// LoadConfig reads the config file (a missing file is not an error: the
// defaults apply) and applies environment overrides on top. An empty path
// means the documented default location (DefaultConfigPath), not "no
// file": a `pf` invocation without --config must still honor
// ~/.config/polyforge/cli.toml.
//
//	PF_SERVER         -> Server
//	PF_API_KEY        -> APIKey
//	PF_DAEMON_SOCKET  -> DaemonSocket
//	PF_USER_ID        -> UserID
//	PF_PROJECT        -> Project
//
// A malformed file IS an error: silent misconfiguration is worse than a
// loud failure.
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Server:       defaultServer,
		DaemonSocket: defaultDaemonSocket,
	}
	if path == "" {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err == nil {
		kv, perr := parseFlatTOML(string(data))
		if perr != nil {
			return cfg, fmt.Errorf("config %s: %w", path, perr)
		}
		applyConfigFile(&cfg, kv)
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	applyEnv(&cfg)
	return cfg, nil
}

// applyConfigFile maps the documented flat keys onto the config.
func applyConfigFile(cfg *Config, kv map[string]string) {
	if v, ok := kv["server"]; ok {
		cfg.Server = v
	}
	if v, ok := kv["api_key"]; ok {
		cfg.APIKey = v
	}
	if v, ok := kv["daemon_socket"]; ok {
		cfg.DaemonSocket = v
	}
	if v, ok := kv["user_id"]; ok {
		cfg.UserID = v
	}
	if v, ok := kv["project"]; ok {
		cfg.Project = v
	}
}

// applyEnv overlays environment overrides (env beats config file).
func applyEnv(cfg *Config) {
	if v := os.Getenv("PF_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv("PF_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("PF_DAEMON_SOCKET"); v != "" {
		cfg.DaemonSocket = v
	}
	if v := os.Getenv("PF_USER_ID"); v != "" {
		cfg.UserID = v
	}
	if v := os.Getenv("PF_PROJECT"); v != "" {
		cfg.Project = v
	}
}

// parseFlatTOML parses the flat subset of TOML this CLI documents for
// cli.toml: `key = "value"` (or single-quoted literal strings) with `#`
// comments and blank lines. Tables, arrays and non-string values are out
// of scope by ruling — the file carries exactly five string keys — and
// are rejected rather than guessed at. NARROWING, deliberate: escape
// sequences are NOT supported — a double-quoted value containing a
// backslash is rejected below — and values are taken literally between
// their quotes. A `#` inside a quoted value is data, not a comment.
func parseFlatTOML(text string) (map[string]string, error) {
	out := map[string]string{}
	for ln, line := range strings.Split(text, "\n") {
		// Strip comments (TOML: # starts a comment outside a string).
		line = stripTOMLComment(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected `key = \"value\"`, got %q", ln+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", ln+1)
		}
		if len(val) < 2 {
			return nil, fmt.Errorf("line %d: value must be a quoted string", ln+1)
		}
		quote := val[0]
		if (quote != '"' && quote != '\'') || val[len(val)-1] != quote {
			return nil, fmt.Errorf("line %d: value must be a quoted string, got %q", ln+1, val)
		}
		inner := val[1 : len(val)-1]
		if quote == '"' && strings.ContainsAny(inner, "\\\n") {
			return nil, fmt.Errorf("line %d: escapes and multi-line strings are not supported", ln+1)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", ln+1, key)
		}
		out[key] = inner
	}
	return out, nil
}

// stripTOMLComment cuts a line at the first `#` that is OUTSIDE any
// quoted string (a `#` inside quotes is data — an api key may contain
// one). Double-quoted strings skip the character after a backslash so an
// escaped quote cannot end the scan early; the value itself is still
// rejected later when it contains an escape (the documented narrowing).
// Single-quoted literal strings have no escapes in TOML, so every `#`
// after an opening `'` is data.
func stripTOMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			switch {
			case quote == '"' && c == '\\':
				i++ // skip the escaped character
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '#':
			return line[:i]
		}
	}
	return line
}
