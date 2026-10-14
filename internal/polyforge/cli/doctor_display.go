package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/silgrid/polyforge/internal/version"
)

// doctor_display.go: the harness progress-display checks of `pf doctor`
// (polyforge#142). The watcher produces progress whether or not anyone
// sees it; these checks tell whether the user's harness is wired to show
// it — Claude Code through its statusLine, pi through the pf-chat
// extension of the polyforge pi package — and, when it is not, print a
// copyable snippet. tools/install.sh prints the same verdicts through
// `pf doctor --harness`.
//
// The judgement is static: the statusLine command and the absolute
// files it names are read, never executed (a user's command may have
// side effects or hang), and no user file is ever written. Verdicts:
// pass = will show; warn = will not show, or cannot tell (the detail
// says which); skip = the harness is not set up here. Never fail — a
// display gap does not change the doctor exit code.

const (
	docChClaudeDisplay = "claude progress display"
	docChPiDisplay     = "pi progress display"
)

// statuslineCommand is the progress face a harness status line runs.
const statuslineCommand = "pf watch --statusline --harness-session -"

// pfStatuslineRe matches a `pf watch ... --statusline` invocation on
// one line (bare `pf` or a path ending in /pf).
var pfStatuslineRe = regexp.MustCompile(`(^|[\s/;|&(])pf\s+watch\b.*--statusline`)

// polyforgePkgRe matches a pi package source of the polyforge repo.
var polyforgePkgRe = regexp.MustCompile(`github\.com[:/]silgrid/polyforge(\.git)?(@|$)`)

// maxScriptBytes bounds a statusLine script read (a binary named on the
// command line is not a script).
const maxScriptBytes = 1 << 20

// harnessDisplayChecks answers both harness checks for this user.
func harnessDisplayChecks() []doctorCheck {
	home, _ := os.UserHomeDir()
	piDir := os.Getenv("PI_CODING_AGENT_DIR")
	if piDir == "" {
		piDir = filepath.Join(home, ".pi", "agent")
	}
	return []doctorCheck{claudeDisplayCheck(home), piDisplayCheck(piDir)}
}

// runsPfStatusline reports whether shell text invokes the progress face
// (comment lines ignored).
func runsPfStatusline(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") && pfStatuslineRe.MatchString(l) {
			return true
		}
	}
	return false
}

// statusLineJSON renders a copyable `"statusLine": {...}` fragment.
func statusLineJSON(sl map[string]any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(sl)
	return `"statusLine": ` + strings.TrimSpace(b.String())
}

// claudeDisplayCheck judges whether Claude Code's statusLine shows the
// progress rows: directly, or through a script its command names.
func claudeDisplayCheck(home string) doctorCheck {
	c := doctorCheck{Name: docChClaudeDisplay}
	dir := filepath.Join(home, ".claude")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		c.Status, c.Detail = "skip", "no "+dir+": Claude Code is not set up here"
		return c
	}
	path := filepath.Join(dir, "settings.json")
	full := statusLineJSON(map[string]any{"type": "command", "command": statuslineCommand})
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Detail = "warn", "will not show: "+path+" does not exist — create it with {"+full+"}"
		return c
	}
	if err != nil {
		c.Status, c.Detail = "warn", "cannot tell: "+err.Error()
		return c
	}
	var cfg struct {
		StatusLine map[string]any `json:"statusLine"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		c.Status, c.Detail = "warn", "cannot tell: "+path+" does not parse as a settings object: "+err.Error()
		return c
	}
	cmd, _ := cfg.StatusLine["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		c.Status, c.Detail = "warn", "will not show: no statusLine command in "+path+" — add "+full
		return c
	}
	if runsPfStatusline(cmd) {
		c.Status, c.Detail = "pass", "the statusLine command runs pf watch --statusline"
		return c
	}
	for _, tok := range strings.Fields(cmd) {
		f := strings.Trim(tok, `"'`)
		if strings.HasPrefix(f, "~/") {
			f = filepath.Join(home, f[2:])
		}
		f = strings.NewReplacer("${HOME}", home, "$HOME", home).Replace(f)
		if !filepath.IsAbs(f) {
			continue
		}
		st, err := os.Stat(f)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxScriptBytes {
			continue
		}
		if body, err := os.ReadFile(f); err == nil && runsPfStatusline(string(body)) {
			c.Status, c.Detail = "pass", "the statusLine script "+f+" runs pf watch --statusline"
			return c
		}
	}
	// Append, never replace: the wrapper feeds the session JSON to the
	// user's own command first (its output kept, ended by one newline),
	// then to the progress face, whose rows follow below.
	sl := map[string]any{}
	for k, v := range cfg.StatusLine {
		sl[k] = v
	}
	sl["command"] = `input=$(cat); printf '%s\n' "$(printf '%s' "$input" | { ` +
		strings.TrimRight(strings.TrimSpace(cmd), ";") + `; })"; printf '%s' "$input" | ` +
		statuslineCommand + ` 2>/dev/null`
	c.Status, c.Detail = "warn", "will not show: the statusLine command in "+path+
		" does not run pf watch --statusline — to keep its output and append the progress rows, set "+statusLineJSON(sl)
	return c
}

// piDisplayCheck judges whether pi loads the pf-chat extension: the
// polyforge pi package (which carries it) or the extension file itself
// is listed in pi's settings.
func piDisplayCheck(agentDir string) doctorCheck {
	c := doctorCheck{Name: docChPiDisplay}
	if st, err := os.Stat(agentDir); err != nil || !st.IsDir() {
		c.Status, c.Detail = "skip", "no "+agentDir+": pi is not set up here"
		return c
	}
	src := "git:git@github.com:silgrid/polyforge.git"
	if version.Version != "dev" {
		src += "@" + version.Version
	}
	hint := "will not show: the polyforge pi package (pf-chat extension) is not installed — run: pi install " + src
	path := filepath.Join(agentDir, "settings.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Detail = "warn", hint
		return c
	}
	if err != nil {
		c.Status, c.Detail = "warn", "cannot tell: "+err.Error()
		return c
	}
	var cfg struct {
		Packages   []json.RawMessage `json:"packages"`
		Extensions []string          `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		c.Status, c.Detail = "warn", "cannot tell: "+path+" does not parse as a settings object: "+err.Error()
		return c
	}
	for _, p := range cfg.Packages {
		var s string
		if json.Unmarshal(p, &s) != nil {
			var o struct {
				Source string `json:"source"`
			}
			_ = json.Unmarshal(p, &o)
			s = o.Source
		}
		if polyforgePkgRe.MatchString(s) {
			c.Status, c.Detail = "pass", fmt.Sprintf("pi package %s carries the pf-chat extension", s)
			return c
		}
	}
	for _, e := range cfg.Extensions {
		if filepath.Base(e) == "pf-chat.ts" {
			c.Status, c.Detail = "pass", "pi extension "+e+" is listed"
			return c
		}
	}
	c.Status, c.Detail = "warn", hint
	return c
}
