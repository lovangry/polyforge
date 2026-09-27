package daemon

// daemon.go: the Daemon core. Responsibilities per 04 §2.1 (exactly five):
// job admission + capsule create/seal, persistent WI-Agent derivation and
// supervision (via the AgentSpawner face), watcher driving (dirty marks
// only), mechanical full-scan publish execution (INV-D3), and the graceful
// shutdown sequence. The Daemon NEVER executes step commands (INV-D2) and
// never drives workflow logic.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/aihub"
	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/publish"
	"github.com/silgrid/polyforge/internal/sync/recover"
	"github.com/silgrid/polyforge/internal/sync/revision"
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
	// StaleHeartbeatThreshold configures the stale-machines status signal
	// (04 §7.2/§9): a machine whose last successful heartbeat is older than
	// this is listed in the status output. Monitoring signal only — no
	// auto-takeover is ever derived from it. Zero selects
	// DefaultStaleHeartbeatThreshold.
	StaleHeartbeatThreshold time.Duration
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
	// DeliverAnswer relays an answer to a needs_input-waiting agent.
	DeliverAnswer(relay AnswerRelay) error
	// Alive reports process liveness.
	Alive() bool
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

	mu       sync.Mutex
	jobs     map[string]*jobRuntime // wiID → live bookkeeping
	shutdown chan struct{}
	shutOnce sync.Once
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
	st, err := store.Load()
	if err != nil {
		return nil, err
	}
	client := aihub.New(cfg.AIHubURL, cfg.APIKey)
	mutex := freeze.NewManager()
	stager := freeze.NewStager(cfg.DataDir)
	pubStore := publish.NewStore(cfg.DataDir)
	d := &Daemon{
		cfg:      cfg,
		client:   client,
		state:    store,
		st:       st,
		jobq:     NewJobQueue(),
		modes:    NewModeCtl(),
		ledger:   NewAnswerLedger(),
		beats:    NewBeatTracker(),
		pub:      publish.New(&remoteAdapter{c: client}, mutex, stager, pubStore),
		spawner:  spawner,
		jobs:     map[string]*jobRuntime{},
		shutdown: make(chan struct{}),
	}
	return d, nil
}

// remoteAdapter bridges aihub.Client to the publish.Remote face.
type remoteAdapter struct{ c *aihub.Client }

func (r *remoteAdapter) PutBlock(ctx context.Context, wi string, seq int64, sha string, block []byte, token string) error {
	return r.c.PutBlock(wi, seq, sha, block, token)
}
func (r *remoteAdapter) PostManifest(ctx context.Context, wi string, m revision.Manifest, sha, token string) (int64, error) {
	return r.c.PostManifest(wi, m, sha, token)
}
func (r *remoteAdapter) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.c.GetHead(wi)
}

// recoverRemote adapts the client to the recover.Remote face (manifest
// content via the in-process transport until the Core exposes an endpoint).
type recoverRemote struct{ c *aihub.Client }

func (r *recoverRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.c.GetHead(wi)
}
func (r *recoverRemote) ListRevisions(ctx context.Context, wi string, since int64) ([]revision.RevisionMeta, error) {
	return r.c.ListRevisions(wi, since)
}
func (r *recoverRemote) GetManifest(ctx context.Context, wi string, seq int64) (revision.Manifest, error) {
	return r.c.GetManifest(wi, seq)
}
func (r *recoverRemote) GetBlock(ctx context.Context, wi, sha string) ([]byte, error) {
	return r.c.GetBlock(wi, sha)
}

// ---------------------------------------------------------------------------
// Preflight (04 §6.1 step 1): refuse BEFORE any server call — never a
// "claimed but no local site" state.

// ErrPreflight is the DAEMON-507 shape.
var ErrPreflight = errors.New("daemon: local preflight failed (DAEMON-507)")

type preflightFunc func() error

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
	e := d.jobq.Take()
	if e == nil {
		return false, nil
	}
	if err := d.spawnJob(ctx, *e); err != nil {
		d.jobq.Complete(e.WorkItemID) // slot not held
		return true, err
	}
	return true, nil
}

func (d *Daemon) spawnJob(ctx context.Context, e QEntry) error {
	wi := e.WorkItemID
	if err := d.Preflight(ctx); err != nil {
		return err
	}
	// Step 4: persist the claim intent BEFORE any irreversible action.
	idem := fmt.Sprintf("pf-%s-%s-%d", wi, e.Intent, time.Now().UnixNano())
	pending := PendingJob{
		WorkItemID: wi, IdempotencyKey: idem, Intent: e.Intent,
		CreatedAt: NowStamp(), Status: PendingIntentPersisted,
	}
	if err := d.state.AppendPending(d.st, pending); err != nil {
		return err
	}
	// Step 5: claim (resume is a new claim; takeover is admin force-takeover
	// and consumes the returned new-generation credentials directly — 02
	// §6.2: never a second claim).
	var resp *aihub.ClaimResponse
	var cerr error
	switch e.Intent {
	case "takeover":
		resp, cerr = d.client.ForceTakeover(wi, d.cfg.MachineID)
	default:
		resp, cerr = d.client.Claim(wi, idem, d.cfg.MachineID)
	}
	if cerr != nil {
		d.state.RemovePending(d.st, wi, idem)
		return cerr
	}
	pending.Status = PendingClaimConfirmed
	pending.Generation = resp.Generation
	pending.GenerationToken = resp.GenerationToken
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
	// replacement; seeds the publish state.
	recRemote := &recoverRemote{c: d.client}
	recRes, err := recover.Recover(ctx, capc, recRemote)
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
	handle, err := d.spawner.Spawn(ctx, SpawnParams{
		WorkItemID: wi, CapsuleDir: capc.Dir, Generation: resp.Generation,
		SessionID: sessionID, Adapter: d.cfg.Adapter, InitialPrompt: prompt, Bootstrap: boot,
	})
	if err != nil {
		return err
	}
	// Step 10: formal jobs[] record; drop the pending intent.
	if err := d.state.UpsertJob(d.st, Job{
		WorkItemID: wi, CapsuleID: capc.ID(), Generation: resp.Generation,
		HarnessAdapter: d.cfg.Adapter, HarnessSessionID: sessionID,
		MainAgentPID: handle.PID(), MainAgentStartedAt: handle.StartedAt(),
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
		// transcript archive rides the last revision.
		fe, err := d.pub.FinalFlush(ctx, publish.FlushRequest{
			WorkItemID: wi, CapsuleDir: jr.cap.Dir, Generation: d.jobGeneration(wi),
			Token: jr.token, Archive: func() error {
				_, aerr := jr.cap.ArchiveTranscript(jr.adapter, d.jobGeneration(wi))
				return aerr
			},
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
	out := &PublishOutcome{Published: res.Published, Idempotent: res.Idempotent, HeadSeq: res.HeadSeq, ManifestSha: res.HeadManifestSha}
	return out, nil
}

// freezeConditions builds the 05 §2.7 conditions for a job: the live agent's
// quiesce confirmation when it is alive; the all-writers-dead shortcut when
// it is not (a dead agent cannot ack, and its process group residue is
// terminated by the supervisor).
func (d *Daemon) freezeConditions(jr *jobRuntime, milestone bool) freeze.Conditions {
	alive := jr.agent != nil && jr.agent.Alive()
	cond := freeze.Conditions{Timeout: 30 * time.Second, Milestone: milestone}
	if alive {
		cond.Quiescer = quiescerOf{jr}
	} else {
		cond.ProcessGroupDead = func() bool { return true }
	}
	return cond
}

type quiescerOf struct{ jr *jobRuntime }

func (q quiescerOf) RequestQuiesce(ctx context.Context) error { return q.jr.agent.Quiesce(ctx) }

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

func (d *Daemon) fetchDrainWave(ctx context.Context) {
	if d.jobq.Running() >= d.jobq.MaxConcurrent() {
		return // cap reached: pause wave fetches (not convergence)
	}
	rq, err := d.client.ReadyQueue(d.cfg.DrainProject)
	if err != nil {
		return // scheduler unreachable: idle-poll again next tick (Drain's idle state)
	}
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

// Shutdown runs the graceful-shutdown sequence:
//  1. latch shutting_down FIRST (before any other action);
//  2. external submissions now refuse (DAEMON-410) and the Drain loop
//     stopped fetching (same flag, different path);
//  3. per active job: notify stop-advancing, wait for the interruptible
//     boundary (step boundary + sync boundary);
//  4. final flush per WI (the daemon publishes on the agent's behalf) +
//     end the generation (pause) with the Daemon-generated evidence;
//  5. remove job records, stop watchers, seal capsules read-only;
//  6. the caller exits.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.modes.SetShuttingDown()
	d.shutOnce.Do(func() { close(d.shutdown) })

	d.mu.Lock()
	wis := make([]string, 0, len(d.jobs))
	for wi := range d.jobs {
		wis = append(wis, wi)
	}
	d.mu.Unlock()

	for _, wi := range wis {
		d.mu.Lock()
		jr := d.jobs[wi]
		d.mu.Unlock()
		if jr == nil {
			continue
		}
		jr.ring.Append(EvJobStopped, map[string]any{"cause": "shutdown"})
		// Step 3: stop-advancing notice + wait for the safe boundary.
		if jr.agent != nil && jr.agent.Alive() {
			if err := jr.agent.NotifyStop(ctx); err == nil {
				_ = jr.agent.Quiesce(ctx)
			}
		}
		// Step 4: final flush on the agent's behalf.
		outcome, err := d.PublishRequest(ctx, wi, "pause")
		if err != nil || outcome == nil || outcome.FlushEvidence == nil {
			return fmt.Errorf("%w: %s: %v", ErrFlushFailed, wi, err)
		}
		// End the generation (the daemon holds the token; 04 §6.2 A path
		// step 3 — the shutdown-interrupted close is a NORMAL end).
		if err := d.client.EndGeneration(wi, "pause", "", jr.token, outcome.FlushEvidence); err != nil {
			// 04 §7.3: a rolled-back credential is isolated and the shutdown
			// moves on — this WI is already dead server-side; failing the
			// whole shutdown would hold every OTHER WI's close hostage.
			if isSecretRolledBack(err) {
				_ = d.quarantineRolledBack(wi)
				continue
			}
			return fmt.Errorf("%w: %s end-generation: %v", ErrFlushFailed, wi, err)
		}
		d.sealJob(wi, jr)
	}
	return nil
}

// sealJob stops the watcher, removes bookkeeping, seals the capsule
// read-only (kept on disk; deletion only via the Core protection flow).
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
// restart: alive agents (pid + creation-time match) are re-attached as-is;
// dead ones get their residual process group collected and are re-derived
// with the SAME session id (the only session-reuse case, 03 §4.4).
func (d *Daemon) RecoverFromState(ctx context.Context) error {
	for i := range d.st.Jobs {
		j := d.st.Jobs[i]
		alive := processAlive(j.MainAgentPID, j.MainAgentStartedAt)
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
		if !alive {
			// Re-derive: same generation, same machine → SAME session id
			// (crash continuation, INV-14/D4: a new INSTANCE of the same
			// role, never a per-step agent).
			if d.spawner != nil {
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
					MainAgentPID: handle.PID(), MainAgentStartedAt: handle.StartedAt(), Status: JobRunning,
				})
			}
		}
		jr.watcher = watcher.Start(capc.Dir, 0)
	}
	// Orphan pending intents (04 §6.1 orphan collection): claims confirmed
	// but never derived — re-queue them for a fresh spawn pass.
	for _, p := range d.st.PendingJobs {
		if p.Status == PendingSessionPersisted || p.Status == PendingClaimConfirmed {
			d.jobq.Enqueue(QEntry{WorkItemID: p.WorkItemID, Priority: "high", Intent: p.Intent})
		}
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

// RunHeartbeat beats until ctx ends. Every SUCCESSFUL beat is recorded in
// the beat tracker (the stale-machines status signal, 04 §7.2/§9) — failed
// beats deliberately do not advance the clock, so a Core outage ages the
// machine out exactly like a stopped daemon would.
func (d *Daemon) RunHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := d.client.Heartbeat(d.cfg.MachineID, d.cfg.Version, d.modes.Current()); err == nil {
				d.beats.Record(d.cfg.MachineID, time.Now())
			}
		}
	}
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
		return d.modes.Set(mode)
	case ModeDrain:
		if d.cfg.DrainProject == "" {
			return fmt.Errorf("daemon: drain mode requires a configured project")
		}
		return d.modes.Set(ModeDrain)
	}
	return fmt.Errorf("daemon: unknown mode %q", mode)
}
