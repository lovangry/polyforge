// daemon.go: the `pf daemon` subcommand — the per-machine Polyforge
// Daemon, the single WI Job executor for Interactive/Batch/Drain,
// supervising one persistent Harness RPC WI Agent per work item
// (00 §3.1/§3.4/§3.6), publishing Revisions to AIHub over HTTP only.
//
// It is dispatched in cmd/pf/main.go BEFORE the CLI verb dispatcher: it
// is not a one-shot verb. Its flag set, env handling and config
// precedence are its own, and its error path is a process exit (fatal →
// os.Exit(1)) — a long-running supervisor must not be flattened into the
// CLI verbs' exit-code surface. Its graceful-shutdown epilogue RETURNS:
// cmd/pf/main.go returns immediately after runDaemon so the process ends
// with the ZERO status a supervisor reads as a clean stop; only on the
// DAEMON-511 flush-did-not-converge refusal does it deliberately stay up
// forever awaiting human attention.
//
// Design authority: docs/design/polyforge-vnext/04-local-runtime.md (all),
// 00-architecture.md §3.5 (graceful shutdown), §3.4a (answer relay),
// 00-architecture.md §3.2 / 07-deployment-pilot.md §3 (deployment-unit
// merge ruling: the work-machine binary is this single `pf` binary, CLI
// verbs + daemon subcommand; the binary name stays `pf` until the
// cutover rename ruling fires).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/silgrid/polyforge/internal/daemon"
	"github.com/silgrid/polyforge/internal/mainagent"
)

// runDaemon is the `pf daemon` subcommand body. Fatal paths are process
// exits (fatal → os.Exit(1)); the DAEMON-511 refusal blocks forever; and
// the graceful-shutdown epilogue RETURNS — cmd/pf/main.go returns
// immediately after this function, so the process ends with the zero
// status (never a fall-through to the CLI verb dispatcher, which would
// print `unknown command "daemon"` and exit 2).
func runDaemon(args []string) {
	// ExitOnError preserves the standalone-entry behavior this wiring
	// had as cmd/pf-daemon: -h prints the flag defaults and exits 0, a
	// malformed flag exits 2. The flag defaults themselves keep the
	// same names, env fallbacks and precedence as before the merge.
	fs := flag.NewFlagSet("pf daemon", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	var (
		// Default aihub base URL: port 8420, the SAME default the aihub
		// service listens on (cmd/aihub --listen ":8420", the single-port A5
		// ruling) and the pf CLI points at (internal/cli defaultServer;
		// api/openapi.yaml servers: http://localhost:8420). A default
		// install must be able to talk without flags.
		aihubURL     = fs.String("aihub-url", envOr("POLYFORGE_AIHUB_URL", "http://127.0.0.1:8420"), "AIHub Core base URL")
		apiKey       = fs.String("api-key", os.Getenv("POLYFORGE_API_KEY"), "machine api key (or POLYFORGE_API_KEY)")
		apiKeyFile   = fs.String("api-key-file", "", "read the machine api key from this 0600 file (daemon/credentials convention)")
		machineID    = fs.String("machine-id", envOr("POLYFORGE_MACHINE_ID", ""), "this machine's id (heartbeats, claims)")
		socketPath   = fs.String("socket", envOr("POLYFORGE_SOCKET", "/run/polyforge.sock"), "Unix domain socket operations face")
		home         = fs.String("home", envOr("POLYFORGE_HOME", os.Getenv("HOME")+"/.polyforge"), "Polyforge home (daemon data under <home>/daemon)")
		workspace    = fs.String("workspace-root", "", "workspace root (capsules live under <root>/capsules); precedence: this flag > POLYFORGE_WORKSPACE > <home>/workspace")
		workflowsDir = fs.String("workflows-dir", "", "daemon checkout workflows/ dir (defaults <home>/workflows)")
		skillsDir    = fs.String("skills-dir", "", "daemon checkout skills/ dir (defaults <home>/skills)")
		mode         = fs.String("mode", "interactive", "startup mode: interactive | batch | drain")
		project      = fs.String("project", "", "project whose ready queue Batch/Drain consume")
		adapter      = fs.String("adapter", "pi", "harness adapter: pi | claude")
		piBin        = fs.String("pi-bin", "pi", "pi executable (tests/fakes override)")
		claudeBin    = fs.String("claude-bin", "claude", "claude executable (tests/fakes override)")
		version      = fs.String("version-flag", "vnext-dev", "runtime version stamp for heartbeats")
	)
	fs.Parse(args) // ExitOnError: malformed flag exits 2, -h exits 0

	// A stray positional must be REJECTED, not ignored: Go's flag package
	// stops parsing at the first non-flag argument, so `pf daemon stray
	// --workspace-root X` would silently swallow every later flag and
	// start on the DEFAULT workspace. Everything after a bare `--` lands
	// in fs.Args() the same way. Refuse with the usage surface and exit
	// 2 (the CLI's own exit code for a usage error).
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "pf daemon: unexpected argument(s): %v\n\n", fs.Args())
		fs.Usage()
		os.Exit(2)
	}

	// Workspace-root precedence, resolved AFTER flag parsing so --home is
	// already final: an explicit --workspace-root wins, then the
	// POLYFORGE_WORKSPACE env, then <home>/workspace — the same home that
	// derives the daemon data, workflows and skills dirs, so --home moves
	// the WHOLE local layout consistently (capsules included) instead of
	// leaving them under the old default.
	*workspace = resolveWorkspaceRoot(*home, *workspace)

	if *apiKeyFile != "" {
		raw, err := os.ReadFile(*apiKeyFile)
		if err != nil {
			fatal("read api key file: %v", err)
		}
		key := trimNewlines(string(raw))
		if key == "" {
			fatal("api key file is empty")
		}
		*apiKey = key
	}
	if *machineID == "" {
		host, _ := os.Hostname()
		*machineID = "m-" + host
	}
	if *apiKey == "" {
		fatal("machine api key required (--api-key / --api-key-file / POLYFORGE_API_KEY); register this machine via POST /api/v1/machines/heartbeat first")
	}
	if *workflowsDir == "" {
		*workflowsDir = filepath.Join(*home, "workflows")
	}
	if *skillsDir == "" {
		*skillsDir = filepath.Join(*home, "skills")
	}

	cfg := daemon.Config{
		AIHubURL: *aihubURL, APIKey: *apiKey, MachineID: *machineID,
		DataDir:       filepath.Join(*home, "daemon"),
		WorkspaceRoot: *workspace,
		WorkflowsDir:  *workflowsDir,
		SkillsDir:     *skillsDir,
		DrainProject:  *project,
		Adapter:       *adapter,
		Version:       *version,
	}

	// The supervisor is handed the resolved api key ONCE (04 §8 env
	// sanitize: the key lives only in the daemon, never in a spawned
	// agent's environment — not even via a renamed variable).
	d, err := daemon.New(cfg, mainagent.NewSupervisor(*piBin, *claudeBin, *apiKey))
	if err != nil {
		fatal("daemon init: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Startup mode transition (interactive/batch/drain; drain's loop is
	// started below only in drain mode).
	switch *mode {
	case "interactive", "batch", "drain":
		if err := d.SetStartupMode(*mode); err != nil {
			fatal("startup mode: %v", err)
		}
	default:
		fatal("unknown --mode %q", *mode)
	}

	// Crash recovery from daemon/state.json (04 §7.1): re-attach alive
	// agents, re-derive dead ones with their recorded session ids.
	if err := d.RecoverFromState(ctx); err != nil {
		fatal("crash recovery: %v", err)
	}

	// Heartbeat goroutine (04 §7.2): 60s cadence, mode rides along.
	go d.RunHeartbeat(ctx, 0)

	// Operations face: local UDS only. A live socket elsewhere = another
	// daemon = exit (single-instance lock).
	api, err := daemon.NewAPIServer(d, *socketPath)
	if err != nil {
		fatal("operations face: %v", err)
	}
	go func() {
		// Serve returns http.ErrServerClosed on the GRACEFUL shutdown path
		// (runDaemon calls api.Close after the shutdown sequence). That is
		// not an error: treating it as one (polyforge#17 dogfood finding)
		// made the goroutine os.Exit(1) the whole process mid-epilogue — a
		// clean shutdown exited 1 and never printed its final line, racing
		// the main path's own exit.
		if serr := api.Serve(); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			fatal("operations face serve: %v", serr)
		}
	}()

	// Main loop until a signal (or ctx end) latches the shutdown sequence.
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}

	fmt.Fprintln(os.Stderr, "pf-daemon: shutdown sequence starting (graceful; NOT a drain idle)")
	if err := d.Shutdown(context.Background()); err != nil {
		// DAEMON-511: flush did not converge — do NOT exit; human attention
		// required (04 §6.5 step 5's refusal).
		fmt.Fprintf(os.Stderr, "pf-daemon: %v — process stays up\n", err)
		// Keep the face alive for an operator.
		<-make(chan struct{})
	}
	api.Close()
	fmt.Fprintln(os.Stderr, "pf-daemon: bye")
	// Fall off the end of runDaemon: the process ends with the zero
	// status a supervisor (systemd/launchd) reads as a clean stop.
}

// fatal is the daemon's process-exit error path, unchanged from the
// standalone cmd/pf-daemon entry: print to stderr prefixed the same way
// the internal/daemon package prefixes its own diagnostics, then exit 1.
// The "pf-daemon:" prefix is kept on purpose — internal/daemon prints it
// and is not touched by the merge — so one daemon's stderr stays
// uniformly prefixed.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pf-daemon: "+format+"\n", args...)
	os.Exit(1)
}

// envOr returns the env value when set, else the default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// trimNewlines strips leading/trailing CR/LF/space bytes (key files).
// The cutset is exactly "\r\n " — NOT TrimSpace, which would also strip
// tabs and every other whitespace byte a key must never silently lose.
func trimNewlines(s string) string {
	return strings.Trim(s, "\r\n ")
}

// resolveWorkspaceRoot applies the workspace-root precedence: explicit
// flag value > POLYFORGE_WORKSPACE env > <home>/workspace (home already
// flag-resolved, so --home derives the workspace too).
func resolveWorkspaceRoot(home, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("POLYFORGE_WORKSPACE"); v != "" {
		return v
	}
	return filepath.Join(home, "workspace")
}
