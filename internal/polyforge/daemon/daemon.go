// Package daemon is the phase-3 assembly of the v1 local runtime: the
// UDS HTTP face the pf CLI speaks (internal/common/protocol route
// family, pinned by internal/polyforge/cli/daemon.go), the Runtime
// (lifecycle) and Observe (supervision) modules wired over the five
// protocol messages, and the machine-level operations — the three
// admission modes, the pending-job dispatcher, the drain loop, the ship
// delivery bridge and the graceful shutdown sequence.
//
// Design authority: docs/design/polyforge-vnext/v1-architecture-spec.md
// (rulings C1–C3), v1-spec-full.md (§一 §二 §五 §十一), 04-local-runtime.md
// (§3 modes, §5 routes/error codes, §6.1 admission, §6.5 shutdown — the
// v0-era publish machinery it describes is deleted per C2/C3; flush is
// the only archival verb and Observe, not a publish loop, executes it).
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
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// Hub is the aihub face the daemon itself needs beyond Runtime's
// session-claim/end use: the ready-queue projection (batch recommendation,
// drain loop), the terminal-state check of the wrap/pause arbitration,
// and — through the generic Do — the memory faces the F6 lanes proxy
// through (search with an excerpt, memory create). *aihubclient.Client
// satisfies it.
type Hub interface {
	runtime.Aihub
	Queue(project string) ([]api.WorkItem, error)
	// SessionVerify is the v1.5 M4 compare-only holder check — the
	// periodic per-RUNNING-job revalidation the observe monitor
	// piggybacks on its tick. *aihubclient.Client satisfies it.
	SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error)
	// Do performs one typed request against aihub (the client's own
	// generic entry): the F6 memory adapters use it for the calls the
	// shared client carries no typed method for.
	Do(method, path string, body, out any) error
}

// Config wires the daemon. Zero fields take the defaults in fill().
type Config struct {
	MachineID          string
	SocketPath         string // UDS listen path (default /run/polyforge.sock)
	WorkspaceRoot      string // <root>/capsules/<wi> are the capsule worktrees
	DataDir            string // runtime state.json + events.log + daemon-state.json
	Project            string // ready-queue scope for batch/drain
	GitRemoteBase      string // <base>/<wi>.git are the aihub bare repos
	WorkflowsDir       string // daemon checkout's workflows/
	ExecutorPromptFile string
	PiBin              string
	// HarnessSessionsRoot overrides the harness's default session store
	// for the live-transcript locator (F3); "" = ~/.pi/agent/sessions.
	HarnessSessionsRoot string
	CgroupRoot          string
	APIKey              string // machine credential; NEVER reaches an agent env
	MaxConcurrent       int    // cross-mode concurrency cap (default 4)
	// RecallTopK is the F6-A claim-time memory snapshot's hit count
	// (0 = the runtime default, 10; bounded at 50 there).
	RecallTopK int
	Mode       string // initial mode; "" = not specified (persisted mode wins; default interactive)
	Harness    string // harness adapter name (default "pi")

	// Test/ops knobs.
	Spawner     runtime.Spawner  // nil = the production pi spawner
	Hub         Hub              // required; aihubclient.Client satisfies
	PushFunc    observe.PushFunc // nil = the shared gitops choke point
	ObservePoll time.Duration    // observe execution.json poll tick
	// Observe carries the observe lane's supervision windows into the
	// assembly — the injection seam the fault-injection scenarios shrink
	// (hang watchdog, idle watchdog, liveness-probe bound, answer-ack
	// window). Zero fields keep the observe defaults
	// (10min / 10min / 2min / 2min).
	Observe           ObserveWindows
	DrainInterval     time.Duration // drain wave-fetch period (5s)
	DispatchTick      time.Duration // dispatcher/backfill period (200ms)
	NoticeAckBound    time.Duration // stop_notice ack wait (30s)
	ShutdownFlushWait time.Duration // per-job flush(shutdown) wait (60s)
	ShutdownBudget    time.Duration // whole-shutdown ceiling (5min)
	ExitPoll          time.Duration // adapter exit-poll period (200ms)
}

// ObserveWindows is the observe-lane supervision-window subset of the
// observe config that daemon.Config passes through. All four default
// to zero — observe's own fill() then applies the production defaults —
// so a production Config with an untouched Observe field assembles the
// production windows.
type ObserveWindows struct {
	// HangWindow is the stream-silence window while a turn is in flight
	// that arms the liveness probe.
	HangWindow time.Duration
	// IdleWindow is the no-record-no-turn quiet window that arms the
	// idle watchdog's liveness probe.
	IdleWindow time.Duration
	// ProbeTimeout bounds one liveness probe's ack wait.
	ProbeTimeout time.Duration
	// AnswerAckWindow bounds the agent's pf/answer consumption ack.
	AnswerAckWindow time.Duration
}

func (c *Config) fill() {
	if c.SocketPath == "" {
		c.SocketPath = "/run/polyforge.sock"
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.Harness == "" {
		c.Harness = "pi"
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
	if c.NoticeAckBound == 0 {
		c.NoticeAckBound = 30 * time.Second
	}
	if c.ShutdownFlushWait == 0 {
		c.ShutdownFlushWait = 60 * time.Second
	}
	if c.ShutdownBudget == 0 {
		c.ShutdownBudget = 5 * time.Minute
	}
	if c.ExitPoll == 0 {
		c.ExitPoll = 200 * time.Millisecond
	}
}

// Daemon is the assembled local runtime.
type Daemon struct {
	cfg   Config
	hub   Hub
	modes *ModeCtl
	q     *jobQueue

	rt  *runtime.Runtime
	obs *observe.Observe

	rtMsgs   chan protocol.RTMsg
	obMsgs   chan protocol.OBMsg
	attachCh chan observe.Attach

	// sealGuard serializes the two close paths (CLI wrap/pause vs the
	// agent's flush(wrap)): exactly one close may run per work item; the
	// late arrival answers DAEMON-409 (the wrap dual-path arbitration).
	sealMu   sync.Mutex
	sealedWI map[string]bool

	obsCtx    context.Context
	cancelObs context.CancelFunc
	obsDone   chan struct{} // closed when observe.Run (and every monitor) exited
	runCtx    context.Context
	cancelRun context.CancelFunc
	rtDone    chan struct{} // closed when the runtime message loop exited

	shutOnce    sync.Once
	shutDone    chan struct{} // closed when the FIRST shutdown sequence finished
	shutMu      sync.Mutex
	shutdownErr error

	// drainDegraded is the drain degraded-signal episode flag: set when
	// the first drain_degraded event of a silent→degraded transition has
	// been emitted, cleared on the next successful wave fetch. Only the
	// drain ticker goroutine touches it (fetchDrainWave), so it carries
	// no lock.
	drainDegraded bool

	adapMu   sync.Mutex
	adapters map[string]*agentAdapter

	lst net.Listener
	srv *http.Server

	// faceMu guards srv and the faceClosed latch: Run assembles and
	// starts serving the face only after restart reconciliation, so an
	// external Shutdown arriving inside that window runs concurrently
	// with Run's face setup — the latch lets whichever side loses the
	// race tear the listener down instead of serving on a face the
	// shutdown sequence already closed.
	faceMu     sync.Mutex
	faceClosed bool

	// faceOwned latches once this process bound the UDS face (the
	// single-instance guard passed): only a daemon that owns the socket
	// may rewrite the shared on-disk bookkeeping — a startup abort must
	// never persist its (never-seeded) view over the running daemon's
	// state.
	faceOwned bool

	// dataLock is the M5 flock on the daemon data dir (held for the
	// process's lifetime, released by the kernel at exit): the
	// same-machine dual-daemon TOCTOU guard ALONGSIDE the socket probe.
	// The probe alone races (both processes can observe a dead socket
	// and both bind); the flock is the kernel-serialized half.
	dataLock *os.File
}

// New validates and assembles the daemon. Restart reconciliation is NOT
// run here — Run does it once its goroutines are live.
func New(cfg Config) (*Daemon, error) {
	if cfg.Hub == nil {
		return nil, errors.New("daemon: Config.Hub is required")
	}
	if cfg.DataDir == "" || cfg.WorkspaceRoot == "" {
		return nil, errors.New("daemon: DataDir and WorkspaceRoot are required")
	}
	cfg.fill()

	rtCfg := runtime.Config{
		MachineID:           cfg.MachineID,
		WorkspaceRoot:       cfg.WorkspaceRoot,
		DataDir:             cfg.DataDir,
		GitRemoteBase:       cfg.GitRemoteBase,
		WorkflowsDir:        cfg.WorkflowsDir,
		Harness:             cfg.Harness,
		PiBin:               cfg.PiBin,
		HarnessSessionsRoot: cfg.HarnessSessionsRoot,
		CgroupRoot:          cfg.CgroupRoot,
		SocketPath:          cfg.SocketPath, // the spawned agent's PF_DAEMON_SOCKET (flush proxy, §五)
		EnvSecrets:          []string{cfg.APIKey},
		ExecutorPromptFile:  cfg.ExecutorPromptFile,
	}
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

	d := &Daemon{
		cfg:      cfg,
		hub:      cfg.Hub,
		modes:    NewModeCtl(),
		q:        NewJobQueue(cfg.MaxConcurrent),
		rtMsgs:   make(chan protocol.RTMsg, 64),
		obMsgs:   make(chan protocol.OBMsg, 64),
		attachCh: make(chan observe.Attach, 64),
		sealedWI: map[string]bool{},
		adapters: map[string]*agentAdapter{},
		shutDone: make(chan struct{}),
	}
	rtCfg.EventTap = d.tapRuntimeEvent

	rtCfg.RecordTap = d.tapRecord
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
		// F3: the live-transcript locator the monitor's commit-boundary
		// sync calls when the runtime recorded no live path; the F2 slug
		// resolver lets CLI-side route calls by the operator's spelling
		// (a "<project>#<seq>") find the canonically-keyed monitor.
		LocateLive: func(capsuleDir string, spawnAt time.Time) string {
			return runtime.LocateLiveTranscript(cfg.HarnessSessionsRoot, capsuleDir, spawnAt)
		},
		Resolve: d.resolveWI,
		// M4 wiring: the periodic session revalidation rides the monitor
		// tick against the shared aihub client (fencing-on-use discovery —
		// a superseded daemon learns within ~5min even with zero push
		// traffic; unit-tested in observe, daemon-wired here).
		SessionVerify: cfg.Hub,
		// F6-B: the narrow memory channel's proxy seam — recall/remember
		// over the UDS face, all daemon-side against aihub (memface.go).
		MemoryFace: memFace{hub: cfg.Hub},
		// Bug-4 seam: the supervision windows pass through; zero fields
		// keep the observe defaults.
		HangWindow:      cfg.Observe.HangWindow,
		IdleWindow:      cfg.Observe.IdleWindow,
		ProbeTimeout:    cfg.Observe.ProbeTimeout,
		AnswerAckWindow: cfg.Observe.AnswerAckWindow,
	})
	if err != nil {
		return nil, err
	}
	d.rt = rt
	d.obs = obs

	// The persisted mode/concurrency bookkeeping (the daemon's OWN state
	// file; jobs/attempts bookkeeping is Runtime's state.json and is
	// never duplicated here).
	if err := d.loadDaemonState(); err != nil {
		return nil, err
	}
	// cfg.Mode is an EXPLICIT flag override only (#18): unset ("") keeps
	// the persisted mode — a drain daemon restarting stays drain — and
	// loadDaemonState has already defaulted to interactive when no state
	// file exists. An explicit flag equal to the loaded mode is a no-op
	// (Set would refuse drain→drain as a non-exit transition).
	if cfg.Mode != "" && cfg.Mode != d.modes.Current() {
		if err := d.modes.Set(cfg.Mode); err != nil {
			return nil, fmt.Errorf("daemon: initial mode %q: %w", cfg.Mode, err)
		}
		// The flag override must survive the restart it rode in on:
		// without this persist, a `--mode interactive` that overrides a
		// persisted drain stays interactive in memory only — the NEXT boot
		// would load the stale drain again (the fx3 gap).
		d.persistModeQuietly()
	}
	return d, nil
}

// Run drives the daemon until a graceful shutdown completes (or the
// caller's context ends, which triggers the same shutdown sequence).
// It returns when the process should exit.
func (d *Daemon) Run(ctx context.Context) error {
	// Startup askpass sweep (crash residue): a previous daemon killed
	// mid-push leaves one-shot credential scripts — they hold push
	// secrets — under <DataDir>/git/askpass. Nothing can be live this
	// early, so every entry there is residue. Best-effort, non-fatal.
	if err := gitops.SweepAskpass(filepath.Join(d.cfg.DataDir, "git")); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: askpass sweep: %v\n", err)
	}
	d.runCtx, d.cancelRun = context.WithCancel(ctx)
	d.obsCtx, d.cancelObs = context.WithCancel(d.runCtx)

	d.rtDone = make(chan struct{})
	go func() { d.rt.Run(d.runCtx); close(d.rtDone) }()
	d.obsDone = make(chan struct{})
	go func() { d.obs.Run(d.obsCtx); close(d.obsDone) }()
	go d.bridge()

	// M5: flock the daemon data dir BEFORE the socket probe — the two
	// together are the single-instance guard. The dial probe alone is a
	// TOCTOU: two daemons started in the same window can both observe a
	// dead socket and both bind it; the flock is the kernel-serialized
	// half (LOCK_EX|LOCK_NB on DataDir/daemon.lock), held for Run's
	// lifetime and released when Run returns.
	lock, lerr := acquireDataDirLock(d.cfg.DataDir)
	if lerr != nil {
		d.abortStartup()
		return lerr
	}
	d.dataLock = lock
	defer d.releaseDataLock()

	// Single-instance guard FIRST — before ANY push lane can open. The
	// UDS face is the one cross-process serialization point this data-dir
	// has: an old daemon mid-graceful-shutdown keeps its listener (it
	// closes only at step 5 of the sequence) while its in-flight
	// recovery pushes are still running, so dialing the socket here is
	// the only reliable "is the old process gone yet?" check. Restart
	// reconciliation used to run BEFORE this guard, which let a daemon
	// started in that window push the same work items from a second
	// process — the per-WI git mutex is process-local, so nothing
	// serialized the two lanes and the server's ref compare-and-swap
	// rejected the duplicate pushes. Guarding first makes the new
	// process abort before it can open a push lane at all.
	lst, err := d.listenUDS()
	if err != nil {
		// Startup abort: unwind the lanes this process already started,
		// but persist NOTHING — this process never owned the socket, so
		// the shared on-disk bookkeeping (state.json) belongs to the
		// daemon that still holds it.
		d.abortStartup()
		return err
	}
	d.faceOwned = true
	// d.lst is read by closeFace under faceMu; publish it under the
	// same lock so a shutdown arriving in the next instant cannot race
	// the assignment.
	d.faceMu.Lock()
	d.lst = lst
	d.faceMu.Unlock()

	// Restart reconciliation (§十一): every recorded job is re-derived
	// against aihub's own verdict — strictly after this process owns
	// the socket (see the guard note above), and before the face serves
	// a single request: a client that reaches the daemon must see a
	// reconciled runtime, never a seeded record's pre-reconciliation
	// snapshot (stale agent pids, superseded lineages).
	if err := d.rt.Restart(d.runCtx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: restart reconciliation failed: %v\n", err)
	}
	d.reattachRecovered()

	serveErr, serving := d.startFace(lst)
	if !serving {
		// A shutdown sequence closed the face while this process was
		// reconciling: the listener is already gone, so there is nothing
		// left to drive — Run exits exactly like the shutDone case.
		return nil
	}

	dispatch := time.NewTicker(d.cfg.DispatchTick)
	defer dispatch.Stop()
	drain := time.NewTicker(d.cfg.DrainInterval)
	defer drain.Stop()
	for {
		select {
		case <-ctx.Done():
			// The caller gave up: run the same bounded graceful sequence.
			return d.Shutdown(context.Background())
		case <-d.shutDone:
			return nil
		case err := <-serveErr:
			if err != nil && err != http.ErrServerClosed {
				_ = d.Shutdown(context.Background())
				return err
			}
			return nil
		case <-dispatch.C:
			d.dispatch()
		case <-drain.C:
			d.fetchDrainWave()
		}
	}
}

// startFace begins serving the UDS face on lst. It reports false
// when a shutdown sequence already closed the face (the shutdown-
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
	d.srv = &http.Server{Handler: d.handler()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.srv.Serve(lst) }()
	return serveErr, true
}

// closeFace tears the UDS face down (runShutdown step 5): the serving
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

// abortStartup unwinds a daemon whose single-instance guard refused
// the UDS face: stop the already-started lanes and wait for them, but
// persist nothing (see faceOwned). Run returns the guard error right
// after; the process exits without ever having touched a push lane.
func (d *Daemon) abortStartup() {
	if d.cancelObs != nil {
		d.cancelObs()
	}
	if d.obsDone != nil {
		<-d.obsDone
	}
	if d.cancelRun != nil {
		d.cancelRun()
	}
	if d.rtDone != nil {
		<-d.rtDone
	}
}

// reattachRecovered synthesizes the observe attachments for jobs the
// restart reconciliation re-derived: their RTRecovered messages found
// no live monitor (a fresh daemon has none), so the daemon re-attaches
// them through the same RTStarted shape observe builds monitors on.
func (d *Daemon) reattachRecovered() {
	for _, ji := range d.rt.List() {
		if ji.Agent == nil {
			continue
		}
		select {
		case d.attachCh <- d.buildAttach(protocol.RTMsg{
			Kind: protocol.RTStarted, SpawnN: ji.SpawnN,
			WorkItem: ji.WorkItem, AgentPID: ji.AgentPID,
			// the in-process secret re-arms the M4 revalidation for
			// reattached jobs (it rides RTMsg, never a log surface).
			SessionID: ji.SessionID,
		}, ji):
		case <-d.runCtx.Done():
			return
		}
	}
}

// buildAttach turns one runtime snapshot into an observe attachment:
// the protocol message plus the agent handle adapted to observe's face.
// The per-derivation transcript facts (F3: the capsule copy path, the
// live file, the spawn moment) ride the WIInfo the monitor's
// commit-boundary transcript sync reads.
func (d *Daemon) buildAttach(msg protocol.RTMsg, ji runtime.JobInfo) observe.Attach {
	return observe.Attach{
		Msg:    msg,
		Handle: d.installAdapter(ji),
		WI: observe.WIInfo{
			WorkItem:       ji.WorkItem,
			Project:        ji.Project,
			CapsuleDir:     ji.CapsuleDir,
			BundleName:     ji.BundleName,
			Git:            ji.Git,
			Transcript:     ji.Transcript,
			LiveTranscript: ji.LiveTranscript,
			SpawnAt:        ji.SpawnAt,
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
		select {
		case <-d.runCtx.Done():
			return
		case msg, ok := <-d.rtMsgs:
			if !ok {
				return
			}
			at := observe.Attach{Msg: msg}
			if msg.Kind == protocol.RTStarted || msg.Kind == protocol.RTRecovered {
				// Emission-time counter registration (the flush fence's
				// recovery window, #8): the hub records the new derivation
				// counter the moment Runtime announces it — before the
				// monitor adopts the attachment — so a just-resumed
				// agent's flush(reason) never eats a STALE_SPAWN refusal
				// in the window between the RTRecovered emission and the
				// monitor's adoption.
				d.obs.NoteSpawn(msg.WorkItem, msg.SpawnN)
				if ji, live := d.rt.Info(msg.WorkItem); live && ji.Agent != nil {
					at = d.buildAttach(msg, ji)
				}
			}
			select {
			case d.attachCh <- at:
			case <-d.runCtx.Done():
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The agent adapter: runtime.AgentHandle → observe.Handle. Runtime owns
// the process; Observe owns the watching contract. The adapter bridges
// the two exported faces without either lane knowing the other.

type agentAdapter struct {
	h        runtime.AgentHandle
	records  chan []byte
	exit     chan struct{}
	pollStop chan struct{}
	retire   sync.Once
}

// installAdapter registers the CURRENT handle of a work item as the
// tap target and returns its observe face. A previous adapter is retired
// (its exit poll stops; its records channel stops receiving).
func (d *Daemon) installAdapter(ji runtime.JobInfo) *agentAdapter {
	a := &agentAdapter{
		h:        ji.Agent,
		records:  make(chan []byte, 1024),
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

func (a *agentAdapter) Exit() <-chan struct{}  { return a.exit }
func (a *agentAdapter) Records() <-chan []byte { return a.records }

// SendControl composes the runtime handle's write+ack pair into the
// observe face's single bounded call. A written-but-unacked method maps
// to observe.ErrAnswerAckPending (delivered, consumption pending) per
// the ledger's two-phase contract.
func (a *agentAdapter) SendControl(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
	ack := protocol.ControlAck{Method: msg.Method, Seq: msg.Seq}
	if err := a.h.SendControl(msg); err != nil {
		return ack, err
	}
	bound := 30 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if b := time.Until(dl); b > 0 && b < bound {
			bound = b
		}
	}
	if err := a.h.WaitControlAck(ctx, msg, bound); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ack, observe.ErrAnswerAckPending
		}
		return ack, err
	}
	ack.OK = true
	return ack, nil
}

// tapRecord feeds one raw stdout record line to the work item's current
// adapter (the runtime RecordTap binding). Bounded: a full buffer drops
// rather than ever blocking the runtime's stdout reader.
func (d *Daemon) tapRecord(wi string, line []byte) {
	d.adapMu.Lock()
	a := d.adapters[wi]
	d.adapMu.Unlock()
	if a == nil {
		return
	}
	select {
	case a.records <- line:
	default:
	}
}

// ---------------------------------------------------------------------------
// Dispatcher (04 §6.1d backfill) and drain loop (04 §6.1c).

// dispatch starts pending jobs into free slots, priority-desc then FIFO,
// and reconciles the queue's running marks against the runtime's live
// job set first (a job sealed by wrap/pause/the flush path frees its
// slot without telling the queue).
func (d *Daemon) dispatch() {
	if d.modes.ShuttingDown() {
		return
	}
	live := map[string]bool{}
	for _, ji := range d.rt.List() {
		live[ji.WorkItem] = true
	}
	d.q.Reconcile(live)
	for {
		e := d.q.Take()
		if e == nil {
			return
		}
		if err := d.rt.StartIntent(d.runCtx, e.WorkItemID, e.Intent); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: start %s (%s) failed: %v\n", e.WorkItemID, e.Intent, err)
			continue // slot stays free; the next pending entry backfills
		}
		d.q.MarkRunning(e.WorkItemID)
	}
}

// fetchDrainWave pulls the ready queue and enqueues what is claimable
// (04 §6.1c): only in drain mode, never once shutting_down latched, and
// only while a concurrency slot is free. Authorization-class denials
// raise the degraded signal — a typed drain_degraded event in the
// observe lane's durable log, once per silent→degraded episode (the
// flag clears on the next successful fetch); transport-class losses
// stay silent idle.
func (d *Daemon) fetchDrainWave() {
	if !d.modes.AdmitDrainWave() || !d.q.SlotFree() {
		return
	}
	items, err := d.hub.Queue(d.cfg.Project)
	if err != nil {
		if authDenied(err) {
			fmt.Fprintf(os.Stderr, "daemon: drain fetch degraded: ready queue of project %q denied (%v)\n", d.cfg.Project, err)
			if !d.drainDegraded && d.obs != nil {
				d.drainDegraded = true
				_ = d.obs.Events().Append("", observe.EvDrainDegraded, map[string]any{
					"project": d.cfg.Project,
					"class":   "authorization",
					"error":   err.Error(),
				})
			}
		}
		return
	}
	d.drainDegraded = false // episode cleared: this fetch succeeded
	for _, it := range items {
		if it.BundleName == nil {
			continue // registration tickets never execute
		}
		if d.q.IsRunning(it.ID) || d.q.IsPending(it.ID) {
			continue
		}
		d.q.Enqueue(qEntry{WorkItemID: it.ID, Priority: it.Priority, Intent: "claim"})
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
	return lst, nil
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

// sealHeld reports whether a close path currently holds the seal guard
// of wi (the wrap dual-path arbitration's non-claiming read; the flush
// gate distinguishes a REAL in-progress close from the recovery
// window's transient).
func (d *Daemon) sealHeld(wi string) bool {
	d.sealMu.Lock()
	defer d.sealMu.Unlock()
	return d.sealedWI[wi]
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
		_ = pid
		f.Close()
		return nil, fmt.Errorf("daemon: another daemon holds the data-dir lock %s", path)
	}
	// best-effort pid stamp (diagnostics; the lock itself is the guard)
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}
