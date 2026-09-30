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
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// Hub is the aihub face the daemon itself needs beyond Runtime's claim/
// end-generation use: the ready-queue projection (batch recommendation,
// drain loop) and the terminal-state check of the wrap/pause arbitration.
// *aihubclient.Client satisfies it.
type Hub interface {
	runtime.Aihub
	Queue(project string) ([]api.WorkItem, error)
}

// Config wires the daemon. Zero fields take the defaults in fill().
type Config struct {
	MachineID         string
	SocketPath        string // UDS listen path (default /run/polyforge.sock)
	WorkspaceRoot     string // <root>/capsules/<wi> are the capsule worktrees
	DataDir           string // runtime state.json + events.log + daemon-state.json
	Project           string // ready-queue scope for batch/drain
	GitRemoteBase     string // <base>/<wi>.git are the aihub bare repos
	WorkflowsDir      string // daemon checkout's workflows/
	ExecutorPromptFile string
	PiBin             string
	// HarnessSessionsRoot overrides the harness's default session store
	// for the live-transcript locator (F3); "" = ~/.pi/agent/sessions.
	HarnessSessionsRoot string
	CgroupRoot        string
	APIKey            string // machine credential; NEVER reaches an agent env
	MaxConcurrent     int    // cross-mode concurrency cap (default 4)
	Mode              string // initial mode; "" = not specified (persisted mode wins; default interactive)
	Harness           string // harness adapter name (default "pi")

	// Test/ops knobs.
	Spawner           runtime.Spawner   // nil = the production pi spawner
	Hub               Hub               // required; aihubclient.Client satisfies
	PushFunc          observe.PushFunc  // nil = the shared gitops choke point
	ObservePoll       time.Duration   // observe execution.json poll tick
	// Observe carries the observe lane's supervision windows into the
	// assembly — the injection seam the fault-injection scenarios shrink
	// (hang watchdog, liveness-probe bound, answer-ack window). Zero
	// fields keep the observe defaults (10min / 2min / 2min).
	Observe           ObserveWindows
	DrainInterval     time.Duration   // drain wave-fetch period (5s)
	DispatchTick      time.Duration   // dispatcher/backfill period (200ms)
	NoticeAckBound    time.Duration   // stop_notice ack wait (30s)
	ShutdownFlushWait time.Duration  // per-job flush(shutdown) wait (60s)
	ShutdownBudget    time.Duration  // whole-shutdown ceiling (5min)
	ExitPoll          time.Duration  // adapter exit-poll period (200ms)
}

// ObserveWindows is the observe-lane supervision-window subset of the
// observe config that daemon.Config passes through. All three default
// to zero — observe's own fill() then applies the production defaults —
// so a production Config with an untouched Observe field assembles the
// production windows.
type ObserveWindows struct {
	// HangWindow is the stream-silence window while a turn is in flight
	// that arms the liveness probe.
	HangWindow time.Duration
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

	rtMsgs  chan protocol.RTMsg
	obMsgs  chan protocol.OBMsg
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

	shutOnce  sync.Once
	shutDone  chan struct{} // closed when the FIRST shutdown sequence finished
	shutMu    sync.Mutex
	shutdownErr error

	adapMu    sync.Mutex
	adapters  map[string]*agentAdapter

	lst net.Listener
	srv *http.Server
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
		MachineID:      cfg.MachineID,
		WorkspaceRoot: cfg.WorkspaceRoot,
		DataDir:        cfg.DataDir,
		GitRemoteBase:  cfg.GitRemoteBase,
		WorkflowsDir:   cfg.WorkflowsDir,
		Harness:        cfg.Harness,
		PiBin:          cfg.PiBin,
		HarnessSessionsRoot: cfg.HarnessSessionsRoot,
		CgroupRoot:     cfg.CgroupRoot,
		SocketPath:     cfg.SocketPath, // the spawned agent's PF_DAEMON_SOCKET (flush proxy, §五)
		EnvSecrets:     []string{cfg.APIKey},
		ExecutorPromptFile: cfg.ExecutorPromptFile,
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
		// Bug-4 seam: the supervision windows pass through; zero fields
		// keep the observe defaults.
		HangWindow:      cfg.Observe.HangWindow,
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

	// Restart reconciliation (§十一): every recorded job is re-derived
	// against aihub's own verdict BEFORE the daemon serves anything.
	if err := d.rt.Restart(d.runCtx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: restart reconciliation failed: %v\n", err)
	}
	d.reattachRecovered()

	lst, err := d.listenUDS()
	if err != nil {
		_ = d.Shutdown(context.Background())
		return err
	}
	d.lst = lst
	d.srv = &http.Server{Handler: d.handler()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.srv.Serve(lst) }()

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
			Kind: protocol.RTStarted, AttemptID: ji.InstanceID,
			WorkItem: ji.WorkItem, AgentPID: ji.AgentPID,
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
		Msg: msg,
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
				// Emission-time instance registration (the flush fence's
				// recovery window, #8): the hub records the new lineage id
				// the moment Runtime announces it — before the monitor
				// adopts the attachment — so a just-resumed agent's
				// flush(reason) never eats a STALE_ATTEMPT refusal in the
				// window between the RTRecovered emission and the
				// monitor's adoption.
				if msg.AttemptID != "" {
					d.obs.NoteInstance(msg.WorkItem, msg.AttemptID)
				}
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
	h       runtime.AgentHandle
	attempt string
	records chan []byte
	exit    chan struct{}
	pollStop chan struct{}
	retire   sync.Once
}

// installAdapter registers the CURRENT handle of a work item as the
// tap target and returns its observe face. A previous adapter is retired
// (its exit poll stops; its records channel stops receiving).
func (d *Daemon) installAdapter(ji runtime.JobInfo) *agentAdapter {
	a := &agentAdapter{
		h:        ji.Agent,
		attempt:  ji.InstanceID,
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

func (a *agentAdapter) AttemptID() string      { return a.attempt }
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
// raise the degraded signal; transport-class losses stay silent idle.
func (d *Daemon) fetchDrainWave() {
	if !d.modes.AdmitDrainWave() || !d.q.SlotFree() {
		return
	}
	items, err := d.hub.Queue(d.cfg.Project)
	if err != nil {
		if authDenied(err) {
			fmt.Fprintf(os.Stderr, "daemon: drain fetch degraded: ready queue of project %q denied (%v)\n", d.cfg.Project, err)
		}
		return
	}
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
