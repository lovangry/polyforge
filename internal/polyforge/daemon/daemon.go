// Package daemon is the phase-3 assembly of the v1 local runtime: the
// UDS HTTP face the pf CLI speaks (internal/common/protocol route
// family, pinned by internal/polyforge/cli/daemon.go), the Runtime
// (lifecycle) and Observe (supervision) modules wired over the five
// protocol messages, and the machine-level operations — the three
// admission modes, the pending-job dispatcher, the auto-claim loop and the
// shutdown.
//
// Current behavior: docs/design/polyforge-vnext/reference.md §6 (routes,
// lifecycle, shutdown, configuration); decisions: v2-converge.md §5–§6.
// The v1 records it was first built from (v1-architecture-spec, v1-spec-full,
// 04-local-runtime) are under docs/design/polyforge-vnext/archive/.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/bridge"
	"github.com/silgrid/polyforge/internal/polyforge/driver"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/mapping"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// Hub is the aihub face the daemon itself needs beyond Runtime's
// session-claim/end use: the ready-queue projection (batch recommendation,
// drain loop), the terminal-state check of the wrap/pause arbitration,
// and — through the generic Do — the memory faces the F6 lanes proxy
// through (search with an excerpt, memory create). Heartbeat is the M3
// machines liveness face the heartbeat loop POSTs on its own cadence.
// *aihubclient.Client satisfies it.
type Hub interface {
	runtime.Aihub
	Queue(project string) ([]api.WorkItem, error)
	// SessionVerify is the v1.5 M4 compare-only holder check — the
	// periodic per-RUNNING-job revalidation the observe monitor
	// piggybacks on its tick. *aihubclient.Client satisfies it.
	SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error)
	// Heartbeat posts one machines/heartbeat upsert (M3 liveness face).
	Heartbeat(req aihubclient.MachineHeartbeat) (*aihubclient.MachineHeartbeatResult, error)
	// Do performs one typed request against aihub (the client's own
	// generic entry): the F6 memory adapters use it for the calls the
	// shared client carries no typed method for.
	Do(method, path string, body, out any) error
}

// Config wires the daemon. Zero fields take the defaults in fill().
type Config struct {
	MachineID string
	// DisplayName is the display-only daemon name (default: hostname).
	DisplayName string
	SocketPath  string
	// Mapping is the REQUIRED workspace→projects plan of record
	// (polyforge#113), assembled in main.go from the shared config's
	// [workspace."<path>"] sections. Capsules route per project through
	// the inverse mapping: <mapped-workspace>/capsules/<project>-<seq>. There is
	// no single workspace and no daemon project scope anymore — batch
	// and drain iterate ALL mapped projects. New validates the mapping
	// is non-empty and every mapped workspace exists with capsules/
	// creatable.
	Mapping       *mapping.Plan
	DataDir       string
	GitRemoteBase string
	WorkflowsDir  string // daemon checkout's workflows/
	PiBin         string
	// ClaudeBin is the Claude Code binary for steps whose harness is
	// claude (polyforge#159; --claude-bin / cli.toml claude_bin; default
	// "claude").
	ClaudeBin string
	// HarnessRoot is the parent root of the three harness state dirs
	// (polyforge#171: <harness-root>/step-agent, /step-agent-sessions,
	// /claude-config). "" = the default <data-dir>; the doctor's claude
	// entry reports the three dirs' existence and the claude-config
	// settings.json env keys (set/unset only) off it.
	HarnessRoot string
	// OperatorAgentDir is the SOURCE of the dedicated step agent
	// directory's minimal set (addendum 2, owner ruling A): the
	// operator's agent dir by default. Explicit (flag/env) sources whose
	// auth.json is missing refuse the daemon start.
	OperatorAgentDir         string
	OperatorAgentDirExplicit bool
	APIKey                   string // machine credential; NEVER reaches an agent env
	MaxConcurrent            int    // cross-mode concurrency cap (default 4)
	// EnvWarnings are the boot warnings for retired POLYFORGE_* variables
	// still set to a differing value (main.go retiredEnvWarnings): each
	// lands one machine-level retired_env_ignored event and rides
	// GET /status for pf doctor.
	EnvWarnings []string
	// RecallTopK is the F6-A claim-time memory snapshot's hit count
	// (0 = the runtime default, 10; bounded at 50 there).
	RecallTopK int
	Auto       *bool // initial auto switch; nil = not specified (persisted state wins; default off)

	// HeartbeatEvery is the machines liveness cadence (M3): how often the
	// daemon POSTs /api/v1/machines/heartbeat. <= 0 disables the
	// heartbeat loop entirely; `pf daemon` wires the flag -heartbeat-every
	// whose default (60s) is the production cadence the aihub side's
	// 5×-default stale window assumes.
	HeartbeatEvery time.Duration

	// Test/ops knobs.
	Spawner runtime.Spawner // nil = the production pi spawner
	// AgentStarter replaces the execution-engine starter the runtime
	// uses (nil = the production Go driver, bridge.Start). Test seam:
	// the in-process daemon tests bind a fake engine so they can script
	// agent behavior without a real driver. There is no selection and
	// no fallback — the driver IS the engine; this only swaps which
	// starter binds it.
	AgentStarter runtime.AgentStartFunc
	Hub          Hub              // required; aihubclient.Client satisfies
	PushFunc     observe.PushFunc // nil = the shared gitops choke point
	ObservePoll  time.Duration    // observe execution.json poll tick
	// Supervision carries the supervisor's global bounds into the
	// assembly (X silence, Y tool, T step timeout, N driver stall, the
	// model-error grace; zero fields keep the documented defaults) and
	// the M4 revalidation cadence. Step overrides live in workflow.md.
	Supervision   SupervisionConfig
	DrainInterval time.Duration // drain wave-fetch period (5s)
	DispatchTick  time.Duration // dispatcher/backfill period (200ms)
	ExitPoll      time.Duration // adapter exit-poll period (200ms)
	// RestartHubWait is how long one hub call of the restart
	// reconciliation may go unanswered before the socket serves anyway
	// (0 = the runtime's default, 5s; see runtime.Config.RestartHubWait).
	RestartHubWait time.Duration
}

// SupervisionConfig is the supervisor's global-default surface
// (v2-converge 2.4): daemon flags map onto it one to one.
type SupervisionConfig struct {
	// SilenceWindow is X: no work-class signal after dispatch (rule 0)
	// and no model output while waiting for the model (rule 2). Zero =
	// by the harness's output granularity (10m token-level, 30m
	// message-level).
	SilenceWindow time.Duration
	// ToolWindow is Y: one tool running longer than this is hung
	// (default 30m).
	ToolWindow time.Duration
	// StepTimeout is T: cumulative step runtime (default 90m; a step's
	// own budget.wall_minutes overrides it).
	StepTimeout time.Duration
	// StallWindow is N: the driver between steps (not waiting on a human)
	// for longer than this is stalled (default 10m; negative disables).
	StallWindow time.Duration
	// ModelErrorGrace is the rule-1 grace after an error-ended turn
	// (default 1m).
	ModelErrorGrace time.Duration
	// SessionRevalidateEvery is the M4 periodic session-revalidation
	// cadence (0 = observe's default 5m; negative = disabled).
	SessionRevalidateEvery time.Duration
}

func (c SupervisionConfig) limits() supervise.Defaults {
	return supervise.Defaults{
		Silence: c.SilenceWindow, Tool: c.ToolWindow,
		StepTimeout: c.StepTimeout, ModelErrorGrace: c.ModelErrorGrace,
	}
}

func (c *Config) fill() {
	if c.SocketPath == "" {
		c.SocketPath = "/run/polyforge.sock"
	}
	// The Go execution driver is THE engine (polyforge#106): the legacy
	// WI-Agent selection surface is deleted — there is nothing to
	// default and nothing to flip.
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.ObservePoll == 0 {
		c.ObservePoll = 500 * time.Millisecond
	}
	if c.DrainInterval == 0 {
		c.DrainInterval = 5 * time.Second
	}
	if c.DispatchTick == 0 {
		c.DispatchTick = 200 * time.Millisecond
	}
	if c.ExitPoll == 0 {
		c.ExitPoll = 200 * time.Millisecond
	}
}

// Daemon is the assembled local runtime.
type Daemon struct {
	// claudeVer caches the claude --version probe of GET /status.
	claudeVer versionCache
	chats     *chatStore
	subs      *subStore // pf watch: chat subscriptions, cursors, watcher liveness
	cfg       Config
	hub       Hub
	modes     *ModeCtl
	q         *jobQueue

	rt  *runtime.Runtime
	obs *observe.Observe

	rtMsgs   *supervise.Queue[protocol.RTMsg]
	obMsgs   *supervise.Queue[protocol.OBMsg]
	attachCh *supervise.Queue[observe.Attach]
	// attachedMu guards attached: the work items whose monitor-building
	// RTStarted attachment went to the observe lane (the bridge, a
	// re-attachment) with no RTFinished since — their monitor exists, or
	// is about to once observe drains attachCh.
	attachedMu sync.Mutex
	attached   map[string]bool

	// sealGuard serializes the close paths (CLI wrap/pause): exactly one
	// close may run per work item; the late arrival answers DAEMON-409.
	sealMu   sync.Mutex
	sealedWI map[string]bool

	runCtx    context.Context // the loops' context; the shutdown cancels it
	cancelRun context.CancelFunc

	shutOnce sync.Once
	shutDone chan struct{} // closed when the shutdown finished

	// queueFetchDenied is the denial episode flag: set when the first
	// queue_fetch_denied event of a healthy→denied transition has been
	// emitted, cleared on the next successful wave fetch. Only the
	// auto-pull ticker goroutine touches it (fetchAutoWave), so it
	// carries no lock.
	queueFetchDenied bool
	// yamlRefused is project → the last workspace-yaml refusal logged for
	// it: auto-pull skips a project whose .polyforge.yaml is invalid
	// (nothing is claimed, the WIs stay queued for other machines) and
	// logs a given refusal once, not every tick. Same single-goroutine
	// ownership as queueFetchDenied.
	yamlRefused map[string]string
	// bundleRefused is bundle → the last load refusal logged for it
	// (polyforge#175): auto-pull skips ready work items on an installed
	// bundle that fails to load instead of enqueueing a claim bound to
	// refuse every wave; logged once per distinct refusal. Same
	// single-goroutine ownership as yamlRefused.
	bundleRefused map[string]string

	// sup hands out the per-WI supervision links shared by the driver
	// bridge (facts out, verdicts in, harness trackers) and the observe
	// monitors.
	sup *supervise.Registry

	adapMu   sync.Mutex
	adapters map[string]*agentAdapter

	lst net.Listener
	srv *http.Server

	// faceMu guards lst, srv, dataLock and the faceClosed latch: a
	// Shutdown can run concurrently with Run's startup, and the latch lets
	// whichever side loses the race tear the listener down instead of
	// serving on a face the shutdown already closed.
	faceMu     sync.Mutex
	faceClosed bool

	// dataLock is the M5 flock on the daemon data dir (held for the
	// process's lifetime, released by the kernel at exit): the
	// same-machine dual-daemon TOCTOU guard ALONGSIDE the socket probe.
	// The probe alone races (both processes can observe a dead socket
	// and both bind); the flock is the kernel-serialized half.
	dataLock *os.File

	// stepOriginOf resolves a /step/* caller to its work item (nil = the
	// UDS peer's process tree, peerStepOrigin; tests bind a stub).
	stepOriginOf func(*http.Request) (stepJob, string)
}

// New validates and assembles the daemon. Restart reconciliation is NOT
// run here — Run does it once its goroutines are live.
func New(cfg Config) (*Daemon, error) {
	if cfg.Hub == nil {
		return nil, errors.New("daemon: Config.Hub is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("daemon: DataDir is required")
	}
	if cfg.Mapping == nil || cfg.Mapping.Empty() {
		return nil, errors.New("daemon: a non-empty [workspace] mapping is required — add at least one [workspace.\"<path>\"] section with a projects list to the config (`pf doctor` checks the mapping)")
	}
	// Relative paths are a footgun measured live on the window rig: the
	// spawned git commands run with cmd.Dir = the capsule, so a relative
	// DataDir resolves GIT_DIR under the capsule ('not a git repository').
	// Absolutize the root before anything derives paths from it.
	if abs, aerr := filepath.Abs(cfg.DataDir); aerr == nil {
		cfg.DataDir = abs
	}
	// Workspace validation (#103, extended to every mapped workspace by
	// polyforge#113): the daemon never creates a workspace directory
	// itself — the operator (or installer) selects pre-existing ones —
	// and the only subtree it ever writes under any of them is
	// capsules/. Both facts are enforced here for EVERY mapped
	// workspace, each failure naming its workspace, before any serving.
	for _, ws := range cfg.Mapping.Workspaces() {
		if err := validateWorkspace(ws); err != nil {
			return nil, err
		}
	}
	cfg.fill()

	// addendum 2 (owner ruling A): the dedicated step agent directory is
	// synced at daemon start — the only point that also prunes entries
	// outside the minimal set; the spawner refreshes the allowlisted
	// members before every spawn. A source that is explicitly configured but cannot
	// present its credentials refuses startup; the implicit default
	// without file credentials carries on with a warning.
	rtCfg := runtime.Config{
		MachineID:                cfg.MachineID,
		Mapping:                  cfg.Mapping,
		DataDir:                  cfg.DataDir,
		GitRemoteBase:            cfg.GitRemoteBase,
		WorkflowsDir:             cfg.WorkflowsDir,
		PiBin:                    cfg.PiBin,
		ClaudeBin:                cfg.ClaudeBin,
		HarnessRoot:              cfg.HarnessRoot,
		OperatorAgentDir:         cfg.OperatorAgentDir,
		OperatorAgentDirExplicit: cfg.OperatorAgentDirExplicit,
		EnvSecrets:               []string{cfg.APIKey},
		RestartHubWait:           cfg.RestartHubWait,
	}
	rtCfg.FillDefaults()
	if err := rtCfg.SyncStepAgentDir(); err != nil {
		return nil, err
	}
	// The step processes' session store is the locator's root too: the
	// filled runtime value (the dedicated step-agent-sessions dir, or the
	// operator's explicit override) governs both the child env and the
	// live-transcript scan below.
	cfg.HarnessRoot = rtCfg.HarnessRoot
	// Runtime-sourced diagnostic events (a seal push that failed after
	// its retries, pilot item 3) land in the observe lane's durable
	// events log next to its own entries; the tap reads d.obs at call
	// time, so it is bound below, once the Daemon value is live and
	// before runtime.New consumes the cfg.
	// The production spawner takes the runtime config BY POINTER
	// (runtime.DefaultSpawner): fields bound after assembly (the
	// RecordTap binding below included) are read at spawn time from the
	// shared config.
	sp := cfg.Spawner
	if sp == nil {
		sp = runtime.DefaultSpawner(&rtCfg)
	}
	// v2 (polyforge#106): the Go driver bridge IS the execution engine —
	// bound unconditionally, no selection flag, no fallback. The
	// AgentStarter config knob only swaps WHICH starter binds (tests).
	var d *Daemon // bound below; the starter closure reads it at call time
	if cfg.AgentStarter != nil {
		rtCfg.AgentStarter = cfg.AgentStarter
	} else {
		rtCfg.AgentStarter = func(ctx context.Context, in runtime.AgentStartInput) (runtime.AgentHandle, error) {
			return bridge.Start(ctx, bridge.Config{
				WI:               in.WI,
				CapsuleDir:       in.CapsuleDir,
				BundleName:       in.BundleName,
				SpawnN:           in.SpawnN,
				Spawner:          in.Spawner,
				StepCandidates:   in.StepCandidates,
				InterruptedCause: in.InterruptedCause,
				StepEnded:        in.StepEnded,
				// polyforge#171: the resume decision for claude dispatches
				// locates the native session under the claude-config dir
				// the spawner hard-set (never the daemon's process env).
				ClaudeConfigDir: rtCfg.ClaudeConfigDir(),
				// driver events land in the durable per-WI event log
				// (monotonic ids); the supervision link is the WI's
				// shared driver <-> supervisor channel
				Log:    d.driverLog(),
				Link:   d.sup.Link(in.WI),
				Limits: cfg.Supervision.limits(),
			})
		}
	}

	subs, err := newSubStore(filepath.Join(cfg.DataDir, chatSubsFile))
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	d = &Daemon{
		chats:    &chatStore{path: filepath.Join(cfg.DataDir, chatsFile)},
		subs:     subs,
		cfg:      cfg,
		hub:      cfg.Hub,
		modes:    NewModeCtl(),
		q:        NewJobQueue(cfg.MaxConcurrent, func() map[string]bool { return d.occupiedSlots() }),
		rtMsgs:   supervise.NewQueue[protocol.RTMsg](),
		obMsgs:   supervise.NewQueue[protocol.OBMsg](),
		attachCh: supervise.NewQueue[observe.Attach](),
		sealedWI: map[string]bool{},
		adapters: map[string]*agentAdapter{},
		shutDone: make(chan struct{}),
		sup:      supervise.NewRegistry(),
	}
	d.runCtx, d.cancelRun = context.WithCancel(context.Background())
	rtCfg.EventTap = d.tapRuntimeEvent

	// F6-A: the claim-time memory snapshot seam — the daemon binds it to
	// aihub's memory search (bounded excerpts; the machine credential
	// stays here, never in an agent env). See memface.go. The hit count
	// rides the runtime config (RecallTopK; 0 keeps the default 10).
	rtCfg.RecallTopK = cfg.RecallTopK
	rtCfg.MemoryRecall = recallSnapshot(cfg.Hub)
	rt := runtime.New(rtCfg, cfg.Hub, sp, d.rtMsgs, d.obMsgs)
	obs, err := observe.New(observe.Config{
		RT:           d.attachCh,
		OB:           d.obMsgs,
		DataDir:      cfg.DataDir,
		PollInterval: cfg.ObservePoll,
		PushFunc:     cfg.PushFunc,
		// F3: the live-transcript locator the monitor's per-dispatch
		// commit-boundary sync calls; the F2 slug
		// resolver lets CLI-side route calls by the operator's spelling
		// (a "<project>#<seq>") find the canonically-keyed monitor.
		LocateLive: func(capsuleDir string, spawnAt time.Time, harness, session string) string {
			return runtime.LocateTranscript(rtCfg.StepAgentSessionsDir(), rtCfg.ClaudeConfigDir(), capsuleDir, spawnAt, harness, session)
		},
		Resolve: d.resolveWI,
		// M4 wiring: the periodic session revalidation rides the monitor
		// tick against the shared aihub client (fencing-on-use discovery —
		// a superseded daemon learns within ~5min even with zero push
		// traffic; unit-tested in observe, daemon-wired here).
		SessionVerify: cfg.Hub,
		// the supervisor's bounds pass through; zero fields keep the
		// documented defaults
		Registry:        d.sup,
		ModelErrorGrace: cfg.Supervision.ModelErrorGrace,
		DriverStall:     cfg.Supervision.StallWindow,
		// M4 cadence rides the same pass-through (0 = the 5m default,
		// negative = disabled).
		SessionRevalidateEvery: cfg.Supervision.SessionRevalidateEvery,
	})
	if err != nil {
		return nil, err
	}
	d.rt = rt
	d.obs = obs
	for _, w := range cfg.EnvWarnings {
		_ = obs.Events().Append("", observe.EvRetiredEnvIgnored, map[string]any{"warning": w})
	}
	d.q.persist = d.persistQueue

	// The persisted mode/concurrency bookkeeping (the daemon's OWN state
	// file; jobs/attempts bookkeeping is Runtime's state.json and is
	// never duplicated here).
	if err := d.loadDaemonState(); err != nil {
		return nil, err
	}
	if note := capOverrideNotice(d.q.ExplicitMax(), cfg.MaxConcurrent); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	// cfg.Auto is an EXPLICIT flag override only (#18): nil keeps the
	// persisted switch — an auto daemon restarting stays auto — and
	// loadDaemonState has already defaulted to off when no state file
	// exists.
	if cfg.Auto != nil && *cfg.Auto != d.modes.Auto() {
		if err := d.modes.SetAuto(*cfg.Auto); err != nil {
			return nil, fmt.Errorf("daemon: initial auto: %w", err)
		}
		// The flag override must survive the restart it rode in on:
		// without this persist, `-auto=false` over a persisted auto-on
		// would be undone by the NEXT boot (the fx3 gap).
		d.persistModeQuietly()
	}
	return d, nil
}

// validateWorkspace enforces the startup workspace contract (#103):
// the selected workspace directory must EXIST (stat only — the daemon
// never creates it; the operator or installer owns that) and
// <workspace>/capsules must be creatable and writable — the only place
// inside the user's workspace the daemon ever writes. Each failure is a
// hard startup error naming the workspace path, returned before any
// serving begins.
func validateWorkspace(ws string) error {
	st, err := os.Stat(ws)
	if err != nil {
		return fmt.Errorf("daemon: workspace %s: %w", ws, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("daemon: workspace %s is not a directory", ws)
	}
	capsules := filepath.Join(ws, "capsules")
	if err := os.MkdirAll(capsules, 0o755); err != nil {
		return fmt.Errorf("daemon: workspace %s: capsules/ cannot be created: %w", ws, err)
	}
	probe, err := os.CreateTemp(capsules, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("daemon: workspace %s: capsules/ is not writable: %w", ws, err)
	}
	name := probe.Name()
	probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("daemon: workspace %s: capsules/ write probe residue: %w", ws, err)
	}
	return nil
}

// Run drives the daemon until a shutdown completes (or the caller's
// context ends, which triggers the same shutdown).
// It returns when the process should exit.
func (d *Daemon) Run(ctx context.Context) error {
	// M5: flock the daemon data dir BEFORE the socket probe — the two
	// together are the single-instance guard. The dial probe alone is a
	// TOCTOU: two daemons started in the same window can both observe a
	// dead socket and both bind it; the flock is the kernel-serialized
	// half (LOCK_EX|LOCK_NB on DataDir/daemon.lock), held for Run's
	// lifetime and released when Run returns. Both come before any lane
	// starts: a refused start has nothing to unwind, and only the lock
	// holder ever writes the data dir (see ownsDataDir).
	lock, err := acquireDataDirLock(d.cfg.DataDir)
	if err != nil {
		return err
	}
	defer d.releaseDataLock()
	// Startup askpass sweep (crash residue): a previous daemon killed
	// mid-push leaves one-shot credential scripts — they hold push
	// secrets — under <DataDir>/git/askpass. Only the data-dir lock holder
	// sweeps, so nothing of another daemon can be live there: every entry
	// is residue. Best-effort, non-fatal.
	if err := gitops.SweepAskpass(filepath.Join(d.cfg.DataDir, "git")); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: askpass sweep: %v\n", err)
	}
	// Same for the carrier lane's pf-carrier-* temporary bare repos.
	if err := gitops.SweepCarrierTemps(filepath.Join(d.cfg.DataDir, "git")); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: carrier temp sweep: %v\n", err)
	}
	lst, err := d.listenUDS()
	if err != nil {
		_ = lock.Close()
		return err
	}
	// closeFace and ownsDataDir read both under faceMu: a shutdown
	// arriving in the next instant cannot race the assignment.
	d.faceMu.Lock()
	d.dataLock, d.lst = lock, lst
	d.faceMu.Unlock()

	stop := context.AfterFunc(ctx, d.cancelRun) // the caller giving up stops the loops too
	defer stop()
	go d.rt.Run(d.runCtx)
	go d.obs.Run(d.runCtx)
	go d.bridge()

	// Restart reconciliation (§十一): every recorded job is re-derived
	// against aihub's own verdict — strictly after this process owns
	// the data dir and the socket (no second process may push the same
	// work items), and before the face serves a single request: a client
	// that reaches the daemon must see a reconciled runtime. Only a hung
	// hub cuts the wait short (one reconciliation hub call unanswered for
	// RestartHubWait): the records still unverified are then shown waiting
	// for aihub, refuse their closes meanwhile, and the runtime finishes
	// them (announcing what it brings back) once the hub answers.
	if err := d.rt.Restart(d.runCtx); err != nil {
		// The runtime could not be reconciled as a whole (a missing
		// capsule or an unmapped project is NOT such a case — v2-converge
		// §6 blocks only that work item at aihub and Restart returns nil).
		// Stop without serving or persisting: state.json stays the
		// pre-reconciliation truth for the next start, after the manual
		// remediation the error names; the process exits non-zero.
		d.closeFace()
		d.cancelRun()
		d.rt.Close()
		for _, ji := range d.rt.List() {
			if ji.Agent != nil {
				_ = ji.Agent.Terminate(10 * time.Second)
			}
		}
		return &needsOperatorError{fmt.Errorf("restart reconciliation failed: %w", err)}
	}
	d.reattachRecovered()
	// v2 §5/§6: replay the persisted submit queue (after the B1 claiming
	// journal replay inside Restart, so a WI is never claimed twice) and
	// tell every recovered/parked WI the daemon restarted.
	d.replaySubmitQueue()
	d.announceRestart()

	serveErr, serving := d.startFace(lst)
	if !serving {
		// A shutdown closed the face while this process was reconciling:
		// nothing is left to drive.
		return nil
	}

	dispatch := time.NewTicker(d.cfg.DispatchTick)
	defer dispatch.Stop()
	drain := time.NewTicker(d.cfg.DrainInterval)
	defer drain.Stop()
	// M3 heartbeat: its own goroutine on runCtx (the shutdown cancels it)
	// so liveness never blocks or is blocked by the dispatch/drain loop.
	// Log-only failures inside; never fatal.
	go d.heartbeatLoop(d.runCtx)
	for {
		select {
		case <-ctx.Done():
			// The caller gave up: run the same shutdown.
			return d.Shutdown(context.Background(), "caller-context: the Run caller's context is done")
		case <-d.shutDone:
			return nil
		case err := <-serveErr:
			if err != nil && err != http.ErrServerClosed {
				_ = d.Shutdown(context.Background(), fmt.Sprintf("serve-error: %v", err))
				return err
			}
			// Only the shutdown closes the face, and it still has the
			// runtime's Close to run: wait for it.
			<-d.shutDone
			return nil
		case <-dispatch.C:
			d.dispatch()
		case <-drain.C:
			d.fetchAutoWave()
		}
	}
}

// startFace begins serving the UDS face on lst. It reports false
// when a shutdown already closed the face (the shutdown-
// during-reconciliation window): the listener is then dead and Run
// must not serve on it. The srv assignment happens under faceMu so a
// concurrent closeFace either sees this server (and closes it, making
// Serve answer ErrServerClosed) or has already latched faceClosed.
func (d *Daemon) startFace(lst net.Listener) (chan error, bool) {
	d.faceMu.Lock()
	defer d.faceMu.Unlock()
	if d.faceClosed {
		return nil, false
	}
	d.srv = &http.Server{Handler: d.handler(), ConnContext: credConnContext}
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.srv.Serve(lst) }()
	return serveErr, true
}

// closeFace tears the UDS face down (the shutdown's last step): the serving
// server if Run started it, otherwise the bare listener (a shutdown
// that raced the startup reconciliation window, before Serve began).
// Idempotent under the mutex; a second close is a no-op.
func (d *Daemon) closeFace() {
	d.faceMu.Lock()
	defer d.faceMu.Unlock()
	if d.faceClosed {
		return
	}
	d.faceClosed = true
	if d.srv != nil {
		_ = d.srv.Close()
		return
	}
	if d.lst != nil {
		_ = d.lst.Close()
	}
}

// reattachRecovered synthesizes the observe attachments for jobs the
// restart reconciliation re-derived: their RTRecovered messages found
// no live monitor (a fresh daemon has none), so the daemon re-attaches
// them through the same RTStarted shape observe builds monitors on.
func (d *Daemon) reattachRecovered() {
	for _, ji := range d.rt.List() {
		if !d.rt.RestartAttach(ji.WorkItem) {
			continue // the runtime's reconciliation lane announces it itself
		}
		d.reattachJob(ji)
	}
}

// reattachJob synthesizes ONE job's observe attachment through the
// RTStarted shape. A retained-paused job's monitor was released at the
// pause close (RTFinished), and the in-place resume's RTRecovered
// alone cannot rebuild it — observe drops a recovered message for an
// unmonitored work item (a fresh daemon's restart reconciliation has
// the same shape, which is why reattachRecovered exists). The monitor
// this creates reads the capsule's execution.json itself, so the
// pending-question projection (and the M4 revalidation) come back with
// it.
func (d *Daemon) reattachJob(ji runtime.JobInfo) {
	if ji.Agent == nil {
		return
	}
	d.noteAttached(ji.WorkItem, true)
	d.attachCh.Push(d.buildAttach(protocol.RTMsg{
		Kind: protocol.RTStarted, SpawnN: ji.SpawnN,
		WorkItem: ji.WorkItem, AgentPID: ji.AgentPID,
		// the in-process secret re-arms the M4 revalidation for
		// reattached jobs (it rides RTMsg, never a log surface).
		SessionID: ji.SessionID,
	}, ji))
}

// buildAttach turns one runtime snapshot into an observe attachment:
// the protocol message plus the agent handle adapted to observe's face.
func (d *Daemon) buildAttach(msg protocol.RTMsg, ji runtime.JobInfo) observe.Attach {
	return observe.Attach{
		Msg:    msg,
		Handle: d.installAdapter(ji),
		WI: observe.WIInfo{
			WorkItem:   ji.WorkItem,
			Project:    ji.Project,
			CapsuleDir: ji.CapsuleDir,
			BundleName: ji.BundleName,
			Git:        ji.Git,
		},
	}
}

// tapRuntimeEvent lands one runtime-sourced diagnostic event in the
// observe lane's durable events log (the seal push_failed trace).
func (d *Daemon) tapRuntimeEvent(wi, kind string, body map[string]any) {
	if d.obs == nil {
		return
	}
	_ = d.obs.Events().Append(wi, kind, body)
}

// resolveWI maps any work-item id form to the canonical id the runtime
// and observe key everything by (F2): the runtime's alias table first
// (live jobs), aihub's id-or-slug endpoint otherwise (sealed or unknown
// work items). "" only when both paths fail — callers degrade to the
// input spelling, which is the pre-F2 behavior.
func (d *Daemon) resolveWI(wi string) string {
	if c := d.rt.Resolve(wi); c != "" {
		return c
	}
	item, err := d.hub.GetWorkItem(wi)
	if err != nil || item == nil || item.ID == "" {
		return ""
	}
	return item.ID
}

// canonWI is resolveWI with the pass-through fallback: an unresolvable
// spelling keeps flowing exactly as it did before F2 (the claim path
// resolves it server-side).
func (d *Daemon) canonWI(wi string) string {
	if c := d.resolveWI(wi); c != "" {
		return c
	}
	return wi
}

// bridge translates Runtime's RTMsg stream into observe.Attach values:
// the handle and per-WI facts ride WITH the lifecycle message (the
// observe lane's attachment contract; the five message kinds stay the
// only control vocabulary).
func (d *Daemon) bridge() {
	for {
		for {
			msg, ok := d.rtMsgs.Pop()
			if !ok {
				break
			}
			if msg.Kind == protocol.RTRecovered && !d.isAttached(msg.WorkItem) {
				if _, monitored := d.obs.Progress(msg.WorkItem); !monitored {
					// a recovery of a job no monitor watches yet, nor one
					// whose RTStarted attachment is still on its way —
					// the restart reconciliation's, a deferred verify's, a
					// live resume's: its RTStarted attachment (the startup
					// re-attachment, the runtime's own announcement, the
					// resume route) builds the monitor; observe would only
					// log the message as dropped
					continue
				}
			}
			at := observe.Attach{Msg: msg}
			if msg.Kind == protocol.RTStarted || msg.Kind == protocol.RTRecovered {
				if ji, live := d.rt.Info(msg.WorkItem); live && ji.Agent != nil {
					at = d.buildAttach(msg, ji)
					if msg.Kind == protocol.RTStarted {
						d.noteAttached(msg.WorkItem, true)
					}
				}
			}
			if msg.Kind == protocol.RTFinished {
				d.noteAttached(msg.WorkItem, false)
			}
			d.attachCh.Push(at)
		}
		select {
		case <-d.runCtx.Done():
			return
		case <-d.rtMsgs.Notify():
		}
	}
}

// noteAttached records (or, on RTFinished, forgets) that wi's
// monitor-building attachment went to the observe lane.
func (d *Daemon) noteAttached(wi string, on bool) {
	d.attachedMu.Lock()
	defer d.attachedMu.Unlock()
	if !on {
		delete(d.attached, wi)
		return
	}
	if d.attached == nil {
		d.attached = map[string]bool{}
	}
	d.attached[wi] = true
}

func (d *Daemon) isAttached(wi string) bool {
	d.attachedMu.Lock()
	defer d.attachedMu.Unlock()
	return d.attached[wi]
}

// ---------------------------------------------------------------------------
// The agent adapter: runtime.AgentHandle → observe.Handle. Runtime owns
// the process; Observe owns the watching contract. The adapter bridges
// the two exported faces without either lane knowing the other.

type agentAdapter struct {
	h        runtime.AgentHandle
	exit     chan struct{}
	pollStop chan struct{}
	retire   sync.Once
}

// installAdapter registers the CURRENT handle of a work item and returns
// its observe face. A previous adapter is retired (its exit poll stops).
func (d *Daemon) installAdapter(ji runtime.JobInfo) *agentAdapter {
	a := &agentAdapter{
		h:        ji.Agent,
		exit:     make(chan struct{}),
		pollStop: make(chan struct{}),
	}
	d.adapMu.Lock()
	if old := d.adapters[ji.WorkItem]; old != nil {
		old.retireAdapter()
	}
	d.adapters[ji.WorkItem] = a
	d.adapMu.Unlock()
	go a.pollDone(d.cfg.ExitPoll)
	return a
}

func (a *agentAdapter) retireAdapter() {
	a.retire.Do(func() { close(a.pollStop) })
}

// pollDone synthesizes the exit signal from the runtime handle's Done
// poll (runtime.AgentHandle exposes no channel face; the poll is the
// adapter's price for staying inside the exported surface).
func (a *agentAdapter) pollDone(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.pollStop:
			return
		case <-t.C:
			if a.h.Done() {
				close(a.exit)
				return
			}
		}
	}
}

func (a *agentAdapter) Exit() <-chan struct{} { return a.exit }

// driverLog is the driver's EventLogger: it resolves the observe event
// log at call time (the log exists only after observe.New).
func (d *Daemon) driverLog() driver.EventLogger { return driverEventLog{d} }

type driverEventLog struct{ d *Daemon }

func (l driverEventLog) Append(wi, kind string, body any) error {
	if l.d.obs == nil {
		return nil
	}
	return l.d.obs.Events().Append(wi, kind, body)
}

// ---------------------------------------------------------------------------
// Dispatcher (reference.md §6.3 backfill) and auto-pull loop (reference.md §6.3).

// occupiedSlots is the set of WIs holding a concurrency slot: the
// runtime's live jobs, whoever started them (dispatch, restart recovery,
// resume-blocked). Finding 2 (v1.5 review): a RETAINED paused job
// holds no agent and no writer — it must not pin a slot (four
// operator-paused jobs would otherwise starve the whole daemon).
func (d *Daemon) occupiedSlots() map[string]bool {
	live := map[string]bool{}
	for _, ji := range d.rt.List() {
		if ji.State == runtime.StatePaused {
			continue
		}
		live[ji.WorkItem] = true
	}
	return live
}

// dispatch starts pending jobs into free slots, priority-desc then FIFO.
// Each claim runs in its own goroutine: a slow capsule or task-repo
// clone does not hold the Run loop, so the loop keeps serving shutdown,
// drain and the next dispatch ticks. The queue counts a Take()n entry
// against the slots until it settles, so the concurrency cap holds across
// the goroutines.
func (d *Daemon) dispatch() {
	for !d.modes.ShuttingDown() {
		e := d.q.Take()
		if e == nil {
			return
		}
		// A claim that races the shutdown latch defers itself in the
		// runtime (BeginShutdown); nothing here waits for it.
		go d.startEntry(*e)
	}
}

// startEntry runs one Take()n queue entry's claim to its verdict and
// settles the entry.
func (d *Daemon) startEntry(e qEntry) {
	d.rt.NoteOrigin(e.WorkItemID, e.Origin)
	d.rt.NoteModels(e.WorkItemID, e.Models)
	err := d.rt.Start(d.runCtx, e.WorkItemID)
	if runtime.IsClaimDeferred(err) {
		// the shutdown stopped the claim: the entry stays in flight on
		// disk (the queue file keeps it) and a claim that landed stays
		// in the claiming journal — the next start replays them.
		return
	}
	if errors.Is(err, runtime.ErrClaimClosed) {
		// an operator close settled the claim (Stop during CLAIMING)
		if serr := d.q.Settle(e.WorkItemID); serr != nil {
			fmt.Fprintf(os.Stderr, "daemon: submit queue persist after %s: %v\n", e.WorkItemID, serr)
		}
		return
	}
	var ae *api.Error
	if errors.As(err, &ae) && ae.Code == api.ErrDaemonHubUnavailable {
		// The hub could not be reached, or the claim's answer was lost: the
		// accepted submit stays queued and is retried with backoff (never
		// dropped on a transient blip, never blocking the other entries). A
		// claim that may have landed keeps its claiming-journal entry, so
		// the retry replays the SAME claim and adopts the session.
		// One log line per episode.
		if e.Deferrals == 0 {
			fmt.Fprintf(os.Stderr, "daemon: start %s deferred, will retry with backoff: %v\n", e.WorkItemID, err)
		}
		if rerr := d.q.Requeue(e.WorkItemID); rerr != nil {
			fmt.Fprintf(os.Stderr, "daemon: submit queue persist after %s: %v\n", e.WorkItemID, rerr)
		}
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: start %s failed: %v\n", e.WorkItemID, err)
		// The submit was answered `accepted`: record the refusal as a
		// durable event on the WI so watchers can surface it (v2 §5).
		d.recordSubmitRejected(e, err)
	}
	if serr := d.q.Settle(e.WorkItemID); serr != nil {
		fmt.Fprintf(os.Stderr, "daemon: submit queue persist after %s: %v\n", e.WorkItemID, serr)
	}
}

// fetchAutoWave pulls the ready queue and enqueues what is claimable
// (reference.md §6.3): only with auto on, never once shutting_down latched, and
// never beyond the concurrency it can actually start — the wave's
// free-slot budget is spent per enqueued entry (audit B3a: one wave
// enqueues at most the CURRENT free-slot count; the rest of the ready
// queue stays on aihub for the next wave) and is reduced by the entries
// ALREADY queued — including ones held back in their DAEMON-503
// backoff: they occupy no running slot yet, but they are waiting for
// one, so a wave that ignored them could push the pending set past
// max_concurrent. The auto switch is RE-CHECKED before each enqueue:
// the wave's ready-queue fetch is a batch of HTTP round trips, and a
// `pf ops auto off` landing mid-fetch must stop THIS wave too — the
// switch flip's purge (autoOffPurge) only sees entries already landed.
// Authorization-class denials raise the denial signal — a typed
// queue_fetch_denied event in the observe lane's durable log, once per
// healthy→denied episode (the flag clears on the next successful
// fetch); transport-class losses stay silent idle.
func (d *Daemon) fetchAutoWave() {
	if !d.modes.AdmitAutoPull() {
		return
	}
	// all pending entries (backoff included) are waiting for a slot:
	// the wave may only enqueue what is left of the concurrency cap.
	budget := d.q.FreeSlots() - d.q.LenPending()
	if budget <= 0 {
		return
	}
	// polyforge#113: no daemon-level project scope exists — the drain
	// wave iterates ALL mapped projects. A per-project authorization-class
	// denial raises the denial signal naming THAT project; healthy
	// projects still enqueue (partial coverage beats none, and the
	// denial episode is visible).
	var items []api.WorkItem
	denied := false
	for _, project := range d.cfg.Mapping.Projects() {
		if _, lerr := d.rt.LoadWorkspace(project); lerr != nil {
			if d.yamlRefused == nil {
				d.yamlRefused = map[string]string{}
			}
			if d.yamlRefused[project] != lerr.Error() {
				d.yamlRefused[project] = lerr.Error()
				fmt.Fprintf(os.Stderr, "daemon: drain skips project %q (nothing claimed): %v\n", project, lerr)
			}
			continue
		}
		delete(d.yamlRefused, project)
		pis, err := d.hub.Queue(project)
		if err != nil {
			if authDenied(err) {
				denied = true
				fmt.Fprintf(os.Stderr, "daemon: auto-pull fetch denied: ready queue of project %q refused authorization (%v)\n", project, err)
				if !d.queueFetchDenied && d.obs != nil {
					d.queueFetchDenied = true
					_ = d.obs.Events().Append("", observe.EvQueueFetchDenied, map[string]any{
						"project": project,
						"class":   "authorization",
						"error":   err.Error(),
					})
				}
			}
			continue
		}
		items = append(items, pis...)
	}
	if !denied {
		d.queueFetchDenied = false // episode cleared: this fetch fully succeeded
	}
	var badBundles map[string]error
	if len(items) > 0 {
		badBundles = d.rt.RefusedBundles()
	}
	for name := range d.bundleRefused {
		if _, still := badBundles[name]; !still {
			delete(d.bundleRefused, name)
		}
	}
	for _, it := range items {
		if budget == 0 {
			break // the wave spent its slots; the next wave claims the rest
		}
		if it.BundleName == nil {
			continue // registration tickets never execute
		}
		if berr, bad := badBundles[*it.BundleName]; bad {
			// polyforge#175: the bundle fails to load (e.g. an unprefixed
			// candidate) — a claim would be refused; leave the WI queued
			// for other machines and log the refusal once
			if d.bundleRefused == nil {
				d.bundleRefused = map[string]string{}
			}
			if d.bundleRefused[*it.BundleName] != berr.Error() {
				d.bundleRefused[*it.BundleName] = berr.Error()
				fmt.Fprintf(os.Stderr, "daemon: drain skips work items on bundle %q (nothing claimed): %v\n", *it.BundleName, berr)
			}
			continue
		}
		if d.q.IsRunning(it.ID) || d.q.IsPending(it.ID) {
			continue
		}
		// auto off may land while this wave was fetching (its round trips
		// are the window): the switch is re-checked under the queue lock
		// the auto-off purge takes, so an entry either sees auto off or
		// lands before the purge and is dropped by it (audit B3 race,
		// wi_seWTgHeKtaa8).
		ok, err := d.q.EnqueueIf(qEntry{WorkItemID: it.ID, Priority: it.Priority, Origin: runtime.Origin{Kind: runtime.OriginAuto}}, d.modes.AdmitAutoPull)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: drain enqueue %s: queue persist failed: %v\n", it.ID, err)
			continue // nothing landed: the budget is not spent
		}
		if !ok {
			return // auto is off: nothing more may be enqueued
		}
		budget--
	}
}

// autoOffPurge removes the submit queue's not-yet-started origin=auto
// entries (v2-converge §5: auto off = only explicit pf submit; audit
// B3b) — they would otherwise start later as slots free (dispatch runs
// regardless of the switch) and replay after a restart. Each dropped
// entry lands a submit_dropped event; a persist failure is loud and
// leaves the queue as it was (the switch itself already flipped).
func (d *Daemon) autoOffPurge() {
	dropped, err := d.q.PurgeOrigin(runtime.OriginAuto)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: auto-off queue purge failed: %v\n", err)
		return
	}
	for _, e := range dropped {
		fmt.Fprintf(os.Stderr, "daemon: auto off: pending %s dropped from the submit queue\n", e.WorkItemID)
		if d.obs != nil {
			body := map[string]any{
				"reason":       "auto off",
				"submitted_at": e.SubmittedAt.UTC().Format(time.RFC3339Nano),
			}
			if e.Origin.Kind != "" {
				body["origin"] = e.Origin
			}
			_ = d.obs.Events().Append(e.WorkItemID, observe.EvSubmitDropped, body)
		}
	}
}

// authDenied classifies a ready-queue error as authorization-class: a
// typed aihub/api error whose code or derived HTTP status is 401/403/404
// (404 is the server folding a non-member's canRead denial). Everything
// else — transport loss, 5xx — is connection-class (drain's silent idle).
func authDenied(err error) bool {
	var ae *api.Error
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.HTTPStatus() {
	case 401, 403, 404:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// UDS listener

func (d *Daemon) listenUDS() (net.Listener, error) {
	path := d.cfg.SocketPath
	// Single-instance lock: a live listener on the socket is another
	// daemon; a dead one is a stale socket file to remove.
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, fmt.Errorf("daemon: another daemon is listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	lst, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o660)
	// credListener (polyforge#110): capture each accepted conn's UDS
	// peer credential at accept time so the POST /shutdown route can
	// attribute the shutdown to the dialing process. The wrapper is
	// transparent otherwise; closeFace closes it like any listener.
	return &credListener{Listener: lst}, nil
}

// ---------------------------------------------------------------------------
// Seal guard (the wrap dual-path arbitration).

func (d *Daemon) claimSeal(wi string) bool {
	d.sealMu.Lock()
	defer d.sealMu.Unlock()
	if d.sealedWI[wi] {
		return false
	}
	d.sealedWI[wi] = true
	return true
}

func (d *Daemon) releaseSeal(wi string) {
	d.sealMu.Lock()
	delete(d.sealedWI, wi)
	d.sealMu.Unlock()
}

// terminalStatus reports whether an aihub status is terminal for the
// wrap/pause arbitration's graceful-already-terminal answer.
func terminalStatus(s string) bool {
	switch s {
	case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
		return true
	}
	return false
}

// mapCloseErr translates a Runtime.Stop failure for the HTTP face:
// a typed api error passes through (CORE-* transparency), a lost race
// ("no active job") maps to the graceful already-terminal answer, and
// everything else is DAEMON-500.
func (d *Daemon) mapCloseErr(err error) (graceful bool, ae *api.Error) {
	if strings.Contains(err.Error(), "no active job") {
		return true, nil
	}
	if errors.As(err, &ae) {
		return false, ae
	}
	return false, &api.Error{Code: api.ErrDaemonInternal, Detail: err.Error()}
}

// ---------------------------------------------------------------------------
// M5: the data-dir flock (single-instance guard, kernel half).

// releaseDataLock drops the data-dir flock (Run returning is the
// daemon's end; the next process may take over the data dir).
// ownsDataDir reports whether this process holds daemon.lock: only the
// holder writes the data dir's state (a start the lock refused never
// seeded its runtime and must not overwrite the running daemon's).
func (d *Daemon) ownsDataDir() bool {
	d.faceMu.Lock()
	defer d.faceMu.Unlock()
	return d.dataLock != nil
}

func (d *Daemon) releaseDataLock() {
	d.faceMu.Lock()
	defer d.faceMu.Unlock()
	if d.dataLock != nil {
		_ = d.dataLock.Close()
		d.dataLock = nil
	}
}

// acquireDataDirLock takes an exclusive non-blocking flock on
// <DataDir>/daemon.lock and answers the held file (the caller keeps it
// open for the daemon's lifetime; the kernel releases it at exit). A
// held lock is another daemon on this data dir — the same machine's
// second instance, the TOCTOU the socket probe alone cannot close.
func acquireDataDirLock(dataDir string) (*os.File, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid, _ := os.ReadFile(path)
		f.Close()
		holder := ""
		if p := strings.TrimSpace(string(pid)); p != "" {
			holder = " (pid " + p + ")"
		}
		return nil, fmt.Errorf("daemon: another daemon%s holds the data-dir lock %s", holder, path)
	}
	// best-effort pid stamp (diagnostics; the lock itself is the guard)
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}
