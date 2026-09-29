// Package cli implements the `pf` client CLI of Polyforge v1.
//
// Routing split (13 user verbs + the M1–M5 management face):
//
//	Direct to aihub HTTP (internal/common/api types, aihubclient client):
//	  file, queue, cancel, recall — plus the management face M1–M5
//	  (users, api-keys, projects, backup, rotate-secret).
//
//	Local daemon UDS HTTP face (04 §5.1 minus publish_request; flush
//	replaces it, ruling C2 — the client surface is pinned in
//	internal/common/protocol and daemon.go below):
//	  submit, observe, answer, batch-confirm, pause, resume, wrap,
//	  ops, ship.
//
// Output: human-readable tables by default; --json emits the server's
// JSON response as-is. Errors are decoded from the typed errStatus
// envelope (common/api.Error: {code, detail, extra}) and rendered with
// the code prominent; exit codes follow the wire status class:
// 2xx → 0, 4xx → 1, 5xx → 2 (usage errors also 2).
//
// Configuration precedence: command-line flags > PF_* environment >
// ~/.config/polyforge/cli.toml (flat string-keyed TOML subset) > defaults.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default aihub listen address.
const defaultServer = "http://localhost:8420"

// Default daemon unix domain socket (04 §5.1: local machine only, no
// network face).
const defaultDaemonSocket = "/run/polyforge.sock"

// Config holds the resolved CLI configuration.
type Config struct {
	Server       string // aihub base URL
	APIKey       string // aihub api key (Bearer)
	DaemonSocket string // daemon unix socket path
	UserID       string // caller identity for answer.answered_by
	Project      string // optional default project
}

// DefaultConfigPath returns the single documented config location:
// $XDG_CONFIG_HOME|~/.config / polyforge / cli.toml. One location, no
// secondary lookup.
func DefaultConfigPath() string {
	base, err := os.UserConfigDir()
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
// defaults apply) and applies environment overrides on top. An empty
// path means the documented default location, not "no file".
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
	for k, v := range kv {
		switch k {
		case "server":
			cfg.Server = v
		case "api_key":
			cfg.APIKey = v
		case "daemon_socket":
			cfg.DaemonSocket = v
		case "user_id":
			cfg.UserID = v
		case "project":
			cfg.Project = v
		}
	}
}

// applyEnv overlays environment overrides (env beats the config file).
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

// parseFlatTOML parses the flat TOML subset documented for cli.toml:
// `key = "value"` (or single-quoted literals) with `#` comments and
// blank lines. Tables, arrays and non-string values are rejected rather
// than guessed at. Deliberate narrowing: escape sequences are not
// supported — a double-quoted value containing a backslash is rejected —
// and values are taken literally between their quotes. A `#` inside a
// quoted value is data, not a comment.
func parseFlatTOML(text string) (map[string]string, error) {
	out := map[string]string{}
	for ln, line := range strings.Split(text, "\n") {
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

// stripTOMLComment cuts a line at the first `#` OUTSIDE any quoted string.
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
