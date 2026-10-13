package daemon

// main.go: the `pf daemon` process entry. Assembles the daemon Config
// from flags, environment (POLYFORGE_*), the shared CLI config chain
// (PF_SERVER / PF_API_KEY / PF_DAEMON_SOCKET) and the documented
// defaults, then runs the daemon with SIGTERM/SIGINT wired to the
// graceful shutdown sequence.
//
// polyforge#113: there is NO launch-time workspace and NO project scope
// anymore. The [workspace."<path>"] mapping in the shared config (the
// --config file, defaulting to the standard cli.toml location) is the
// only workspace source — the daemon refuses to start without a non-empty
// mapping, validates every mapped workspace, routes capsules per project
// through the inverse mapping, and iterates all mapped projects when
// auto-pulling. The retired selection surfaces (the -workspace flag, the
// --project flag and their env mirrors) are refused loudly at boot.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// Main executes `pf daemon <flags>` and returns the process exit code.
func Main(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pf daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "shared config file: its [workspace] mapping is the capsule-placement source (default ~/.config/polyforge/cli.toml)")
	socket := fs.String("socket", "", "unix socket path (env PF_DAEMON_SOCKET; default /run/polyforge.sock)")
	server := fs.String("server", "", "aihub base URL (env PF_SERVER)")
	apiKey := fs.String("api-key", "", "machine aihub api key (env PF_API_KEY)")
	machineID := fs.String("machine-id", "", "daemon id (env POLYFORGE_MACHINE_ID; default m-<hostname>); recorded in <data-dir>/daemon-id on first start, which then wins — a differing value refuses to start")
	displayName := fs.String("display-name", "", "daemon display name shown by `pf ops status` (env POLYFORGE_DISPLAY_NAME; default hostname)")
	dataDir := fs.String("data-dir", "", "daemon data dir (env POLYFORGE_DATA_DIR)")
	workflowsDir := fs.String("workflows-dir", "", "workflows/ checkout dir (default ./workflows)")
	piBin := fs.String("pi-bin", "", "harness binary (default pi)")
	sessionsRoot := fs.String("harness-sessions-root", "", "harness session store root for the live-transcript sync (env POLYFORGE_HARNESS_SESSIONS_ROOT; default <data-dir>/step-agent-sessions, the step processes' own session store)")
	operatorAgentDir := fs.String("operator-agent-dir", "", "SOURCE of the dedicated step agent directory's minimal set — model service credentials, model configuration and subagent definitions are copied from here into <data-dir>/step-agent (env POLYFORGE_OPERATOR_AGENT_DIR; default ~/.pi/agent). A configured source without a readable auth.json refuses the start")
	gitRemoteBase := fs.String("git-remote-base", "", "aihub bare-repo base URL (default <server>/git)")
	maxConcurrent := fs.Int("max-concurrent-jobs", 0, "max concurrent jobs (default 4)")
	recallTopK := fs.Int("recall-topk", 0, "claim-time memory snapshot hit count (env POLYFORGE_RECALL_TOPK; default 10, max 50)")
	auto := fs.Bool("auto", false, "auto-pull: the daemon claims ready-queue work itself (default off; unset keeps the persisted switch)")
	// Supervision bounds (v2-converge 2.4): Go duration strings, flags
	// only (no env fallbacks). Omitted or zero keeps the documented
	// default; a step may override them in workflow.md.
	silenceWindow := fs.Duration("silence-window", 0, "X: no real work signal after dispatch, or no model output while waiting for the model, before the step is judged dead (default 10m for token-level harnesses, 30m for message-level)")
	toolWindow := fs.Duration("tool-window", 0, "Y: one tool running longer than this is judged hung (default 30m)")
	stepTimeout := fs.Duration("step-timeout", 0, "T: cumulative step runtime bound across its dispatches (default 90m; a step's budget.wall_minutes overrides it)")
	stallWindow := fs.Duration("stall-window", 0, "N: the driver between steps (not waiting on a human) for longer than this is judged stalled (default 10m; negative disables)")
	modelErrorGrace := fs.Duration("model-error-grace", 0, "grace after a turn ends on a model error before the step is judged dead (default 1m)")
	revalidateEvery := fs.Duration("revalidate-every", 0, "M4 periodic session revalidation cadence (0 = default 5m; negative disables)")
	heartbeatEvery := fs.Duration("heartbeat-every", 60*time.Second, "machines heartbeat cadence (0 = disabled)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pf daemon: unexpected arguments %v\n", fs.Args())
		return 2
	}
	autoCfg := resolveAuto(fs, *auto)
	shared, err := cli.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "pf daemon: %v\n", err)
		return 1
	}
	if *socket == "" {
		*socket = shared.DaemonSocket
	}
	if *server == "" {
		*server = shared.Server
	}
	if *apiKey == "" {
		*apiKey = shared.APIKey
	}

	// The mapping is the ONLY workspace source (polyforge#113): a config
	// with zero [workspace] sections — the fresh-install template shape —
	// refuses startup here, before the socket exists, with the shape
	// guidance and the doctor pointer.
	if shared.Mapping.Empty() {
		where := *cfgPath
		if where == "" {
			where = cli.DefaultConfigPath()
		}
		fmt.Fprintf(stderr, "pf daemon: refusing to start: no workspace mapping is configured — add at least one [workspace.\"<path>\"] section with a projects = [\"...\"] list to %s; the daemon places capsules at <workspace>/capsules/<wi> per the claim's project (`pf doctor` checks the mapping)\n", where)
		return 1
	}

	home, _ := os.UserHomeDir()
	base := os.Getenv("POLYFORGE_DATA_DIR")
	if base == "" {
		base = filepath.Join(home, ".local", "share", "polyforge")
	}
	if *dataDir == "" {
		*dataDir = base
	}
	if *machineID == "" {
		*machineID = os.Getenv("POLYFORGE_MACHINE_ID")
	}
	// v2 identity: <data-dir>/daemon-id is THE id once written; a flag/env
	// value that disagrees refuses to start (never silently switch).
	explicitID := *machineID != ""
	if *machineID == "" {
		host, _ := os.Hostname()
		*machineID = "m-" + strings.ToLower(host)
	}
	id, err := ResolveDaemonID(*dataDir, *machineID, explicitID)
	if err != nil {
		fmt.Fprintf(stderr, "pf daemon: refusing to start: %v\n", err)
		return 1
	}
	*machineID = id
	if *displayName == "" {
		*displayName = os.Getenv("POLYFORGE_DISPLAY_NAME")
	}
	if *displayName == "" {
		*displayName, _ = os.Hostname()
	}
	if *workflowsDir == "" {
		*workflowsDir = "workflows"
	}
	if *sessionsRoot == "" {
		*sessionsRoot = os.Getenv("POLYFORGE_HARNESS_SESSIONS_ROOT")
	}
	if *operatorAgentDir == "" {
		*operatorAgentDir = os.Getenv("POLYFORGE_OPERATOR_AGENT_DIR")
	}
	if *gitRemoteBase == "" {
		*gitRemoteBase = strings.TrimSuffix(*server, "/") + "/git"
	}
	if *recallTopK == 0 {
		if v := os.Getenv("POLYFORGE_RECALL_TOPK"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*recallTopK = n
			}
		}
	}

	cfg := Config{
		MachineID:           *machineID,
		DisplayName:         *displayName,
		SocketPath:          *socket,
		Mapping:             shared.Mapping,
		DataDir:             *dataDir,
		GitRemoteBase:       *gitRemoteBase,
		WorkflowsDir:        *workflowsDir,
		PiBin:               *piBin,
		HarnessSessionsRoot: *sessionsRoot,
		// An operator-named source is EXPLICIT: a missing auth.json
		// under it refuses the start rather than running step agents
		// without credentials (addendum 2).
		OperatorAgentDir:         *operatorAgentDir,
		OperatorAgentDirExplicit: *operatorAgentDir != "",
		APIKey:                   *apiKey,
		MaxConcurrent:            *maxConcurrent,
		RecallTopK:               *recallTopK,
		Auto:                     autoCfg,
		HeartbeatEvery:           *heartbeatEvery,
		Hub:                      aihubclient.New(*server, *apiKey),
		// Zero fields fall through to the documented defaults.
		Supervision: SupervisionConfig{
			SilenceWindow:          *silenceWindow,
			ToolWindow:             *toolWindow,
			StepTimeout:            *stepTimeout,
			StallWindow:            *stallWindow,
			ModelErrorGrace:        *modelErrorGrace,
			SessionRevalidateEvery: *revalidateEvery,
		},
	}
	d, err := New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "pf daemon: %v\n", err)
		return 1
	}

	// SIGTERM/SIGINT run the same bounded graceful sequence the
	// POST /shutdown route triggers. The signal VALUE is observed via a
	// Notify channel (polyforge#110): a NotifyContext cancellation
	// reports only "context canceled", so the cause record could never
	// name which signal fired. One shot: after the first delivery
	// signal.Stop restores the default disposition, so a second signal
	// aborts waits hard — the same semantics NotifyContext's stop gave.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		sig := <-sigCh
		signal.Stop(sigCh) // one shot: a second signal aborts waits hard
		if err := d.Shutdown(context.Background(), SignalShutdownCause(sig)); err != nil {
			fmt.Fprintf(stderr, "pf daemon: shutdown: %v\n", err)
		}
	}()

	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "pf daemon: %v\n", err)
		return 1
	}
	return 0
}

// resolveAuto maps -auto to the initial auto switch (nil = unspecified:
// the persisted switch wins).
func resolveAuto(fs *flag.FlagSet, auto bool) *bool {
	autoSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auto" {
			autoSet = true
		}
	})
	if !autoSet {
		return nil
	}
	return &auto
}
