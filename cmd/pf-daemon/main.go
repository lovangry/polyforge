// Command pf-daemon is the per-machine Polyforge Daemon: the single WI Job
// executor for Interactive/Batch/Drain, supervising one persistent Harness
// RPC WI Agent per work item (00 §3.1/§3.4/§3.6), publishing Revisions to
// AIHub over HTTP only.
//
// Design authority: docs/design/polyforge-vnext/04-local-runtime.md (all),
// 00-architecture.md §3.5 (graceful shutdown), §3.4a (answer relay).
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
	"syscall"

	"github.com/silgrid/polyforge/internal/daemon"
	"github.com/silgrid/polyforge/internal/mainagent"
)

func main() {
	var (
		// Default aihub base URL: port 8420, the SAME default the aihub
		// service listens on (cmd/aihub --listen ":8420", the single-port A5
		// ruling) and the pf CLI points at (internal/cli defaultServer;
		// api/openapi.yaml servers: http://localhost:8420). A default
		// three-binary install must be able to talk without flags.
		aihubURL     = flag.String("aihub-url", envOr("POLYFORGE_AIHUB_URL", "http://127.0.0.1:8420"), "AIHub Core base URL")
		apiKey       = flag.String("api-key", os.Getenv("POLYFORGE_API_KEY"), "machine api key (or POLYFORGE_API_KEY)")
		apiKeyFile   = flag.String("api-key-file", "", "read the machine api key from this 0600 file (daemon/credentials convention)")
		machineID    = flag.String("machine-id", envOr("POLYFORGE_MACHINE_ID", ""), "this machine's id (heartbeats, claims)")
		socketPath   = flag.String("socket", envOr("POLYFORGE_SOCKET", "/run/polyforge.sock"), "Unix domain socket operations face")
		home         = flag.String("home", envOr("POLYFORGE_HOME", os.Getenv("HOME")+"/.polyforge"), "Polyforge home (daemon data under <home>/daemon)")
		workspace    = flag.String("workspace-root", "", "workspace root (capsules live under <root>/capsules); precedence: this flag > POLYFORGE_WORKSPACE > <home>/workspace")
		workflowsDir = flag.String("workflows-dir", "", "daemon checkout workflows/ dir (defaults <home>/workflows)")
		skillsDir    = flag.String("skills-dir", "", "daemon checkout skills/ dir (defaults <home>/skills)")
		mode         = flag.String("mode", "interactive", "startup mode: interactive | batch | drain")
		project      = flag.String("project", "", "project whose ready queue Batch/Drain consume")
		adapter      = flag.String("adapter", "pi", "harness adapter: pi | claude")
		piBin        = flag.String("pi-bin", "pi", "pi executable (tests/fakes override)")
		claudeBin    = flag.String("claude-bin", "claude", "claude executable (tests/fakes override)")
		version      = flag.String("version-flag", "vnext-dev", "runtime version stamp for heartbeats")
	)
	flag.Parse()

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

	d, err := daemon.New(cfg, mainagent.NewSupervisor(*piBin, *claudeBin))
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
		// (main calls api.Close after the shutdown sequence). That is not
		// an error: treating it as one (polyforge#17 dogfood finding) made
		// the goroutine os.Exit(1) the whole process mid-epilogue — a
		// clean shutdown exited 1 and never printed its final line, racing
		// main's own exit path.
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
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pf-daemon: "+format+"\n", args...)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func trimNewlines(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == '\n' || s[start] == '\r' || s[start] == ' ') {
		start++
	}
	for end > start && (s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == ' ') {
		end--
	}
	return s[start:end]
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
