package daemon

// harnesses.go: the daemon's view of the step harness binaries
// (polyforge#159), reported on GET /status so `pf doctor` — which runs
// CLI-side and cannot see the daemon's environment (a systemd unit, say)
// — judges what the daemon will actually run. Only facts are reported:
// whether the binary resolves, its version (claude --version; cached per
// binary path + mtime), and whether the credential variables are SET.
// No value is ever reported, no login is attempted, no model is called.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// verifiedClaudeVersions are the Claude Code versions the adapter's
// argv/stream contract was verified against. Another version is a doctor
// warning, never a dispatch refusal (the CLI auto-updates).
var verifiedClaudeVersions = map[string]bool{"2.1.296": true}

// harnessStatus is one harness's /status entry.
type harnessStatus struct {
	Bin      string `json:"bin"`
	Found    bool   `json:"found"`
	Path     string `json:"path,omitempty"`
	Version  string `json:"version,omitempty"`
	Verified bool   `json:"verified,omitempty"`
	// VersionError is the --version probe's failure, when it failed.
	VersionError string `json:"version_error,omitempty"`
	// set|unset only — never a value. polyforge#171: the toml line is
	// gone (the credential key was deleted from the config; authority
	// moves to the claude-config settings.json env block, reported by
	// the doctor reshape below this closeout).
	AuthTokenEnv    string `json:"auth_token_env,omitempty"`
	APIKeyEnv       string `json:"api_key_env,omitempty"`
	BaseURLEnv      string `json:"base_url_env,omitempty"`
	BackgroundTasks string `json:"background_tasks,omitempty"`
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
	if p, err := exec.LookPath(cl.Bin); err == nil {
		cl.Found, cl.Path = true, p
		cl.Version, cl.VersionError = d.claudeVersion(p)
		cl.Verified = verifiedClaudeVersions[cl.Version]
	}
	return map[string]harnessStatus{"pi": pi, "claude": cl}
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
