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
//	  --strict-mcp-config --disable-slash-commands --safe-mode
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
// server; --disable-slash-commands keeps the human's skills out;
// --safe-mode is the umbrella over every customization surface the
// enumerated flags do not list (output styles, workflows, themes,
// keybindings and whatever a later release adds). The
// --tools allowlist is the whole tool surface: no background, scheduling
// or messaging tool exists in the step (background tasks are also off
// through CLAUDE_CODE_DISABLE_BACKGROUND_TASKS and --bare). No
// --fallback-model: switching models is D7's alone.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

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
	// polyforge#176: the candidate's effort rides --effort
	args = append(args, harness.Candidate{Harness: harness.Claude, Effort: p.Effort}.EffortArgs()...)
	args = append(args,
		"--tools", ClaudeStepTools,
		"--allowedTools", ClaudeStepTools,
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--safe-mode",
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
	if s.cfg.DaemonSocket != "" {
		additions = append(additions, "PF_DAEMON_SOCKET="+s.cfg.DaemonSocket)
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
	// Zero credential injection (polyforge#171, closing out #159's three-layer blind spot):
	// the adapter NEVER injects ANTHROPIC_AUTH_TOKEN or any other
	// credential — the settings.json env block under claude-config is
	// the single authority layer, applied by claude itself.
	// A daemon started from inside a Claude Code session inherits that
	// session's CLAUDECODE / CLAUDE_CODE_* markers and switches; none of
	// them may shape the step (clean context) — only the switches the
	// adapter sets above. CLAUDE_CONFIG_DIR is stripped the same way: it
	// is hard-set by the adapter (an operator env value would hijack the
	// step's config dir and would duplicate the env entry).
	//
	// ANTHROPIC_*: EVERY inherited ANTHROPIC_* is stripped too, so the
	// `env` block of <claude-config>/settings.json is the ONLY source of
	// a claude step's credential, endpoint and model defaults. Whatever
	// the daemon inherited (e.g. the tmux server's ANTHROPIC_BASE_URL /
	// AUTH_TOKEN / MODEL of a human's session) must not reach a step, and
	// a variable in the process env could otherwise override or add to
	// settings.json. Set every ANTHROPIC_* a step needs in settings.json
	// (`pf daemon ensure` hands the daemon no ANTHROPIC_* either: see
	// supervisor/env.go). pi steps strip nothing extra here, but they too
	// see only the daemon's environment — for an ensure-started daemon
	// the allow-list — so a pi provider key must be in the step agent
	// dir's auth.json (copied from the operator agent dir) or a literal
	// in its models.json; a key that lives only in the daemon's
	// environment (a provider *_API_KEY, a models.json `$NAME`) does not
	// reach pi steps.
	inherited := newEnvSanitizer(s.cfg.EnvSecrets...).childEnv()
	env := make([]string, 0, len(inherited)+len(additions))
	for _, kv := range inherited {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") || name == "CLAUDE_CONFIG_DIR" ||
			strings.HasPrefix(name, "ANTHROPIC_") {
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
	proc, stdin, err := startHarness(cmd, p, false)
	if err != nil {
		return nil, err
	}
	_, werr := stdin.Write([]byte(p.InitialPrompt))
	cerr := stdin.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, injectionFailed(proc, werr)
	}
	return proc, nil
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
