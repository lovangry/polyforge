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
	"time"

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
	Mapping      *mapping.Plan // the workspace→projects plan of record (non-nil; possibly empty)

	// The watch lane's config (wi_LMSxPTQTtz5x). The summarize model is
	// EXPLICIT CONFIG ONLY ([watch.summarize_model.<harness>] sections):
	// no default, no env fallback — there is deliberately no PF_WATCH
	// spelling for it. The cadence keys resolve flag > env > config with
	// env as fill-in only (PF_WATCH_* applies when the cli.toml key is
	// absent); the defaults (3s / 1m / off) are applied by
	// ResolveWatchCadence, below. HarnessSessionsRoot is the optional
	// sessions-root override the watch module's transcript location
	// resolution consumes.
	HarnessSessionsRoot string
	WatchSummarize      map[string]string // harness → model
	WatchPoll           time.Duration     // 0 = unset (default applies)
	WatchLog            time.Duration     // 0 = unset (default applies)
	WatchHeartbeat      time.Duration     // 0 = unset or the literal off
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
//	PF_WATCH_POLL / PF_WATCH_LOG / PF_WATCH_HEARTBEAT -> the watch
//	  cadence keys, FILL-IN ONLY: a present cli.toml key wins
//	  (precedence flag > env > config)
//
// A malformed file IS an error: silent misconfiguration is worse than a
// loud failure. So is the retired old shape (a flat `project` key), a
// mapping that fails validation, and a malformed watch cadence value —
// all refuse with guidance, for every pf command and the daemon alike
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
		models   map[string]string
	)
	data, err := os.ReadFile(path)
	if err == nil {
		var perr error
		flat, sections, models, perr = parseConfigTOML(string(data))
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
		if len(models) > 0 {
			cfg.WatchSummarize = models
		}
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	applyEnv(&cfg)
	if err := applyWatchKeys(&cfg, flat, path); err != nil {
		return cfg, err
	}
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
		case "harness_sessions_root": // the watch lane's sessions-root override
			cfg.HarnessSessionsRoot = v
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
}

// workspaceSection is one parsed `[workspace."<key>"]` section.
type workspaceSection struct {
	key      string
	projects []string
	line     int
}

// The known flat keys. `core` are the v1 keys; `watch` are the watch
// lane's flat keys (wi_LMSxPTQTtz5x). Known flat keys are ROOT-LEVEL
// wherever they appear in the file — the watch lane's own configs write
// them after the [watch.summarize_model] sections they need, and the
// keys stay unambiguous, so the parser hoists them instead of rejecting.
var (
	coreFlatKeys  = map[string]bool{"server": true, "url": true, "api_key": true, "daemon_socket": true, "user_id": true}
	watchFlatKeys = map[string]bool{"harness_sessions_root": true, "watch_poll": true, "watch_log": true, "watch_heartbeat": true}
)

// parseConfigTOML parses the TOML subset documented for cli.toml:
//
//   - `key = "value"` flat string keys: the known set (server/url alias,
//     api_key, daemon_socket, user_id, harness_sessions_root,
//     watch_poll, watch_log, watch_heartbeat) is stored; the known keys
//     are root-level wherever they appear; unknown flat keys at the top
//     level are ignored exactly as before;
//   - section headers of two families: `[workspace."<path>"]` (quoted
//     key mandatory) — inside, only `projects = ["a", "b"]`; and
//     `[watch.summarize_model.<harness>]` (wi_LMSxPTQTtz5x) — inside,
//     only `model = "<value>"`, answering the per-harness model map.
//
// Tables outside the two families, unquoted section keys, non-array or
// wrongly-typed projects values, a bare `projects` key at the top level,
// and duplicate keys/sections are line-named parse errors, never
// guesses. Deliberate narrowing: escape sequences are not supported — a
// double-quoted value containing a backslash is rejected — and values
// are taken literally between their quotes. A `#` inside a quoted value
// is data, not a comment.
func parseConfigTOML(text string) (map[string]string, []workspaceSection, map[string]string, error) {
	flat := map[string]string{}
	models := map[string]string{}
	var sections []workspaceSection
	var (
		curKind    string // "" top level | "workspace" | "watch.summarize_model"
		curWS      = -1   // index into sections when curKind == "workspace"
		curHarness string
	)
	for ln, line := range strings.Split(text, "\n") {
		line = stripTOMLComment(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			kind, key, err := parseSectionHeader(line, ln+1)
			if err != nil {
				return nil, nil, nil, err
			}
			switch kind {
			case "workspace":
				for _, s := range sections {
					if s.key == key {
						return nil, nil, nil, fmt.Errorf("line %d: duplicate [workspace] section for %s", ln+1, key)
					}
				}
				sections = append(sections, workspaceSection{key: key, line: ln + 1})
				curWS = len(sections) - 1
			case "watch.summarize_model":
				if _, dup := models[key]; dup {
					return nil, nil, nil, fmt.Errorf("line %d: duplicate [watch.summarize_model.%s] section", ln+1, key)
				}
				models[key] = ""
				curHarness = key
			}
			curKind = kind
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, nil, nil, fmt.Errorf("line %d: expected `key = \"value\"`, got %q", ln+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, nil, nil, fmt.Errorf("line %d: empty key", ln+1)
		}
		if coreFlatKeys[key] || watchFlatKeys[key] {
			// Known flat key: root-level wherever it appears.
			inner, err := parseQuotedValue(val, ln+1)
			if err != nil {
				return nil, nil, nil, err
			}
			if _, dup := flat[key]; dup {
				return nil, nil, nil, fmt.Errorf("line %d: duplicate key %q", ln+1, key)
			}
			flat[key] = inner
			continue
		}
		switch curKind {
		case "workspace":
			// Inside a workspace section: only the projects array.
			if key != "projects" {
				return nil, nil, nil, fmt.Errorf("line %d: a [workspace] section carries only `projects = [\"...\"]`, got %q", ln+1, key)
			}
			if len(sections[curWS].projects) > 0 {
				return nil, nil, nil, fmt.Errorf("line %d: duplicate projects array in the [workspace] section of %s", ln+1, sections[curWS].key)
			}
			list, err := parseStringArray(val, ln+1)
			if err != nil {
				return nil, nil, nil, err
			}
			sections[curWS].projects = append(sections[curWS].projects, list...)
		case "watch.summarize_model":
			// Inside a summarize-model section: only the model value.
			if key != "model" {
				return nil, nil, nil, fmt.Errorf("line %d: a [watch.summarize_model.%s] section carries only `model = \"<value>\"`, got %q", ln+1, curHarness, key)
			}
			if models[curHarness] != "" {
				return nil, nil, nil, fmt.Errorf("line %d: duplicate model in the [watch.summarize_model.%s] section", ln+1, curHarness)
			}
			inner, err := parseQuotedValue(val, ln+1)
			if err != nil {
				return nil, nil, nil, err
			}
			models[curHarness] = inner
		default:
			if key == "projects" {
				return nil, nil, nil, fmt.Errorf("line %d: `projects` belongs inside a [workspace.\"<path>\"] section, not the top level", ln+1)
			}
			// Unknown top-level flat keys are stored exactly as before
			// (the retired-key check in LoadConfig reads them;
			// applyConfigFile ignores the ones it does not know).
			inner, ierr := parseQuotedValue(val, ln+1)
			if ierr != nil {
				return nil, nil, nil, ierr
			}
			if _, dup := flat[key]; dup {
				return nil, nil, nil, fmt.Errorf("line %d: duplicate key %q", ln+1, key)
			}
			flat[key] = inner
		}
	}
	return flat, sections, models, nil
}

// parseSectionHeader validates one `[...]` line. Two section families
// are legal:
//
//   - `[workspace."<key>"]` (quoted key mandatory) — the mapping plan;
//   - `[watch.summarize_model.<harness>]` — the per-harness summarize
//     model (wi_LMSxPTQTtz5x); the harness is a bare word.
func parseSectionHeader(line string, ln int) (kind, key string, err error) {
	if !strings.HasSuffix(line, "]") {
		return "", "", fmt.Errorf("line %d: malformed section header %q", ln, line)
	}
	inner := strings.TrimSpace(line[1 : len(line)-1])
	const wsPrefix = `workspace.`
	if strings.HasPrefix(inner, wsPrefix) {
		keyExpr := strings.TrimSpace(inner[len(wsPrefix):])
		if len(keyExpr) < 2 {
			return "", "", fmt.Errorf("line %d: section key must be a quoted path: [workspace.\"<path>\"]", ln)
		}
		quote := keyExpr[0]
		if (quote != '"' && quote != '\'') || keyExpr[len(keyExpr)-1] != quote {
			return "", "", fmt.Errorf("line %d: section key must be a QUOTED path: [workspace.\"<path>\"]", ln)
		}
		key := keyExpr[1 : len(keyExpr)-1]
		if quote == '"' && strings.ContainsAny(key, "\\\n") {
			return "", "", fmt.Errorf("line %d: escapes are not supported in section keys", ln)
		}
		if key == "" {
			return "", "", fmt.Errorf("line %d: empty workspace key", ln)
		}
		return "workspace", key, nil
	}
	const watchPrefix = `watch.summarize_model.`
	if strings.HasPrefix(inner, watchPrefix) {
		harness := strings.TrimSpace(inner[len(watchPrefix):])
		if harness == "" {
			return "", "", fmt.Errorf("line %d: empty harness — write [watch.summarize_model.<harness>]", ln)
		}
		for _, r := range harness {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return "", "", fmt.Errorf("line %d: harness %q must be a bare word: [watch.summarize_model.<harness>]", ln, harness)
			}
		}
		return "watch.summarize_model", harness, nil
	}
	return "", "", fmt.Errorf("line %d: unknown section %q — only [workspace.\"<path>\"] and [watch.summarize_model.<harness>] sections are supported", ln, line)
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

// ---------------------------------------------------------------------------
// The watch lane's cadence keys (wi_LMSxPTQTtz5x): flag > env > config,
// env as fill-in only, defaults 3s / 1m / off, heartbeat "off" literal.
// ---------------------------------------------------------------------------

// WatchCadence is the resolved cadence triple; Heartbeat 0 = off.
type WatchCadence struct {
	Poll      time.Duration
	Log       time.Duration
	Heartbeat time.Duration
}

// The cadence defaults (spec): poll 3s, log 1m, heartbeat off.
const (
	DefaultWatchPoll      = 3 * time.Second
	DefaultWatchLog       = time.Minute
	DefaultWatchHeartbeat = time.Duration(0)
)

// WatchFlagOverrides is the flag layer of the cadence precedence (nil =
// flag absent; the flag values are parsed by the verb's cadenceFlag).
type WatchFlagOverrides struct {
	Poll, Log, Heartbeat *time.Duration
}

// ResolveWatchCadence resolves the cadence: flag > config > default,
// where the config values already carry the env fill-in (LoadConfig
// applies PF_WATCH_* only when the cli.toml key is absent). All parsing
// happened at load and at flag parse; nothing here can fail.
func (c Config) ResolveWatchCadence(o WatchFlagOverrides) WatchCadence {
	pick := func(flagV *time.Duration, cfgV, def time.Duration) time.Duration {
		switch {
		case flagV != nil:
			return *flagV
		case cfgV > 0:
			return cfgV
		default:
			return def
		}
	}
	return WatchCadence{
		Poll:      pick(o.Poll, c.WatchPoll, DefaultWatchPoll),
		Log:       pick(o.Log, c.WatchLog, DefaultWatchLog),
		Heartbeat: pick(o.Heartbeat, c.WatchHeartbeat, DefaultWatchHeartbeat),
	}
}

// applyWatchKeys resolves the watch cadence keys: the cli.toml flat
// value wins; PF_WATCH_* fills an ABSENT key only (precedence flag >
// env > config); malformed values refuse loudly naming the key. There is
// deliberately NO env spelling for the summarize model — explicit config
// only. flat is nil when no config file exists (env-only configurations
// still work).
func applyWatchKeys(cfg *Config, flat map[string]string, path string) error {
	// src answers the value and its ORIGIN (the cli.toml key, or the
	// env spelling that filled it in): a malformed value refuses naming
	// what the operator actually wrote.
	src := func(key, env string) (string, string) {
		if flat != nil {
			if v := flat[key]; v != "" {
				return v, key
			}
		}
		return os.Getenv(env), env
	}
	wrap := func(err error) error {
		if path != "" {
			return fmt.Errorf("config %s: %w", path, err)
		}
		return fmt.Errorf("config: %w", err)
	}
	raw, origin := src("watch_poll", "PF_WATCH_POLL")
	poll, err := parseCadenceRaw(origin, raw, false)
	if err != nil {
		return wrap(err)
	}
	raw, origin = src("watch_log", "PF_WATCH_LOG")
	log, err := parseCadenceRaw(origin, raw, false)
	if err != nil {
		return wrap(err)
	}
	raw, origin = src("watch_heartbeat", "PF_WATCH_HEARTBEAT")
	hb, err := parseCadenceRaw(origin, raw, true)
	if err != nil {
		return wrap(err)
	}
	cfg.WatchPoll, cfg.WatchLog, cfg.WatchHeartbeat = poll, log, hb
	return nil
}

// parseCadenceRaw parses one cadence value: "" = absent (0; the
// resolver's default applies). The heartbeat additionally accepts the
// literal `off` (spec decision 3: the same encoding in cli.toml and
// env). Anything else must be a Go duration; a negative value is
// nonsense and refuses.
func parseCadenceRaw(key, raw string, allowOff bool) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	if allowOff && raw == "off" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s = %q is not a duration%s: %v", key, raw, offHint(allowOff), err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s = %q must not be negative", key, raw)
	}
	return d, nil
}

// offHint is the heartbeat's "or off" hint in parse errors.
func offHint(allowOff bool) string {
	if allowOff {
		return ` (or the literal "off")`
	}
	return ""
}
