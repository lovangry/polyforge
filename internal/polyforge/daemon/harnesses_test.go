package daemon

// harnesses_test.go: the /status harness view (polyforge#159) plus the
// polyforge#171 claude facts — the claude-config settings.json env block
// read in-memory as set/unset ONLY (never a value — the credential must
// not survive the JSON), the three harness dirs' existence, the pairing
// heuristics' warnings, and the JSON never carrying a credential value.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarnessStatusesNeverCarryValues: the /status harness view reports
// the claude binary, its version (cached per path+mtime) and set/unset
// only — never a credential value.
func TestHarnessStatusesNeverCarryValues(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	count := filepath.Join(dir, "count")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x >> "+count+"\necho '2.1.296 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	os.Unsetenv("ANTHROPIC_API_KEY")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-SENTINEL")
	d := &Daemon{cfg: Config{ClaudeBin: bin, PiBin: "/nonexistent/pi"}}
	hs := d.harnessStatuses()
	cl := hs["claude"]
	if !cl.Found || cl.Version != "2.1.296" || !cl.Verified || cl.AuthTokenEnv != "set" || cl.APIKeyEnv != "unset" {
		t.Fatalf("claude status %+v", cl)
	}
	if hs["pi"].Found {
		t.Fatal("a missing pi binary reported found")
	}
	if len(cl.SettingsEnv) != 0 {
		t.Fatalf("no settings.json: the entry should be shapeless: settings=%v", cl.SettingsEnv)
	}
	blob, _ := json.Marshal(hs)
	if strings.Contains(string(blob), "SENTINEL") {
		t.Fatalf("status carries a credential value: %s", blob)
	}
	d.harnessStatuses() // cached: --version runs once per binary path+mtime
	if raw, _ := os.ReadFile(count); strings.Count(string(raw), "x") != 1 {
		t.Fatalf("--version ran %d times", strings.Count(string(raw), "x"))
	}
	d2 := &Daemon{cfg: Config{ClaudeBin: "/nonexistent/claude"}}
	if d2.harnessStatuses()["claude"].Found {
		t.Fatal("a missing claude binary reported found")
	}
}

// factsProbe runs claudeConfigFacts with one harness root.
func factsProbe(harnessRoot, dataDir string) (map[string]string, map[string]harnessDirStatus, string) {
	d := &Daemon{cfg: Config{HarnessRoot: harnessRoot, DataDir: dataDir}}
	return d.claudeConfigFacts()
}

// TestClaudeConfigFacts: the claude-config settings.json is the doctor's
// credential authority view (polyforge#171, ruling 8) — env block facts
// as set/unset only, the three harness dirs' existence, the pairing
// heuristics (oat01+gateway URL rejected; CLAUDE_CONFIG_DIR override),
// and never a value.
func TestClaudeConfigFacts(t *testing.T) {
	root := t.TempDir()
	ccDir := filepath.Join(root, "claude-config")
	if err := os.MkdirAll(ccDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(ccDir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// the dir-path fall-through: an empty harness root uses the data dir
	_, dirs, _ := factsProbe("", root)
	if want := filepath.Join(root, "claude-config"); dirs["claude_config"].Path != want {
		t.Fatalf("fall-through dir path %q, want %q", dirs["claude_config"].Path, want)
	}

	// absent settings.json: all-unset shape, no warning
	facts, dirs, warn := factsProbe(root, "")
	if facts != nil || warn != "" {
		t.Fatalf("absent settings.json: facts=%v warn=%q", facts, warn)
	}
	for _, name := range []string{"step_agent", "step_agent_sessions"} {
		if dirs[name].Exists {
			t.Fatalf("%s exists, want missing", name)
		}
	}
	if want := filepath.Join(root, "claude-config"); dirs["claude_config"].Path != want {
		t.Fatalf("dir path %q, want %q", dirs["claude_config"].Path, want)
	}
	if dirs["claude_config"].Path == dirs["step_agent"].Path || dirs["claude_config"].Path == dirs["step_agent_sessions"].Path ||
		dirs["step_agent"].Path == dirs["step_agent_sessions"].Path {
		t.Fatalf("the three dirs are not siblings: %v", dirs)
	}

	// the gateway pair: facts set, warning raised, no value in any fact
	const token = "sk-ant-oat01-SENTINEL-171"
	write(fmt.Sprintf(`{"env":{"ANTHROPIC_AUTH_TOKEN":%q,"ANTHROPIC_BASE_URL":"https://gw.example"}}`, token))
	facts, _, warn = factsProbe(root, "")
	if facts["ANTHROPIC_AUTH_TOKEN"] != "set" || facts["ANTHROPIC_BASE_URL"] != "set" || facts["CLAUDE_CONFIG_DIR"] != "unset" {
		t.Fatalf("gateway facts %v", facts)
	}
	if warn == "" || !strings.Contains(warn, settingsTokenPrefix) || !strings.Contains(warn, "rejected") {
		t.Fatalf("gateway warning %q", warn)
	}
	if strings.Contains(warn, "SENTINEL") || strings.Contains(warn, "eyJ") {
		t.Errorf("the warning carries the token value")
	}
	blob, _ := json.Marshal(facts)
	if strings.Contains(string(blob), token) {
		t.Errorf("the facts JSON carries the value")
	}

	// the official pair (an oat token, no BASE_URL): no pairing warning
	write(fmt.Sprintf(`{"env":{"ANTHROPIC_AUTH_TOKEN":%q}}`, token))
	if _, _, warn := factsProbe(root, ""); warn != "" {
		t.Fatalf("official pair warns %q", warn)
	}

	// a CLAUDE_CONFIG_DIR override
	write(`{"env":{"ANTHROPIC_AUTH_TOKEN":"tok-SENTINEL","CLAUDE_CONFIG_DIR":"/cfg"}}`)
	_, _, warn = factsProbe(root, "")
	if warn == "" || !strings.Contains(warn, "CLAUDE_CONFIG_DIR") || !strings.Contains(warn, "overrides") {
		t.Fatalf("override warning %q", warn)
	}

	// both heuristics at once: both warnings, joined
	write(fmt.Sprintf(`{"env":{"ANTHROPIC_AUTH_TOKEN":%q,"ANTHROPIC_BASE_URL":"https://gw","CLAUDE_CONFIG_DIR":"/cfg"}}`, token))
	_, _, warn = factsProbe(root, "")
	for _, frag := range []string{"rejected", "CLAUDE_CONFIG_DIR", "overrides"} {
		if !strings.Contains(warn, frag) {
			t.Fatalf("double warning %q misses %q", warn, frag)
		}
	}

	// torn JSON: no facts, a decode warning (never a value)
	write("{not json")
	facts, _, warn = factsProbe(root, "")
	if len(facts) != 0 || warn == "" || !strings.Contains(warn, "does not parse") {
		t.Fatalf("torn facts %v %q", facts, warn)
	}

	// a set credential still never lands in any rendered fact
	write(`{"env":{"ANTHROPIC_AUTH_TOKEN":"eyJ-SENTINEL-NOT-oat01"}}`)
	facts, _, warn = factsProbe(root, "")
	if facts["ANTHROPIC_AUTH_TOKEN"] != "set" {
		t.Fatalf("plain token facts %v", facts)
	}
	if warn != "" {
		t.Fatalf("plain token warns %q", warn)
	}

	// the dirs' existence flips when they are created (daemon-held layout)
	if err := os.MkdirAll(filepath.Join(root, "step-agent"), 0o755); err == nil {
		if _, dirs, _ := factsProbe(root, ""); !dirs["step_agent"].Exists {
			t.Fatal("the created step-agent dir is reported missing")
		}
	} else {
		t.Errorf("mkdir step-agent failed: %v", err)
	}
	_ = dirs
}
