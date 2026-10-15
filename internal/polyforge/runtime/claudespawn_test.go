package runtime

// claudespawn_test.go: the polyforge#159 Claude Code step spawner — the
// argv/env golden (clean context flags, the tool allowlist, background
// off, the token only ever in the environment) and the stdin contract
// (the prompt is written, then stdin is closed). No real `claude` runs:
// the binary is a shell stand-in.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/harness"
)

const testSession = "0b9f6a52-3c1d-4e8f-9a7b-1c2d3e4f5a6b"

func TestClaudeBuildCmdArgvEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	sp := &claudeSpawner{cfg: &Config{ClaudeBin: "/opt/claude", MachineID: "m-1", AnthropicAuthToken: "tok-SENTINEL-159"}}
	cmd, err := sp.buildCmd(SpawnParams{WorkItemID: "wi_1", CapsuleDir: "/caps", Model: "claude-opus-5-5", HarnessSession: testSession})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/claude", "-p", "--bare",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--session-id", testSession,
		"--model", "claude-opus-5-5",
		"--tools", ClaudeStepTools, "--allowedTools", ClaudeStepTools,
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--strict-mcp-config", "--disable-slash-commands"}
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv\n got %q\nwant %q", cmd.Args, want)
	}
	if cmd.Dir != "/caps" {
		t.Fatalf("cwd %q", cmd.Dir)
	}
	argv := strings.Join(cmd.Args, " ")
	for _, banned := range []string{"tok-SENTINEL-159", "--setting-sources", "--fallback-model", "--mcp-config", "--dangerously", "bypassPermissions", "--agents", "--system-prompt", "--add-dir", "--plugin-dir"} {
		if strings.Contains(argv, banned) {
			t.Fatalf("argv carries %q: %s", banned, argv)
		}
	}
	tools := map[string]bool{}
	for _, n := range strings.Split(ClaudeStepTools, ",") {
		tools[n] = true
	}
	for _, bg := range ClaudeBackgroundTools {
		if tools[bg] {
			t.Fatalf("background tool %s is in the step allowlist", bg)
		}
	}
	env := strings.Join(cmd.Env, "\n")
	for _, kv := range []string{"PF_WI_ID=wi_1", "PF_DAEMON_ID=m-1", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_STARTUP_FAILURE_RESULTS=1", "ANTHROPIC_AUTH_TOKEN=tok-SENTINEL-159"} {
		if !strings.Contains(env, kv) {
			t.Fatalf("env lacks %s", kv)
		}
	}
	if strings.Contains(env, "PI_CODING_AGENT_DIR") {
		t.Fatal("a claude step gets no pi agent dir")
	}

	// resume: --resume <uuid>, no --session-id; no model → no --model
	cmd, err = sp.buildCmd(SpawnParams{CapsuleDir: "/caps", Resume: true, HarnessSession: testSession})
	if err != nil {
		t.Fatal(err)
	}
	argv = strings.Join(cmd.Args, " ")
	if !strings.Contains(argv, "--resume "+testSession) || strings.Contains(argv, "--session-id") || strings.Contains(argv, "--model") {
		t.Fatalf("resume argv %s", argv)
	}

	// an invalid session id is refused (the id may come from the journal)
	if _, err := sp.buildCmd(SpawnParams{CapsuleDir: "/caps", HarnessSession: "../x"}); err == nil {
		t.Fatal("invalid session id accepted")
	}
}

// The daemon's own ANTHROPIC_AUTH_TOKEN wins over cli.toml: it is
// inherited and the config value is not added; without either, no token.
func TestClaudeTokenPrecedence(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	sp := &claudeSpawner{cfg: &Config{AnthropicAuthToken: "toml-token"}}
	cmd, _ := sp.buildCmd(SpawnParams{CapsuleDir: "/c", HarnessSession: testSession})
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "ANTHROPIC_AUTH_TOKEN=env-token") || strings.Contains(env, "toml-token") {
		t.Fatalf("env precedence broken")
	}
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	sp = &claudeSpawner{cfg: &Config{}}
	cmd, _ = sp.buildCmd(SpawnParams{CapsuleDir: "/c", HarnessSession: testSession})
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("no token configured, env carries %q", kv)
		}
	}
}

// The prompt rides stdin and stdin is closed afterwards (the stand-in's
// `cat` only finishes on EOF); the spawn journal is written first.
func TestClaudeSpawnWritesPromptAndClosesStdin(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "stdin.txt")
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat > " + out + ".tmp && mv " + out + ".tmp " + out + "\necho '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var journaled bool
	var lines []string
	sp := DefaultSpawner(&Config{ClaudeBin: bin})
	h, err := sp.Spawn(context.Background(), SpawnParams{
		CapsuleDir: dir, InitialPrompt: "do the step", Harness: harness.Claude, HarnessSession: testSession,
		Journal:  func(int, string) error { journaled = true; return nil },
		OnRecord: func(l string) { lines = append(lines, l) },
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !h.Done() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !h.Done() {
		_ = h.Terminate(time.Second)
		t.Fatal("the stand-in never saw stdin EOF")
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != "do the step" {
		t.Fatalf("stdin %q (%v)", got, err)
	}
	if !journaled {
		t.Fatal("spawn journal not written")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"result"`) {
		t.Fatalf("stdout records %q", lines)
	}
}

func TestHarnessSpawnerRefusesUnknownHarness(t *testing.T) {
	if _, err := DefaultSpawner(&Config{}).Spawn(context.Background(), SpawnParams{Harness: "codex", InitialPrompt: "x"}); err == nil {
		t.Fatal("unknown harness accepted")
	}
}

// The seal/pause sync copies a claude dispatch's NATIVE session (found
// by the journaled uuid) onto that dispatch's own capsule file — on a
// resumed dispatch too (the native file is outside the capsule).
func TestSyncLiveTranscriptClaude(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	capDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(capDir, "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(cfgDir, "projects", "-caps", testSession+".jsonl")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := sessionPathFor(capDir, "fp#2")
	line := `{"step_id":"fix","spawn_token":"fp#2","spawn_n":3,"resume":true,"started_at":"2026-10-15T00:00:00Z","transcript":"` + dst + `","harness":"claude","harness_session":"` + testSession + `"}` + "\n"
	if err := os.MkdirAll(filepath.Join(capDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "runtime-state", "spawn-journal.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{}
	r.syncLiveTranscript(capDir)
	got, err := os.ReadFile(dst)
	if err != nil || !strings.Contains(string(got), `"assistant"`) {
		t.Fatalf("capsule copy %q (%v)", got, err)
	}
}
