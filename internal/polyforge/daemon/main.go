package daemon

// main.go: the `pf daemon` process entry. Assembles the daemon Config
// from flags, environment (POLYFORGE_*), the shared CLI config chain
// (PF_SERVER / PF_API_KEY / PF_DAEMON_SOCKET / PF_PROJECT) and the
// documented defaults, then runs the daemon with SIGTERM/SIGINT wired
// to the graceful shutdown sequence.

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

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// Main executes `pf daemon <flags>` and returns the process exit code.
func Main(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pf daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "shared config file (default ~/.config/polyforge/cli.toml)")
	socket := fs.String("socket", "", "unix socket path (env PF_DAEMON_SOCKET; default /run/polyforge.sock)")
	server := fs.String("server", "", "aihub base URL (env PF_SERVER)")
	apiKey := fs.String("api-key", "", "machine aihub api key (env PF_API_KEY)")
	machineID := fs.String("machine-id", "", "machine identity (env POLYFORGE_MACHINE_ID; default m-<hostname>)")
	workspaceRoot := fs.String("workspace-root", "", "capsule workspace root (env POLYFORGE_WORKSPACE_ROOT)")
	dataDir := fs.String("data-dir", "", "daemon data dir (env POLYFORGE_DATA_DIR)")
	project := fs.String("project", "", "ready-queue project scope for batch/drain (env PF_PROJECT)")
	workflowsDir := fs.String("workflows-dir", "", "workflows/ checkout dir (default ./workflows)")
	executorPrompt := fs.String("executor-prompt", "", "executor skill file injected into every agent (default ./skills/pf-execute/SKILL.md)")
	piBin := fs.String("pi-bin", "", "harness binary (default pi)")
	sessionsRoot := fs.String("harness-sessions-root", "", "harness session store root for the live-transcript sync (env POLYFORGE_HARNESS_SESSIONS_ROOT; default ~/.pi/agent/sessions)")
	cgroupRoot := fs.String("cgroup-root", "", "cgroup v2 root for layer-0 cleanup (env POLYFORGE_CGROUP_ROOT)")
	gitRemoteBase := fs.String("git-remote-base", "", "aihub bare-repo base URL (default <server>/git)")
	maxConcurrent := fs.Int("max-concurrent-jobs", 0, "max concurrent jobs (default 4)")
	recallTopK := fs.Int("recall-topk", 0, "claim-time memory snapshot hit count (env POLYFORGE_RECALL_TOPK; default 10, max 50)")
	mode := fs.String("mode", "", "initial mode: interactive|batch|drain (default interactive)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pf daemon: unexpected arguments %v\n", fs.Args())
		return 2
	}

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
	if *project == "" {
		*project = shared.Project
	}
	if *project == "" {
		*project = os.Getenv("PF_PROJECT")
	}

	home, _ := os.UserHomeDir()
	base := os.Getenv("POLYFORGE_DATA_DIR")
	if base == "" {
		base = filepath.Join(home, ".local", "share", "polyforge")
	}
	if *dataDir == "" {
		*dataDir = base
	}
	if *workspaceRoot == "" {
		*workspaceRoot = os.Getenv("POLYFORGE_WORKSPACE_ROOT")
	}
	if *workspaceRoot == "" {
		*workspaceRoot = filepath.Join(base, "workspace")
	}
	if *machineID == "" {
		*machineID = os.Getenv("POLYFORGE_MACHINE_ID")
	}
	if *machineID == "" {
		host, _ := os.Hostname()
		*machineID = "m-" + strings.ToLower(host)
	}
	if *workflowsDir == "" {
		*workflowsDir = "workflows"
	}
	if *executorPrompt == "" {
		*executorPrompt = filepath.Join("skills", "pf-execute", "SKILL.md")
	}
	if !fileExists(*executorPrompt) {
		fmt.Fprintf(stderr, "pf daemon: executor prompt %s not found (run from the daemon checkout or pass --executor-prompt)\n", *executorPrompt)
		return 1
	}
	if *cgroupRoot == "" {
		*cgroupRoot = os.Getenv("POLYFORGE_CGROUP_ROOT")
	}
	if *sessionsRoot == "" {
		*sessionsRoot = os.Getenv("POLYFORGE_HARNESS_SESSIONS_ROOT")
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
		SocketPath:          *socket,
		WorkspaceRoot:       *workspaceRoot,
		DataDir:             *dataDir,
		Project:             *project,
		GitRemoteBase:       *gitRemoteBase,
		WorkflowsDir:        *workflowsDir,
		ExecutorPromptFile:  *executorPrompt,
		PiBin:               *piBin,
		HarnessSessionsRoot: *sessionsRoot,
		CgroupRoot:          *cgroupRoot,
		APIKey:              *apiKey,
		MaxConcurrent:       *maxConcurrent,
		RecallTopK:          *recallTopK,
		Mode:                *mode,
		Hub:                 aihubclient.New(*server, *apiKey),
	}
	d, err := New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "pf daemon: %v\n", err)
		return 1
	}

	// SIGTERM/SIGINT run the same bounded graceful sequence the
	// POST /shutdown route triggers.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // one shot: a second signal aborts waits hard
		if err := d.Shutdown(context.Background()); err != nil {
			fmt.Fprintf(stderr, "pf daemon: shutdown: %v\n", err)
		}
	}()

	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "pf daemon: %v\n", err)
		return 1
	}
	return 0
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
