package daemon

// harnesses.go: the daemon's view of the step harness binaries
// (polyforge#159), reported on GET /status so `pf doctor` — which runs
// CLI-side and cannot see the daemon's environment (a systemd unit, say)
// — judges what the daemon will actually run. Only facts are reported:
// whether the binary resolves, its version (claude --version; cached per
// binary path + mtime), and whether the credential variables are SET.
//
// polyforge#171 (ruling 8): the claude entry additionally reports the
// harness-root layout and the single credential authority layer —
//   - the three harness dirs' existence (step-agent, step-agent-sessions,
//     claude-config under the daemon's harness root);
//   - the claude-config/settings.json env block's key facts
//     (ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL,
//     CLAUDE_CONFIG_DIR) as
//     set/unset — values are read in-memory for the pairing heuristic
//     and NEVER reported;
//   - its heuristic warnings: an OAuth setup-token (sk-ant-oat01-)
//     paired with a gateway BASE_URL is a rejected combination, and a
//     CLAUDE_CONFIG_DIR set in the settings env block would override the
//     adapter's hard-set value.
//
// No value is ever reported, no login is attempted, no model is called.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// minClaudeVersion is the oldest Claude Code version the adapter's
// argv/stream contract was verified against. The CLI auto-updates, so a
// newer version passes; an older or unparsable one is a doctor warning,
// never a dispatch refusal (decision 2026-10-16 G15).
const minClaudeVersion = "2.1.296"

// claudeVersionAtLeast reports whether v (a dotted numeric version, e.g.
// "2.1.296") is >= min. An unparsable v is false.
func claudeVersionAtLeast(v, min string) bool {
	a, okA := parseDotted(v)
	b, okB := parseDotted(min)
	if !okA || !okB {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}

// parseDotted parses "1.2.3" into its numeric parts; anything else fails.
func parseDotted(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// harnessDirStatus is one harness dir's existence fact (dir → path +
// exists) of the claude status entry (polyforge#171, ruling 8).
type harnessDirStatus struct {
	Path   string `json:"path,omitempty"`
	Exists bool   `json:"exists"`
}

// settingsEnvKeys are the env block keys the doctor reports facts about.
var settingsEnvKeys = []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR"}

// settingsTokenPrefix is the OAuth setup-token prefix Claude Code mints
// (`claude setup-token`, scoped to the subscription, ~365d live).
const settingsTokenPrefix = "sk-ant-oat01-"

// harnessStatus is one harness's /status entry.
type harnessStatus struct {
	Bin     string `json:"bin"`
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	// Verified: Version parses and is >= MinVersion.
	Verified   bool   `json:"verified,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
	// VersionError is the --version probe's failure, when it failed.
	VersionError string `json:"version_error,omitempty"`
	// set|unset only — never a value.
	AuthTokenEnv    string `json:"auth_token_env,omitempty"`
	APIKeyEnv       string `json:"api_key_env,omitempty"`
	BaseURLEnv      string `json:"base_url_env,omitempty"`
	BackgroundTasks string `json:"background_tasks,omitempty"`

	// polyforge#171 claude entry:
	// SettingsEnv is the claude-config settings.json env block's key
	// facts (key → set|unset; values read in-memory only, never reported).
	SettingsEnv map[string]string `json:"settings_env,omitempty"`
	// HarnessDirs is the claude status entry's layout facts: the three
	// harness dirs under the daemon's harness root (polyforge#171, rule 3).
	HarnessDirs map[string]harnessDirStatus `json:"harness_dirs,omitempty"`
	// SettingsWarning is the settings.json pairing heuristic result (the
	// oat01+gateway-URL rejection and the CLAUDE_CONFIG_DIR override),
	// pre-computed daemon-side (the CLI never sees a value).
	SettingsWarning string `json:"settings_warning,omitempty"`
}

// versionCache memoizes `claude --version` per binary path + mtime.
type versionCache struct {
	mu    sync.Mutex
	key   string
	ver   string
	error string
}

func setOrUnset(v string) string {
	if v != "" {
		return "set"
	}
	return "unset"
}

// harnessStatuses answers the /status harnesses section.
func (d *Daemon) harnessStatuses() map[string]harnessStatus {
	pi := harnessStatus{Bin: d.cfg.PiBin}
	if pi.Bin == "" {
		pi.Bin = "pi"
	}
	if p, err := exec.LookPath(pi.Bin); err == nil {
		pi.Found, pi.Path = true, p
	}
	cl := harnessStatus{Bin: d.cfg.ClaudeBin}
	if cl.Bin == "" {
		cl.Bin = "claude"
	}
	cl.AuthTokenEnv = setOrUnset(os.Getenv("ANTHROPIC_AUTH_TOKEN"))
	cl.APIKeyEnv = setOrUnset(os.Getenv("ANTHROPIC_API_KEY"))
	cl.BaseURLEnv = setOrUnset(os.Getenv("ANTHROPIC_BASE_URL"))
	cl.BackgroundTasks = "disabled by the adapter (--bare, CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1, --tools allowlist)"
	cl.SettingsEnv, cl.HarnessDirs, cl.SettingsWarning = d.claudeConfigFacts()
	if p, err := exec.LookPath(cl.Bin); err == nil {
		cl.Found, cl.Path = true, p
		cl.Version, cl.VersionError = d.claudeVersion(p)
		cl.MinVersion = minClaudeVersion
		cl.Verified = claudeVersionAtLeast(cl.Version, minClaudeVersion)
	}
	return map[string]harnessStatus{"pi": pi, "claude": cl}
}

// claudeConfigFacts reads the claude-config settings.json in-memory. It
// never carries a value. Absent: all-unset facts, no warning (nothing
// configured is exactly the doctor's "carries nothing" verdict, and the
// in-repo fake-cfg tests below pin the un-configured default shape).
func (d *Daemon) claudeConfigFacts() (map[string]string, map[string]harnessDirStatus, string) {
	root := d.cfg.HarnessRoot
	if root == "" {
		root = d.cfg.DataDir
	}
	ccDir := filepath.Join(root, "claude-config")
	dirs := map[string]harnessDirStatus{
		"step_agent":          {Path: filepath.Join(root, "step-agent")},
		"step_agent_sessions": {Path: filepath.Join(root, "step-agent-sessions")},
		"claude_config":       {Path: ccDir},
	}
	for k, s := range dirs {
		st, err := os.Stat(s.Path)
		dirs[k] = harnessDirStatus{Path: s.Path, Exists: err == nil && st.IsDir()}
	}
	raw, err := os.ReadFile(filepath.Join(ccDir, "settings.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, dirs, ""
		}
		return nil, dirs, "the claude-config settings.json is unreadable (" + err.Error() + ")"
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Env == nil {
		return nil, dirs, "the claude-config settings.json does not parse into an `env` block — whether the steps would run on credentials is unknowable from here"
	}
	facts := map[string]string{}
	for _, k := range settingsEnvKeys {
		facts[k] = setOrUnset(doc.Env[k])
	}
	var warns []string
	token, baseUrl := doc.Env["ANTHROPIC_AUTH_TOKEN"], doc.Env["ANTHROPIC_BASE_URL"]
	if strings.HasPrefix(token, settingsTokenPrefix) && baseUrl != "" {
		warns = append(warns, "OAuth token (sk-ant-oat01-, a `claude setup-token` mint) paired with a gateway BASE_URL — that combination is rejected by Claude Code: use either the official API with no BASE_URL, or a gateway token; this is the doctor's pairing clause")
	}
	if doc.Env["CLAUDE_CONFIG_DIR"] != "" {
		warns = append(warns, "the claude-config settings.json env block sets CLAUDE_CONFIG_DIR: remove it — the adapter hard-sets the step's config dir and the settings value overrides the adapter's")
	}
	return facts, dirs, strings.Join(warns, "; ")
}

// modelsEnvRefRe matches pi's models.json environment interpolation:
// `$NAME` or `${NAME}`.
var modelsEnvRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// piModelsEnvRefs (env hygiene) answers the variable NAMES the step agent
// dir's models.json references through `$NAME` / `${NAME}` interpolation
// in an apiKey or header value — pi resolves them from the step's
// environment, which an ensure-started daemon limits to the allow-list.
// Sorted, deduplicated; never a value (a literal key or a `!command`
// value contributes nothing). Absent or unparsable file: none.
func (d *Daemon) piModelsEnvRefs() []string {
	root := d.cfg.HarnessRoot
	if root == "" {
		root = d.cfg.DataDir
	}
	raw, err := os.ReadFile(filepath.Join(root, "step-agent", "models.json"))
	if err != nil {
		return nil
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(v any, key string, inHeaders bool)
	walk = func(v any, key string, inHeaders bool) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				walk(c, k, inHeaders || k == "headers")
			}
		case []any:
			for _, c := range t {
				walk(c, key, inHeaders)
			}
		case string:
			if (key != "apiKey" && !inHeaders) || strings.HasPrefix(t, "!") {
				return
			}
			for _, m := range modelsEnvRefRe.FindAllStringSubmatch(t, -1) {
				seen[m[1]+m[2]] = true
			}
		}
	}
	walk(doc, "", false)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// claudeVersion runs `<path> --version` (bounded), cached per path+mtime.
func (d *Daemon) claudeVersion(path string) (string, string) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err.Error()
	}
	key := path + "@" + st.ModTime().UTC().Format(time.RFC3339Nano)
	c := &d.claudeVer
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key {
		return c.ver, c.error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	c.key, c.ver, c.error = key, "", ""
	if err != nil {
		c.error = "claude --version: " + err.Error()
		return c.ver, c.error
	}
	if f := strings.Fields(string(out)); len(f) > 0 {
		c.ver = f[0]
	}
	return c.ver, c.error
}
