// Package runtime implements the v1 Runtime lane: the WI lifecycle manager
// (Start / Stop / Recover / Restart), the pi-harness agent spawner with the
// real RPC envelope, the six-layer process cleanup and the recovery engine.
//
// Current behavior: docs/design/polyforge-vnext/reference.md §3.5 (session
// state), §6.2 (restart, recovery engine, cleanup) and §8 (git); decisions:
// v2-converge.md §6. The v1 records are under docs/design/polyforge-vnext/archive/.
// The Runtime talks to aihub through the shared aihubclient, to capsule git
// through the shared gitops choke point, and to the Observe lane ONLY
// through channels carrying common/protocol message types.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/common/workflow"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/mapping"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// Aihub is the server face Runtime needs (v1.5 session model: the
// generation-era Claim/EndGeneration are replaced by the session
// endpoints). *aihubclient.Client satisfies it.
type Aihub interface {
	SessionClaim(req api.SessionClaimRequest) (*api.SessionClaimResponse, error)
	SessionAttach(wi string, req api.SessionAttachRequest) (*api.SessionClaimResponse, error)
	SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error)
	SessionEnd(wi string, req api.SessionEndRequest) error
	GetWorkItem(idOrSlug string) (*api.WorkItem, error)
}

// Config is the daemon-supplied runtime configuration.
type Config struct {
	// AgentStarter is the execution-engine binding — the ONLY engine seam
	// and the driver is the only engine (polyforge#106). The daemon binds
	// bridge.Start here unconditionally at assembly; tests bind
	// driver-compatible scripted starters. nil is a MISASSEMBLY, not an
	// engine choice: dispatch refuses it loudly (there is no fallback
	// engine).
	AgentStarter AgentStartFunc
	MachineID    string // registered machine identity
	// Mapping is the workspace→projects plan of record (polyforge#113):
	// the daemon assembles it from the shared cli.toml ([workspace.
	// "<path>"] sections) and the runtime routes every capsule through it
	// — <mapped-workspace>/capsules/<project>-<seq> per the claim answer.
	// There is no single workspace and no default project anymore. The
	// daemon refuses to start when the mapping is empty; a recorded
	// mid-flight project that falls out of it blocks only that work item
	// at aihub (restart.go, blockUnrecoverable).
	Mapping       *mapping.Plan
	DataDir       string // state.json + external git-dirs live here
	GitRemoteBase string // e.g. https://aihub.example/git → <base>/<wi>.git
	WorkflowsDir  string // daemon checkout's workflows/ (materialization source)
	PiBin         string // harness binary (default "pi")
	// ClaudeBin is the Claude Code binary (default "claude"; cli.toml
	// claude_bin / --claude-bin), used by steps whose harness is claude.
	ClaudeBin string
	// HarnessRoot is the parent root of the three harness state dirs
	// (polyforge#171): <harness-root>/step-agent (the dedicated step
	// agent dir), <harness-root>/step-agent-sessions (the pi steps'
	// session store) and <harness-root>/claude-config (claude's config
	// dir: settings.json — the single credential authority layer —
	// plus projects/ and debug/). Resolved as --harness-root (cli.toml
	// [daemon] harness_root) > DataDir; the Dir() derivations and
	// stepagentdir.go hold the map.
	HarnessRoot string
	// OperatorAgentDir is the SOURCE of the dedicated step agent
	// directory's minimal set (addendum 2, owner ruling A): the
	// operator's agent dir by default; a flag/env-configured source is
	// Explicit — a missing auth.json there is a spawn-refusing error,
	// while the implicit default without file credentials is carried on
	// with a warning (the harness adapter may hold credentials
	// elsewhere).
	OperatorAgentDir         string
	OperatorAgentDirExplicit bool
	MaxRecoveries            int // consecutive recovery failures before bounded fail (default 2; C7 — the only recovery ledger)
	// EndRetryBase/EndRetryMax shape the backoff of the durable
	// SessionEnd(blocked) retry (end_pending records and the re-claim
	// abandon lane): first retry after Base, doubling up to Max
	// (defaults 2s / 60s). Retries run until the hub accepts the close.
	EndRetryBase time.Duration
	EndRetryMax  time.Duration
	// RestartHubWait is how long one hub call of the restart
	// reconciliation (a verify, a replayed claim, an attach) may go
	// unanswered before Restart returns and lets the daemon serve
	// (default 5s); the reconciliation then finishes in the background,
	// its unverified records shown waiting for aihub (restart.go). Local
	// and git work never ends the wait early.
	RestartHubWait time.Duration
	// EventTap, when non-nil, receives runtime-sourced diagnostic events
	// (kind "push_failed" on a seal push that failed after its retries)
	// so they land in the daemon's durable events log next to the
	// observe lane's own entries. The daemon binds it to the observe
	// EventLog; nil (tests) disables it.
	EventTap func(wi, kind string, body map[string]any)
	// EnvSecrets are configured secret values (machine api key, push
	// credential) the spawned agents' environment must never carry, even
	// under a renamed variable.
	EnvSecrets []string
	// RecallTopK bounds the F6-A claim-time memory snapshot: how many
	// semantic hits over the goal+content query land in the capsule's
	// runtime-state/recalled-memories.json (default 10, capped at 50).
	RecallTopK int
	// MemoryRecall, when non-nil, is the F6-A claim-time semantic-recall
	// seam: materialize calls it with the claim's goal+content, the work
	// item's own project (audit B5: the snapshot is scoped to that project,
	// not to everything the daemon credential can read) and the top-K, and
	// writes the answer as a static snapshot for the agent.
	// The daemon binds it to aihub's memory search face (the machine
	// credential stays with the daemon — agents hold no aihub credential
	// of any kind); nil disables the snapshot entirely (slim installs,
	// most unit tests).
	MemoryRecall func(query, project string, topK int) ([]RecalledMemory, error)
}

func (c *Config) fill() {
	if c.OperatorAgentDir == "" {
		c.OperatorAgentDir = OperatorAgentDirDefault()
		c.OperatorAgentDirExplicit = false
	}
	// #171/#159: step processes run with their own session store
	// (PI_CODING_AGENT_SESSION_DIR under the harness root), so the
	// live-transcript locator's root follows them there by
	// default. A harness-root override (tests, relocated installs)
	// moves the whole harness tree together. An empty-DataDir bare
	// Config (unit tests) keeps no redirect.
	if c.HarnessRoot == "" && c.DataDir != "" {
		c.HarnessRoot = c.DataDir
	}
	if c.MaxRecoveries <= 0 {
		c.MaxRecoveries = 2
	}
	if c.RecallTopK <= 0 {
		c.RecallTopK = defaultRecallTopK
	}
	if c.EndRetryBase <= 0 {
		c.EndRetryBase = 2 * time.Second
	}
	if c.EndRetryMax <= 0 {
		c.EndRetryMax = 60 * time.Second
	}
	if c.RestartHubWait <= 0 {
		c.RestartHubWait = 5 * time.Second
	}
}

// Runtime is the per-daemon WI lifecycle manager.
type Runtime struct {
	cfg     Config
	hub     Aihub
	spawner Spawner
	toOB    *supervise.Queue[protocol.RTMsg]
	fromOB  *supervise.Queue[protocol.OBMsg]

	mu   sync.Mutex
	jobs map[string]*job
	// slots (under mu) are the per-work-item state locks (locks.go), by
	// canonical id.
	slots map[string]*wiSlot
	// aliases maps every NON-canonical id form the runtime has ever been
	// asked to run (a "<project>#<seq>" slug, most often) to the
	// canonical work-item id the claim resolved it to. In-memory only:
	// Resolve's unknown-input answer is "", and the daemon's resolver
	// falls back to aihub for those (a restart re-learns on demand).
	aliases map[string]string
	// origins: submit origins noted by the daemon, consumed at job creation.
	origins map[string]Origin
	// manualModels: `pf submit --models` lists noted by the daemon,
	// consumed by the next Start of that work item (like origins).
	manualModels map[string][]string
	// manualMu guards the daemon-local manual-models.json (manualmodels.go).
	manualMu sync.Mutex

	// opMu serializes the mutating operations (Start/Stop/recover/Restart):
	// the message loop and the daemon's direct calls share it, so the state
	// machine never sees two drivers at once. It is NOT held across the slow
	// network steps: a claim's capsule clone and materialize run
	// with the job CLAIMING in the table; a close's or a recovery's pushes
	// and SessionEnd run with the job latched busy (offLock); a claim's
	// compensation SessionEnd runs with its work item latched ending
	// (claimEndOffLock); the claim's own hub calls (probe, journal verify,
	// SessionClaim) run with the work item latched starting
	// (startingClaims); a resume's verify and attach, the restart
	// reconciliation's and the deferred retry's verifies with the job
	// latched busy (the restart's journal replays under the restarting
	// latch). Each re-takes the lock and re-checks what it relies on (the
	// job's identity, the start's admission, the shutdown latch) before
	// mutating. No aihub call runs under opMu.
	opMu      sync.Mutex
	statePath string
	// persistGen (under mu) numbers persist's snapshots in the order they
	// were taken; persistMu serializes the writes, and a write whose
	// snapshot is older than the one already on disk (persistDone) is
	// skipped — its change is part of that newer snapshot — so an older
	// snapshot never lands last. persistErr is the outcome of the write
	// that landed persistDone.
	persistGen  uint64
	persistMu   sync.Mutex
	persistDone uint64
	persistErr  error

	// down latches the shutdown (BeginShutdown, under mu): once latched a
	// new Start answers DAEMON-410 without claiming, a CLAIMING one is
	// cancelled and deferred (its claiming-journal entry kept for the next
	// daemon start), and no agent is started any more. installs counts the
	// agent starts under way (admit / release): every door that brings an
	// agent to life takes a token first, so once the latch is down and the
	// count is back at zero the live job set is final. installsIdle, when
	// set, is closed as the count reaches zero (BeginShutdown waits on it).
	down         bool
	installs     int
	installsIdle chan struct{}
	// endingClaims (under mu) latches the work-item keys (submitted
	// spelling and canonical id) of a claim whose session end is on the
	// wire outside opMu (claimEndOffLock): a Start of the same work item
	// meanwhile answers the typed transient instead of replaying the
	// journal entry of a session that close is ending.
	endingClaims map[string]int
	// startingClaims (under mu) latches the work-item keys (submitted
	// spelling, then the canonical id) of a Start between its first hub
	// call and its CLAIMING placeholder (claimLocked): those hub calls
	// run outside opMu, and a second Start of the same work item answers
	// the typed transient meanwhile, a Stop DAEMON-409.
	startingClaims map[string]bool
	// claimMu serializes the claiming journal's read-modify-writes: the
	// claim-end retry lane writes it without opMu.
	claimMu sync.Mutex
	// replays tracks the restart's journal-replay claim goroutines
	// (runReplays).
	replays sync.WaitGroup
	// restarting (under mu) is latched while the restart reconciliation
	// runs (Restart, then its lane after an early return): it releases opMu
	// around every network step, and meanwhile a deferred verify waits
	// (retries) and a Start answers the typed transient.
	restarting bool
	// restartServed (under mu): Restart returned before its
	// reconciliation finished — the daemon serves, its startup
	// re-attachment pass already ran, and the lane hands every job it
	// brings back to life to the observe lane itself.
	restartServed bool
	// restartAttached (under mu): the work items that were live when
	// Restart returned before its lane finished — exactly the set the
	// daemon's startup re-attachment pass hands to observe (RestartAttach);
	// every job the lane brings to life afterwards it announces itself.
	restartAttached map[string]bool
	// laneHubSince (under mu): the moment the restart lane's current hub
	// call (a verify, a replayed claim, an attach) went out — zero while
	// it runs local or git work. Restart's wait bounds only hub calls.
	laneHubSince time.Time

	// The durable-close retry lane (endpending.go): WIs whose
	// SessionEnd(blocked) retry goroutine is running (one per WI), and
	// the stop signal Close fires.
	endMu       sync.Mutex
	endRetrying map[string]bool
	endStop     chan struct{}
	endStopOnce sync.Once
}

// job is one WI's runtime bookkeeping.

// AgentStartInput carries the exported fields the execution engine
// starter needs (the bridge cannot see the runtime's unexported job).
type AgentStartInput struct {
	WI         string
	Project    string
	CapsuleDir string
	BundleName string
	SpawnN     int64
	// StepCandidates is the polyforge#79 effective per-step candidate
	// map this derivation dispatches under (nil = no yaml layer — the
	// bundle's own resolution). The bridge forwards it to the driver.
	// Every candidate is `<harness>:<model>` (polyforge#175).
	StepCandidates map[string][]string
	// InterruptedCause is the recovery lane's cause string when THIS
	// derivation is a recovery re-derivation ("" on a fresh claim): the
	// driver archives the interrupted dispatch's infra attempt from it.
	InterruptedCause string
	// Spawner is the runtime's process spawner (production: pi).
	Spawner Spawner
}

// AgentStartFunc starts the execution engine for a claimed work item
// and returns a supervision handle. The daemon binds the Go driver
// bridge here unconditionally; nil is refused loudly at dispatch —
// there is no fallback engine (polyforge#106).
type AgentStartFunc func(ctx context.Context, in AgentStartInput) (AgentHandle, error)

type job struct {
	wi      string
	project string
	// slug is the work item's human-facing name ("<project>#<seq>") from
	// the claim answer — display only; every key and path stays on wi,
	// except the capsule directory name (api.PathName).
	slug string
	// sessionID is the v1.5 SECRET (B3): 64-hex minted at claim, the git
	// push password. It never reaches the capsule, agent env, argv or
	// any log/event surface — only state.json and the one-shot askpass.
	sessionID string
	// sessionFingerprint is the PUBLIC handle (api.SessionFingerprint).
	sessionFingerprint string
	state              JobState
	stopReason         string // sealed-in reason while SEALING (restart can finish the stop)
	// sealedPush records that the close's seal push already reached the
	// hub: a seal retry or the restart's stop finisher then skips the
	// push (after a STALE_SESSION the credential is dead). Persisted
	// with the record.
	sealedPush bool
	capsuleDir string
	git        gitops.Opts // Username=wi, Secret=session_id (reference.md §3.4)
	transcript string
	bundleName string
	// origin is how the job was submitted (nil = unknown).
	origin *Origin
	// item is the claim answer's work-item snapshot (the F1 materializer
	// input). In-memory only: the durable copy is the capsule's
	// runtime-state/work-item.json, which recovery re-derives from aihub
	// (GetWorkItem) when a capsule predates it.
	item *api.WorkItem
	// repos is the claim-served task-repo inventory (api.Claim.Repos):
	// materialize clones each entry into repos/<name> before the agent
	// starts. In-memory only — recovery never re-materializes, so it is
	// not part of the persisted job record.
	repos []api.ProjectRepo

	// stepCandidates is the polyforge#79 claim-time compose: the
	// per-step post-precedence candidate lists (step id → list; a
	// present-empty entry governs and means no switchable chain). In-
	// memory only; the durable snapshot is the capsule.json audit.
	stepCandidates map[string][]string
	// manualModels is the work item's `pf submit --models` list (top
	// precedence level; kept daemon-side in manual-models.json).
	manualModels []string

	agent          AgentHandle
	agentPID       int
	agentStartedAt string

	spawnN       int
	recoverFails int // consecutive recovery failures; reset on an accepted step result (C7)
	// acceptedAtRelaunch is the accepted-result count (execution state)
	// the last relaunch started from — the watermark the next recovery
	// entry compares against to tell real progress from another failure.
	acceptedAtRelaunch int64
	// lastRelaunchRecovery: the job's live relaunch was a RECOVERY's (not
	// the original claim's) — the mark that lets the ledger count a
	// relaunch that stabilized nothing while never counting the original
	// claim's own death as a recovery failure.
	lastRelaunchRecovery bool
	// cleanPause mirrors the hub's operator-pause flag: latched true by
	// the pause close (Finding 2) so the retained Paused record carries
	// the same marker the hub does; false everywhere else (a graceful
	// shutdown keeps it false — the attach-on-restart shape, M3).
	cleanPause bool
	// endNote/endHead: the pending SessionEnd(blocked) verdict of an
	// end_pending job (see StateEndPending).
	endNote string
	endHead string
	// endPushGaveUp: the end_pending chain push met a PERMANENT rejection
	// (hook refusal, non-fast-forward, size limit) — stop pushing, close
	// without it (the commits stay in the local capsule). Persisted, so a
	// restart's retry does not push again either.
	endPushGaveUp bool
	// sealPushFailNoted: this SEALING series already raised its
	// push_failed event (one per series, not one per seal retry).
	sealPushFailNoted bool
	// awaitingHub: the restart-time session verify could not reach aihub
	// and is being retried (awaitVerify) — status/watch name it, a close
	// is answered "waiting for aihub". Persisted with the record; the
	// next start's verify re-derives it.
	awaitingHub bool
	// claim is the CLAIMING placeholder's lane (B-8); nil otherwise.
	claim *claimLane
	// restartPending: a record Restart seeded whose session verify has
	// not been answered yet (the reconciliation lane, restart.go). The
	// seed latches it busy until the lane reaches it.
	restartPending bool
	// restartDeferNoted: Restart returned with this record still pending
	// and showed it waiting for aihub (its deferred event is out).
	restartDeferNoted bool
	// busy counts the slow network steps of this job running outside
	// opMu (offLock): while it is non-zero no other door may act on the
	// job — Stop answers 409, recovery / supersession / resume / the
	// seal and verify retries / a claim over it back off.
	busy int
	mu   sync.Mutex
}

// isBusy reports whether a network step of j runs outside opMu.
func (j *job) isBusy() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.busy > 0
}

// offLock runs fn — one job's slow network step: a chain push, the
// task-repo WIP push, a SessionEnd — with opMu RELEASED (the claim's
// pattern carried to the closes and the recovery). The caller holds opMu on
// entry and holds it again on return; in between the job is latched busy,
// so no other door interleaves with the step and one work item's hung push
// no longer freezes the stop, pause, resume, wrap and recovery of every
// other. It reports whether j is still the job table's entry for its work
// item once the lock is retaken — when not, the caller must not mutate it.
func (r *Runtime) offLock(j *job, fn func()) (current bool) {
	j.mu.Lock()
	j.busy++
	j.mu.Unlock()
	r.opMu.Unlock()
	defer func() {
		r.opMu.Lock()
		j.mu.Lock()
		j.busy--
		j.mu.Unlock()
		current = r.jobOf(j.wi) == j
	}()
	fn()
	return
}

// errBusy is the refusal of a door that finds j's network step in
// flight (offLock, or a claim/resume latch): the in-flight step decides
// the job.
func errBusy(wi string) error {
	return &api.Error{Code: api.ErrDaemonConflict,
		Detail: fmt.Sprintf("%s: a close, recovery, resume or claim of this work item is in progress (its push or hub call is under way); retry once it settles", wi)}
}

// claimLane is the bookkeeping of one start between its claim and its
// agent (state CLAIMING): the context the clone/materialize runs under,
// a close the operator requested meanwhile and its outcome. ctx, cancel
// and done are fixed at creation; closeReq and closeErr are guarded by
// the job's mu.
type claimLane struct {
	ctx    context.Context
	cancel context.CancelFunc
	// answer is the claim answer (the sealed-head guard reads it).
	answer api.SessionClaim
	// done closes once the claim settled (RUNNING, closed, abandoned or
	// deferred); a Stop that requested a close waits on it.
	done chan struct{}
	// closeReq is the close a Stop asked for (wrapped|failed|paused);
	// closeErr its outcome, valid once done is closed.
	closeReq string
	closeErr error
}

// New builds a Runtime. toOB receives RT→Observe messages, fromOB carries
// Observe→Runtime messages; both sides speak only common/protocol types.
func New(cfg Config, hub Aihub, spawner Spawner, toOB *supervise.Queue[protocol.RTMsg], fromOB *supervise.Queue[protocol.OBMsg]) *Runtime {
	cfg.fill()
	return &Runtime{
		cfg:       cfg,
		hub:       hub,
		spawner:   spawner,
		toOB:      toOB,
		fromOB:    fromOB,
		jobs:      map[string]*job{},
		aliases:   map[string]string{},
		endStop:   make(chan struct{}),
		statePath: filepath.Join(cfg.DataDir, "runtime-state.json"),
	}
}

// DefaultSpawner returns the production spawner: each derivation goes
// to its step harness's spawner (pi or claude, polyforge#159). It takes
// the runtime config BY POINTER: the spawners read its fields at spawn
// time from the shared config, so any field bound after assembly is seen.
func DefaultSpawner(cfg *Config) Spawner {
	return &harnessSpawner{pi: newPiSpawner(cfg), claude: &claudeSpawner{cfg: cfg}}
}

// Run is the OB-message loop: it consumes Observe messages until ctx ends.
// Every message is fenced by the derivation counter; the fencing and
// per-state drops live here (§二: spawn_n ≠ current ⇒ dropped; SEALING
// drops a same-derivation WINeedsRecovery).
func (r *Runtime) Run(ctx context.Context) error {
	for {
		for {
			msg, ok := r.fromOB.Pop()
			if !ok {
				break
			}
			r.handleOB(ctx, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.fromOB.Notify():
		}
	}
}

func (r *Runtime) handleOB(ctx context.Context, msg protocol.OBMsg) {
	r.mu.Lock()
	j := r.jobs[msg.WorkItem]
	r.mu.Unlock()
	if j == nil {
		return // unknown WI: nothing to fence on, drop
	}
	state, spawn := j.snapshot()
	if state == StateClaiming {
		// no monitor watches a CLAIMING job (it has no agent yet): any
		// message for it is stale — never let it quarantine or drop the
		// placeholder (B-8)
		return
	}
	// Finding 1(a) (v1.5 review, MAJOR): OBSuperseded is a SESSION-level
	// verdict, not a derivation-level one — it is EXEMPT from the
	// spawn-equality fence. Fencing it would lose it: a verify in flight
	// while a recovery bumps spawn N→N+1 emits OBSuperseded(N), the fence
	// would drop it at the counter mismatch, and the freshly-derived agent
	// of a WI held elsewhere would run unsupervised forever
	// (detection/push/revalidation all suppressed, the monitor latched).
	// Every other OB kind stays fenced (B2).
	if msg.Kind != protocol.OBSuperseded && msg.SpawnN != spawn {
		return // stale handle: dropped (recovery-loop guard, §二)
	}
	// NEW-1: OBSuperseded bypasses the spawn fence but is
	// fenced on the SESSION fingerprint instead — a stale message buffered
	// in the OB channel must never kill a successor job of the same WI
	// (a re-claim inside the message-residence window). An empty
	// SessionFP is processed.
	if msg.Kind == protocol.OBSuperseded && msg.SessionFP != "" {
		if fp := j.fingerprint(); fp != "" && fp != msg.SessionFP {
			return // a DIFFERENT session's supersession verdict: not ours
		}
	}
	switch msg.Kind {
	case protocol.OBSuperseded:
		// v1.5 M4: the observe lane's periodic session revalidation
		// learned the WI is held elsewhere. NOT a recovery: the new owner
		// owns the work. Kill the tree, quarantine the capsule, drop the
		// record — and make NO hub transition (fencing-on-use already
		// rejects our writes; a SessionEnd would be refused as stale
		// anyway, and a blocked verdict would sabotage the new owner).
		// Reached regardless of the derivation counter (Finding 1(a)).
		if state == StateDone {
			return
		}
		r.supersedeOp(ctx, j, msg.Detail)
	case protocol.OBNeedsRecovery:
		if state != StateRunning {
			// SEALING: push already landed, drop; RECOVERING: duplicate
			// death signal; DONE: terminal.
			return
		}
		r.recoverOp(ctx, j, msg.Reason, msg.Detail)
	}
}

// send queues one RT→Observe message. The queue is unbounded and never
// drops: lifecycle facts must reach Observe (v2-converge §2.2), and the
// sender never blocks.
func (r *Runtime) send(msg protocol.RTMsg) { r.toOB.Push(msg) }

// Close stops the durable-close retry goroutines. Call once the runtime
// message loop (Run) has returned; it is idempotent.
func (r *Runtime) Close() {
	r.endStopOnce.Do(func() {
		if r.endStop != nil {
			close(r.endStop)
		}
	})
}

// closing reports whether Close ran (the daemon is shutting down).
func (r *Runtime) closing() bool {
	if r.endStop == nil {
		return false
	}
	select {
	case <-r.endStop:
		return true
	default:
		return false
	}
}

func (r *Runtime) jobOf(wi string) *job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[wi]
}

// persist rewrites state.json from the live job set. A CLAIMING
// placeholder is never written: its durable form is the claiming journal
// (B-8), which the next start replays.
//
// The snapshot is taken and numbered under mu (no I/O there); the write
// runs under persistMu alone. A caller whose snapshot is older than the
// one already written returns that write's outcome without writing: it
// changed the job set before the newer snapshot was taken, so its change
// is on disk.
func (r *Runtime) persist() error {
	r.mu.Lock()
	r.persistGen++
	gen := r.persistGen
	recs := make([]*jobRecord, 0, len(r.jobs))
	for _, j := range r.jobs {
		if st, _ := j.snapshot(); st == StateClaiming {
			continue
		}
		recs = append(recs, j.record())
	}
	r.mu.Unlock()
	blob, err := json.MarshalIndent(&stateFile{Version: 1, MachineID: r.cfg.MachineID, Jobs: recs}, "", "  ")
	if persistSnapped != nil {
		persistSnapped(gen)
	}
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	if gen <= r.persistDone {
		return r.persistErr
	}
	if err == nil {
		err = writeStateFn(r.statePath, blob, stateFilePerm)
	}
	r.persistDone, r.persistErr = gen, err
	return err
}

// writeStateFn is persist's file write; persistSnapped, when set, runs
// between a snapshot and its write (seams for the write-ordering tests).
var (
	writeStateFn   = atomicfile.Write
	persistSnapped func(gen uint64)
)

func (r *Runtime) dropJob(j *job) {
	r.mu.Lock()
	delete(r.jobs, j.wi)
	// an alias outlives its job harmlessly (Resolve then answers "" and
	// the daemon falls back to aihub), but dropping it keeps the map
	// honest about what is live under this daemon.
	for a, id := range r.aliases {
		if id == j.wi {
			delete(r.aliases, a)
		}
	}
	r.mu.Unlock()
}

// NoteAlias records that alias (a slug or any other non-canonical id
// form) names the canonical work item — the daemon registers the
// operator's submit-time spelling so later route lookups by the same
// spelling resolve locally without an aihub round-trip.
func (r *Runtime) NoteAlias(alias, canonical string) {
	if alias == "" || canonical == "" || alias == canonical {
		return
	}
	r.mu.Lock()
	r.aliases[alias] = canonical
	r.mu.Unlock()
}

// NoteOrigin records the origin the NEXT job started for wi will carry.
// The daemon calls it right before Start; claimPlaceholder consumes it.
func (r *Runtime) NoteOrigin(wi string, o Origin) {
	r.mu.Lock()
	if r.origins == nil {
		r.origins = map[string]Origin{}
	}
	r.origins[wi] = o
	r.mu.Unlock()
}

// NoteModels records the manual `pf submit --models` list the NEXT
// Start of wi will compose as the top precedence level. A nil/empty list
// notes nothing.
func (r *Runtime) NoteModels(wi string, models []string) {
	if len(models) == 0 {
		return
	}
	r.mu.Lock()
	if r.manualModels == nil {
		r.manualModels = map[string][]string{}
	}
	r.manualModels[wi] = models
	r.mu.Unlock()
}

func (r *Runtime) takeModels(wi string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.manualModels[wi]
	delete(r.manualModels, wi)
	return m
}

// takeOrigin pops the noted origin for any of the given id spellings.
func (r *Runtime) takeOrigin(ids ...string) *Origin {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found *Origin
	for _, id := range ids {
		if o, ok := r.origins[id]; ok {
			o := o
			found = &o
			delete(r.origins, id)
		}
	}
	return found
}

// Resolve maps any id form this runtime knows — a live job key (already
// canonical) or a registered alias — to the canonical work-item id;
// "" when the form is unknown to this daemon (the daemon's resolver
// then falls back to aihub's id-or-slug endpoint).
func (r *Runtime) Resolve(id string) string {
	if id == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[id]; ok {
		return id // a job key is canonical by construction (F2)
	}
	return r.aliases[id]
}

// capsuleDirForProject resolves the capsule base for one project through
// the inverse mapping (polyforge#113): <workspace>/capsules/<name>, name
// being the work item's api.PathName ("<project>-<seq>"). An
// unmapped project answers the typed *mapping.UnmappedError — callers on
// the claim path re-surface it as the typed DAEMON-412 refusal BEFORE any
// capsule or claim state exists.
func (r *Runtime) capsuleDirForProject(project, name string) (string, error) {
	ws, err := r.cfg.Mapping.WorkspaceFor(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(ws, "capsules", name), nil
}

func (r *Runtime) gitDirFor(wi string) string {
	return filepath.Join(r.cfg.DataDir, "git", wi+".git")
}

func (r *Runtime) remoteURLFor(wi string) string {
	if r.cfg.GitRemoteBase == "" {
		return ""
	}
	return strings.TrimSuffix(r.cfg.GitRemoteBase, "/") + "/" + wi + ".git"
}

// sessionPathFor is the capsule-side session copy path every derivation
// declares in its spawn journal (session/session-<fence-token>.jsonl): a
// fresh derivation gets its own token-named file, so a re-derivation
// never clobbers a dead derivation's transcript, while a resumed
// derivation keeps the file it continues. The token is the session
// fingerprint (+#N after re-derivations) — public, never the secret.
func sessionPathFor(capsuleDir, token string) string {
	return filepath.Join(capsuleDir, capsule.DirSession, "session-"+token+".jsonl")
}

// randomKey mints an idempotency key (NOT a secret: it only needs
// uniqueness for the server's D4 replay dedup).
func randomKey() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// mintSessionID mints the v1.5 SECRET (reference.md §3.1): 32 crypto-random
// bytes, hex — 64 chars. A CSPRNG failure is FATAL to the start: a
// session secret must never fall back to a clock (that would be a
// guessable credential, the exact failure B3 exists to prevent).
func mintSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("runtime: session secret mint failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------------------
// Start (§一 Runtime): claim → clone/init capsule → materialize → spawn →
// RTStarted.

// Start claims wi and runs it to a live agent. Idempotency rides on the
// claim idempotency key: a retry after a lost response re-claims the same
// session server-side (D4). The claim is the CAS SessionClaim (a paused
// or held WI answers NOT_CLAIMABLE/ALREADY_HELD — the honest refusal,
// never a guess; a WI whose holder is dead is first force-ended by an
// admin, `pf ops end-session`, which returns it to queued for an
// ordinary claim). The secret is minted HERE and journaled durably BEFORE the
// call (B1): a crash between journal and call is recoverable by replay,
// a crash after the call by verify — the ambiguous-claim window is
// closed from both sides.
//
// Locking (B-8): opMu is held for the pre-claim checks, the journal and
// the CLAIMING placeholder, released for every hub call of the claim
// (the work item latched starting meanwhile, claimLocked), for the
// capsule clone and the materialize, and taken again to install the
// agent (or to settle the claim: an operator close, a shutdown deferral,
// the blocked compensation). Start still returns only once the claim
// settled.
func (r *Runtime) Start(ctx context.Context, wi string) error {
	manual := r.takeModels(wi) // always consumed: a refused start must not leak it
	if r.isDown() {
		return claimDeferredError(wi)
	}
	key := r.Resolve(wi)
	if key == "" {
		key = wi
	}
	s, ok := r.tryLockWI(key)
	if !ok {
		return claimInFlightError(wi)
	}
	r.opMu.Lock()
	j, err := r.claimLocked(ctx, wi, manual)
	r.opMu.Unlock()
	r.unlockWI(key, s)
	if err != nil {
		return err
	}
	// The claim landed and the job sits CLAIMING in the table: the clone
	// and the materialize run WITHOUT opMu, so one slow or hung
	// clone no longer freezes the stops, pauses, resumes, recoveries and
	// claims of every other work item.
	return r.runClaim(ctx, wi, j)
}

// claimLocked is Start's locked first half: the pre-claim checks, the
// journaled claim and the CLAIMING placeholder. The caller holds opMu on
// entry and on return; the hub calls in between — the work-item probe, the
// verify of a journaled claim and SessionClaim itself — run with opMu
// RELEASED, so one hung hub call no longer freezes the doors of
// every other work item. Meanwhile the work item's keys (the submitted
// spelling, then the probe's canonical id) are latched starting — a
// concurrent Start of it answers the typed transient, a Stop DAEMON-409 —
// and a retained paused record the claim may replace is latched busy. The
// latches are released only once the CLAIMING placeholder is in the table
// (or the start settled otherwise), so a work item is never claimed twice
// in-process.
func (r *Runtime) claimLocked(ctx context.Context, wi string, manual []string) (*job, error) {
	if err := r.startGate(wi, nil); err != nil {
		return nil, err
	}
	if !r.latchStarting(wi) {
		return nil, claimInFlightError(wi)
	}
	keys := []string{wi}
	var latched []*job
	defer func() {
		r.unlatchStarting(keys...)
		for _, p := range latched {
			p.mu.Lock()
			p.busy--
			p.mu.Unlock()
		}
	}()
	latchPaused := func(key string) {
		if j := r.jobOf(key); j != nil {
			for _, p := range latched {
				if p == j {
					return
				}
			}
			j.mu.Lock()
			j.busy++
			j.mu.Unlock()
			latched = append(latched, j)
		}
	}
	// own reports whether the job the gate meets under key is one this
	// start latched itself (the retained paused record it may replace).
	own := func(key string) *job {
		j := r.jobOf(key)
		for _, p := range latched {
			if p == j {
				return j
			}
		}
		return nil
	}
	latchPaused(wi)
	// Pre-claim placement + workspace-yaml check (polyforge#113, #79):
	// resolve the work item's project (read-only GetWorkItem), then load
	// its workspace's .polyforge.yaml strictly — BEFORE anything is
	// journaled or claimed. An unmapped project (DAEMON-412) or an
	// invalid yaml (DAEMON-414) refuses here: no capsule, and the WI stays
	// queued at aihub so another machine may take it — a bad local file
	// must not block the team's task. A failed probe claims nothing
	// either (no project, no workspace to read): the caller retries.
	var probe *api.WorkItem
	var perr error
	r.hubUnlocked(func() { probe, perr = r.hub.GetWorkItem(wi) })
	if err := r.startGate(wi, own(wi)); err != nil {
		return nil, err
	}
	if perr != nil {
		var ae *api.Error
		if errors.As(perr, &ae) && ae.HTTPStatus() < 500 {
			// the hub answered and refused (unknown WI, forbidden): final
			return nil, fmt.Errorf("runtime: %s: work-item probe refused (not claiming): %w", wi, perr)
		}
		// transport loss or a hub 5xx: typed transient, the caller retries
		return nil, &api.Error{Code: api.ErrDaemonHubUnavailable,
			Detail: fmt.Sprintf("%s: work-item probe failed (not claiming; retry later): %v", wi, perr)}
	}
	if probe == nil || probe.ProjectName == "" {
		return nil, fmt.Errorf("runtime: %s: work-item probe answered no project (not claiming)", wi)
	}
	if id := probe.ID; id != "" && id != wi {
		// the canonical id is known now: latch it too, so a start of the
		// same work item under another spelling cannot claim alongside
		if !r.latchStarting(id) {
			return nil, claimInFlightError(wi)
		}
		keys = append(keys, id)
		if err := r.startGate(id, own(id)); err != nil {
			return nil, err
		}
		latchPaused(id)
	}
	wcfg, werr := r.LoadWorkspace(probe.ProjectName)
	if werr != nil {
		return nil, werr
	}
	// polyforge#175: a bundle whose candidates are not `<harness>:<model>`
	// is a local defect too — refuse before the claim so the work item
	// stays queued (claimPlaceholder re-checks as the backstop).
	if probe.BundleName != nil {
		if berr := r.CheckBundle(wi, *probe.BundleName); berr != nil {
			return nil, berr
		}
	}
	// B1: the claim's idempotency key AND the session secret are
	// journaled durably BEFORE the claim call. A crash anywhere between
	// the claim and the spawn would otherwise orphan a running session
	// nobody can name again — the journal lets Restart verify the
	// journaled session, replay the SAME claim (same key → same response,
	// D4) and either finish the start or compensate with
	// SessionEnd(blocked).
	//
	// A journal entry left by an earlier attempt whose claim answer was
	// lost (transport error, timeout, hub 5xx — see below) is REUSED
	// rather than replaced: this start is then a replay of the same claim,
	// so a claim the hub had committed is adopted instead of
	// orphaned.
	sessionID, key, replay, err := r.claimIntent(wi, func() error { return r.startGate(wi, own(wi)) })
	if err != nil {
		return nil, err
	}
	if err := r.noteClaiming(wi, sessionID, key, manual); err != nil {
		return nil, fmt.Errorf("runtime: %s: claiming journal write failed (refusing to claim): %w", wi, err)
	}
	req := api.SessionClaimRequest{
		WorkItemID:     wi,
		SessionID:      sessionID,
		MachineID:      r.cfg.MachineID,
		IdempotencyKey: key,
	}
	// No gate after the claim: whatever happened meanwhile (a shutdown
	// latch included) the answer is settled below — claimPlaceholder
	// defers a landed claim the shutdown caught, journal kept.
	var resp *api.SessionClaimResponse
	r.hubUnlocked(func() { resp, err = r.hub.SessionClaim(req) })
	if err != nil {
		if claimRefused(err) {
			if replay {
				// a REPLAY of a claim whose verify matched — the session
				// is open at the hub under the secret we hold. Dropping
				// the journal would orphan it: end it blocked instead
				// (abandonClaim keeps the entry until the close lands).
				return nil, r.abandonClaim(&job{wi: wi, sessionID: sessionID}, wi, err,
					"the replayed claim of an open session was refused: "+err.Error())
			}
			// the hub answered and refused: the claim never opened,
			// nothing can dangle — drop the journal entry so neither a
			// retry nor a restart replays a dead intention.
			r.clearClaimingSession(wi, sessionID)
			return nil, err
		}
		// Transport loss, timeout or a hub 5xx: the claim MAY have been
		// committed with the answer lost. The journal entry (the only
		// copy of the secret) is KEPT and the start answers the typed
		// transient, so the dispatcher requeues with backoff; the retry
		// replays the same claim (claimIntent), and a restart in between
		// replays it from the journal (resumeClaiming).
		return nil, &api.Error{Code: api.ErrDaemonHubUnavailable,
			Detail: fmt.Sprintf("%s: claim answer lost (claiming journal kept; the retry replays the same claim): %v", wi, err)}
	}
	// the paused records this start latched are released before the
	// placeholder replaces one of them (the deferred release then finds
	// none left)
	for _, p := range latched {
		p.mu.Lock()
		p.busy--
		p.mu.Unlock()
	}
	latched = nil
	return r.claimPlaceholder(ctx, wi, sessionID, resp, wcfg, manual)
}

// startGate is the pre-claim admission check of a Start of key (opMu
// held), re-run after each of its unlocked hub calls: no shutdown latch,
// no session end of an earlier claim on the wire, no other job of the
// work item in the table — except own, the retained paused record this
// start itself latched, and any other retained paused record (Finding 2:
// not an active run; claimPlaceholder replaces it once the hub accepted
// the new claim, and the hub's own ALREADY_HELD gate refuses a plain
// claim of a paused-held WI).
func (r *Runtime) startGate(key string, own *job) error {
	if r.isDown() {
		return claimDeferredError(key)
	}
	if r.isRestarting() {
		// the restart reconciliation is still waiting on aihub (its
		// journal replays and records decide which work items are ours):
		// transient — the dispatcher and the resume-blocked door retry
		return &api.Error{Code: api.ErrDaemonHubUnavailable,
			Detail: fmt.Sprintf("%s: the daemon's restart reconciliation is still waiting for aihub; retry later", key)}
	}
	if r.claimEnding(key) {
		return &api.Error{Code: api.ErrDaemonHubUnavailable,
			Detail: fmt.Sprintf("%s: the session end of an earlier claim of this work item is still on the wire; retry later", key)}
	}
	j := r.jobOf(key)
	if j == nil || j == own {
		return nil
	}
	if j.isBusy() {
		// its close, recovery or resume is on the wire (B-8): transient —
		// the dispatcher and the resume-blocked door retry a DAEMON-503
		return &api.Error{Code: api.ErrDaemonHubUnavailable,
			Detail: fmt.Sprintf("%s: a close, recovery or resume of this work item is still on the wire; retry later", key)}
	}
	if st, _ := j.snapshot(); st != StatePaused {
		return fmt.Errorf("runtime: %s already active (state %s)", key, st)
	}
	return nil
}

// hubUnlocked runs fn — one hub call of a claim or a resume that has no
// job of its own to latch busy — with opMu RELEASED; the caller holds
// opMu on entry and holds it again on return, and re-checks whatever it
// relies on.
func (r *Runtime) hubUnlocked(fn func()) {
	r.opMu.Unlock()
	defer r.opMu.Lock()
	fn()
}

// latchStarting latches key as claiming (under mu); false when another
// start already holds it.
func (r *Runtime) latchStarting(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startingClaims[key] {
		return false
	}
	if r.startingClaims == nil {
		r.startingClaims = map[string]bool{}
	}
	r.startingClaims[key] = true
	return true
}

func (r *Runtime) unlatchStarting(keys ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		delete(r.startingClaims, k)
	}
}

// claimStarting reports whether a start of key is between its first hub
// call and its CLAIMING placeholder (claimLocked).
func (r *Runtime) claimStarting(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startingClaims[key]
}

// claimInFlightError is the typed transient a Start of a work item meets
// while another start of it is on its claim's hub calls.
func claimInFlightError(wi string) error {
	return &api.Error{Code: api.ErrDaemonHubUnavailable,
		Detail: fmt.Sprintf("%s: another start of this work item is claiming (its hub calls are under way); retry later", wi)}
}

// claimRefused reports whether a SessionClaim error is the hub's own
// typed refusal (a 4xx envelope): the claim did not open. Anything else —
// a transport error, a timeout, a 5xx — leaves the outcome unknown.
func claimRefused(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.HTTPStatus() < 500
}

// claimIntent answers the session secret and idempotency key this start
// claims with. With no journal entry for wi it mints a fresh pair.
// An entry left by an earlier attempt whose answer was lost is checked
// first: SessionVerify says the journaled session holds the WI → the claim
// landed, reuse the pair so the claim is a D4 replay that hands the same
// answer back; verify says it does not → the earlier claim never landed or
// was ended since (a replay could hand back a stale cached answer) — drop
// the entry and mint fresh; verify unreachable → the typed transient, entry
// kept. The verify runs with opMu released (hubUnlocked); gate re-checks
// the start's admission once the lock is back.
func (r *Runtime) claimIntent(wi string, gate func() error) (sessionID, key string, replay bool, err error) {
	if rec := r.loadClaiming()[wi]; rec != nil && rec.SessionID != "" && rec.IdempotencyKey != "" {
		var v *api.SessionVerifyResponse
		var verr error
		r.hubUnlocked(func() { v, verr = r.hub.SessionVerify(rec.WorkItemID, rec.SessionID) })
		if gerr := gate(); gerr != nil {
			return "", "", false, gerr // entry kept: nothing was decided
		}
		switch {
		case verr != nil && !claimRefused(verr):
			return "", "", false, &api.Error{Code: api.ErrDaemonHubUnavailable,
				Detail: fmt.Sprintf("%s: verify of the journaled claim failed (claiming journal kept; retry later): %v", wi, verr)}
		case verr == nil && v.Matches:
			return rec.SessionID, rec.IdempotencyKey, true, nil
		}
		r.clearClaimingSession(wi, rec.SessionID)
	}
	sessionID, err = mintSessionID()
	if err != nil {
		return "", "", false, err
	}
	return sessionID, randomKey(), false, nil
}

// claimPlaceholder is the locked post-claim bookkeeping of one OPEN claim
// (fresh or replayed; opMu held): the canonical id, the replaced paused
// record, the manual models, the job itself, its capsule placement and
// the per-step compose — then the job enters the table as the CLAIMING
// placeholder (B-8) with the context its clone/materialize runs under
// (runClaim carries it to a live agent). Every failure after the claim
// is compensated with SessionEnd(blocked) — the D5 rule carried into the
// session model: a post-claim failure must never leave an orphaned
// running session; the ticket needs human attention, and the hub's
// session row is cleared either way. The claiming journal entry is
// cleared on every settled outcome.
//
// sessionID is THIS daemon's minted secret (the journal replay path passes
// the journaled one back; the claim answer's fingerprint must agree).
//
// wi may arrive as the canonical id OR any id-or-slug form the aihub claim
// endpoint accepts ("<project>#<seq>"): the job and every machine-side
// path derived from it — external git-dir, push remote, spawn journal —
// are keyed by the CANONICAL work-item id the claim answer carries (F2).
// The capsule directory alone is named for humans: the claim answer's
// slug in its path-safe form "<project>-<seq>" (api.PathName). A raw slug
// never reaches any path: its "#" is the URL fragment separator (a
// slug-built push remote answers 404) and a glob character. The submitted
// spelling is registered as an alias so lookups by it still resolve.
func (r *Runtime) claimPlaceholder(ctx context.Context, wi, sessionID string, resp *api.SessionClaimResponse, wcfg *workspace.Config, manual []string) (*job, error) {
	c := resp.Claim
	// canonical id resolution (F2): the claim answer's work item is the
	// authority; the claim's own WorkItemID and the answered record's ID
	// agree server-side, the record wins here because it also feeds the
	// materialized task-definition file.
	id := resp.WorkItem.ID
	if id == "" {
		id = c.WorkItemID
	}
	r.NoteAlias(wi, id)
	if existing := r.jobOf(id); existing != nil {
		// Finding 2 (v1.5 review): a RETAINED paused record is not an
		// active run — a successful claim over it (the holder daemon kept
		// the paused record; an admin force-ended the session and the
		// WI was claimed again) replaces the dead record: the new session owns
		// the WI, and prepareCapsule quarantines the old scene on the
		// session-marker mismatch exactly as it would have after a
		// restart release. Every other live state keeps the duplicate
		// refusal — for a Restart replay of the claiming journal this
		// means the original start registered the job before the crash
		// (the state.json record reconciles it), so only the journal is
		// stale.
		if existing.state != StatePaused {
			r.clearClaiming(wi)
			return nil, fmt.Errorf("runtime: %s already active (state %s)", id, existing.state)
		}
		r.dropJob(existing)
		// the alias registered above died with the record; re-note it so
		// later route lookups by the submitted spelling still resolve.
		r.NoteAlias(wi, id)
		if err := r.persist(); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", id, err)
		}
	}
	fp := api.SessionFingerprint(sessionID)
	if c.SessionFingerprint != "" && c.SessionFingerprint != fp {
		// the server computed a different fingerprint for our secret: the
		// answer belongs to another session (a stale idempotency cache on
		// the server side). Refuse rather than adopt someone else's claim.
		return nil, fmt.Errorf("runtime: %s: claim answer fingerprint %s does not match this session (%s)", id, c.SessionFingerprint, fp)
	}
	// The manual `pf submit --models` list is daemon-local: a submit that
	// carries one records it under the daemon's data dir; a re-claim
	// (pf resume of a blocked WI) carries none and reads it back from
	// there. Never from the capsule — the agent writes there and the
	// capsule travels to other machines with the WI.
	if len(manual) > 0 {
		r.saveManualModels(id, manual)
	} else {
		manual = r.loadManualModels(id)
	}
	j := &job{
		wi:                 id,
		project:            resp.WorkItem.ProjectName,
		slug:               resp.WorkItem.Slug,
		sessionID:          sessionID,
		sessionFingerprint: fp,
		state:              StateClaiming,
		git: gitops.Opts{
			WorkItemID: id,
			// §1.6 fencing-on-use backup layer: the PUBLIC fingerprint.
			Fingerprint: fp,
			GitDir:      r.gitDirFor(id),
			RemoteURL:   r.remoteURLFor(id),
			Username:    id,
			Secret:      sessionID, // v1.5: the session secret IS the push password
			EventSink:   r.gitEventSink(id),
			// once-per-repo carrier_push_failed state: per job, so a
			// re-claim (a new job) alerts afresh.
			CarrierAlerts: &gitops.CarrierAlerts{},
		},
		bundleName:   c.BundleName,
		repos:        c.Repos,
		item:         &resp.WorkItem,
		origin:       r.takeOrigin(wi, id),
		manualModels: manual,
	}
	// polyforge#113: the capsule routes by the claim answer's project
	// through the inverse mapping. An unmapped answer here (a mapping
	// edit between the pre-check and the claim answer, or a journal
	// replay over a changed config) compensates with the D5 blocked end —
	// no capsule directory is ever touched.
	capDir, rerr := r.capsuleDirForProject(j.project, api.PathName(j.slug, id))
	if rerr != nil {
		err2 := unmappedProjectError(j.project)
		return nil, r.abandonClaim(j, wi, err2, err2.Detail)
	}
	j.capsuleDir = capDir
	j.git.CapsuleDir = capDir
	j.transcript = sessionPathFor(j.capsuleDir, fp)

	// polyforge#79: the workspace yaml was loaded and validated BEFORE the
	// claim (Start / the journal replay); compose the per-step candidates
	// for the claimed project from that snapshot.
	// polyforge#175: a bundle whose candidates are not `<harness>:<model>`
	// (or that still declares a `harness` key) is a load error naming the
	// file and the step; the claim ends blocked with it.
	fm, ferr := r.readBundleFrontmatter(j.bundleName)
	if ferr != nil {
		return nil, r.abandonClaim(j, wi, ferr, ferr.Error())
	}
	j.stepCandidates = workspace.ComposeCandidates(fm, wcfg, j.project, j.manualModels)

	cctx, cancel := context.WithCancel(ctx)
	j.claim = &claimLane{ctx: cctx, cancel: cancel, done: make(chan struct{}), answer: c}
	r.mu.Lock()
	r.jobs[id] = j
	aborted := r.down
	r.mu.Unlock()
	if aborted {
		cancel() // a shutdown latched while this claim ran: do not even clone
	}
	return j, nil
}

// runClaim carries a CLAIMING job to a live agent. The caller does NOT
// hold opMu (B-8): the capsule clone, the sealed-head guard and the
// materialize run under the claim's own context, and opMu is taken only
// to settle (Start's lane, and the restart's journal replays).
func (r *Runtime) runClaim(ctx context.Context, wi string, j *job) error {
	note, sceneErr := r.claimScene(j)
	s := r.lockWI(j.wi)
	defer r.unlockWI(j.wi, s)
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.finishClaim(ctx, wi, j, sceneErr, note)
}

// claimScene lays the claimed capsule out: clone (or reuse this
// session's own husk), the cross-daemon sealed-head guard, the spawn
// counter seed and the materialize (task-repo clones included). It runs
// without opMu on Start's lane and touches nothing but this job's own
// scene. A failure answers the error and the close note.
func (r *Runtime) claimScene(j *job) (note string, err error) {
	ctx := j.claim.ctx
	if err := ctx.Err(); err != nil {
		return err.Error(), err
	}
	if err := r.prepareCapsule(ctx, j); err != nil {
		return "prepare capsule: " + err.Error(), err
	}
	if err := r.verifySealedHead(j, j.claim.answer); err != nil {
		return err.Error(), err
	}
	// B2 spawn_n source: a FRESHLY CLAIMED capsule may carry another
	// session's spawn journal (the re-claim/attach clone path — the
	// journal rides capsule git), so the derivation counter continues
	// where the capsule's history left off; a brand-new capsule answers
	// 0 and the first derivation is #1.
	j.mu.Lock()
	j.spawnN = spawnJournalMaxN(j.capsuleDir)
	j.mu.Unlock()
	if err := r.materialize(ctx, j); err != nil {
		// A typed task-repo clone failure (DAEMON-507) keeps its detail as
		// the close note; every other materialization failure names itself.
		note := err.Error()
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.ErrDaemonSpawnFail && ae.Detail != "" {
			note = ae.Detail
		}
		return note, err
	}
	return "", nil
}

// finishClaim settles a CLAIMING job (opMu held). In order: a close the
// operator requested meanwhile is honoured (closeClaim); a shutdown
// (BeginShutdown, or the start's own context ending) DEFERS the claim — the
// placeholder goes, the claiming-journal entry stays, and the next daemon
// start replays it, so a cancelled clone never turns the work item
// blocked; a scene failure is the D5 blocked compensation; otherwise the
// engine starts and the job turns RUNNING.
func (r *Runtime) finishClaim(ctx context.Context, wi string, j *job, sceneErr error, note string) error {
	cl := j.claim
	var closeErr error
	defer func() {
		cl.cancel()
		j.mu.Lock()
		cl.closeErr = closeErr
		j.mu.Unlock()
		close(cl.done)
	}()
	j.mu.Lock()
	closeReq := cl.closeReq
	j.mu.Unlock()
	if closeReq != "" {
		// the close's own outcome goes to the Stop that asked for it; the
		// start answers the settled close either way (never a retry: a
		// failed session-end is the blocked-close retry's business)
		closeErr = r.closeClaim(wi, j, closeReq)
		return fmt.Errorf("%w: %s (%s)", ErrClaimClosed, j.wi, closeReq)
	}
	if r.isDown() || ctx.Err() != nil {
		r.dropJob(j)
		fmt.Fprintf(os.Stderr, "runtime: %s: claim deferred by the shutdown (claiming journal kept; the next start replays it)\n", j.wi)
		return claimDeferredError(wi)
	}
	if sceneErr != nil {
		r.dropJob(j)
		return r.abandonClaim(j, wi, sceneErr, note)
	}
	if r.jobOf(j.wi) != j {
		// defensive: nothing but finishClaim drops a placeholder, but an
		// agent must never be installed for a job the table no longer
		// holds — end the session blocked instead.
		err := fmt.Errorf("runtime: %s: the claim's placeholder left the job table before its agent started", j.wi)
		return r.abandonClaim(j, wi, err, err.Error())
	}
	return r.installClaim(ctx, wi, j)
}

// installClaim starts the execution engine for a materialized CLAIMING
// job and turns it RUNNING (opMu held).
func (r *Runtime) installClaim(ctx context.Context, wi string, j *job) error {
	id := j.wi
	// The Go driver is the engine. The daemon binds bridge.Start as
	// AgentStarter at assembly; the driver owns the step state machine from
	// here on. There is no second engine to fall back to (polyforge#106): a
	// nil starter is a misassembly and refuses the claim loudly.
	starter := r.cfg.AgentStarter
	if starter == nil {
		err2 := fmt.Errorf("runtime misassembly: no execution engine bound (Config.AgentStarter is nil) — the legacy in-package WI-Agent engine was deleted (polyforge#106); the daemon binds the driver bridge starter unconditionally at assembly")
		r.dropJob(j)
		return r.abandonClaim(j, wi, err2, err2.Error())
	}
	if !r.admit() {
		// the shutdown latched since finishClaim's check: no agent starts
		// behind it — the claim is deferred like any the shutdown caught
		r.dropJob(j)
		fmt.Fprintf(os.Stderr, "runtime: %s: claim deferred by the shutdown (claiming journal kept; the next start replays it)\n", j.wi)
		return claimDeferredError(wi)
	}
	{
		// The derivation IS a derivation (B2): advance the counter so the
		// RTStarted emission carries SpawnN ≥ 1. The observe lane refuses
		// to attach a monitor on SpawnN ≤ 0 ("RTStarted without
		// handle/spawn_n/wi-info"), and the step ordinals the driver
		// derives seed from this counter — both the fence and the monitor
		// face need it to move.
		j.mu.Lock()
		j.spawnN++
		spawnN := int64(j.spawnN)
		j.mu.Unlock()
		h, err2 := starter(ctx, AgentStartInput{
			WI:         j.wi,
			Project:    j.project,
			CapsuleDir: j.capsuleDir,
			BundleName: j.bundleName,
			SpawnN:     spawnN,
			Spawner:    r.spawner,
			// polyforge#79: the claim-time compose rides to the driver —
			// every dispatch of this derivation resolves its model from
			// the post-precedence effective list.
			StepCandidates: j.stepCandidates,
		})
		if err2 != nil {
			r.release()
			r.dropJob(j)
			return r.abandonClaim(j, wi, err2, "driver start: "+err2.Error())
		}
		// Bind the handle as the job's agent so the supervision and answer
		// surfaces can reach the engine. A bridge handle fronts no child
		// process group (its PID() answers the hosting daemon's own pid —
		// never signalable), so the recorded pid is zeroed for it; a
		// scripted test engine's real child pid is recorded verbatim for
		// the tree killer.
		pid := h.PID()
		if pid == os.Getpid() {
			pid = 0
		}
		j.mu.Lock()
		j.agent = h
		j.agentPID = pid
		j.agentStartedAt = h.StartedAt()
		j.mu.Unlock()
		r.release()
	}
	if err := j.transition(StateRunning); err != nil {
		return err // unreachable: only finishClaim (opMu held) leaves CLAIMING
	}
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", id, err)
	}
	// The start settled: the claiming journal entry has done its job.
	r.clearClaiming(wi)
	j.mu.Lock()
	pid := j.agentPID
	spawnN := j.spawnN
	j.mu.Unlock()
	r.send(protocol.RTMsg{
		Kind:     protocol.RTStarted,
		SpawnN:   spawnN,
		WorkItem: id,
		AgentPID: pid,
		// the secret rides in-process to the observe lane only (its M4
		// session revalidation needs it); never a log, never the capsule.
		SessionID: j.sessionID,
	})
	return nil
}

// unmappedProjectError is the typed DAEMON-412 placement refusal.
func unmappedProjectError(project string) *api.Error {
	return &api.Error{
		Code: api.ErrDaemonUnmappedProject,
		Detail: fmt.Sprintf("project %q is not in the [workspace] mapping of this machine — the claim is refused before any capsule is created; add the project to a [workspace.\"<path>\"] section in the config (`pf doctor` checks the mapping)",
			project),
	}
}

// LoadWorkspace is the polyforge#79 pre-claim read (fresh, no cache, so
// an edit governs the next claim with no daemon restart): resolve the
// project's workspace through the inverse mapping (DAEMON-412 when
// unmapped) and strictly validate <root>/.polyforge.yaml through the
// shared loader, its per-step keys against the step ids of the installed
// workflows and every candidate's `<harness>:` prefix (polyforge#175)
// — DAEMON-414 when invalid. The parsed config is nil when the
// workspace carries no yaml (absent-equivalent: bundle defaults).
// Nothing is claimed or created by it; the daemon's submit face and
// auto-pull call it too, so a refusal reaches the operator before enqueue.
func (r *Runtime) LoadWorkspace(project string) (*workspace.Config, error) {
	ws, err := r.cfg.Mapping.WorkspaceFor(project)
	if err != nil {
		return nil, unmappedProjectError(project)
	}
	wcfg, err := workspace.Load(ws, r.cfg.WorkflowsDir, r.cfg.Mapping.ProjectsOf(ws))
	if err != nil {
		return nil, &api.Error{
			Code:   api.ErrDaemonWorkspaceYaml,
			Detail: fmt.Sprintf("the workspace yaml of project %q refused the claim before it was made (the work item stays queued): %v", project, err),
		}
	}
	return wcfg, nil
}

// EvClaimPausedEndedBlocked is the runtime event of a pf pause that
// landed while the claim was still materializing (B-8): there is no
// scene to retain a paused record for, so the session ended blocked and
// `pf resume` re-claims the work item.
const EvClaimPausedEndedBlocked = "claim_paused_ended_blocked"

// CheckBundle is the polyforge#175 pre-claim bundle read: the installed
// bundle's candidates must all be `<harness>:<model>` (and it may carry
// no `harness` key). A refusal is the typed DAEMON-414 naming the file
// and step — nothing is claimed, the work item stays queued. An empty
// bundle name (not bound yet) passes.
func (r *Runtime) CheckBundle(wi, bundle string) error {
	if bundle == "" {
		return nil
	}
	if bundle == workflow.SharedDir {
		return &api.Error{
			Code:   api.ErrDaemonWorkspaceYaml,
			Detail: fmt.Sprintf("the workflow bundle of %s refused the claim before it was made (the work item stays queued): %q holds the shared step definitions and is not a workflow bundle", wi, workflow.SharedDir),
		}
	}
	if _, ferr := r.readBundleFrontmatter(bundle); ferr != nil {
		return &api.Error{
			Code:   api.ErrDaemonWorkspaceYaml,
			Detail: fmt.Sprintf("the workflow bundle of %s refused the claim before it was made (the work item stays queued): %v", wi, ferr),
		}
	}
	return nil
}

// RefusedBundles answers the installed bundles whose frontmatter fails to
// load (polyforge#175: an unprefixed candidate, a retired `harness`
// key, or torn yaml), name → load error — the auto-pull wave skips work
// items on them instead of enqueueing a claim that is bound to refuse.
func (r *Runtime) RefusedBundles() map[string]error {
	_, bad, _ := workspace.Bundles(r.cfg.WorkflowsDir, "")
	return bad
}

// readBundleFrontmatter reads the pinned bundle's workflow.md
// frontmatter from the daemon checkout. A torn frontmatter, or one whose
// candidate format is refused (polyforge#175), answers the load error
// naming the file and the step; a missing one answers (nil, nil).
func (r *Runtime) readBundleFrontmatter(bundleName string) (*workflow.Frontmatter, error) {
	if bundleName == "" || r.cfg.WorkflowsDir == "" {
		return nil, nil
	}
	fm, err := workflow.ReadFrontmatter(filepath.Join(r.cfg.WorkflowsDir, bundleName, "workflow.md"))
	if err != nil {
		return nil, fmt.Errorf("workflow bundle %s refused: %w", bundleName, err)
	}
	return fm, nil
}

// abandonClaim is the D5 compensation carried into the session model:
// every post-claim failure ends the session BLOCKED at aihub (the hub
// clears the session row; the WI leaves the claimable set until a human
// intervenes) and clears the claiming journal, then hands the original
// error back to the caller — a bad remote, a diverged bundle, a spawn
// refusal all need human attention, never an orphaned running session
// and never a daemon crash. A failed CLOSE keeps the journal entry (a
// later restart replays the same key and retries the compensation); it
// is loud but never masks the original error.
func (r *Runtime) abandonClaim(j *job, submitted string, origErr error, note string) error {
	if cerr := r.claimEndOffLock(j.wi, submitted, api.SessionEndRequest{
		SessionID: j.sessionID,
		Status:    api.StatusBlocked,
		Note:      note,
	}); cerr != nil {
		// v2-converge §6 bug 1: a clone failure is most often an
		// unreachable aihub, so the compensation fails too — it must not
		// be a one-shot. The journal entry (the only copy of the secret)
		// stays, the close is retried in-process with backoff, and a
		// restart replays the journal.
		fmt.Fprintf(os.Stderr, "runtime: %s: session-end(blocked) after failed start failed (claiming entry kept; retrying with backoff): %v\n", j.wi, cerr)
		r.retryClaimEnd(submitted, j.wi, j.sessionID, note)
		return origErr
	}
	r.clearClaimingSession(submitted, j.sessionID)
	return origErr
}

// claimEndOffLock sends the SessionEnd of a claim that never got (or no
// longer has) a job in the table — the compensation of a failed start,
// an operator close during the claim — with opMu RELEASED:
// the caller holds opMu on entry and again on return.
// Meanwhile the work item's keys are latched ending, so a Start of it
// answers the typed transient (the dispatcher retries) rather than
// replaying the claim this close is ending.
func (r *Runtime) claimEndOffLock(wi, submitted string, req api.SessionEndRequest) error {
	keys := []string{wi}
	if submitted != wi {
		keys = append(keys, submitted)
	}
	r.mu.Lock()
	if r.endingClaims == nil {
		r.endingClaims = map[string]int{}
	}
	for _, k := range keys {
		r.endingClaims[k]++
	}
	r.mu.Unlock()
	r.opMu.Unlock()
	defer func() {
		r.opMu.Lock()
		r.mu.Lock()
		for _, k := range keys {
			if r.endingClaims[k]--; r.endingClaims[k] <= 0 {
				delete(r.endingClaims, k)
			}
		}
		r.mu.Unlock()
	}()
	return r.hub.SessionEnd(wi, req)
}

// claimEnding reports whether a claim of wi is ending (claimEndOffLock).
func (r *Runtime) claimEnding(wi string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endingClaims[wi] > 0
}

// verifySealedHead is the cross-daemon continue guard (v2-converge §7): a
// blocked WI claimed by a DIFFERENT machine than the one that sealed it must
// find the sealing daemon's recorded capsule head in its fresh clone,
// otherwise the clone is pre-blocked state (the #116 lesson) and the start
// is refused. A same-machine continue, or a WI with no recorded seal, is
// not gated here.
func (r *Runtime) verifySealedHead(j *job, c api.SessionClaim) error {
	if c.SealedHead == "" || c.SealedMachine == "" || c.SealedMachine == r.cfg.MachineID {
		return nil
	}
	head, err := gitops.Head(j.git)
	if err == nil && head == c.SealedHead {
		return nil
	}
	return &api.Error{Code: api.ErrDaemonSealedHeadMismatch,
		Detail: fmt.Sprintf("continue refused: the work item was sealed blocked by machine %q at capsule head %s, but the aihub capsule this daemon cloned is at %q (%v) — the sealing daemon's last push did not land; continue it on %q, or abandon (pf cancel)",
			c.SealedMachine, c.SealedHead, head, err, c.SealedMachine)}
}

// prepareCapsule makes CapsuleDir a git worktree whose git-dir is EXTERNAL
// (ruling #2: agents never reach hooks/config/index). The aihub bare repo is
// cloned straight into that layout (gitops.Clone: the git-dir at
// o.GitDir, a one-line .git pointer file in the worktree; an
// empty-history clone succeeds with a warning).
//
// v1.5 capsule ownership: generations no longer discriminate a reusable
// husk from a foreign scene — the session does. materialize stamps the
// owning session's FINGERPRINT into sync/session-handle (daemon-private,
// gitignored: it deliberately does NOT travel with a re-claim clone), so:
//
//	amarker == this session's fingerprint → this session's own
//	  crashed-Start husk (or attach-resume scene): reuse in place;
//	anything else (another session's scene, a sealed close, a pre-v1.5
//	  capsule with no marker) → quarantine, never reuse.
func (r *Runtime) prepareCapsule(ctx context.Context, j *job) error {
	o := j.git
	pointer := filepath.Join(o.CapsuleDir, ".git")
	if isRegularFile(pointer) && dirExists(o.GitDir) &&
		readSessionMarker(o.CapsuleDir) == j.sessionFingerprint {
		return nil // this session's own crashed-Start husk: reuse
	}
	// Anything else (another session's scene, an unprepared husk, a
	// pointer without a git-dir, an orphan git-dir with no worktree) is
	// quarantined as ONE unit: worktree AND external git-dir move aside
	// together, so the fresh clone can never meet a previous session's
	// HEAD/index (a stale git-dir reused under a fresh worktree made
	// every later push non-fast-forward).
	if err := quarantineCapsule(o.CapsuleDir, o.GitDir); err != nil {
		return err
	}
	if o.RemoteURL == "" {
		// A capsule whose origin cannot be resolved is never silently
		// init'ed locally and restarted from step 1 (there is no local-init
		// escape hatch: aihub always supplies the remote at claim). Loud and
		// typed: the caller abandons the claim.
		return &api.Error{Code: api.ErrDaemonCapsuleClone,
			Detail: fmt.Sprintf("capsule clone failed: no remote URL is configured for %s — aihub always supplies one at claim; refusing a local init", j.wi)}
	}
	if err := gitops.Clone(ctx, o); err != nil {
		// aihub creates the bare repo at claim, so a clone failure is
		// never "nothing published yet": refuse loudly (the caller
		// abandons the claim) rather than init locally and restart the
		// workflow from step 1.
		return &api.Error{Code: api.ErrDaemonCapsuleClone,
			Detail: fmt.Sprintf("capsule clone failed: %v", err)}
	}
	return nil
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// relSessionMarker is the capsule-local session ownership tag: the
// fingerprint of the session that owns this worktree, under sync/ —
// daemon-private, gitignored, so it never travels with a clone
// and never reaches the revision chain.
const relSessionMarker = "sync/session-handle"

// writeSessionMarker stamps the owning session's fingerprint (B3 public
// handle; the secret itself never lands in the capsule).
func writeSessionMarker(capsuleDir, fingerprint string) error {
	path := filepath.Join(capsuleDir, filepath.FromSlash(relSessionMarker))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fingerprint+"\n"), 0o644)
}

// readSessionMarker answers the owning session's fingerprint ("" when
// absent or unreadable).
func readSessionMarker(capsuleDir string) string {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, filepath.FromSlash(relSessionMarker)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// quarantineCapsule moves a stale/foreign/superseded capsule aside, never
// deleting it (v1.5 naming, reference.md §3.5: capsules/<name> →
// capsules/<name>.quarantined.<ts> — a sibling, so the scene sits next to
// whatever replaces it and the bytes always survive).
func quarantineCapsule(capsuleDir, gitDir string) error {
	ts := strconv.FormatInt(time.Now().UnixNano(), 36)
	// The external git-dir is part of the same unit: it moves under the
	// same timestamp, as a sibling of the original, never deleted. The
	// two moves are ALL-OR-NOTHING (v2-converge §6): a failed second move
	// puts the first back, so the scene is never split between a
	// quarantined git-dir and a live worktree pointing at it.
	movedGit := ""
	if gitDir != "" && dirExists(gitDir) {
		movedGit = gitDir + ".quarantined." + ts
		if err := renameFn(gitDir, movedGit); err != nil {
			return err
		}
	}
	if !dirExists(capsuleDir) {
		return nil // nothing to move (a capsule never materialized)
	}
	if err := renameFn(capsuleDir, capsuleDir+".quarantined."+ts); err != nil {
		if movedGit != "" {
			if rerr := renameFn(movedGit, gitDir); rerr != nil {
				return fmt.Errorf("quarantine split the scene: moving the worktree failed (%w) and restoring the git-dir from %s failed (%v)", err, movedGit, rerr)
			}
		}
		return fmt.Errorf("quarantine of the capsule failed, git-dir restored: %w", err)
	}
	return nil
}

// renameFn is the filesystem move quarantineCapsule uses (a seam for the
// injected-failure test).
var renameFn = os.Rename

// materialize lays out the capsule: workflows/, capsule.json, session.json,
// .gitignore and the runtime-state/ tree. The bundle is materialized as ONE
// atomic set with a completion marker (F7, materialize.go; no digest
// verification — there is no digest chain, polyforge#149 B9). The claim's
// task-repo inventory is cloned into repos/<name> BEFORE the capsule
// identity is written, so the recorded baselines are the true claim-time
// SHAs.
func (r *Runtime) materialize(ctx context.Context, j *job) error {
	for _, d := range []string{
		capsule.DirArtifacts, capsule.DirLogs, capsule.DirRepos,
		capsule.DirRuntimeState, capsule.DirSession, capsule.DirWorkflows,
		filepath.Join(capsule.DirLogs, "session"),
	} {
		if err := os.MkdirAll(filepath.Join(j.capsuleDir, filepath.FromSlash(d)), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(j.capsuleDir, ".gitignore"),
		[]byte(capsule.GitignoreContent+gitignoreSyncLine), 0o644); err != nil {
		return err
	}
	// Task repos land before capsule.json so the identity below can
	// record their claim-time HEAD SHAs. The clones carry their own
	// independent .git directories and repos/ is capsule-gitignored,
	// so the capsule's history never absorbs them.
	repoSHAs, err := r.cloneTaskRepos(ctx, j)
	if err != nil {
		return err
	}
	// Session ownership lives in the daemon-private sync/session-handle
	// marker, not in the identity.
	idn := capsule.Identity{
		WorkItemID: j.wi,
		CapsuleID:  "cap_" + j.wi,
		Project:    j.project,
		BundleName: j.bundleName,
		Repos:      repoSHAs,
		CreatedAt:  nowStamp(),
	}
	// polyforge#79: the claim-time audit record — the parsed yaml
	// verbatim and the resolved repo sources. A no-yaml claim records
	// NOTHING (omitempty, never fabricated) and its legacy fields are
	// unchanged. (No per-step effective-candidate list is recorded: the
	// workspace yaml is the recovery re-derivation's own governing source.)
	blob, _ := json.MarshalIndent(idn, "", "  ")
	if err := atomicfile.Write(filepath.Join(j.capsuleDir, capsule.CapsuleJSON), blob, 0o644); err != nil {
		return err
	}
	// v1.5: the session ownership marker — this session's fingerprint,
	// under gitignored sync/ so a re-claim clone re-stamps its own.
	if err := writeSessionMarker(j.capsuleDir, j.sessionFingerprint); err != nil {
		return err
	}
	// SessionID carries the PUBLIC fingerprint (B3): session.json is a
	// capsule-committed file, so the secret must never ride it.
	// No harness here: it is per step (polyforge#159), named by each
	// dispatch's spawn-journal line.
	sess := capsule.Session{
		SessionID:      j.sessionFingerprint,
		TranscriptPath: j.transcript,
		UpdatedAt:      nowStamp(),
	}
	sblob, _ := json.MarshalIndent(sess, "", "  ")
	if err := atomicfile.Write(filepath.Join(j.capsuleDir, capsule.SessionJSON), sblob, 0o644); err != nil {
		return err
	}
	// Task-repo inventory and the F1 task-definition file land before
	// the bundle copy; the materialized work-item.json is the ONE
	// authoritative statement of the ask inside the capsule (v1 has no
	// agent→server read path, so without this file the agent cannot even
	// know its goal).
	if err := r.writeWorkItemFile(j); err != nil {
		return err
	}
	// F6-A: the claim-time memory snapshot — a static recall answer the
	// agent reads like any other capsule input. Best-effort by design:
	// an unreachable aihub or a zero-hit query degrades to an empty list
	// plus a notice, never a failed spawn (see recall.go).
	r.writeRecalledMemories(j)
	// F7: the bundle copy is ONE atomic set with a completion marker
	// (materialize.go): the bundle lands under workflows/<bundle>, and
	// sync/materialized.json gates every later consumer.
	return r.materializeBundle(j)
}

// writeWorkItemFile materializes runtime-state/work-item.json (F1): the
// claim answer's task definition — goal, content, labels, priority,
// milestone, bundle pin, project — written in full at Start. Idempotent
// in the recovery direction: a re-materialize of the same generation
// writes the same claim-time facts (goal semantics never change inside a
// generation), and ensureWorkItemFile below is the missing-file variant
// the recovery engine uses.
func (r *Runtime) writeWorkItemFile(j *job) error {
	return r.writeWorkItemDoc(j, j.item)
}

// ensureWorkItemFile repairs a capsule whose work-item.json is missing
// (a pre-F1 capsule, or a torn scene): the in-memory claim
// snapshot answers when present, aihub does otherwise. Best-effort — a
// repair failure is loud but never blocks recovery.
func (r *Runtime) ensureWorkItemFile(j *job) {
	path := filepath.Join(j.capsuleDir, filepath.FromSlash(capsule.WorkItemJSON))
	if _, err := os.Stat(path); err == nil {
		return
	}
	item := j.item
	if item == nil {
		fetched, err := r.hub.GetWorkItem(j.wi)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: work-item.json missing and aihub lookup failed: %v\n", j.wi, err)
			return
		}
		item = fetched
	}
	if err := r.writeWorkItemDoc(j, item); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: work-item.json repair failed: %v\n", j.wi, err)
	}
}

func (r *Runtime) writeWorkItemDoc(j *job, item *api.WorkItem) error {
	if item == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(j.capsuleDir, capsule.DirRuntimeState), 0o755); err != nil {
		return err
	}
	doc := &capsule.WorkItemDoc{
		ID:         j.wi,
		Slug:       item.Slug,
		Project:    j.project,
		Goal:       item.Goal,
		Content:    item.Content,
		Labels:     item.Labels,
		Priority:   item.Priority,
		Milestone:  item.Milestone,
		BundleName: j.bundleName,
		UpdatedAt:  nowStamp(),
	}
	if doc.Labels == nil {
		doc.Labels = []string{}
	}
	if doc.Priority == "" {
		doc.Priority = api.PriorityNormal
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(j.capsuleDir, filepath.FromSlash(capsule.WorkItemJSON)), blob, 0o644)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, ierr := os.ReadFile(p)
		if ierr != nil {
			return ierr
		}
		return atomicfile.Write(target, data, 0o444)
	})
}

// ---------------------------------------------------------------------------
// Stop (§一): seal → final push → SessionEnd → RTFinished.

// Stop seals wi with a terminal reason (protocol.FinishWrapped | -Failed)
// — or performs the operator pause close when reason is
// protocol.FinishPaused (Finding 2: the record is RETAINED in StatePaused,
// no read-only seal, observe released; see pauseOp). The agent tree is
// torn down, the capsule's final state is committed and pushed, and the
// session is ended server-side (SessionEnd, fenced on our secret). For
// a terminal reason the scene is made read-only and Observe hears
// WIFinished; a SessionEnd failure leaves the job in SEALING (persisted
// with its reason) and the close is retried in the background
// (retrySeal) — and by a restart's finisher if the daemon stops first —
// never a silent half-seal.
//
// A job still CLAIMING (B-8: its capsule clone/materialize runs outside
// opMu) has no agent and no scene to seal: the close cancels the clone
// and waits for the claim lane to settle it (closeClaim) — opMu is not
// held while waiting.
func (r *Runtime) Stop(ctx context.Context, wi, reason string) error {
	id := r.Resolve(wi)
	if id == "" {
		id = wi
	}
	s, ok := r.tryLockWI(id)
	if !ok {
		return errBusy(id)
	}
	locked := true
	defer func() {
		if locked {
			r.unlockWI(id, s)
		}
	}()
	r.opMu.Lock()
	j := r.resolveJob(wi)
	if j == nil {
		starting := r.claimStarting(wi)
		r.opMu.Unlock()
		if starting {
			// a start of it is on its claim's hub calls (claimLocked):
			// there is no job to close yet — the operator retries once
			// the claim sits CLAIMING (or settled)
			return &api.Error{Code: api.ErrDaemonConflict,
				Detail: fmt.Sprintf("%s: a start of this work item is claiming (its hub calls are under way); retry once it settles", wi)}
		}
		return fmt.Errorf("runtime: %s: no active job", wi)
	}
	if j.isBusy() {
		r.opMu.Unlock()
		return errBusy(j.wi)
	}
	if done := j.requestClaimClose(reason); done != nil {
		r.opMu.Unlock()
		r.unlockWI(id, s) // the claim lane settles the close under the lock
		locked = false
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("runtime: %s: close requested during the claim; still settling: %w", j.wi, ctx.Err())
		}
		j.mu.Lock()
		err := j.claim.closeErr
		j.mu.Unlock()
		return err
	}
	defer r.opMu.Unlock()
	return r.stopOp(ctx, j, reason, false, "")
}

// requestClaimClose records a close on a CLAIMING job and cancels its
// clone/materialize; it answers the channel that closes once the claim
// lane settled, or nil when the job is not CLAIMING. The first request
// wins. Caller holds opMu (so the state cannot leave CLAIMING meanwhile).
func (j *job) requestClaimClose(reason string) <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != StateClaiming || j.claim == nil {
		return nil
	}
	if j.claim.closeReq == "" {
		j.claim.closeReq = reason
	}
	j.claim.cancel()
	return j.claim.done
}

// closeClaim ends a CLAIMING job's session on the operator's request
// (opMu held): the agent never started and nothing was produced, so
// there is no seal push — the session is ended with the requested
// verdict and the placeholder dropped. A pause has no retained record to
// attach to (no capsule was materialized for it), so it closes BLOCKED:
// the session is cleared and `pf resume` re-claims the work item. A
// failed close is never left dangling: the durable blocked-close retry
// takes it (the claiming-journal entry stays until the hub accepts).
func (r *Runtime) closeClaim(wi string, j *job, reason string) error {
	r.dropJob(j)
	status := reason
	note := "closed by the operator while the claim was materializing (the agent never started)"
	if reason == protocol.FinishPaused {
		status = api.StatusBlocked
		note = "paused by the operator while the claim was materializing (the agent never started); pf resume re-claims it"
		if r.cfg.EventTap != nil {
			r.cfg.EventTap(j.wi, EvClaimPausedEndedBlocked, map[string]any{
				"session_fingerprint": j.sessionFingerprint,
				"note":                note,
			})
		}
	}
	// The (partly) materialized scene never ran an agent and belongs to a
	// session that ends here: move it aside now rather than leave it for
	// the next claim's prepareCapsule to find.
	if err := quarantineCapsule(j.capsuleDir, j.git.GitDir); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: quarantine of the closed claim's capsule failed (scene left in place): %v\n", j.wi, err)
	}
	if err := r.claimEndOffLock(j.wi, wi, api.SessionEndRequest{
		SessionID: j.sessionID,
		Status:    status,
		Note:      note,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: session-end(%s) of the claim failed (claiming entry kept; retrying a blocked close): %v\n", j.wi, status, err)
		r.retryClaimEnd(wi, j.wi, j.sessionID, note)
		return fmt.Errorf("runtime: %s: the claim was stopped but its session-end failed (retrying a blocked close in the background): %w", j.wi, err)
	}
	r.clearClaimingSession(wi, j.sessionID)
	if status != api.StatusBlocked {
		r.clearManualModels(j.wi)
	}
	return nil
}

// isDown reports whether the shutdown latch is down.
func (r *Runtime) isDown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.down
}

// admit takes an agent-start token: false once the shutdown latched (the
// caller starts no agent). A taken token is given back with release once
// the started agent's handle is bound to its job (or the start failed).
func (r *Runtime) admit() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		return false
	}
	r.installs++
	return true
}

// release gives an agent-start token back.
func (r *Runtime) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.installs--; r.installs == 0 && r.installsIdle != nil {
		close(r.installsIdle)
		r.installsIdle = nil
	}
}

// claimDeferredError is the typed answer of a start the shutdown stopped
// (DAEMON-410): nothing turned blocked; a claim that landed stays in the
// claiming journal and the next daemon start replays it.
// It unwraps to that *api.Error, so the faces answer it as such.
func claimDeferredError(wi string) error {
	return &claimDeferred{&api.Error{Code: api.ErrDaemonGone,
		Detail: fmt.Sprintf("%s: the daemon is shutting down; the start is deferred (a landed claim stays in the claiming journal and resumes at the next start)", wi)}}
}

type claimDeferred struct{ ae *api.Error }

func (e *claimDeferred) Error() string { return e.ae.Error() }
func (e *claimDeferred) Unwrap() error { return e.ae }

// ErrClaimClosed is the Start answer of a claim an operator close (Stop
// during CLAIMING) settled: the start is over and must not be retried.
var ErrClaimClosed = errors.New("runtime: the claim was closed by the operator before its agent started")

// IsClaimDeferred reports whether a Start error is the shutdown deferral.
func IsClaimDeferred(err error) bool {
	var cd *claimDeferred
	return errors.As(err, &cd)
}

// BeginShutdown is the shutdown's first runtime step: it latches down,
// so no new Start claims and no agent starts any more, cancels every
// CLAIMING job's clone/materialize and waits — bounded by ctx — for those
// claims to settle and for the agent starts already under way to bind
// their handles. A cancelled claim is deferred, never blocked: its
// journal entry stays for the next daemon start. Nothing else is waited
// for: a Start still on its pre-claim steps defers itself at the latch, a
// close or recovery on a hung push or hub call starts no agent once it
// returns, and the restart reconciliation decides nothing after the
// latch. Once it returned the live job set only shrinks. The restart's
// journal-replay goroutines are waited for within the same bound.
func (r *Runtime) BeginShutdown(ctx context.Context) {
	r.mu.Lock()
	r.down = true
	var lanes []*claimLane
	for _, j := range r.jobs {
		j.mu.Lock()
		if j.state == StateClaiming && j.claim != nil {
			lanes = append(lanes, j.claim)
		}
		j.mu.Unlock()
	}
	var idle chan struct{}
	if r.installs > 0 {
		if r.installsIdle == nil {
			r.installsIdle = make(chan struct{})
		}
		idle = r.installsIdle
	}
	r.mu.Unlock()
	for _, cl := range lanes {
		cl.cancel()
	}
	for i, cl := range lanes {
		select {
		case <-cl.done:
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "runtime: shutdown: %d claim(s) still settling at the wait's end\n", len(lanes)-i)
			return
		}
	}
	if idle != nil {
		select {
		case <-idle:
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "runtime: shutdown: agent start(s) still binding at the wait's end\n")
			return
		}
	}
	// the restart's journal-replay goroutines (runReplays) end with their
	// claims: wait for them within the same bound so none outlives the
	// shutdown
	replaysDone := make(chan struct{})
	go func() { r.replays.Wait(); close(replaysDone) }()
	select {
	case <-replaysDone:
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "runtime: shutdown: journal replays still settling at the wait's end\n")
		return
	}
}

// supersedeOp is the OBSuperseded response (v1.5 M4 + m3): the periodic
// session revalidation learned this WI is held by another daemon. The
// other owner now owns the work, so this daemon makes NO hub transition
// (fencing-on-use rejects our writes; a SessionEnd would be refused as
// STALE and a blocked verdict would sabotage the new owner), runs NO
// recovery, and instead: kills the local tree, quarantines the capsule
// in place (capsules/<name>.quarantined.<ts>), drops the state.json record
// and emits the owner-visible event — the local capsule (and any answers
// recorded in it) did not travel, so "answers may be lost; question
// re-opened" (m3).
func (r *Runtime) supersedeOp(ctx context.Context, j *job, detail string) {
	s, ok := r.tryLockWI(j.wi)
	if !ok {
		// another operation of the work item is under way: never
		// quarantine the scene under it; a job it leaves behind keeps its
		// monitor, which re-emits the verdict
		return
	}
	defer r.unlockWI(j.wi, s)
	r.opMu.Lock()
	defer r.opMu.Unlock()
	// The job may have settled (Stop finished, bounded fail) between
	// handleOB's fetch and this lock: a gone job needs nothing.
	if r.jobOf(j.wi) != j {
		return
	}
	if j.isBusy() {
		// a close or recovery step is in flight outside opMu (B-8): never
		// quarantine the scene under its push. That step meets the dead
		// session itself (a refused push or session end); a job it leaves
		// behind keeps its monitor, which re-emits the verdict.
		return
	}
	r.killAgentTree(j)
	if err := quarantineCapsule(j.capsuleDir, j.git.GitDir); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: superseded capsule quarantine failed (scene left in place): %v\n", j.wi, err)
	}
	r.dropJob(j)
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	if r.cfg.EventTap != nil {
		r.cfg.EventTap(j.wi, "session_superseded", map[string]any{
			"session_fingerprint": j.sessionFingerprint,
			"note":                "answers may be lost; question re-opened",
			"detail":              detail,
		})
	}
	// Finding 4 (v1.5 review, MINOR): the observe release. Without it the
	// superseded monitor loop never stops (a 500ms goroutine leaked per
	// supersession), the latestSpawn entry is never pruned and the answer
	// ledger never forgets the work item. RTFinished(superseded) is the
	// same release message stopOp sends — observe stops the monitor,
	// prunes the fence map and drops the ledger entries. It also closes
	// Finding 1(b)'s re-emit loop: the superseded monitor keeps
	// re-emitting OBSuperseded until exactly this release arrives.
	// re-derive or re-send these two messages.
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: j.wi, Reason: protocol.FinishSuperseded})
	_ = ctx
}

// stopOp runs one close to the end. reason is the close verdict (a
// terminal wrapped|failed|blocked, or paused — the operator pause); note
// is the human-facing elaboration the SessionEnd carries ("" for the
// ordinary closes — the interrupted-seal finisher names its
// cause-unknown close here).
func (r *Runtime) stopOp(ctx context.Context, j *job, reason string, alreadySealing bool, note string) error {
	// Finding 2 (v1.5 review, MAJOR): the operator pause is its own close.
	// It does NOT pass through SEALING (no terminal verdict follows), it
	// retains the record (the secret must survive locally for the M3
	// attach) and it releases observe with RTFinished(paused). Only the
	// CLI pause route produces the paused reason, and only for a RUNNING
	// job — a repeat pause is answered by the daemon's already-paused
	// graceful path before it ever gets here.
	if reason == protocol.FinishPaused && !alreadySealing {
		return r.pauseOp(ctx, j)
	}
	// alreadyPushed: a resumed close (seal retry, restart finisher,
	// pending-close replay) whose seal push already reached the hub. A
	// fresh close always pushes.
	alreadyPushed := false
	if alreadySealing {
		j.mu.Lock()
		alreadyPushed = j.sealedPush
		j.mu.Unlock()
	}
	if !alreadySealing {
		// Closing a RETAINED paused record (wrap/fail after an operator
		// pause, Finding 2): no SEALING hop — the record transitions
		// Paused → Done directly at the end. The agent is already gone
		// (the pause close killed it); everything else is the ordinary
		// terminal close.
		j.mu.Lock()
		fromPaused := j.state == StatePaused
		j.mu.Unlock()
		if fromPaused {
			j.mu.Lock()
			j.stopReason = reason
			j.sealedPush = false
			j.mu.Unlock()
			r.persist()
		} else {
			if err := j.transition(StateSealing); err != nil {
				return err
			}
			j.mu.Lock()
			j.stopReason = reason
			j.sealedPush = false
			j.mu.Unlock()
			r.persist()
		}
	}
	// Six-layer teardown: no writer survives into the sealed scene.
	r.killAgentTree(j)

	// The network half — final push, task-repo WIP, SessionEnd — runs
	// outside opMu with the job latched busy (offLock, B-8): the state
	// already says SEALING (or the retained record carries the terminal
	// stop reason), so every other door refuses this job meanwhile.
	finalReason := reason
	var endErr error
	if !r.offLock(j, func() {
		finalReason, endErr = r.sealNetwork(j, reason, note, alreadyPushed)
	}) {
		return fmt.Errorf("runtime: %s: the job left the job table during its close", j.wi)
	}
	if endErr != nil {
		if st, _ := j.snapshot(); st == StateSealing {
			// Persist the push outcome and retry the close in the
			// background: without the retry the job would sit
			// SEALING — agent dead, every `pf wrap` answering "a close is
			// already in progress" — until a daemon restart ran the
			// finisher.
			if perr := r.persist(); perr != nil {
				fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, perr)
			}
			r.retrySeal(j, note)
		}
		return fmt.Errorf("runtime: %s: session-end failed (job left sealing, retrying): %w", j.wi, endErr)
	}
	if err := sealReadOnly(j.capsuleDir); err != nil {
		// The generation is closed; a chmod failure is a scene-preservation
		// wart, not a seal failure. Loud, non-fatal.
		fmt.Fprintf(os.Stderr, "runtime: %s: read-only seal incomplete: %v\n", j.wi, err)
	}
	if err := j.transition(StateDone); err != nil {
		return err
	}
	r.dropJob(j)
	if finalReason != protocol.FinishBlocked {
		// terminal on this daemon: the manual --models list dies with it
		// (a blocked end keeps it for the re-claim).
		r.clearManualModels(j.wi)
	}
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: j.wi, Reason: finalReason})
	return nil
}

// sealNetwork is stopOp's network half, run outside opMu (offLock): the
// final push — UNLESS this close's seal push already reached the hub
// (alreadyPushed: a seal retry or the restart finisher after a
// successful push; a second push could only fail against a revoked
// credential) — the task-repo WIP lane and the fenced SessionEnd. It
// answers the close's final reason (a failed push degrades it to
// blocked) and the SessionEnd error.
func (r *Runtime) sealNetwork(j *job, reason, note string, alreadyPushed bool) (string, error) {
	finalReason := reason
	chainOK := true
	if !alreadyPushed {
		// The seq trailer is gitops' business (derived in-lock).
		// CommitPush is honest on every path — including a clean tree,
		// where it pushes the standing HEAD — so a failed first push is
		// retried directly via gitops.Push below. A persistently failed
		// push degrades the terminal reason to blocked — the local commit
		// keeps the bytes, but the server never heard the final state.
		r.syncLiveTranscript(j.capsuleDir)
		_, _, _, perr := gitops.CommitPush(j.git, "seal: "+reason)
		for i := 0; i < 2 && perr != nil; i++ {
			_, perr = gitops.Push(j.git)
		}
		if perr != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: final push failed after retries: %v\n", j.wi, perr)
			// Degrade to blocked (kept semantics: the local commit keeps the
			// bytes; the server never heard the final state) — but leave a
			// durable, field-diagnosable trace in the daemon events log too:
			// the pause/wrap close path's seal failure is otherwise visible
			// only in this process's stderr.
			// One event per sealing series: a seal retry (retrySeal)
			// re-runs this push and must not repeat it.
			j.mu.Lock()
			noted := j.sealPushFailNoted
			j.sealPushFailNoted = true
			j.mu.Unlock()
			if r.cfg.EventTap != nil && !noted {
				r.cfg.EventTap(j.wi, "push_failed", map[string]any{
					"phase": "seal", "reason": reason, "error": perr.Error(),
				})
			}
			finalReason = protocol.FinishBlocked
			chainOK = false
		} else {
			// The final state reached the server: a seal retry (below)
			// or the restart finisher must not push again — after a
			// STALE_SESSION the credential is dead and the push could
			// only fail.
			j.mu.Lock()
			j.sealedPush = true
			j.mu.Unlock()
		}
	}
	// Task-repo uncommitted work (polyforge#167): every close but wrapped
	// snapshots it to refs/pf-wip so a later re-claim restores it; the
	// wrapped close only drops stale WIP refs. Before SessionEnd (the
	// session credential dies with it); the tree is already killed.
	r.saveTaskWIP(j, finalReason != protocol.FinishWrapped, chainOK)
	// v1.5: the terminal close is SessionEnd — fenced on the session
	// hash. A STALE_SESSION refusal (this daemon was superseded between
	// the seal push and here) surfaces as the error and leaves the job
	// SEALING, exactly like a hub outage: never a silent half-seal.
	// The seal retry then finishes it locally (retrySeal).
	// The status vocabulary is wrapped|failed|blocked — plus "paused",
	// which the CLI pause path passes through (merge amend: paused routes
	// to the M3 pause transition; the session is KEPT and clean_pause=true
	// rides the same atomic fenced update — the ONLY pause producer is
	// the operator CLI, so it is always a clean pause).
	return finalReason, r.hub.SessionEnd(j.wi, api.SessionEndRequest{
		SessionID:  j.sessionID,
		Status:     finalReason,
		Note:       note,
		CleanPause: finalReason == api.StatusPaused,
	})
}

// pauseOp is the operator pause close (v1.5 review Finding 2, MAJOR):
// the M3 pause transition WITH record retention. A pause that
// sealed the capsule read-only, transitioned DONE and dropped the record
// would destroy the session secret locally — nobody could ever attach
// again, `pf resume` would print a promise no restart could fulfill, and
// a plain claim would answer ALREADY_HELD. The contract:
//
//	RUNNING → PAUSED, persisted FIRST (a crash anywhere after the
//	transition leaves a Paused record the restart reconciles);
//	six-layer kill of the tree; commit+push the dirty capsule state
//	(the same seal-commit semantics as stopOp — the bytes must survive
//	the pause, and a failed push is LOG-ONLY here: the verdict is not
//	terminal, so it never degrades to blocked — the next attach's
//	commit-dirty/observe pushes carry the state);
//	SessionEnd(paused, CleanPause=true) — the hub keeps the session
//	row and marks the operator pause (the only pause producer is the
//	operator CLI, so it is always a clean pause);
//	RTFinished(paused) — the observe release (Finding 4): the monitor
//	stops, latestSpawn is pruned, the ledger forgets the work item;
//	NO read-only seal, NO dropJob, NO DONE — the record and its secret
//	stay so the holder daemon's restart can attach once the operator
//	clears clean_pause (M3).
//
// A SessionEnd failure leaves the record PAUSED against a hub that
// still says running: the restart reconciliation self-heals exactly
// that shape (verify matches + running + a local Paused record →
// resume through the recovery engine), so the error is loud but never
// a dangling half-close.
func (r *Runtime) pauseOp(ctx context.Context, j *job) error {
	if err := j.transition(StatePaused); err != nil {
		return err
	}
	j.mu.Lock()
	j.stopReason = protocol.FinishPaused
	j.sealedPush = false
	j.cleanPause = true
	j.mu.Unlock()
	r.persist()
	// Six-layer teardown: no writer survives into the retained scene.
	r.killAgentTree(j)
	// The retained record carries no live agent: clear the pid so the
	// status face (and the restart's kill pass) read the paused job as
	// agentless — the tree is collected, and the later attach derives a
	// fresh pid of its own.
	j.mu.Lock()
	j.agentPID = 0
	j.agentStartedAt = ""
	j.mu.Unlock()
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	// The network half runs outside opMu with the job latched busy
	// (offLock, B-8): the record already says PAUSED.
	var endErr error
	if !r.offLock(j, func() { endErr = r.pauseNetwork(j) }) {
		return fmt.Errorf("runtime: %s: the job left the job table during its pause close", j.wi)
	}
	if endErr != nil {
		return fmt.Errorf("runtime: %s: session-end(paused) failed (record left paused; the next restart reconciles): %w", j.wi, endErr)
	}
	// The observe release (Finding 4): stops the monitor loop, prunes the
	// latestSpawn entry, forgets the ledger. The retained record keeps
	// its bookkeeping only.
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: j.wi, Reason: protocol.FinishPaused})
	_ = ctx
	return nil
}

// pauseNetwork is pauseOp's network half, run outside opMu (offLock):
// commit+push the dirty capsule (a failed push is log-only — see
// pauseOp), the task-repo WIP lane and SessionEnd(paused). It answers
// the SessionEnd error.
func (r *Runtime) pauseNetwork(j *job) error {
	r.syncLiveTranscript(j.capsuleDir)
	_, _, _, perr := gitops.CommitPush(j.git, "seal: "+protocol.FinishPaused)
	for i := 0; i < 2 && perr != nil; i++ {
		_, perr = gitops.Push(j.git)
	}
	if perr != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: pause seal push failed after retries (local commit stands): %v\n", j.wi, perr)
		// Log-only (never a blocked degrade — see the doc comment). The
		// durable trace rides the same push_failed event face stopOp uses.
		if r.cfg.EventTap != nil {
			r.cfg.EventTap(j.wi, "push_failed", map[string]any{
				"phase": "pause-seal", "reason": protocol.FinishPaused, "error": perr.Error(),
			})
		}
	}
	// The task repos' uncommitted work rides refs/pf-wip
	// (polyforge#167): an attach resumes in place, but a later
	// re-claim restores from the carrier.
	r.saveTaskWIP(j, true, perr == nil)
	return r.hub.SessionEnd(j.wi, api.SessionEndRequest{
		SessionID: j.sessionID,
		Status:    api.StatusPaused,
		// always true: the operator CLI is the only pause producer (M3)
		CleanPause: true,
	})
}

// sealReadOnly makes the preserved scene read-only (files 0444, dirs 0555;
// symlinks untouched — chmod never follows a link out of the scene).
// Best-effort per entry: the walk reports the first hard error.
func sealReadOnly(root string) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return os.Chmod(p, 0o555)
		}
		if info.Mode().IsRegular() {
			return os.Chmod(p, 0o444)
		}
		return nil
	})
}

// DriverAnswerHandle returns the bridge Handle for a driver-driven WI
// (nil when the job's agent is not a bridge handle — the WI-Agent path
// answers through the observe lane instead).
func (r *Runtime) DriverAnswerHandle(wi string) interface {
	Answer(questionID, answer, answeredBy string) error
} {
	id := r.Resolve(wi)
	if id == "" {
		id = wi
	}
	j := r.jobOf(id)
	if j == nil {
		return nil
	}
	j.mu.Lock()
	h := j.agent
	j.mu.Unlock()
	if bh, ok := h.(interface {
		Answer(questionID, answer, answeredBy string) error
	}); ok {
		return bh
	}
	return nil
}

// CanonicalID resolves any WI spelling (id or slug) to the canonical ID.
func (r *Runtime) CanonicalID(wi string) string {
	if id := r.Resolve(wi); id != "" {
		return id
	}
	return wi
}
