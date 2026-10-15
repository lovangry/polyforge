package runtime

// claudespawn.go: the step spawner for the Claude Code harness
// (polyforge#159). One step = one headless `claude -p` process:
//
//	<claude_bin> -p --bare
//	  --output-format stream-json --verbose --include-partial-messages
//	  --session-id <uuid> | --resume <uuid>
//	  [--model <claude id>]
//	  --tools <ClaudeStepTools> --allowedTools <ClaudeStepTools>
//	  --permission-mode dontAsk --permission-prompts none
//	  --strict-mcp-config --disable-slash-commands
//
// cwd = the capsule root, own process group (as pi); stdin = the
// assembled step prompt (the SAME text pi gets), closed once written.
//
// The step's Claude Code context is daemon-owned: <harness-root>/
// claude-config is the config dir (its settings.json env block is the
// single credential authority layer, polyforge#159 closeout #171), so
// the daemon hard-sets CLAUDE_CONFIG_DIR there, and switches off
// nonessential traffic (CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1).
// The daemon never injects a credential: none ride the environment or
// argv — the harness's own settings.json provides them.
//
// Clean step context (a functional requirement, not isolation): --bare
// skips hooks, plugins, LSP, auto-memory, CLAUDE.md discovery and the
// keychain; --strict-mcp-config with no --mcp-config loads no MCP
// server; --disable-slash-commands keeps the human's skills out. The
// --tools allowlist is the whole tool surface: no background, scheduling
// or messaging tool exists in the step (background tasks are also off
// through CLAUDE_CODE_DISABLE_BACKGROUND_TASKS and --bare). No
// --fallback-model: switching models is D7's alone.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/common/harness"
)

// ClaudeStepTools is the step's whole tool surface: pre-approved, and
// nothing else exists. The built-in Agent tool is the fresh-context
// sub-agent (foreground only); WebFetch/WebSearch stay out (pi parity).
const ClaudeStepTools = "Bash,Read,Edit,Write,Glob,Grep,TodoWrite,Agent"

// ClaudeBackgroundTools are tools a step must never have (asserted by
// the argv regression): background shells, monitors, schedulers,
// cross-session messaging.
var ClaudeBackgroundTools = []string{
	"Monitor", "TaskOutput", "TaskStop", "KillShell", "BashOutput",
	"CronCreate", "CronDelete", "CronList", "ScheduleWakeup",
	"RemoteTrigger", "PushNotification", "SendMessage",
}

// claudeSpawner derives Claude Code step processes. Like piSpawner it
// reads the shared config by pointer at spawn time.
type claudeSpawner struct {
	cfg *Config
}

func (s *claudeSpawner) bin() string {
	if s.cfg.ClaudeBin != "" {
		return s.cfg.ClaudeBin
	}
	return "claude"
}

// buildCmd constructs (without starting) one derivation's argv/env/cwd.
func (s *claudeSpawner) buildCmd(p SpawnParams) (*exec.Cmd, error) {
	if !harness.ValidSessionID(p.HarnessSession) {
		return nil, fmt.Errorf("runtime: claude spawn without a valid session id (%q)", p.HarnessSession)
	}
	args := []string{"-p", "--bare",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if p.Resume {
		args = append(args, "--resume", p.HarnessSession)
	} else {
		args = append(args, "--session-id", p.HarnessSession)
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	args = append(args,
		"--tools", ClaudeStepTools,
		"--allowedTools", ClaudeStepTools,
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--strict-mcp-config",
		"--disable-slash-commands",
	)
	cmd := exec.Command(s.bin(), args...)
	cmd.Dir = p.CapsuleDir
	additions := []string{
		"PF_WI_ID=" + p.WorkItemID,
		"PF_DAEMON_ID=" + s.cfg.MachineID,
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_STARTUP_FAILURE_RESULTS=1",
	}
	// polyforge#171: the step's config dir is where its credentials live
	// (claude-config/settings.json's env block — the single authority
	// layer); the adapter hard-sets it and never injects a credential of
	// its own. Only a real root redirects (a bare Config has no claude
	// config dir; the step runs on whatever its own HOME resolves to).
	if ccDir := s.cfg.ClaudeConfigDir(); ccDir != "" {
		additions = append(additions, "CLAUDE_CONFIG_DIR="+ccDir)
	}
	// the "0" value is OFF for everything CLAUDE_CONFIG_DIR-side, so the
	// existence is the semantic (see the WI ruling 5)
	additions = append(additions, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	// env beats the claude-config authority: an inherited
	// ANTHROPIC_AUTH_TOKEN is left as is (deleted with the cli.toml key
	// in the #171 closeout)
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" && s.cfg.AnthropicAuthToken != "" {
		additions = append(additions, "ANTHROPIC_AUTH_TOKEN="+s.cfg.AnthropicAuthToken)
	}
	// A daemon started from inside a Claude Code session inherits that
	// session's CLAUDECODE / CLAUDE_CODE_* markers and switches; none of
	// them may shape the step (clean context) — only the switches the
	// adapter sets above. CLAUDE_CONFIG_DIR is stripped the same way: it
	// is hard-set by the adapter (an operator env value would hijack the
	// step's config dir and would duplicate the env entry); ANTHROPIC_*
	// pass through.
	inherited := newEnvSanitizer(s.cfg.EnvSecrets...).childEnv()
	env := make([]string, 0, len(inherited)+len(additions))
	for _, kv := range inherited {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") || name == "CLAUDE_CONFIG_DIR" {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, additions...)
	return cmd, nil
}

// Spawn derives one Claude Code step process: process group, identity
// journal before the prompt, then the prompt on stdin and stdin closed.
func (s *claudeSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	if p.InitialPrompt == "" {
		return nil, fmt.Errorf("runtime: spawn without an initial prompt")
	}
	cmd, err := s.buildCmd(p)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return nil, fmt.Errorf("runtime: harness start failed: %w", err)
	}
	outW.Close()
	errW.Close()
	proc := newProcOpts(cmd, nil, outR, procOpts{
		taps:   recordTaps(p),
		stderr: errR,
		onExit: p.OnExit,
	})
	if p.Journal != nil {
		if jerr := p.Journal(cmd.Process.Pid, proc.startedAt); jerr != nil {
			stdin.Close()
			_ = proc.Terminate(5 * time.Second)
			return nil, fmt.Errorf("runtime: spawn journal failed (process torn down): %w", jerr)
		}
	}
	_, werr := stdin.Write([]byte(p.InitialPrompt))
	cerr := stdin.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		// a CLI that died at startup (a bad credential, an unknown model)
		// never read the prompt: its stderr is the explanation the driver
		// classifies (EPIPE path, as pi)
		_ = proc.Terminate(5 * time.Second)
		select {
		case <-proc.exited:
		case <-time.After(2 * time.Second):
		}
		if detail := strings.TrimSpace(proc.tail.String()); detail != "" {
			return nil, fmt.Errorf("runtime: initial prompt injection failed (process torn down): %w; harness stderr: %s", werr, detail)
		}
		return nil, fmt.Errorf("runtime: initial prompt injection failed (process torn down): %w", werr)
	}
	return proc, nil
}

// recordTaps wires a dispatch's OnRecord callback into the stdout taps.
func recordTaps(p SpawnParams) []func(string) {
	if p.OnRecord == nil {
		return nil
	}
	return []func(string){p.OnRecord}
}

// harnessSpawner is the production spawner: it dispatches each
// derivation to its step harness's spawner (polyforge#159).
type harnessSpawner struct {
	pi     *piSpawner
	claude *claudeSpawner
}

// Spawn implements Spawner.
func (s *harnessSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	switch harness.Of(p.Harness) {
	case harness.Pi:
		return s.pi.Spawn(ctx, p)
	case harness.Claude:
		return s.claude.Spawn(ctx, p)
	}
	return nil, fmt.Errorf("runtime: unknown step harness %q", p.Harness)
}
