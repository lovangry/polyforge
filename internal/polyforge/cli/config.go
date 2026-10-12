// Package cli implements the `pf` client CLI of Polyforge v1.
//
// Routing split (13 user verbs + the M1–M5 management face):
//
//	Direct to aihub HTTP (internal/common/api types, aihubclient client):
//	  file, queue, cancel, recall — plus the management face M1–M5
//	  (users, api-keys, projects, backup, rotate-secret).
//
//	Local daemon UDS HTTP face (04 §5.1 minus publish_request — the
//	client surface is pinned in internal/common/protocol and daemon.go
//	below):
//	  submit, observe, answer, pause, resume, wrap,
//	  ops, ship.
//
// Output: human-readable tables by default; --json emits the server's
// JSON response as-is. Errors are decoded from the typed errStatus
// envelope (common/api.Error: {code, detail, extra}) and rendered with
// the code prominent; exit codes follow the wire status class:
// 2xx → 0, 4xx → 1, 5xx → 2 (usage errors also 2).
//
// Configuration precedence: command-line flags > PF_* environment >
// ~/.config/polyforge/cli.toml (flat string keys + [workspace."<path>"]
// sections) > defaults. There is NO default project (polyforge#113): the
// [workspace] mapping decides capsule placement, and project scoping is
// per-invocation --project.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/polyforge/mapping"
)

// Default aihub listen address.
const defaultServer = "http://localhost:8420"

// Default daemon unix domain socket (04 §5.1: local machine only, no
// network face).
const defaultDaemonSocket = "/run/polyforge.sock"

// Config holds the resolved CLI configuration.
type Config struct {
	Server       string        // aihub base URL
	APIKey       string        // aihub api key (Bearer)
	DaemonSocket string        // daemon unix socket path
	UserID       string        // caller identity for answer.answered_by
	ChatID       string        // PF_CHAT_ID: the harness chat this process runs in (label, not a credential)
	Mapping      *mapping.Plan // the workspace→projects plan of record (non-nil; possibly empty)
}

// DefaultConfigPath returns the single documented config location:
// $XDG_CONFIG_HOME|~/.config / polyforge / cli.toml. One location, no
// secondary lookup — the legacy ~/.polyforge/config.toml fallback is
// dropped (polyforge#113): a file there is simply not read.
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
// defaults apply, with an EMPTY mapping) and applies environment
// overrides on top. An empty path means the documented default location,
// not "no file".
//
//	PF_SERVER         -> Server
//	PF_API_KEY        -> APIKey
//	PF_DAEMON_SOCKET  -> DaemonSocket
//	PF_USER_ID        -> UserID
//	PF_CHAT_ID        -> ChatID
//
// A malformed file IS an error: silent misconfiguration is worse than a
// loud failure. So is the retired old shape (a flat `project` key), a
// mapping that fails validation — all refuse with guidance, for every pf command and the daemon alike
// (the shared loader is the single choke point).
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Server:       defaultServer,
		DaemonSocket: defaultDaemonSocket,
		Mapping:      &mapping.Plan{},
	}
	if path == "" {
		path = DefaultConfigPath()
	}
	var (
		flat     map[string]string
		sections []workspaceSection
	)
	data, err := os.ReadFile(path)
	if err == nil {
		var perr error
		flat, sections, perr = parseConfigTOML(string(data))
		if perr != nil {
			return cfg, fmt.Errorf("config %s: %w", path, perr)
		}
		if _, retired := flat["project"]; retired {
			return cfg, fmt.Errorf("config %s: the `project` key is retired (polyforge#113) — there is NO default project anymore; declare your workspaces instead:\n"+
				"  [workspace.\"/srv/polyforge/workspace\"]\n"+
				"  projects = [\"p1\", \"p2\"]\n"+
				"Run `pf doctor` to inspect the mapping; the install docs print the migration steps", path)
		}
		applyConfigFile(&cfg, flat)
		entries := make([]mapping.Entry, 0, len(sections))
		for _, s := range sections {
			entries = append(entries, mapping.Entry{Workspace: s.key, Projects: s.projects})
		}
		plan, berr := mapping.Build(entries)
		if berr != nil {
			return cfg, fmt.Errorf("config %s: %w", path, berr)
		}
		cfg.Mapping = plan
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
		case "server", "url": // "url" was the installer's original spelling
			cfg.Server = v
		case "api_key":
			cfg.APIKey = v
		case "daemon_socket":
			cfg.DaemonSocket = v
		case "user_id":
			cfg.UserID = v
		}
	}
}

// applyEnv overlays environment overrides (env beats the config file).
// There is no project variable in the set: the ambient default-project
// selection is retired (polyforge#113) — the mapping in cli.toml is the
// only placement source and --project is per-invocation.
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
	if v := os.Getenv("PF_CHAT_ID"); v != "" {
		cfg.ChatID = v
	}
}

// workspaceSection is one parsed `[workspace."<key>"]` section.
type workspaceSection struct {
	key      string
	projects []string
	line     int
}

// The known flat keys: root-level wherever they appear in the file.
var coreFlatKeys = map[string]bool{"server": true, "url": true, "api_key": true, "daemon_socket": true, "user_id": true}

// parseConfigTOML parses the TOML subset documented for cli.toml:
//
//   - `key = "value"` flat string keys: the known set (server/url alias,
//     api_key, daemon_socket, user_id) is stored; the known keys are
//     root-level wherever they appear; unknown flat keys at the top
//     level are stored and ignored;
//   - one section family: `[workspace."<path>"]` (quoted key mandatory)
//     — inside, only `projects = ["a", "b"]`.
//
// Other tables, unquoted section keys, non-array or wrongly-typed
// projects values, a bare `projects` key at the top level, and duplicate
// keys/sections are line-named parse errors, never guesses. Deliberate
// narrowing: escape sequences are not supported — a double-quoted value
// containing a backslash is rejected — and values are taken literally
// between their quotes. A `#` inside a quoted value is data, not a
// comment.
func parseConfigTOML(text string) (map[string]string, []workspaceSection, error) {
	flat := map[string]string{}
	var sections []workspaceSection
	var (
		inWS  bool
		curWS = -1 // index into sections while inside a [workspace] section
	)
	for ln, line := range strings.Split(text, "\n") {
		line = stripTOMLComment(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			key, err := parseSectionHeader(line, ln+1)
			if err != nil {
				return nil, nil, err
			}
			for _, s := range sections {
				if s.key == key {
					return nil, nil, fmt.Errorf("line %d: duplicate [workspace] section for %s", ln+1, key)
				}
			}
			sections = append(sections, workspaceSection{key: key, line: ln + 1})
			curWS = len(sections) - 1
			inWS = true
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, nil, fmt.Errorf("line %d: expected `key = \"value\"`, got %q", ln+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, nil, fmt.Errorf("line %d: empty key", ln+1)
		}
		if coreFlatKeys[key] {
			// Known flat key: root-level wherever it appears.
			inner, err := parseQuotedValue(val, ln+1)
			if err != nil {
				return nil, nil, err
			}
			if _, dup := flat[key]; dup {
				return nil, nil, fmt.Errorf("line %d: duplicate key %q", ln+1, key)
			}
			flat[key] = inner
			continue
		}
		if inWS {
			// Inside a workspace section: only the projects array.
			if key != "projects" {
				return nil, nil, fmt.Errorf("line %d: a [workspace] section carries only `projects = [\"...\"]`, got %q", ln+1, key)
			}
			if len(sections[curWS].projects) > 0 {
				return nil, nil, fmt.Errorf("line %d: duplicate projects array in the [workspace] section of %s", ln+1, sections[curWS].key)
			}
			list, err := parseStringArray(val, ln+1)
			if err != nil {
				return nil, nil, err
			}
			sections[curWS].projects = append(sections[curWS].projects, list...)
			continue
		}
		if key == "projects" {
			return nil, nil, fmt.Errorf("line %d: `projects` belongs inside a [workspace.\"<path>\"] section, not the top level", ln+1)
		}
		// Unknown top-level flat keys are stored (the retired-key check in
		// LoadConfig reads them; applyConfigFile ignores the rest).
		inner, ierr := parseQuotedValue(val, ln+1)
		if ierr != nil {
			return nil, nil, ierr
		}
		if _, dup := flat[key]; dup {
			return nil, nil, fmt.Errorf("line %d: duplicate key %q", ln+1, key)
		}
		flat[key] = inner
	}
	return flat, sections, nil
}

// parseSectionHeader validates one `[...]` line: the only legal section
// is `[workspace."<key>"]` (quoted key mandatory) — the mapping plan.
// It answers the workspace key.
func parseSectionHeader(line string, ln int) (string, error) {
	if !strings.HasSuffix(line, "]") {
		return "", fmt.Errorf("line %d: malformed section header %q", ln, line)
	}
	inner := strings.TrimSpace(line[1 : len(line)-1])
	const wsPrefix = `workspace.`
	if !strings.HasPrefix(inner, wsPrefix) {
		return "", fmt.Errorf("line %d: unknown section %q — only [workspace.\"<path>\"] sections are supported", ln, line)
	}
	keyExpr := strings.TrimSpace(inner[len(wsPrefix):])
	if len(keyExpr) < 2 {
		return "", fmt.Errorf("line %d: section key must be a quoted path: [workspace.\"<path>\"]", ln)
	}
	quote := keyExpr[0]
	if (quote != '"' && quote != '\'') || keyExpr[len(keyExpr)-1] != quote {
		return "", fmt.Errorf("line %d: section key must be a QUOTED path: [workspace.\"<path>\"]", ln)
	}
	key := keyExpr[1 : len(keyExpr)-1]
	if quote == '"' && strings.ContainsAny(key, "\\\n") {
		return "", fmt.Errorf("line %d: escapes are not supported in section keys", ln)
	}
	if key == "" {
		return "", fmt.Errorf("line %d: empty workspace key", ln)
	}
	return key, nil
}

// parseStringArray parses `["a", "b"]` — an array of quoted strings
// only (the one value shape a workspace section may carry).
func parseStringArray(val string, ln int) ([]string, error) {
	if len(val) < 2 || val[0] != '[' || val[len(val)-1] != ']' {
		return nil, fmt.Errorf("line %d: projects must be an array of quoted strings: projects = [\"a\", \"b\"], got %q", ln, val)
	}
	body := strings.TrimSpace(val[1 : len(val)-1])
	if body == "" {
		return []string{}, nil
	}
	var out []string
	for _, item := range strings.Split(body, ",") {
		item = strings.TrimSpace(item)
		s, err := parseQuotedValue(item, ln)
		if err != nil {
			return nil, fmt.Errorf("line %d: projects entries must be quoted strings: %w", ln, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// parseQuotedValue parses one `"value"` (or single-quoted literal).
func parseQuotedValue(val string, ln int) (string, error) {
	if len(val) < 2 {
		return "", fmt.Errorf("line %d: value must be a quoted string", ln)
	}
	quote := val[0]
	if (quote != '"' && quote != '\'') || val[len(val)-1] != quote {
		return "", fmt.Errorf("line %d: value must be a quoted string, got %q", ln, val)
	}
	inner := val[1 : len(val)-1]
	if quote == '"' && strings.ContainsAny(inner, "\\\n") {
		return "", fmt.Errorf("line %d: escapes and multi-line strings are not supported", ln)
	}
	return inner, nil
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
