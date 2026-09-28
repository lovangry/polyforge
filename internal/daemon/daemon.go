package daemon

// daemon.go: the Daemon core. Responsibilities per 04 §2.1 (exactly five):
// job admission + capsule create/seal, persistent WI-Agent derivation and
// supervision (via the AgentSpawner face), watcher driving (dirty marks
// only), mechanical full-scan publish execution (INV-D3), and the graceful
// shutdown sequence. The Daemon NEVER executes step commands (INV-D2) and
// never drives workflow logic.

// The alive-agent re-attachment after a daemon restart (adopt.go) rebuilds
// only the process-control face: the stdio control channel cannot be
// re-created for an already-running process (the D2-ruled reality).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/aihub"
	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/publish"
	"github.com/silgrid/polyforge/internal/sync/recover"
	"github.com/silgrid/polyforge/internal/sync/watcher"
)

// Config is the daemon's static wiring.
type Config struct {
	AIHubURL      string
	APIKey        string // machine api key (daemon/credentials in production)
	MachineID     string
	DataDir       string // daemon state + staging + pending records
	WorkspaceRoot string // <root>/capsules/<wi> lives here
	WorkflowsDir  string // the daemon's own checkout workflows/<bundle>/
	SkillsDir     string // the daemon's own checkout skills/ (pf-execute)
	Version       string
	DrainProject  string // project whose ready queue Drain consumes
	Adapter       string // default harness adapter (pi|claude)
	// ClaudeSessionStoreRoot overrides the claude harness's DEFAULT
	// session store root for the seal-time terminal-state collection
	// (03 §4.4 / 04 §6.2 A step 2). Empty selects the measured default
	// resolution: $CLAUDE_CONFIG_DIR, else $HOME/.claude (the adapter
	// verification measured conversations under CLAUDE_CONFIG_DIR;
	// the subdirectory layout under the root is version-specific and
	// deliberately not pinned — see sessioncollect.go). A CLI
	// storage-layout change needs at most this knob.
	ClaudeSessionStoreRoot string
	// StaleHeartbeatThreshold configures the stale-machines status signal
	// (04 §7.2/§9): a machine whose last successful heartbeat is older than
	// this is listed in the status output. Monitoring signal only — no
	// auto-takeover is ever derived from it. Zero selects
	// DefaultStaleHeartbeatThreshold.
	StaleHeartbeatThreshold time.Duration
	// QuiesceTimeout bounds ONE write-freeze confirmation wait (05 §2.7 /
	// 04 §6.5 step 3): the window the daemon waits for the WI Agent's
	// pf/quiesce ack (or its proven death) before the forced-trigger
	// escalation (freeze.ErrQuiesceTimeout). Zero selects
	// DefaultQuiesceTimeout. Tests tighten it; production leaves it at the
	// design default.
	QuiesceTimeout time.Duration
}

// SpawnParams is what the daemon hands the agent spawner (04 §6.1 step 9).
type SpawnParams struct {
	WorkItemID    string
	CapsuleDir    string
	Generation    int64
	SessionID     string
	Adapter       string
	InitialPrompt string // pf-execute SKILL.md text + bundle reference header
	Bootstrap     *capsule.Bootstrap
	// OnProcessStarted, when non-nil, is invoked by the spawner ONCE,
	// IMMEDIATELY after the WI Agent process exists and BEFORE Spawn
	// returns, carrying the process identity the pid-reuse guard needs
	// (pid + creation started-at). Its purpose is R3N-F6: the daemon
	// journals that identity into the pending_jobs[] row BEFORE the
	// formal jobs[] record exists, so a crash between the derivation and
	// step 10 leaves a DISCOVERABLE orphan the next boot can adopt instead
	// of blindly re-spawning a second agent over the live one. An error
	// return fails the spawn and must tear the just-started process down.
	//
	// RESIDUAL WINDOW (documented honestly): a crash between the process
	// start and this callback's journal fsync still leaves an
	// UNDISCOVERABLE live agent; the next boot re-spawns per 04 §6.1's
	// orphan paragraph. The window is the spawn call's own prologue
	// (milliseconds) instead of the whole span from derivation to the
	// jobs[] record (the agent's entire first step) it used to be.
	OnProcessStarted func(pid int, startedAt string) error
}

// AgentHandle is the supervision face over one persistent WI Agent process.
type AgentHandle interface {
	PID() int
	StartedAt() string
	// NotifyStop asks the agent to stop advancing at the next safe
	// boundary (graceful shutdown step 3; the daemon cannot pick or skip
	// steps for it — 00 §3.6).
	NotifyStop(ctx context.Context) error
	// Quiesce confirms: no in-flight sync-surface writes, none will start
	// (the freeze.Quiescer face for publish).
	Quiesce(ctx context.Context) error
	// Resume is the freeze-release notification (05 §2.7): the write
	// freeze window is over, the agent may resume sync-surface writes.
	Resume(ctx context.Context) error
	// DeliverAnswer relays an answer to a needs_input-waiting agent.
	DeliverAnswer(relay AnswerRelay) error
	// Alive reports process liveness.
	Alive() bool
	// GroupEmpty reports whether the agent's WHOLE process group — the
	// agent itself and every step agent / command child it spawned — has
	// exited (05 §2.7 shortcut condition ①: every member confirmed gone,
	// not just the top-level pid). Side-effect-free: a caller that finds
	// residue collects it via Terminate, then re-asks.
	GroupEmpty() bool
	// Terminate kills the whole process group: SIGTERM, then SIGKILL after
	// the timeout (04 §6.2 path B / §7.1 residue collection).
	Terminate(timeout time.Duration) error
}

// AgentSpawner derives (or re-derives) one persistent WI Agent.
type AgentSpawner interface {
	Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error)
}

// Daemon wires every subsystem behind the five responsibilities.
type Daemon struct {
	cfg    Config
	client *aihub.Client

	state  *StateStore
	st     *State
	jobq   *JobQueue
	modes  *ModeCtl
	ledger *AnswerLedger
	beats  *BeatTracker

	pub *publish.Publisher

	spawner AgentSpawner

	mu           sync.Mutex
	jobs         map[string]*jobRuntime // wiID → live bookkeeping
	closing      map[string]bool        // wiID → a 04 §6.2 close is mid-flight
	shutdown     chan struct{}
	shutOnce     sync.Once
	shutdownDone chan struct{} // closed when the FIRST Shutdown call completes
	shutdownErr  error         // the first Shutdown's verdict, delivered to joiners

	// drainFetchDegraded is the authorization-class drain-fetch signal
	// (04 §6.1c): the drain loop's ready-queue read was denied (401/403/
	// 404 — the non-member/hidden-project CORE-404), which is NOT Drain's
	// idle state. In-memory only; it re-arms after restart and auto-clears
	// on the first subsequent successful fetch. Guarded by mu.
	drainFetchDegraded bool
}

// jobRuntime is the in-memory per-job assembly.
type jobRuntime struct {
	cap       *capsule.Capsule
	agent     AgentHandle
	ring      *Ring
	watcher   *watcher.Watcher
	token     string
	adapter   string
	sessionID string
}

// New assembles a daemon. spawner may be nil in tests that never spawn.
func New(cfg Config, spawner AgentSpawner) (*Daemon, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.WorkspaceRoot, "capsules"), 0o755); err != nil {
		return nil, err
	}
	store := NewStateStore(cfg.DataDir)
	// R3N-F4 (crash-recoverable swap protocol): reconcile any capsule-tree
	// swap interrupted by the previous daemon life BEFORE anything can lay
	// a fresh tree over the gap — a marker naming the three swap paths
	// survives the crash window between Rename(live,trash) and
	// Rename(staged,live), and the reconciliation completes or rolls back
	// deterministically. Loud, non-fatal per the 04 §7.1 step-5 pattern:
	// one broken capsule must not brick every other WI's startup.
	if err := recover.ReconcileInterruptedSwaps(filepath.Join(cfg.WorkspaceRoot, "capsules")); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: interrupted capsule swap reconciliation reported failures (human attention): %v\n", err)
	}
	// 04 §4.1/§11: create-or-adopt state.json carrying the state-level
	// header (machine_id/mode/workspace_root/started_at are required by
	// validation; an older-format file without them is backfilled first —
	// an upgrade never bricks the daemon on its own previous state file).
	st, err := store.LoadOrAdopt(StateHeader{
		MachineID: cfg.MachineID, Mode: ModeInteractive,
		WorkspaceRoot: cfg.WorkspaceRoot, StartedAt: NowStamp(),
	})
	if err != nil {
		return nil, err
	}
	client := aihub.New(cfg.AIHubURL, cfg.APIKey)
	// Bounded-network fix (polyforge#32 shutdown-path audit): the Core
	// client builds its requests WITHOUT a request context, so neither
	// the shutdown budget nor any other ctx can abort an in-flight Core
	// round trip — a wedged TCP connection would hang the graceful
	// shutdown's final flush / end-generation calls forever (bounded ctx
	// or not). The http.Client round-trip timeout is the only in-scope
	// lever: every Core call the daemon makes — including the publish
	// drains on the shutdown path — is now capped per request.
	client.HTTP = &http.Client{Timeout: CoreCallTimeout}
	mutex := freeze.NewManager()
	stager := freeze.NewStager(cfg.DataDir)
	pubStore := publish.NewStore(cfg.DataDir)
	d := &Daemon{
		cfg:          cfg,
		client:       client,
		state:        store,
		st:           st,
		jobq:         NewJobQueue(),
		modes:        NewModeCtl(),
		ledger:       NewAnswerLedger(),
		beats:        NewBeatTracker(),
		pub:          publish.New(&aihub.RemoteAdapter{Client: client}, mutex, stager, pubStore),
		spawner:      spawner,
		jobs:         map[string]*jobRuntime{},
		closing:      map[string]bool{},
		shutdown:     make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Preflight (04 §6.1 step 1): refuse BEFORE any server call — never a
// "claimed but no local site" state.

// ErrPreflight is the DAEMON-507 shape.
var ErrPreflight = errors.New("daemon: local preflight failed (DAEMON-507)")

// Preflight checks workspace/data/capsule writability and Core reachability.
func (d *Daemon) Preflight(ctx context.Context) error {
	for _, dir := range []string{d.cfg.WorkspaceRoot, d.cfg.DataDir, filepath.Join(d.cfg.WorkspaceRoot, "capsules")} {
		if err := checkWritable(dir); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrPreflight, dir, err)
		}
	}
	// AIHub reachability: the head endpoint on a synthetic id is the
	// cheapest authenticated ping (404-with-auth is a live answer; the
	// error envelope distinguishes reachability).
	if _, _, err := d.client.GetHead("wi_probe_invalid"); err != nil {
		if aihub.IsCode(err, "404") || aihub.IsCode(err, "CORE-404") {
			return nil // server answered: reachable
		}
		return fmt.Errorf("%w: aihub unreachable: %v", ErrPreflight, err)
	}
	return nil
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".pf-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

// ---------------------------------------------------------------------------
// Submit / claim sequence (04 §6.1)

// SubmitIntent is the admission-level request.
type SubmitIntent struct {
	WorkItemID string
	Intent     string // claim | resume | takeover
	Priority   string
}

// ErrTerminalWI is the DAEMON-409 shape for a submit-family intent against
// a work item that can never be claimed again (terminal status, 02 §2.3
// state machine: wrapped/failed/cancelled are forever).
var ErrTerminalWI = errors.New("daemon: work item is terminal")

// checkSubmittable is the submit-family admission precheck (found in the
// polyforge#17 dogfood round): admission used to accept ANY id — a
// submit of a wrapped/failed/cancelled WI answered "accepted" and the
// doomed claim then failed SILENTLY at spawn time (no event, no queue
// record, nothing the operator could see). The precheck refuses ONLY an
// explicit terminal status read from the Core. Every read failure is
// TOLERATED (logged, deferred to the spawn-time claim, which stays the
// authority): a machine key without project membership reads every WI as
// a hidden CORE-404 (02 §4.10 — no existence leak), and that legitimate
// configuration must not turn the precheck into a blanket refusal.
func (d *Daemon) checkSubmittable(wi string) error {
	detail, err := d.client.GetWI(wi)
	if err != nil {
		return nil // spawn-time claim stays the authority for read failures
	}
	if status, _ := detail["status"].(string); status == "wrapped" || status == "failed" || status == "cancelled" {
		return fmt.Errorf("%w: %s is %s; no claim, resume or takeover can start", ErrTerminalWI, wi, status)
	}
	return nil
}

// Submit applies admission (mode + shutdown + duplicates), then enqueues.
// The claim itself runs at SPAWN time (04 §6.1b/6.1d): every spawned job
// walks the full preflight → intent persist → claim → capsule → session →
// derive sequence.
func (d *Daemon) Submit(it SubmitIntent) error {
	switch it.Intent {
	case "claim", "resume", "takeover":
	default:
		return fmt.Errorf("daemon: unknown intent %q", it.Intent)
	}
	if err := d.modes.AdmitSingle(); err != nil {
		return err
	}
	d.mu.Lock()
	_, running := d.jobs[it.WorkItemID]
	d.mu.Unlock()
	if running {
		return fmt.Errorf("daemon: %s already has a running job (DAEMON-409)", it.WorkItemID)
	}
	if d.jobq.IsRunning(it.WorkItemID) {
		return fmt.Errorf("daemon: %s already queued (DAEMON-409)", it.WorkItemID)
	}
	// Precheck AFTER local admission (mode/shutdown/duplicates keep their
	// precedence): refuse what can never be claimed before enqueueing.
	if err := d.checkSubmittable(it.WorkItemID); err != nil {
		return err
	}
	d.jobq.Enqueue(QEntry{WorkItemID: it.WorkItemID, Priority: it.Priority, Intent: it.Intent})
	return nil
}

// BatchSubmit enqueues a human-confirmed wave (04 §6.1b): the WHOLE wave
// enters the pending queue (never truncated, never ordered by input); the
// queue's own ordering + concurrency cap govern spawning.
func (d *Daemon) BatchSubmit(wiIDs []string, priorities map[string]string) error {
	if err := d.modes.AdmitBatch(); err != nil {
		return err
	}
	for _, wi := range wiIDs {
		pri := "normal"
		if p, ok := priorities[wi]; ok {
			pri = p
		}
		d.jobq.Enqueue(QEntry{WorkItemID: wi, Priority: pri, Intent: "claim"})
	}
	return nil
}

// GetRecommendation is the read-only Batch half (04 §6.1b step 1): fetches
// the scheduler's candidates; creates NOTHING.
func (d *Daemon) GetRecommendation(ctx context.Context) (*aihub.ReadyQueue, error) {
	return d.client.ReadyQueue(d.cfg.DrainProject)
}

// SpawnNext takes one queued entry (if a slot is free) and runs the full
// 04 §6.1 sequence for it. The dispatcher loop calls this; tests may drive
// it directly for determinism.
func (d *Daemon) SpawnNext(ctx context.Context) (bool, error) {
	_, spawned, err := d.SpawnNextEntry(ctx)
	return spawned, err
}

// SpawnNextEntry is SpawnNext with the attempted entry exposed for
// ATTRIBUTION (the submit route reports a spawn failure only when the
// failed entry is the one the operator submitted — real-usage fix from
// the polyforge#17 dogfood round: a failed claim/takeover used to be
// swallowed here and the submit answer stayed "accepted" while nothing
// ever ran).
func (d *Daemon) SpawnNextEntry(ctx context.Context) (*QEntry, bool, error) {
	e := d.jobq.Take()
	if e == nil {
		return nil, false, nil
	}
	if err := d.spawnJob(ctx, *e); err != nil {
		d.jobq.Complete(e.WorkItemID) // slot not held
		return e, true, err
	}
	return e, true, nil
}

func (d *Daemon) spawnJob(ctx context.Context, e QEntry) error {
	wi := e.WorkItemID
	if err := d.Preflight(ctx); err != nil {
		return err
	}
	// Step 4: persist the claim intent BEFORE any irreversible action.
	// A re-run (crash-recovery orphan requeue) arrives with the key the
	// journal already persisted: reuse it (04 §6.1 step 4) so the re-claim
	// hits the Core's idempotent cache and returns the SAME attempt
	// (02 §6.1/§6.6) instead of opening a duplicate generation.
	idem := e.IdempotencyKey
	if idem == "" {
		idem = fmt.Sprintf("pf-%s-%s-%d", wi, e.Intent, time.Now().UnixNano())
	}
	pending := PendingJob{
		WorkItemID: wi, IdempotencyKey: idem, Intent: e.Intent,
		CreatedAt: NowStamp(), Status: PendingIntentPersisted,
	}
	if existing := d.st.FindPending(wi, idem); existing != nil {
		// R2N-5: continue from the JOURNALED row. Overwriting it with the
		// fresh intent_persisted record above would destroy the
		// claim_confirmed generation/token/bootstrap a takeover re-run
		// MUST consume below (and regress the journal's status mid-flight
		// for claim/resume too).
		pending = *existing
		// R3N-F6: the journaled process identity (if any) describes the
		// PREVIOUS attempt's process — this re-run derives a NEW one, and
		// OnProcessStarted below rewrites the pair. Clearing the stale
		// values keeps the row honest about the CURRENT attempt if the
		// re-run dies before the callback lands.
		pending.MainAgentPID = 0
		pending.MainAgentStartedAt = ""
	} else if err := d.state.UpdatePending(d.st, pending); err != nil {
		// UpdatePending, not a blind append: the journal is ONE row per WI
		// (R3N-F7) — a leftover row from a failed previous attempt is
		// superseded even when its idempotency key differs (a key change
		// on retry is expected), never allowed to accumulate into the
		// duplicate-rows state validation rejects on the next load.
		return err
	}
	// Step 5: claim (resume is a new claim; takeover is admin force-takeover
	// and consumes the returned new-generation credentials directly — 02
	// §6.2: never a second claim).
	var resp *aihub.ClaimResponse
	var cerr error
	switch {
	case e.Intent == "takeover" && (pending.Status == PendingClaimConfirmed || pending.Status == PendingSessionPersisted):
		// R2N-5: an orphaned TAKEOVER requeue whose journal row is already
		// claim_confirmed/session_persisted must NOT re-call ForceTakeover.
		// 02 §6.2/04 §7.3: the force-takeover call ITSELF ends the old
		// generation and opens generation+1 in one response — calling it
		// again would end the generation the previous daemon life just
		// opened and open ANOTHER one (generation churn), stranding the
		// persisted token. The generation is already open server-side
		// under the persisted token: consume the journaled
		// GenerationToken + Generation (+ journaled capsule_bootstrap)
		// and continue the spawn. Only rows still at intent_persisted
		// re-run ForceTakeover (that takeover never landed server-side).
		if pending.Generation >= 1 && pending.GenerationToken != "" && pending.CapsuleBootstrap != "" {
			resp = &aihub.ClaimResponse{
				Generation:       pending.Generation,
				GenerationToken:  pending.GenerationToken,
				CapsuleBootstrap: json.RawMessage(pending.CapsuleBootstrap),
			}
		} else {
			// Incomplete journal (pre-upgrade row or corruption): the
			// journaled credentials cannot be consumed, so a fresh
			// ForceTakeover is the only honest path to credentials. It
			// opens a NEW generation; that churn is the price of the
			// broken journal and is logged loudly.
			fmt.Fprintf(os.Stderr, "pf-daemon: takeover requeue for %s has an incomplete claim_confirmed journal row (generation=%d token=%v bootstrap=%v); re-running ForceTakeover — this opens a NEW generation\n",
				wi, pending.Generation, pending.GenerationToken != "", pending.CapsuleBootstrap != "")
			resp, cerr = d.client.ForceTakeover(wi, d.cfg.MachineID)
		}
	case e.Intent == "takeover":
		resp, cerr = d.client.ForceTakeover(wi, d.cfg.MachineID)
	default:
		resp, cerr = d.client.Claim(wi, idem, d.cfg.MachineID)
	}
	if cerr != nil {
		// A re-run with a journal-persisted key KEEPS the record on a
		// failed re-claim: the intent is exactly what the next restart needs
		// to retry the same key against the Core's idempotent cache (04
		// §6.1 step 4). A fresh key whose claim was refused has nothing to
		// retry — drop it as before.
		if e.IdempotencyKey == "" {
			d.state.RemovePending(d.st, wi, idem)
		} else {
			fmt.Fprintf(os.Stderr, "pf-daemon: re-run claim for %s failed (key %s kept in the pending journal for the next retry): %v\n", wi, idem, cerr)
		}
		return cerr
	}
	pending.Status = PendingClaimConfirmed
	pending.Generation = resp.Generation
	pending.GenerationToken = resp.GenerationToken
	// Journal the bootstrap too: a takeover re-run that must consume this
	// row (see above) needs every input of the spawn sequence — the
	// bootstrap is the only data-plane read the sequence has (02 §8).
	pending.CapsuleBootstrap = string(resp.CapsuleBootstrap)
	if err := d.state.UpdatePending(d.st, pending); err != nil {
		return err
	}
	// Consume the capsule_bootstrap (02 §8: no data-plane reads needed).
	boot, berr := capsule.ParseBootstrap(resp.CapsuleBootstrap)
	if berr != nil {
		return berr
	}
	// Step 6: capsule create (03 §6.1) — quarantine-or-reuse built in.
	capc, _, err := capsule.Create(d.cfg.WorkspaceRoot, wi, resp.Generation, d.cfg.MachineID, boot.BundleDigest)
	if err != nil {
		return err
	}
	// Step 5 (03 order): inventory checkout BEFORE remote recovery.
	if err := capc.CheckoutInventory(boot); err != nil {
		return err
	}
	// Remote site reconstruction when a HEAD exists (05 §7.4): full
	// replacement; seeds the publish state. The transport is the shared
	// aihub.RemoteAdapter (manifest content travels over the real Core
	// face, the row-19 revisions list with include=ops, polyforge#14).
	recRes, err := recover.Recover(ctx, capc, &aihub.RemoteAdapter{Client: d.client})
	if err != nil {
		return err
	}
	if recRes.Head != nil {
		if serr := d.pub.SeedFromRecovery(wi, resp.Generation, recRes.Head, recRes.LiveFiles, recRes.BytesDigest, recRes.WorktreeDigest); serr != nil {
			return serr
		}
	}
	if err := capc.EnsureHarnessDir(d.cfg.Adapter); err != nil {
		return err
	}
	if err := capc.MarkReady(); err != nil {
		return err
	}
	// Step 5a — step-0 materialization (03 §4.5, wired into the spawn path
	// in fix round 2): the workflow/agent snapshot is materialized from the
	// daemon's OWN checkout and verified against the pin BEFORE any WI
	// Agent is derived. The old path went from capsule create straight to
	// spawn, so a new capsule could spawn without the snapshot and a torn
	// snapshot was never rejected by the production path. The gate is the
	// identity-validated MaterializationComplete: an already-complete
	// snapshot (idempotent same-generation retry) is trusted and NOT
	// re-read from the checkout; anything else is materialized wholesale.
	// Failure fails CLOSED: the typed spawn error surfaces to the submit
	// route and nothing is derived over a missing/torn snapshot.
	matID := capsule.MaterializationIdentity{
		BundleName: boot.BundleName, BundleDigest: boot.BundleDigest,
		Adapters: []string{d.cfg.Adapter},
	}
	if err := d.ensureMaterialized(capc, matID); err != nil {
		return err
	}
	// Step 7: session identity — persisted BEFORE the process exists: the
	// session id lands in the pending record AND in the capsule's
	// harness/<adapter>/session.json (the identity file the WI Agent's
	// crash continuation reads back; 03 §4.4), so a crash after derivation
	// can always recover the id.
	sessionID := capsule.NewSessionID(wi, resp.Generation)
	if err := capc.WriteSessionIdentity(d.cfg.Adapter, sessionID); err != nil {
		return err
	}
	pending.Status = PendingSessionPersisted
	pending.HarnessAdapter = d.cfg.Adapter
	pending.HarnessSessionID = sessionID
	if err := d.state.UpdatePending(d.st, pending); err != nil {
		return err
	}
	// Step 8: pf-execute initial instruction from the daemon's OWN checkout
	// (04 §6.1 step 8 — version pinned to this daemon build; harnesses
	// never install it).
	prompt, err := d.loadExecutorPrompt(boot)
	if err != nil {
		return err
	}
	// Step 9: derive the persistent WI Agent (INV-14: one per WI).
	// R3N-F6: the spawner reports the process identity the moment it
	// exists — journaled into the pending row BELOW, before Spawn returns —
	// so a crash between here and the step-10 jobs[] record leaves a
	// DISCOVERABLE orphan the next boot ADOPTS (04 §6.1 orphan recovery +
	// §7.1 step 3) instead of re-spawning a second agent over the live
	// one (which would re-run checkout/recovery and overwrite unpublished
	// local work, 05 §7.4 same-generation no-replace).
	handle, err := d.spawner.Spawn(ctx, SpawnParams{
		WorkItemID: wi, CapsuleDir: capc.Dir, Generation: resp.Generation,
		SessionID: sessionID, Adapter: d.cfg.Adapter, InitialPrompt: prompt, Bootstrap: boot,
		OnProcessStarted: func(pid int, startedAt string) error {
			pending.MainAgentPID = pid
			pending.MainAgentStartedAt = startedAt
			return d.state.UpdatePending(d.st, pending)
		},
	})
	if err != nil {
		return err
	}
	// Step 10: formal jobs[] record; drop the pending intent.
	if err := d.state.UpsertJob(d.st, Job{
		WorkItemID: wi, CapsuleID: capc.ID(), Generation: resp.Generation,
		HarnessAdapter: d.cfg.Adapter, HarnessSessionID: sessionID,
		MainAgentPID: handle.PID(), MainAgentStartedAt: handle.StartedAt(),
		BundleName: boot.BundleName, BundleDigest: boot.BundleDigest,
		Status: JobRunning,
	}); err != nil {
		return err
	}
	d.state.RemovePending(d.st, wi, idem)
	d.jobq.MarkRunning(wi)

	d.mu.Lock()
	d.jobs[wi] = &jobRuntime{
		cap: capc, agent: handle, ring: &Ring{}, token: resp.GenerationToken,
		adapter: d.cfg.Adapter, sessionID: sessionID,
	}
	jr := d.jobs[wi]
	d.mu.Unlock()
	jr.ring.Append(EvJobStarted, map[string]any{"generation": resp.Generation})
	// Step 11: watcher (dirty marks only — it can never publish).
	jr.watcher = watcher.Start(capc.Dir, 0)
	return nil
}

// loadExecutorPrompt assembles the initial instruction: the pf-execute
// SKILL.md text from the daemon's own checkout plus the bundle reference
// header (04 §6.1 step 8/9, 04 §6.1a injection contract).
func (d *Daemon) loadExecutorPrompt(boot *capsule.Bootstrap) (string, error) {
	if d.cfg.SkillsDir == "" {
		return "", fmt.Errorf("daemon: skills dir not configured")
	}
	raw, err := os.ReadFile(filepath.Join(d.cfg.SkillsDir, "pf-execute", "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("daemon: pf-execute not readable in the daemon checkout: %w", err)
	}
	header := fmt.Sprintf("# Polyforge WI Agent bootstrap\n\nwork_item_id: %s\nbundle_name: %s\nbundle_digest: %s\ncapsule_dir: (delivered via spawn params)\n\n# pf-execute (injected by the daemon, version-locked to it)\n\n",
		boot.WorkItemID, boot.BundleName, boot.BundleDigest)
	return header + string(raw), nil
}

// ---------------------------------------------------------------------------
// publish_request (04 §6.4): the mechanical execution. reason ∈
// step_completed | step_interrupted | wi_outcome | milestone for regular
// triggers; pause|block|wrap|fail additionally produce the Daemon-generated
// flush_evidence for the end-generation call (05 §6.1).

// PublishOutcome is the publish_request response (04 §5.1).
type PublishOutcome struct {
	Published     bool                 `json:"published"`
	Idempotent    bool                 `json:"idempotent"`
	HeadSeq       int64                `json:"head_seq"`
	ManifestSha   string               `json:"manifest_sha"`
	FlushEvidence *aihub.FlushEvidence `json:"flush_evidence,omitempty"`
	// SchemaMismatches is the 03 §5.1a publish-time output-contract
	// signal: per-done-step violations found while validating the STAGED
	// bytes against each step's pinned output schema. SIGNAL, NOT VETO:
	// HEAD (head_seq) advanced regardless; the WI Agent consumes this
	// list per 01-workflow-skills.md §6.3 (falsified-done: done→in_progress
	// + infra_failure attempt archive + bounded candidate advance).
	SchemaMismatches []publish.SchemaMismatch `json:"schema_mismatches,omitempty"`
}

var endReasons = map[string]bool{"pause": true, "block": true, "wrap": true, "fail": true}

// PublishRequest executes one full-scan publish for the WI.
func (d *Daemon) PublishRequest(ctx context.Context, wi, reason string) (*PublishOutcome, error) {
	d.mu.Lock()
	jr, ok := d.jobs[wi]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("daemon: no local job for %s (DAEMON-404)", wi)
	}
	// Freeze faces: quiesce through the live WI Agent, or the process-dead
	// shortcut when it has already exited.
	cond := d.freezeConditions(jr, reason == "milestone")
	if endReasons[reason] {
		// Final flush (05 §6.1): ALWAYS an actual frozen scan; the session
		// transcript archive rides the last revision. The 03 §5.1a signal
		// of the real publish branch (if one runs) is captured through the
		// request hook and surfaced on this response — signal, not veto.
		var signal []publish.SchemaMismatch
		fe, err := d.pub.FinalFlush(ctx, publish.FlushRequest{
			WorkItemID: wi, CapsuleDir: jr.cap.Dir, Generation: d.jobGeneration(wi),
			Token: jr.token, Archive: func() error {
				// 04 §6.2 A step 2 (before the frozen scan, inside the one
				// Archive hook): claude jobs first get their terminal-state
				// session JSONL collected READ-ONLY from the harness default
				// session location into harness/<claude>/session-data/
				// (03 §4.4); a missing source fails the close typed
				// (DAEMON-513). Every other adapter is a strict no-op — the
				// pi path is ZERO-change. Then the shared archive mirrors
				// the transcript to logs/session/<generation>.jsonl.
				if cerr := d.collectTerminalSession(jr); cerr != nil {
					return cerr
				}
				_, aerr := jr.cap.ArchiveTranscript(jr.adapter, d.jobGeneration(wi))
				return aerr
			},
			Signal:     func(ms []publish.SchemaMismatch) { signal = ms },
			Conditions: cond,
		})
		if err != nil {
			jr.ring.Append(EvSyncFailed, map[string]any{"reason": reason, "error": err.Error()})
			// 04 §7.3: a rolled-back generation credential is a dedicated
			// isolation path, not a publish failure to retry.
			return nil, d.classifyWriteErr(wi, err)
		}
		jr.ring.Append(EvPublished, map[string]any{"reason": reason, "final": true})
		return &PublishOutcome{
			Published: true, HeadSeq: fe.PublishedSeq, ManifestSha: fe.ManifestSha, FlushEvidence: fe,
			SchemaMismatches: signal,
		}, nil
	}
	res, err := d.pub.Publish(ctx, publish.Request{
		WorkItemID: wi, CapsuleDir: jr.cap.Dir, Generation: d.jobGeneration(wi),
		Token: jr.token, Milestone: reason == "milestone", Conditions: cond,
	})
	if err != nil {
		jr.ring.Append(EvSyncFailed, map[string]any{"reason": reason, "error": err.Error()})
		// 04 §7.3: secret_rolled_back isolates the scene instead of a retry.
		return nil, d.classifyWriteErr(wi, err)
	}
	if res != nil && !res.Noop {
		jr.ring.Append(EvPublished, map[string]any{"reason": reason, "head_seq": res.HeadSeq})
	}
	out := &PublishOutcome{
		Published: res.Published, Idempotent: res.Idempotent, HeadSeq: res.HeadSeq, ManifestSha: res.HeadManifestSha,
		SchemaMismatches: res.SchemaMismatches,
	}
	return out, nil
}

// freezeConditions builds the 05 §2.7 conditions for a job. When the
// agent is alive, the normal quiesce confirmation path is used. When it is
// dead, the process-group-dead shortcut applies — but HONESTLY (05 §2.7
// condition ①): the shortcut holds only when the WHOLE group (step
// agents, command children, native runner) is verified empty, never merely
// because the top-level agent exited. Residue is collected on the spot
// (04 §6.2 B step 2 / §7.1 residue collection) and the emptiness re-checked.
//
// The ONE condition-① prover (groupDeadProofer) is shared by both shapes
// and also handed to the alive path's quiescer: an agent that dies
// INSIDE the confirmation window is re-judged by the same proof there
// (see quiescerOf.RequestQuiesce) instead of escalating the quiesce
// failure — the polyforge#32 pilot's 20-minute (shutting down) hang.
//
// FAILS CLOSED on the unknown: a nil/unknown agent state NEVER grants the
// dead-writers shortcut. 05 §2.7's shortcut condition ① is "every member
// confirmed gone" — an assumption is the opposite of a confirmation, and
// fail-open here would let a publish race a live writer we lost track of
// (single-writer domain, 03 §7). With the alive-reattach fix every recovered
// job has a handle; the genuinely-unknown case (no handle AND no recorded
// pid) is unverifiable and answers "not dead", so freeze.Acquire then
// fails loudly on its no-quiescer path instead of quietly publishing.
//
// KNOWN v1 LIMIT (condition ②, recorded honestly): whether a re-derived
// replacement agent has already written to the sync surface is NOT
// verifiable here — the daemon tracks no per-write bookkeeping for a
// replacement, so freeze.Conditions.ReplacementWriting is left unset and
// the shortcut is granted on condition ① alone. In every current flow the
// close/publish that builds these conditions and a re-derivation of the
// same WI are never in flight together (sealJob removes the job before any
// re-derivation can start), so the gap is theoretical in v1.
func (d *Daemon) freezeConditions(jr *jobRuntime, milestone bool) freeze.Conditions {
	alive := jr.agent != nil && jr.agent.Alive()
	// The confirmation window is configurable (05 §12 OQ1): production
	// keeps the 30s design default; the shutdown-path tests tighten it.
	cond := freeze.Conditions{Timeout: d.quiesceWindow(), Milestone: milestone}
	proveDead := d.groupDeadProofer(jr)
	if alive {
		cond.Quiescer = quiescerOf{jr: jr, proveDead: proveDead}
		return cond
	}
	cond.ProcessGroupDead = proveDead
	return cond
}

// quiesceWindow resolves the configured write-freeze confirmation window
// (05 §2.7 / 04 §6.5 step 3).
func (d *Daemon) quiesceWindow() time.Duration {
	if d.cfg.QuiesceTimeout > 0 {
		return d.cfg.QuiesceTimeout
	}
	return DefaultQuiesceTimeout
}

// groupDeadProofer builds the ONE 05 §2.7 condition-① prover for a job:
// the process-group-dead judgment with on-the-spot residue collection,
// shared by the dead-agent shortcut path and the alive path's mid-window
// death grant. It FAILS CLOSED: nothing is ever "probably dead".
func (d *Daemon) groupDeadProofer(jr *jobRuntime) func() bool {
	return func() bool {
		if jr.agent != nil {
			if jr.agent.GroupEmpty() {
				return true
			}
			// Residue: the dead agent's group still holds possible writers.
			// Collect it, then re-verify (bounded by the Terminate bound).
			_ = jr.agent.Terminate(10 * time.Second)
			return jr.agent.GroupEmpty()
		}
		// Genuinely unknown (no handle): verify by the RECORDED pid/procfs,
		// never assume. The jobs[] bookkeeping is the only pid source left.
		j := d.st.FindJob(jr.cap.WorkItemID)
		if j == nil || j.MainAgentPID <= 0 {
			fmt.Fprintf(os.Stderr, "pf-daemon: %s has no agent handle and no recorded pid; dead-writers shortcut DENIED (fail closed)\n", jr.cap.WorkItemID)
			return false
		}
		empty, err := ProcessGroupEmpty(j.MainAgentPID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: %s recorded group %d unverifiable; dead-writers shortcut DENIED (fail closed): %v\n", jr.cap.WorkItemID, j.MainAgentPID, err)
			return false
		}
		if empty {
			return true
		}
		// Residue at the recorded pid: collect it (the pid-reuse guard inside
		// skips signaling when the pid was recycled by an unrelated process
		// and only a PROVEN outcome returns nil, R3N-F3), then re-verify by
		// procfs — the re-check below stays the authority and fails closed.
		if cerr := collectResidualProcessGroup(j.MainAgentPID, j.MainAgentStartedAt); cerr != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: %s residual group collection could not prove death: %v\n", jr.cap.WorkItemID, cerr)
		}
		empty, err = ProcessGroupEmpty(j.MainAgentPID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: %s recorded group %d unverifiable after collection; dead-writers shortcut DENIED (fail closed): %v\n", jr.cap.WorkItemID, j.MainAgentPID, err)
			return false
		}
		return empty
	}
}

type quiescerOf struct {
	jr        *jobRuntime
	proveDead func() bool // the 05 §2.7 condition-① prover from freezeConditions
}

func (q quiescerOf) RequestQuiesce(ctx context.Context) error {
	err := controlWatch(ctx, q.jr.agent, q.jr.agent.Quiesce)
	if err == nil {
		return nil // confirmed: no in-flight sync-surface writes, none will start
	}
	if !agentGone(q.jr.agent) {
		// The agent is still alive (stuck, unhealthy, or merely slow): the
		// failure keeps its original verdict — a forced-trigger quiesce
		// failure escalates exactly as before (freeze.ErrQuiesceTimeout /
		// the typed control-path reason).
		return err
	}
	// The agent died inside the confirmation window (the exit-watched
	// short-circuit, a control path that died with the process, or an
	// already-dead handle): 05 §2.7 grants the freeze on condition ① — a
	// PROVEN-empty process group is STRONGER than any ack, and 04 §6.5
	// step 4 requires the group proof, never the bare top-level exit.
	// Escalating here instead was the polyforge#32 pilot failure: the
	// agent died mid-window, the quiesce error escalated into DAEMON-511
	// and the daemon parked in (shutting down) forever with the job still
	// recorded running.
	if q.proveDead != nil && q.proveDead() {
		return nil
	}
	// Residue could not be proven dead: fail closed — the freeze is NOT
	// granted on an assumption (the original error is the honest verdict).
	return err
}

// ReleaseQuiesce implements freeze.Releaser: the staging pin is complete,
// so the WI Agent is told it may resume writes (05 §2.7 release step).
func (q quiescerOf) ReleaseQuiesce(ctx context.Context) error { return q.jr.agent.Resume(ctx) }

func (d *Daemon) jobGeneration(wi string) int64 {
	if j := d.st.FindJob(wi); j != nil {
		return j.Generation
	}
	d.mu.Lock()
	jr, ok := d.jobs[wi]
	d.mu.Unlock()
	if ok {
		return jr.cap.Generation
	}
	return 0
}

// ---------------------------------------------------------------------------
// Drain loop (04 §6.1c) and dispatcher

// Run drives the dispatcher + drain loop until the shutdown latch or ctx
// ends. It returns when the process should exit (after Shutdown completed).
func (d *Daemon) Run(ctx context.Context) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.shutdown:
			return nil
		case <-tick.C:
			// Drain admission: fetch the next wave only in drain mode and
			// only while not shutting down (the flag stops this loop at
			// the SAME instant it refuses external callers — 04 §6.5 2b).
			if d.modes.AdmitDrainWave() {
				d.fetchDrainWave(ctx)
			}
			// Dispatcher: spawn queued jobs into free slots.
			for {
				spawned, err := d.SpawnNext(ctx)
				if err != nil {
					// A failed spawn is recorded on the WI's future; the
					// loop keeps draining the queue.
					_ = spawned
				}
				if !spawned {
					break
				}
			}
		}
	}
}

// drainFetchAuthDenial classifies a ReadyQueue error as authorization-
// class: a typed *aihub.APIError whose HTTP status or Core code is
// 401/403/404 (404-on-ready-queue is the server folding the canRead
// denial of a non-member/hidden project into CORE-404). Everything
// else — transport loss and 5xx alike — stays connection-class
// ("scheduler unreachable", Drain's idle state).
func drainFetchAuthDenial(err error) bool {
	ae, ok := err.(*aihub.APIError)
	if !ok {
		return false
	}
	switch ae.Status {
	case 401, 403, 404:
		return true
	}
	switch ae.Code {
	case "CORE-401", "CORE-403", "CORE-404", "HTTP-401", "HTTP-403", "HTTP-404":
		return true
	}
	return false
}

func (d *Daemon) fetchDrainWave(ctx context.Context) {
	if d.jobq.Running() >= d.jobq.MaxConcurrent() {
		return // cap reached: pause wave fetches (not convergence)
	}
	rq, err := d.client.ReadyQueue(d.cfg.DrainProject)
	if err != nil {
		// Error triage (04 §6.1c): only a genuinely unreachable scheduler
		// (transport loss or a 5xx) is Drain's silent idle state. An
		// authorization-class denial — the non-member/hidden-project
		// CORE-404 the ready-queue read folds canRead into, or 401/403 —
		// is NOT idleness: it raises the degraded signal. Either way the
		// loop keeps polling each tick (degradation is a signal, not a
		// stop); other 4xx verdicts keep the silent behavior.
		if drainFetchAuthDenial(err) {
			d.mu.Lock()
			first := !d.drainFetchDegraded
			d.drainFetchDegraded = true
			d.mu.Unlock()
			// Warn exactly once per episode (the silent→degraded edge).
			if first {
				fmt.Fprintf(os.Stderr, "pf-daemon: drain fetch degraded: machine %s is denied the ready queue of drain project %q (Core says: %v) — check the machine user's project membership; the signal clears on the next successful fetch\n",
					d.cfg.MachineID, d.cfg.DrainProject, err)
			}
		}
		return // idle-poll again next tick
	}
	// Auto-repair: the first subsequent successful fetch ends the episode.
	d.mu.Lock()
	d.drainFetchDegraded = false
	d.mu.Unlock()
	for _, entry := range rq.Executable {
		if entry.BundleName == nil {
			continue // registration tickets never execute (INV-2e)
		}
		if d.jobq.IsRunning(entry.ID) {
			continue
		}
		d.jobq.Enqueue(QEntry{WorkItemID: entry.ID, Priority: entry.Priority, Intent: "claim"})
	}
}

// ---------------------------------------------------------------------------
// Graceful shutdown (00 §3.5 / 04 §6.5)

// ErrFlushFailed is the DAEMON-511 shape: shutdown aborted, process stays
// up, human attention required.
var ErrFlushFailed = errors.New("daemon: shutdown flush failed to converge (DAEMON-511)")

// Bounded windows of the graceful-shutdown path (04 §6.5 + the design's
// intent that shutdown terminates in bounded time; polyforge#32 audit).
const (
	// DefaultQuiesceTimeout is the design's write-freeze confirmation
	// window (05 §2.7 / 04 §6.5 step 3): the wait for the WI Agent's
	// pf/quiesce ack — or its proven death — before the forced-trigger
	// escalation.
	DefaultQuiesceTimeout = 30 * time.Second
	// DefaultShutdownBudget bounds the WHOLE graceful-shutdown sequence
	// when the caller passes an unbounded context (the operations face's
	// hShutdown and the process main both do). It is a LAST-RESORT
	// ceiling on top of the per-wait windows — sized to cover a small
	// active set's notice+quiesce+flush round trips with margin; a budget
	// overrun surfaces as DAEMON-511 (ErrFlushFailed) instead of the
	// unbounded `(shutting down)` hang the polyforge#32 pilot observed.
	DefaultShutdownBudget = 5 * time.Minute
	// CoreCallTimeout caps ONE AIHub round trip (final-flush block
	// uploads, manifest submit, end-generation). The Core client builds
	// requests without a context, so this client-level cap is the only
	// bound those calls have — see New.
	CoreCallTimeout = 2 * time.Minute
)

// Shutdown runs the graceful-shutdown sequence:
//  1. latch shutting_down FIRST (before any other action);
//  2. external submissions now refuse (DAEMON-410) and the Drain loop
//     stopped fetching (same flag, different path);
//  3. per active job: the ONE shared 04 §6.2 A-path close (finish.go
//     closeJob): notify stop-advancing, wait for the interruptible
//     boundary (step boundary + sync boundary);
//  4. final flush per WI (the daemon publishes on the agent's behalf) +
//     end the generation (pause) with the Daemon-generated evidence;
//  5. remove job records, stop watchers, seal capsules read-only;
//  6. the caller exits.
//
// BOUNDED-SHUTDOWN AUDIT (polyforge#32 — the pilot stayed `(shutting
// down)` for 20+ minutes after the killed agent never converged its
// close; SIGTERM was swallowed by the blocked main, only SIGKILL
// cleared it). Every wait the sequence can hit is now bounded:
//
//   - agent-facing waits (stop notice, quiesce acks): each call's own
//     ack deadline, PLUS the agent-exit selection (controlWatch,
//     agentexit.go) — a dead agent short-circuits the wait instead of
//     parking it; a mid-window death grants the write freeze on the
//     proven group-dead condition (quiescerOf.RequestQuiesce);
//   - Core round trips (final flush upload, manifest, end-generation):
//     CoreCallTimeout (New) — the sequence ctx cannot abort them because
//     the Core client ignores request contexts;
//   - watcher stops (sealJob): at most one poll interval, agent-independent;
//   - agent recycling (sealJob Terminate): SIGTERM bound + SIGKILL bound;
//   - the heartbeat goroutine is NOT part of the sequence: it is bound
//     to the process's signal context (cmd main) and each beat to
//     CoreCallTimeout; a beat in flight when the budget fires cannot
//     hold the sequence;
//   - the sequence itself: DefaultShutdownBudget below, the last-resort
//     ceiling over all of the above.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.modes.SetShuttingDown()
	first := false
	d.shutOnce.Do(func() { close(d.shutdown); first = true })
	if !first {
		// JOIN, never race: the daemon face answers the shutdown request
		// and runs the sequence in the background; the process main's own
		// Shutdown call then used to run a SECOND sequence that skipped
		// every job via the one-close guard and let the process EXIT
		// while the first close was still mid-flight — the remaining
		// job stayed running server-side with zero events (found with the
		// real pf-daemon binary in the polyforge#17 dogfood round; no
		// in-process test boots cmd/pf-daemon). A concurrent caller now
		// waits for the in-flight sequence and receives its verdict.
		select {
		case <-d.shutdownDone:
			return d.shutdownErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// The FIRST caller runs the sequence; its verdict is published to
	// joiners via the channel close (write-before-close is the
	// happens-before edge, no lock needed).
	//
	// Bounded sequence (audit item above): both real callers arrive with
	// context.Background() — an UNBOUNDED context. Everything inside has
	// its own window now, but a ceiling over the whole sequence still has
	// to exist for the shutdown to TERMINATE in bounded time: wrap an
	// unbounded ctx with the budget so a pathological combination of
	// slow windows cannot outlive it. A caller-supplied deadline is
	// respected as-is (tighter or looser: it is the caller's call).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), DefaultShutdownBudget)
		defer cancel()
	}
	err := d.runShutdownSequence(ctx)
	d.shutdownErr = err
	close(d.shutdownDone)
	return err
}

// runShutdownSequence is the shutdown body (steps 2–5; step 1 is the latch
// Shutdown itself performed above).
func (d *Daemon) runShutdownSequence(ctx context.Context) error {
	d.mu.Lock()
	wis := make([]string, 0, len(d.jobs))
	for wi := range d.jobs {
		wis = append(wis, wi)
	}
	d.mu.Unlock()

	for _, wi := range wis {
		// Route through the SAME one-close guard as the pause/wrap routes
		// (04 §6.2: ONE close path, no duplication — and never two closes
		// racing on one job): a pause/wrap that already holds the guard is
		// completing the identical A-path close for this WI — skip and let
		// it finish; a job already sealed is done.
		jr, cerr := d.beginClose(wi)
		if cerr != nil {
			if errors.Is(cerr, ErrCloseInProgress) || errors.Is(cerr, ErrNoLocalJob) {
				continue
			}
			return fmt.Errorf("%w: %s: %v", ErrFlushFailed, wi, cerr)
		}
		// Steps 3–4 run in the ONE shared 04 §6.2 A-path close (finish.go):
		// boundary notice, final flush, end the generation (pause) with the
		// Daemon-generated evidence — the shutdown-interrupted close is a
		// NORMAL end (04 §6.2).
		_, err := d.closeJob(ctx, wi, jr, "pause", "shutdown")
		d.endClose(wi)
		if err != nil {
			// 04 §7.3 / 05 §2.9: a rolled-back credential AND a taken-over
			// (stale) generation are both isolation paths — the WI is already
			// dead server-side; failing the whole shutdown would hold every
			// OTHER WI's close hostage.
			if errors.Is(err, ErrSecretRolledBack) || errors.Is(err, ErrStaleGeneration) {
				continue
			}
			return fmt.Errorf("%w: %s: %v", ErrFlushFailed, wi, err)
		}
	}
	return nil
}

// sealJob stops the watcher, removes bookkeeping, seals the capsule
// read-only (kept on disk; deletion only via the Core protection flow).
// Bounded-wait audit (polyforge#32): every wait inside is bounded —
// watcher.Stop blocks at most one poll interval (the loop's select
// returns on the closed stop channel; no agent involvement);
// Terminate is SIGTERM to the group with a bounded empty-wait, then
// SIGKILL with a second bounded wait (04 §6.2 B); SealReadOnly is local
// fsync + chmod, no cross-process waits. Nothing here can park the
// shutdown on a dead or stuck agent.
func (d *Daemon) sealJob(wi string, jr *jobRuntime) {
	if jr.watcher != nil {
		jr.watcher.Stop()
	}
	if jr.agent != nil {
		jr.agent.Terminate(5 * time.Second)
	}
	_ = jr.cap.SealReadOnly()
	d.mu.Lock()
	delete(d.jobs, wi)
	d.mu.Unlock()
	d.jobq.Complete(wi)
	d.state.RemoveJob(d.st, wi)
}

// ---------------------------------------------------------------------------
// Crash recovery (04 §7.1)

// RecoverFromState re-adopts the jobs recorded in state.json after a daemon
// restart: alive agents (pid + creation-time match) are RE-ATTACHED via
// Adopt (04 §7.1 step 3: "存活……不重新派生，重接该 WI Agent……重接不打扰其
// 运行、不重置其 session、不重复派发任何 step；把该 Job 保留在内存中的活跃
// 集合，harness_session_id 不变") — the previous daemon life's supervision
// connection is rebuilt as a process-control-only handle, because the stdio
// control channel is a pipe pair of the OLD daemon process and cannot be
// re-created (D2-ruled reality, documented on Adopt). Dead agents get their
// residual process group COLLECTED (04 §7.1 step 4: SIGTERM to the recorded
// group, SIGKILL after the bound — the agent is dead, but orphaned step
// agents / command children could still be writing) before being re-derived
// with the SAME session id (the only session-reuse case, 03 §4.4).
func (d *Daemon) RecoverFromState(ctx context.Context) error {
	for i := range d.st.Jobs {
		j := d.st.Jobs[i]
		alive := processAlive(j.MainAgentPID, j.MainAgentStartedAt)
		// R3N-F4: the capsule open path checks for a pending swap marker
		// too (idempotent with the New-time sweep; only a concurrent
		// writer could have left one behind).
		if err := recover.ReconcilePendingSwap(filepath.Join(capsule.CapsuleRoot(d.cfg.WorkspaceRoot), j.WorkItemID)); err != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: %s capsule swap reconciliation failed: %v\n", j.WorkItemID, err)
		}
		capc, _, oerr := capsule.Open(d.cfg.WorkspaceRoot, j.WorkItemID)
		if oerr != nil {
			// 04 §7.1 step 5: capsule dir gone = human attention, never a
			// silent rebuild.
			continue
		}
		d.mu.Lock()
		jr := &jobRuntime{
			cap: capc, ring: &Ring{}, token: "", adapter: j.HarnessAdapter, sessionID: j.HarnessSessionID,
		}
		d.jobs[j.WorkItemID] = jr
		d.mu.Unlock()
		d.jobq.MarkRunning(j.WorkItemID)
		if alive {
			// 04 §7.1 step 3: alive ⇒ RE-ATTACH, never re-derive. Before
			// this the runtime kept a nil agent after restart, so proxy
			// operations could not target the live pid, Shutdown could not
			// terminate the agent, and freezeConditions misfired. The adopted
			// handle restores the process-control face; control messaging is
			// lost and fails typed (ErrControlChannelLost). No ring event: the
			// observe vocabulary is the closed nine-type set (04 §5.1a) and
			// wi_agent_rederived would be a FALSE statement for a re-attach
			// (04 §7.1 step 3: "不重新派生").
			jr.agent = Adopt(j.MainAgentPID, j.MainAgentStartedAt)
		} else {
			// 04 §7.1 step 4: collect the residual process group FIRST —
			// the dead agent's orphaned children must stop writing before
			// the re-derived instance starts (single-writer domain, 03 §7).
			// R3N-F3: collection must PROVE the group dead; only a proven
			// (nil) outcome earns the re-derivation. An unproven collection
			// (pid reused with surviving members, or an unverifiable group)
			// DENIES the re-derivation: spawning a second writer generation
			// beside unproven residue is exactly the two-writers violation
			// the collection exists to prevent. The job stays in the
			// runtime without an agent — human attention, never a silent
			// respawn.
			if cerr := collectResidualProcessGroup(j.MainAgentPID, j.MainAgentStartedAt); cerr != nil {
				fmt.Fprintf(os.Stderr, "pf-daemon: %s residual group collection did not prove death; re-derivation DENIED (human attention required): %v\n", j.WorkItemID, cerr)
				jr.watcher = watcher.Start(capc.Dir, 0)
				continue
			}
			// Re-derive: same generation, same machine → SAME session id
			// (crash continuation, INV-14/D4: a new INSTANCE of the same
			// role, never a per-step agent).
			if d.spawner != nil {
				// Same-generation trust gate first (03 §4.5/§7, wired in fix
				// round 2): the re-derived agent may reuse the existing
				// snapshot ONLY when the identity-validated gate says it is
				// complete; anything else (torn swap, missing marker,
				// interrupted staging, deleted target) is re-materialized
				// WHOLESALE before the new instance starts. A materialization
				// that cannot complete fails CLOSED: the job is left in the
				// runtime WITHOUT an agent — human attention, never a silent
				// respawn over a snapshot nobody can vouch for.
				matID := capsule.MaterializationIdentity{
					BundleName: j.BundleName, BundleDigest: j.BundleDigest,
					Adapters: []string{j.HarnessAdapter},
				}
				if merr := d.ensureMaterialized(capc, matID); merr != nil {
					fmt.Fprintf(os.Stderr, "pf-daemon: %s materialization failed before re-derivation; job left without an agent (human attention required): %v\n", j.WorkItemID, merr)
					jr.watcher = watcher.Start(capc.Dir, 0)
					continue
				}
				prompt, perr := d.crashRecoveryPrompt(j)
				if perr != nil {
					return perr
				}
				handle, serr := d.spawner.Spawn(ctx, SpawnParams{
					WorkItemID: j.WorkItemID, CapsuleDir: capc.Dir, Generation: j.Generation,
					SessionID: j.HarnessSessionID, Adapter: j.HarnessAdapter, InitialPrompt: prompt,
				})
				if serr != nil {
					return serr
				}
				jr.agent = handle
				jr.ring.Append(EvWIAgentRederived, map[string]any{"session_id": j.HarnessSessionID})
				_ = d.state.UpsertJob(d.st, Job{
					WorkItemID: j.WorkItemID, CapsuleID: capc.ID(), Generation: j.Generation,
					HarnessAdapter: j.HarnessAdapter, HarnessSessionID: j.HarnessSessionID,
					MainAgentPID: handle.PID(), MainAgentStartedAt: handle.StartedAt(),
					BundleName: j.BundleName, BundleDigest: j.BundleDigest, Status: JobRunning,
				})
			}
		}
		jr.watcher = watcher.Start(capc.Dir, 0)
	}
	// Orphan pending intents (04 §6.1 orphan collection): claim intents the
	// journal persisted but whose spawn never completed — re-queue them for
	// a fresh spawn pass carrying the PERSISTED idempotency key, so the
	// re-claim replays the Core's idempotent cache and returns the SAME
	// attempt instead of a duplicate claim (04 §6.1 step 4/5, 02 §6.6). All
	// three journal statuses are orphans when no jobs[] record exists:
	// intent_persisted (crash before the claim completed — retry the same
	// key), claim_confirmed and session_persisted (crash after the claim,
	// before derivation).
	//
	// R3N-F6 (04 §6.1 orphan-recovery paragraph + 05 §7.4 same-generation
	// no-replace): a session_persisted orphan that carries a JOURNALED
	// process identity (written by the spawner's OnProcessStarted callback
	// before Spawn returned) may hold a LIVE WI Agent. Blindly re-queuing
	// it made spawnJob reuse the generation but unconditionally re-run
	// checkout + remote recovery (a full-replacement rebuild — destroying
	// unpublished local work) and spawn a SECOND agent beside the live one.
	// Instead:
	//   - identity ALIVE (pid + started-at match, 04 §7.1 step 2): ADOPT it
	//     (process-control-only handle, same as the jobs[] alive branch),
	//     complete the jobs[] record from the journal, run NO checkout and
	//     NO re-derivation;
	//   - identity DEAD: collect the residual group first (04 §7.1 step 4
	//     requires a PROVEN collection before any re-derivation), then
	//     requeue for the fresh spawn pass;
	//   - identity UNPROVABLE (pid reused): leave the row and surface it —
	//     requeueing would risk a second writer generation.
	// Rows with NO journaled pid (pre-upgrade rows, or a crash inside the
	// spawn prologue before the callback's fsync) are not discoverable —
	// 03 §4.4 records only {session_id, adapter, created_at} in
	// harness/<adapter>/session.json — and requeue as before; that residual
	// window is documented on SpawnParams.OnProcessStarted.
	orphans := append([]PendingJob(nil), d.st.PendingJobs...)
	for _, p := range orphans {
		if p.Status != PendingSessionPersisted && p.Status != PendingClaimConfirmed && p.Status != PendingIntentPersisted {
			continue
		}
		if d.st.FindJob(p.WorkItemID) != nil {
			continue // the spawn DID complete: not an orphan
		}
		if p.Status == PendingSessionPersisted && p.MainAgentPID >= 1 {
			if processAlive(p.MainAgentPID, p.MainAgentStartedAt) {
				if aerr := d.adoptOrphanAgent(p); aerr != nil {
					fmt.Fprintf(os.Stderr, "pf-daemon: orphan %s has a LIVE recorded agent but could not be adopted; journal row kept, NOT requeued (human attention): %v\n", p.WorkItemID, aerr)
				}
				continue
			}
			// The journaled process is dead: collect its residual group
			// first; only a PROVEN collection earns the requeue (R3N-F3).
			if cerr := collectResidualProcessGroup(p.MainAgentPID, p.MainAgentStartedAt); cerr != nil {
				fmt.Fprintf(os.Stderr, "pf-daemon: orphan %s residual collection did not prove death; journal row kept, NOT requeued (human attention): %v\n", p.WorkItemID, cerr)
				continue
			}
		}
		d.jobq.Enqueue(QEntry{WorkItemID: p.WorkItemID, Priority: "high", Intent: p.Intent, IdempotencyKey: p.IdempotencyKey})
	}
	return nil
}

// adoptOrphanAgent completes 04 §6.1's orphan-recovery paragraph for a
// LIVE discovered orphan (R3N-F6): the previous daemon life derived the WI
// Agent and journaled its identity, then died before the formal jobs[]
// record. The agent is ours and alive — re-attach it (Adopt, 04 §7.1
// step 3: never re-derive, never re-checkout, session unchanged), write
// the jobs[] record the crash interrupted, and clear the journal row.
// Control messaging is lost with the old daemon's pipes and fails typed
// (ErrControlChannelLost) exactly like the jobs[] alive branch.
func (d *Daemon) adoptOrphanAgent(p PendingJob) error {
	capc, _, oerr := capsule.Open(d.cfg.WorkspaceRoot, p.WorkItemID)
	if oerr != nil {
		return fmt.Errorf("capsule not openable: %w", oerr)
	}
	// R3-F1: the jobs[] record this crash-interrupted write completes is
	// the ONLY place the bundle identity survives — adoption DELETES the
	// journal row below, and that row is the only other carrier of the
	// journaled capsule_bootstrap. Without parsing the bundle reference out
	// of it here, the adopted Job lands with an EMPTY BundleName/
	// BundleDigest, and the NEXT dead-agent recovery (04 §7.1 step 4) runs
	// the materialization trust gate with no expected identity:
	// MaterializationComplete fails closed on the empty expectation even
	// with a complete on-disk snapshot, and the job is left without an
	// agent for human attention. Parse the bootstrap the claim journaled
	// and preserve the pinned identity in the record. An ABSENT bootstrap
	// (pre-upgrade rows) adopts as before — degraded, fail-closed later;
	// a PRESENT but unparsable one means a corrupt journal: refuse the
	// adoption, keep the row for human attention (never adopt a live
	// agent under an identity nobody can vouch for).
	var bundleName, bundleDigest string
	if p.CapsuleBootstrap != "" {
		boot, berr := capsule.ParseBootstrap(json.RawMessage(p.CapsuleBootstrap))
		if berr != nil {
			return fmt.Errorf("journaled capsule_bootstrap unparsable: %w", berr)
		}
		if boot.WorkItemID != p.WorkItemID {
			return fmt.Errorf("journaled capsule_bootstrap names work item %s, not %s", boot.WorkItemID, p.WorkItemID)
		}
		bundleName, bundleDigest = boot.BundleName, boot.BundleDigest
	}
	if err := d.state.UpsertJob(d.st, Job{
		WorkItemID: p.WorkItemID, CapsuleID: capc.ID(), Generation: p.Generation,
		HarnessAdapter: p.HarnessAdapter, HarnessSessionID: p.HarnessSessionID,
		MainAgentPID: p.MainAgentPID, MainAgentStartedAt: p.MainAgentStartedAt,
		BundleName: bundleName, BundleDigest: bundleDigest,
		Status: JobRunning,
	}); err != nil {
		return err
	}
	d.mu.Lock()
	jr := &jobRuntime{
		cap: capc, ring: &Ring{}, token: p.GenerationToken,
		adapter: p.HarnessAdapter, sessionID: p.HarnessSessionID,
	}
	// No ring event: the observe vocabulary is the closed nine-type set
	// (04 §5.1a) and a job-started or rederived event would be a FALSE
	// statement for a re-attach (same reasoning as the jobs[] alive branch).
	jr.agent = Adopt(p.MainAgentPID, p.MainAgentStartedAt)
	d.jobs[p.WorkItemID] = jr
	d.mu.Unlock()
	d.jobq.MarkRunning(p.WorkItemID)
	jr.watcher = watcher.Start(capc.Dir, 0)
	return d.state.RemovePending(d.st, p.WorkItemID, p.IdempotencyKey)
}

// ErrMaterialization marks a step-0 materialization failure (03 §4.5):
// the capsule's workflow/agent snapshot could not be landed from the
// daemon's own checkout, or did not end provably complete. The submit
// route recognizes the sentinel and answers HTTP 503 / DAEMON-503 (04
// §5.1: a local-resource failure — the bundle/checkout is unavailable on
// THIS machine — is retryable once the environment is repaired, never a
// DAEMON-500 "internal error"). Before R3-F2 these failures were bare
// fmt.Errorf strings whose "(DAEMON-503)" marker writeSubmitErr could
// not recognize, so the promised 503 contract surfaced as HTTP 500.
var ErrMaterialization = errors.New("daemon: step-0 materialization failed (DAEMON-503)")

// ensureMaterialized guarantees the capsule carries a COMPLETE, trusted
// materialized snapshot (workflow.md + the harness agents dirs, 03 §4.5)
// before a WI Agent is derived — the production wiring fix of round 2
// (F2-3): the claim path and the crash re-derivation both route through
// this gate. Trust is granted ONLY by the identity-validated
// MaterializationComplete (F2-4): a complete snapshot for the SAME pinned
// bundle is reused (03 §7 same-generation continuation — no re-read of the
// checkout); anything else is re-materialized wholesale from the daemon's
// own checkout. Digest drift is warn-and-continue per 03 §4.5 (v1 trusts
// the internal checkout; drift is a configuration incident). Failure is
// the caller's to fail closed on — the typed error names the WI.
func (d *Daemon) ensureMaterialized(capc *capsule.Capsule, id capsule.MaterializationIdentity) error {
	if capc.MaterializationComplete(id) {
		return nil // trusted: same pinned identity, complete set, no residue
	}
	warn, err := capc.Materialize(d.cfg.WorkflowsDir, id.BundleName, id.BundleDigest, id.Adapters)
	if warn != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s materialization bundle digest drift (warn, continuing): %s\n", capc.WorkItemID, warn.Message)
	}
	if err != nil {
		return fmt.Errorf("%w: materialization of %s failed: %v", ErrMaterialization, capc.WorkItemID, err)
	}
	if !capc.MaterializationComplete(id) {
		return fmt.Errorf("%w: materialization of %s did not end complete", ErrMaterialization, capc.WorkItemID)
	}
	return nil
}

// crashRecoveryPrompt rebuilds the executor instruction for a re-derivation
// (same protocol, re-read from the current daemon build, 04 §7.1 step 4).
func (d *Daemon) crashRecoveryPrompt(j Job) (string, error) {
	raw, err := os.ReadFile(filepath.Join(d.cfg.SkillsDir, "pf-execute", "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("daemon: pf-execute not readable: %w", err)
	}
	return fmt.Sprintf("# Polyforge WI Agent re-derivation\n\nwork_item_id: %s\ngeneration: %d\nsession_id: %s (reused: same machine, same generation crash continuation)\n\n# pf-execute\n\n%s",
		j.WorkItemID, j.Generation, j.HarnessSessionID, string(raw)), nil
}

// ---------------------------------------------------------------------------
// Heartbeat (04 §7.2): 60s cadence, mode rides along.

// RunHeartbeat beats until ctx ends. The FIRST beat fires immediately on
// start — 04 §7.2: "registration IS the first heartbeat" (02 §4.10), so a
// freshly started daemon must not sit through a whole empty interval
// unregistered and unobserved. Every SUCCESSFUL beat is recorded in
// the beat tracker (the stale-machines status signal, 04 §7.2/§9) — failed
// beats deliberately do not advance the clock, so a Core outage ages the
// machine out exactly like a stopped daemon would.
func (d *Daemon) RunHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	beat := func() {
		if _, err := d.client.Heartbeat(d.cfg.MachineID, d.cfg.Version, d.modes.Current()); err == nil {
			d.beats.Record(d.cfg.MachineID, time.Now())
		}
	}
	beat() // 04 §7.2: the first beat completes registration — no startup dead window
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}

// setMode applies a mode transition and persists it to state.json's mode
// field in the same step (04 §6.7: "合法后原子更新 daemon/state.json 的
// mode 字段" — the transition and its record are one atomic write).
//
// R3N-F8: a persistence failure must NOT leave the new mode active. The
// state file is the authoritative record (04 §4.1) and the in-memory
// ModeCtl is its fast-path copy, so on a failed ApplyMode the visible
// admission state is ROLLED BACK to the previous mode — the caller gets
// the error and the daemon keeps advertising (and admitting against) the
// last mode that is actually on disk. ModeCtl.Set stays a single atomic
// validate+commit, so no concurrent admission ever observes a torn
// transition.
func (d *Daemon) setMode(next string) error {
	prev := d.modes.Current()
	if err := d.modes.Set(next); err != nil {
		return err
	}
	if err := d.state.ApplyMode(d.st, next); err != nil {
		d.modes.restore(prev)
		return err
	}
	return nil
}

// abortDrain ends Drain mode and persists the transition (04 §6.1c/§6.7)
// with the same persistence-failure rollback as setMode (R3N-F8).
func (d *Daemon) abortDrain() error {
	prev := d.modes.Current()
	if err := d.modes.AbortDrain(); err != nil {
		return err
	}
	if err := d.state.ApplyMode(d.st, ModeInteractive); err != nil {
		d.modes.restore(prev)
		return err
	}
	return nil
}

// staleHeartbeatThreshold resolves the configured threshold (default when
// unset or non-positive).
func (d *Daemon) staleHeartbeatThreshold() time.Duration {
	if d.cfg.StaleHeartbeatThreshold > 0 {
		return d.cfg.StaleHeartbeatThreshold
	}
	return DefaultStaleHeartbeatThreshold
}

// SetStartupMode applies the cmd-level startup mode (cmd/pf-daemon): the
// same admission criteria as a runtime mode(set), plus the refusal to enter
// drain without a project configured.
func (d *Daemon) SetStartupMode(mode string) error {
	switch mode {
	case ModeInteractive, ModeBatch:
		return d.setMode(mode)
	case ModeDrain:
		if d.cfg.DrainProject == "" {
			return fmt.Errorf("daemon: drain mode requires a configured project")
		}
		return d.setMode(ModeDrain)
	}
	return fmt.Errorf("daemon: unknown mode %q", mode)
}
