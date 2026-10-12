// Package runtime implements the v1 Runtime lane: the WI lifecycle manager
// (Start / Stop / Recover / Restart), the pi-harness agent spawner with the
// real RPC envelope, the six-layer process cleanup and the recovery engine.
//
// Design authority: docs/design/polyforge-vnext/v1-architecture-spec.md
// (rulings C1–C3) + v1-spec-full.md (§二 messages, §三 state machine, §六
// git, §七 recovery engine, §八 cleanup, §十一 restart reconciliation).
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
	// engine choice: dispatch refuses it loudly (the legacy in-package
	// WI-Agent spawn this field used to fall back to is deleted).
	AgentStarter AgentStartFunc
	MachineID    string // registered machine identity
	// Mapping is the workspace→projects plan of record (polyforge#113):
	// the daemon assembles it from the shared cli.toml ([workspace.
	// "<path>"] sections) and the runtime routes every capsule through it
	// — <mapped-workspace>/capsules/<wi> per the claim answer's project.
	// There is no single workspace and no default project anymore. The
	// daemon refuses to start when the mapping is empty; a recorded
	// mid-flight project that falls out of it parks only that work item
	// repair_pending (restart.go, v2-converge §6).
	Mapping       *mapping.Plan
	DataDir       string // state.json + external git-dirs live here
	GitRemoteBase string // e.g. https://aihub.example/git → <base>/<wi>.git
	WorkflowsDir  string // daemon checkout's workflows/ (materialization source)
	Harness       string // harness adapter name ("pi")
	PiBin         string // harness binary (default "pi")
	CgroupRoot    string // cgroup v2 root; empty ⇒ layer-0 unavailable
	MaxRecoveries int    // consecutive FAILED recoveries before bounded fail (default 2)
	// RecoveryStabilityWindow bounds the v2 entry-rate storm guard: a
	// recoverCore that re-fires inside this window (default 60s) counts
	// one storm unit; MaxRecoveries rapid re-entries bounded-fail the WI
	// (a relaunch dying faster than it can stabilize never resets the
	// budget — the #97 death loop). A re-entry after a stable gap resets
	// the storm ledger: an isolated later failure is a fresh episode.
	RecoveryStabilityWindow time.Duration
	// EndRetryBase/EndRetryMax shape the backoff of the durable
	// SessionEnd(blocked) retry (end_pending records and the re-claim
	// abandon lane): first retry after Base, doubling up to Max
	// (defaults 2s / 60s). Retries run until the hub accepts the close.
	EndRetryBase time.Duration
	EndRetryMax  time.Duration
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
	// seam: materialize calls it with the claim's goal+content and writes
	// the answer as a static snapshot for the agent. The daemon binds it
	// to aihub's memory search face (the machine credential stays with the
	// daemon — agents hold no aihub credential of any kind); nil disables
	// the snapshot entirely (slim installs, most unit tests).
	MemoryRecall func(query string, topK int) ([]RecalledMemory, error)
}

func (c *Config) fill() {
	// The harness adapter defaults to the only one that exists today
	// (the pi spawner).
	if c.Harness == "" {
		c.Harness = "pi"
	}
	if c.MaxRecoveries <= 0 {
		c.MaxRecoveries = 2
	}
	if c.RecoveryStabilityWindow <= 0 {
		c.RecoveryStabilityWindow = 60 * time.Second
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
	// aliases maps every NON-canonical id form the runtime has ever been
	// asked to run (a "<project>#<seq>" slug, most often) to the
	// canonical work-item id the claim resolved it to. In-memory only:
	// Resolve's unknown-input answer is "", and the daemon's resolver
	// falls back to aihub for those (a restart re-learns on demand).
	aliases map[string]string
	// origins: submit origins noted by the daemon, consumed at job creation.
	origins map[string]Origin

	// opMu serializes the mutating operations (Start/Stop/recover/Restart):
	// the message loop and the daemon's direct calls share it, so the state
	// machine never sees two drivers at once.
	opMu      sync.Mutex
	statePath string

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
	WI            string
	Project       string
	CapsuleDir    string
	BundleName    string
	BundleDigest  string
	SessionHandle string // public fingerprint
	SpawnN        int64
	// StepCandidates is the polyforge#79 effective per-step candidate
	// map this derivation dispatches under (nil = no yaml layer — the
	// bundle's own resolution). The bridge forwards it to the driver.
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
	// sessionID is the v1.5 SECRET (B3): 64-hex minted at claim, the git
	// push password. It never reaches the capsule, agent env, argv or
	// any log/event surface — only state.json and the one-shot askpass.
	sessionID string
	// sessionFingerprint is the PUBLIC handle (api.SessionFingerprint).
	sessionFingerprint string
	state              JobState
	stopReason         string // sealed-in reason while SEALING (restart can finish the stop)
	// sealedPush records that the final capsule state was ALREADY
	// committed and pushed by the driver's own publish (the terminal
	// close of recover.go): stopOp then skips its own seal push. It is
	// set when the SEALING transition records the reason and persisted
	// with it, so a crash between that push and SessionEnd keeps the
	// restart's stop finisher from re-pushing either.
	sealedPush   bool
	capsuleDir   string
	git          gitops.Opts // Username=wi, Secret=session_id (v1.5 §“gitops call sites”)
	transcript   string
	bundleName   string
	bundleDigest string
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

	// workspaceRoot/workspaceYaml are the polyforge#79 claim-time read:
	// the mapped workspace's root and its strictly-validated
	// .polyforge.yaml (nil when the workspace carries none). Read FRESH
	// on every claim — an edit governs the next claim with no daemon
	// restart. In-memory only: the durable snapshot is the capsule.json
	// audit materialize writes (the recovery path rehydrates from
	// there).
	workspaceRoot string
	workspaceYaml *workspace.Config
	// stepCandidates is the polyforge#79 claim-time compose: the
	// per-step post-precedence candidate lists (step id → list; a
	// present-empty entry governs and means no switchable chain). In-
	// memory only; the durable snapshot is the capsule.json audit.
	stepCandidates map[string][]string
	// repoSources is the polyforge#79 local-source record: repo name →
	// the source actually cloned from (the resolved local checkout for
	// yaml-sourced clones). In-memory only; the durable copy is the
	// capsule.json audit.
	repoSources map[string]string

	agent          AgentHandle
	agentPID       int
	agentStartedAt string

	spawnN       int
	recoverFails int // consecutive FAILED recoveries; reset on success
	// stormFails/lastRecoveryAt feed the v2 entry-rate storm guard: a
	// recoverCore re-entry inside RecoveryStabilityWindow counts one
	// storm unit (a fast-dying relaunch never proves stability); a
	// re-entry after the gap resets the ledger. Separate from
	// recoverFails so the ladder's own reset-on-success semantics stay
	// untouched.
	stormFails     int
	lastRecoveryAt time.Time
	// cleanPause mirrors the hub's operator-pause flag: latched true by
	// the pause close (Finding 2) so the retained Paused record carries
	// the same marker the hub does; false everywhere else (a graceful
	// shutdown keeps it false — the attach-on-restart shape, M3).
	cleanPause bool
	// repairPending is the v2 §6 local "pending repair" mark: the record's
	// capsule is missing / its project is unmapped at restart. Non-empty =
	// the reason kind (RepairCapsuleMissing | RepairProjectUnmapped). The
	// job is parked — record and SESSION kept, no agent, no hub call —
	// until the next start or `pf resume` re-checks it. Orthogonal to
	// state (the pre-park state is what the in-place resume continues).
	repairPending string
	// verifyPending marks a record whose restart SessionVerify could not
	// reach aihub: it keeps its place (no agent yet) while the daemon
	// retries the verify with backoff, then proceeds through restartOne.
	// In-memory only — a daemon restart re-verifies everything anyway.
	verifyPending bool
	// endNote/endHead: the pending SessionEnd(blocked) verdict of an
	// end_pending job (see StateEndPending).
	endNote string
	endHead string
	// endStatus is the verdict of the pending SessionEnd; "" = blocked
	// (the bounded-fail close). "canceled" / "failed" ride an abandoned
	// repair_pending WI (AbandonRepair).
	endStatus string
	mu        sync.Mutex
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

// DefaultSpawner returns the production pi-harness spawner. It takes the
// runtime config BY POINTER: the spawner reads its fields at spawn time
// from the shared config, so any field bound after assembly is seen by
// the spawner.
func DefaultSpawner(cfg *Config) Spawner {
	return newPiSpawner(cfg)
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
	// Finding 1(a) (v1.5 review, MAJOR): OBSuperseded is a SESSION-level
	// verdict, not a derivation-level one — it is EXEMPT from the
	// spawn-equality fence. The exact loss the fence used to cause: a
	// verify in flight while a recovery bumps spawn N→N+1 emitted
	// OBSuperseded(N), the fence dropped it at the counter mismatch, and
	// the freshly-derived agent of a WI held elsewhere ran unsupervised
	// forever (detection/push/revalidation all suppressed, the
	// monitor latched). Every other OB kind stays fenced (B2).
	if msg.Kind != protocol.OBSuperseded && msg.SpawnN != spawn {
		return // stale handle: dropped (recovery-loop guard, §二)
	}
	// NEW-1 (delta review): OBSuperseded bypasses the spawn fence but is
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

func (r *Runtime) jobOf(wi string) *job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[wi]
}

// persist rewrites state.json from the live job set.
func (r *Runtime) persist() error {
	r.mu.Lock()
	recs := make([]*jobRecord, 0, len(r.jobs))
	for _, j := range r.jobs {
		recs = append(recs, j.record())
	}
	r.mu.Unlock()
	blob, err := json.MarshalIndent(&stateFile{Version: 1, MachineID: r.cfg.MachineID, Jobs: recs}, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(r.statePath, blob, stateFilePerm)
}

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
// The daemon calls it right before Start; startClaimed consumes it.
func (r *Runtime) NoteOrigin(wi string, o Origin) {
	r.mu.Lock()
	if r.origins == nil {
		r.origins = map[string]Origin{}
	}
	r.origins[wi] = o
	r.mu.Unlock()
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

// cgroupPathFor returns the per-WI cgroup v2 directory (layer 0); empty
// config root means cgroups are off and layer 0 degrades away.
func (r *Runtime) cgroupPathFor(wi string) string {
	if r.cfg.CgroupRoot == "" {
		return ""
	}
	return filepath.Join(r.cfg.CgroupRoot, "polyforge-"+wi)
}

// capsuleDirForProject resolves the capsule base for one project through
// the inverse mapping (polyforge#113): <workspace>/capsules/<wi>. An
// unmapped project answers the typed *mapping.UnmappedError — callers on
// the claim path re-surface it as the typed DAEMON-412 refusal BEFORE any
// capsule or claim state exists.
func (r *Runtime) capsuleDirForProject(project, wi string) (string, error) {
	ws, err := r.cfg.Mapping.WorkspaceFor(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(ws, "capsules", wi), nil
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

// mintSessionID mints the v1.5 SECRET (contract §0): 32 crypto-random
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
func (r *Runtime) Start(ctx context.Context, wi string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if j := r.jobOf(wi); j != nil {
		if st, _ := j.snapshot(); st != StatePaused {
			return fmt.Errorf("runtime: %s already active (state %s)", wi, st)
		}
		// Finding 2 (v1.5 review): a RETAINED paused record is not an
		// active run — the start may proceed over it (startClaimed drops
		// the dead record once the hub accepted the new claim; a
		// plain claim of the paused-held WI is refused by the hub's own
		// ALREADY_HELD gate).
	}
	// polyforge#113 placement pre-check: resolve the work item (read-only
	// GetWorkItem) BEFORE anything is journaled or claimed, and refuse an
	// unmapped project with the typed DAEMON-412 — before any capsule
	// exists and before the aihub-side claim state changes. The mapping
	// governs placement, not knowledge: the hub read is a projection, not
	// a mutation. A FAILED read refuses nothing here — the claim call is
	// the authority for WI existence, and startClaimed re-checks the
	// claim answer's project (the D5 compensation covers the race).
	if probe, perr := r.hub.GetWorkItem(wi); perr == nil && probe != nil && probe.ProjectName != "" {
		if _, merr := r.cfg.Mapping.WorkspaceFor(probe.ProjectName); merr != nil {
			return &api.Error{
				Code: api.ErrDaemonUnmappedProject,
				Detail: fmt.Sprintf("project %q is not in the [workspace] mapping of this machine — the claim is refused before any capsule is created; add the project to a [workspace.\"<path>\"] section in the config (`pf doctor` checks the mapping)",
					probe.ProjectName),
			}
		}
	}
	sessionID, err := mintSessionID()
	if err != nil {
		return err
	}
	// B1: the claim's idempotency key AND the session secret are
	// journaled durably BEFORE the claim call. A crash anywhere between
	// the claim and the spawn would otherwise orphan a running session
	// nobody can name again — the journal lets Restart verify the
	// journaled session, replay the SAME claim (same key → same response,
	// D4) and either finish the start or compensate with
	// SessionEnd(blocked).
	key := randomKey()
	if err := r.noteClaiming(wi, sessionID, key); err != nil {
		return fmt.Errorf("runtime: %s: claiming journal write failed (refusing to claim): %w", wi, err)
	}
	req := api.SessionClaimRequest{
		WorkItemID:     wi,
		SessionID:      sessionID,
		MachineID:      r.cfg.MachineID,
		Owner:          r.cfg.MachineID,
		IdempotencyKey: key,
	}
	resp, err := r.hub.SessionClaim(req)
	if err != nil {
		// the claim never opened: nothing can dangle, drop the journal
		// entry so a restart does not replay a dead intention.
		r.clearClaiming(wi)
		return err
	}
	return r.startClaimed(ctx, wi, sessionID, resp)
}

// startClaimed carries one OPEN claim (fresh or replayed) to a live agent.
// Every failure after this point is compensated with SessionEnd(blocked)
// — the D5 rule carried into the session model: a post-claim failure must
// never leave an orphaned running session; the ticket needs human
// attention, and the hub's session row is cleared either way. The
// claiming journal entry is cleared on every settled outcome.
//
// sessionID is THIS daemon's minted secret (the journal replay path passes
// the journaled one back; the claim answer's fingerprint must agree).
//
// wi may arrive as the canonical id OR any id-or-slug form the aihub claim
// endpoint accepts ("<project>#<seq>"): the job and every path derived
// from it — capsule directory, external git-dir, push remote, spawn
// journal — are keyed by the CANONICAL work-item id the claim answer
// carries (F2). A slug contains "#", the URL fragment separator: a
// slug-built push remote answers 404 and a slug-named directory leaks the
// separator into the harness's session directory naming, so slugs never
// reach any path. The submitted spelling is registered as an alias so
// lookups by it still resolve.
func (r *Runtime) startClaimed(ctx context.Context, wi, sessionID string, resp *api.SessionClaimResponse) error {
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
			return fmt.Errorf("runtime: %s already active (state %s)", id, existing.state)
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
		return fmt.Errorf("runtime: %s: claim answer fingerprint %s does not match this session (%s)", id, c.SessionFingerprint, fp)
	}
	j := &job{
		wi:                 id,
		project:            resp.WorkItem.ProjectName,
		sessionID:          sessionID,
		sessionFingerprint: fp,
		state:              StateRunning,
		git: gitops.Opts{
			WorkItemID: id,
			// §1.6 fencing-on-use backup layer: the PUBLIC fingerprint.
			Fingerprint: fp,
			GitDir:      r.gitDirFor(id),
			RemoteURL:   r.remoteURLFor(id),
			Username:    id,
			Secret:      sessionID, // v1.5: the session secret IS the push password
			EventSink:   r.gitEventSink(id),
		},
		bundleName:   c.BundleName,
		bundleDigest: c.BundleDigest,
		repos:        c.Repos,
		item:         &resp.WorkItem,
		origin:       r.takeOrigin(wi, id),
	}
	// polyforge#113: the capsule routes by the claim answer's project
	// through the inverse mapping. An unmapped answer here (a mapping
	// edit between the pre-check and the claim answer, or a journal
	// replay over a changed config) compensates with the D5 blocked end —
	// no capsule directory is ever touched.
	capDir, rerr := r.capsuleDirForProject(j.project, id)
	if rerr != nil {
		err2 := &api.Error{
			Code: api.ErrDaemonUnmappedProject,
			Detail: fmt.Sprintf("project %q is not in the [workspace] mapping of this machine — the claim is refused before any capsule is created; add the project to a [workspace.\"<path>\"] section in the config (`pf doctor` checks the mapping)",
				j.project),
		}
		return r.abandonClaim(j, wi, err2, err2.Detail)
	}
	j.capsuleDir = capDir
	j.git.CapsuleDir = capDir
	j.transcript = sessionPathFor(j.capsuleDir, fp)

	// polyforge#79: the workspace yaml is read FRESH on every claim (the
	// read lives in the claim path, so an edit governs the next claim
	// with no daemon restart) and an invalid one refuses the claim BEFORE
	// any capsule state exists — the same abandonClaim lane as the
	// unmapped-project refusal above (DAEMON-414).
	if err := r.readWorkspaceYaml(j); err != nil {
		note := err.Error()
		var ae *api.Error
		if errors.As(err, &ae) && ae.Detail != "" {
			note = ae.Detail
		}
		return r.abandonClaim(j, wi, err, note)
	}

	if err := r.prepareCapsule(j); err != nil {
		return r.abandonClaim(j, wi, err, "prepare capsule: "+err.Error())
	}
	if err := r.verifySealedHead(j, c); err != nil {
		return r.abandonClaim(j, wi, err, err.Error())
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
		// the close note; every other materialization failure (CAP-405
		// digest divergence included) names itself.
		note := err.Error()
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.ErrDaemonSpawnFail && ae.Detail != "" {
			note = ae.Detail
		}
		return r.abandonClaim(j, wi, err, note)
	}
	// v2: the Go driver replaces the WI-Agent spawn. The daemon binds
	// bridge.Start as AgentStarter at assembly; the driver owns the step
	// state machine from here on. There is no second engine to fall back
	// to (polyforge#106): a nil starter is a misassembly and refuses the
	// claim loudly.
	starter := r.cfg.AgentStarter
	if starter == nil {
		err2 := fmt.Errorf("runtime misassembly: no execution engine bound (Config.AgentStarter is nil) — the legacy in-package WI-Agent engine was deleted (polyforge#106); the daemon binds the driver bridge starter unconditionally at assembly")
		return r.abandonClaim(j, wi, err2, err2.Error())
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
			WI:            j.wi,
			Project:       j.project,
			CapsuleDir:    j.capsuleDir,
			BundleName:    j.bundleName,
			BundleDigest:  j.bundleDigest,
			SessionHandle: j.sessionFingerprint,
			SpawnN:        spawnN,
			Spawner:       r.spawner,
			// polyforge#79: the claim-time compose rides to the driver —
			// every dispatch of this derivation resolves its model from
			// the post-precedence effective list.
			StepCandidates: j.stepCandidates,
		})
		if err2 != nil {
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
	}
	r.mu.Lock()
	r.jobs[id] = j
	r.mu.Unlock()
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

// readWorkspaceYaml is the polyforge#79 claim-time read (fresh, no
// cache): resolve the workspace root through the same inverse mapping
// placement used (the WorkspaceFor source capsuleDirForProject keys
// off), strictly validate <root>/.polyforge.yaml through the shared
// loader, and stash the parsed snapshot on the job (nil when the
// workspace carries no yaml — absent-equivalent, behavior unchanged).
// An invalid yaml returns the typed DAEMON-414 refusal; the caller
// compensates through abandonClaim and nothing materializes.
//
// When the yaml carries a present `projects` section, the claimed
// project MUST be listed (DAEMON-415 otherwise — no silent registry
// fallback), and the job's repo inventory is built WHOLLY from the yaml
// entries: name + source = the workspace root joined with the entry's
// workspace-root-relative `path`, which flows through the existing
// cloneTaskRepos/gitClone seam verbatim (the recovery contract and the
// DAEMON-507 lane hold unchanged for locally-sourced clones).
func (r *Runtime) readWorkspaceYaml(j *job) error {
	ws, err := r.cfg.Mapping.WorkspaceFor(j.project)
	if err != nil {
		// Unreachable on this path (the capsule-routing site just resolved
		// the same inverse), but never guess: a workspace that cannot be
		// resolved carries no yaml layer.
		return nil
	}
	j.workspaceRoot = ws
	wcfg, err := workspace.Load(ws)
	if err != nil {
		return &api.Error{
			Code:   api.ErrDaemonWorkspaceYaml,
			Detail: fmt.Sprintf("the workspace yaml refused the claim of %s before any capsule was created: %v", j.wi, err),
		}
	}
	j.workspaceYaml = wcfg
	r.composeStepCandidates(j)
	if wcfg == nil || wcfg.Projects == nil {
		return nil // no projects section: the registry inventory stands
	}
	entry, listed := wcfg.Projects[j.project]
	if !listed || entry == nil {
		return &api.Error{
			Code: api.ErrDaemonProjectNotListed,
			Detail: fmt.Sprintf("the workspace yaml refused the claim of %s: project %q is not listed in the projects section of %s — the section is the authoritative repo inventory and no registry fallback is offered; list %q there or remove the section",
				j.wi, j.project, workspace.PathFor(ws), j.project),
		}
	}
	repos := make([]api.ProjectRepo, 0, len(entry.Repos))
	sources := make(map[string]string, len(entry.Repos))
	for _, re := range entry.Repos {
		src := workspace.ResolveRepoSource(ws, re)
		repos = append(repos, api.ProjectRepo{Name: re.Name, URL: src})
		sources[re.Name] = src
	}
	j.repos = repos         // the yaml inventory WHOLLY replaces the claim answer's
	j.repoSources = sources // repo name → resolved local checkout (the audit's repo_sources)
	return nil
}

// composeStepCandidates is the polyforge#79 claim-time compose: the
// four-level precedence over the pinned bundle's frontmatter (read from
// the daemon checkout at WorkflowsDir/<bundle>; no bundle ⇒ nil — the
// yaml still stashes for the audit) and the claim's workspace yaml. The
// composed map rides the job into the driver (AgentStartInput → bridge →
// driver.Config) and into the capsule audit.
func (r *Runtime) composeStepCandidates(j *job) {
	j.stepCandidates = workspace.ComposeCandidates(r.readBundleFrontmatter(j.bundleName), j.workspaceYaml)
}

// readBundleFrontmatter reads the pinned bundle's workflow.md
// frontmatter from the daemon checkout. A torn frontmatter is aihub's
// bundle-validation business at WI creation; the claim treats it as
// absent and lets the driver's own parse surface the defect at
// dispatch.
func (r *Runtime) readBundleFrontmatter(bundleName string) *workflow.Frontmatter {
	if bundleName == "" || r.cfg.WorkflowsDir == "" {
		return nil
	}
	fm, err := workflow.ReadFrontmatter(filepath.Join(r.cfg.WorkflowsDir, bundleName, "workflow.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: bundle frontmatter unreadable (treating as absent): %v\n", bundleName, err)
		return nil
	}
	return fm
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
	if cerr := r.hub.SessionEnd(j.wi, api.SessionEndRequest{
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
	r.clearClaiming(submitted)
	return origErr
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
// cloned (an empty-history clone succeeds with a warning); the shared
// gitops.Clone puts the git-dir inside the worktree, so it is relocated to
// the external location right after, leaving a one-line .git pointer file.
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
func (r *Runtime) prepareCapsule(j *job) error {
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
	if err := os.MkdirAll(filepath.Dir(o.GitDir), 0o755); err != nil {
		return err
	}
	if o.RemoteURL == "" {
		// No capsule remote (tests only; aihub always supplies one at
		// claim): a purely local init.
		if err := gitops.InitLocal(o); err != nil {
			return fmt.Errorf("runtime: %s: local init: %w", j.wi, err)
		}
	} else if err := gitops.Clone(o); err != nil {
		// aihub creates the bare repo at claim, so a clone failure is
		// never "nothing published yet": refuse loudly (the caller
		// abandons the claim) rather than init locally and restart the
		// workflow from step 1.
		return &api.Error{Code: api.ErrDaemonCapsuleClone,
			Detail: fmt.Sprintf("capsule clone failed: %v", err)}
	}
	return gitops.ExternalizeGitDir(o)
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
// deleting it (v1.5 naming, contract §4.4: capsules/<id> →
// capsules/<id>.quarantined.<ts> — a sibling, so the scene sits next to
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

// materialize lays out the capsule: workflows/, agents/,
// capsule.json, session.json, .gitignore and the runtime-state/ tree.
// The bundle is materialized as ONE atomic set with a completion marker
// (F7, materialize.go): a pinned bundle digest is verified against the
// STAGED tree before any live target is touched (CAP-405 on divergence,
// name-derived stand-ins excepted). The claim's task-repo
// inventory is cloned into repos/<name> BEFORE the capsule identity is
// written, so the recorded baselines are the true claim-time SHAs.
func (r *Runtime) materialize(ctx context.Context, j *job) error {
	for _, d := range []string{
		capsule.DirAgents, capsule.DirArtifacts, capsule.DirLogs, capsule.DirRepos,
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
		WorkItemID:   j.wi,
		CapsuleID:    "cap_" + j.wi,
		Project:      j.project,
		BundleName:   j.bundleName,
		BundleDigest: j.bundleDigest,
		Repos:        repoSHAs,
		CreatedAt:    nowStamp(),
	}
	// polyforge#79: the claim-time snapshot doubles as the audit record —
	// the parsed yaml verbatim, the per-step post-precedence candidate
	// lists, the resolved repo sources. A no-yaml claim records NOTHING
	// (omitempty, never fabricated) and its legacy fields are unchanged.
	stampWorkspaceAudit(j, &idn)
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
	sess := capsule.Session{
		Harness:        r.cfg.Harness,
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
	// (materialize.go): the bundle lands under workflows/<bundle> and
	// agents/, and sync/materialized.json gates every later consumer.
	return r.materializeBundle(j)
}

// stampWorkspaceAudit fills the polyforge#79 audit fields of the
// capsule identity from the job's claim-time snapshot: the parsed yaml
// verbatim (the same structures the shared loader answered), the
// per-step post-precedence candidate lists, and the resolved repo
// sources — the workspace-root-joined local checkout for yaml-sourced
// clones, the registry URLs for registry clones under a yaml that
// governs nothing on the repo side. A no-yaml claim records nothing.
func stampWorkspaceAudit(j *job, idn *capsule.Identity) {
	if j.workspaceYaml == nil {
		return
	}
	projects := map[string]capsule.WorkspaceProjectAudit{}
	for name, proj := range j.workspaceYaml.Projects {
		if proj == nil {
			continue
		}
		repos := make([]capsule.WorkspaceRepoAudit, 0, len(proj.Repos))
		for _, re := range proj.Repos {
			repos = append(repos, capsule.WorkspaceRepoAudit{Name: re.Name, Path: re.Path})
		}
		projects[name] = capsule.WorkspaceProjectAudit{Repos: repos}
	}
	doc := &capsule.WorkspaceYamlAuditDoc{Projects: projects}
	if wd := j.workspaceYaml.WorkflowDefaults; wd != nil {
		doc.WorkflowDefaults = &capsule.WorkspaceDefaultsAuditDoc{
			ModelCandidates: wd.ModelCandidates,
			StepKinds:       wd.StepKinds,
		}
	}
	idn.WorkspaceYaml = doc
	if len(j.stepCandidates) > 0 {
		idn.EffectiveCandidates = j.stepCandidates
	}
	if len(j.repoSources) > 0 {
		idn.RepoSources = j.repoSources
	} else {
		// A yaml that governs nothing on the repo side (workflow-only):
		// the audit still records the sources actually cloned from — the
		// claim answer's registry URLs.
		reg := map[string]string{}
		for _, pr := range j.repos {
			reg[pr.Name] = pr.URL
		}
		if len(reg) > 0 {
			idn.RepoSources = reg
		}
	}
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
		ID:           j.wi,
		Slug:         item.Slug,
		Project:      j.project,
		Goal:         item.Goal,
		Content:      item.Content,
		Labels:       item.Labels,
		Priority:     item.Priority,
		Milestone:    item.Milestone,
		BundleName:   j.bundleName,
		BundleDigest: j.bundleDigest,
		UpdatedAt:    nowStamp(),
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
// with its reason) so a restart finishes the close — never a silent
// half-seal.
func (r *Runtime) Stop(ctx context.Context, wi, reason string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	j := r.resolveJob(wi)
	if j == nil {
		return fmt.Errorf("runtime: %s: no active job", wi)
	}
	return r.stopOp(ctx, j, reason, false, false)
}

// supersedeOp is the OBSuperseded response (v1.5 M4 + m3): the periodic
// session revalidation learned this WI is held by another daemon. The
// other owner now owns the work, so this daemon makes NO hub transition
// (fencing-on-use rejects our writes; a SessionEnd would be refused as
// STALE and a blocked verdict would sabotage the new owner), runs NO
// recovery, and instead: kills the local tree, quarantines the capsule
// in place (capsules/<id>.quarantined.<ts>), drops the state.json record
// and emits the owner-visible event — the local capsule (and any answers
// recorded in it) did not travel, so "answers may be lost; question
// re-opened" (m3).
func (r *Runtime) supersedeOp(ctx context.Context, j *job, detail string) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	// The job may have settled (Stop finished, bounded fail) between
	// handleOB's fetch and this lock: a gone job needs nothing.
	if r.jobOf(j.wi) != j {
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

func (r *Runtime) stopOp(ctx context.Context, j *job, reason string, alreadySealing bool, alreadyPushed bool) error {
	// Finding 2 (v1.5 review, MAJOR): the operator pause is its own close.
	// It does NOT pass through SEALING (no terminal verdict follows), it
	// retains the record (the secret must survive locally for the M3
	// attach) and it releases observe with RTFinished(paused). Only the
	// CLI pause route produces the paused reason, and only for a RUNNING
	// job — a repeat pause is answered by the daemon's already-paused
	// graceful path before it ever gets here.
	if reason == protocol.FinishPaused && !alreadySealing {
		return r.pauseOp(ctx, j, alreadyPushed)
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
			j.sealedPush = alreadyPushed
			j.mu.Unlock()
			r.persist()
		} else {
			if err := j.transition(StateSealing); err != nil {
				return err
			}
			j.mu.Lock()
			j.stopReason = reason
			// Persisted with the reason: a daemon crash between the earlier
			// push and SessionEnd must not make the restart's stop finisher
			// re-push either.
			j.sealedPush = alreadyPushed
			j.mu.Unlock()
			r.persist()
		}
	}
	// Six-layer teardown: no writer survives into the sealed scene.
	r.killAgentTree(j)

	// Final push (§五) — UNLESS the final state was already committed
	// and pushed (alreadyPushed: the terminal close of recover.go, the
	// C3 engine published the terminal execution.json): a second push
	// can only race it or fail against the revoked attempt credential.
	// Only the read-only seal and the terminal close remain.
	finalReason := reason
	if !alreadyPushed {
		// The seq trailer is gitops' business (derived in-lock).
		// CommitPush is honest on every path — including a clean tree,
		// where it pushes the standing HEAD — so a failed first push is
		// retried directly via gitops.Push below. A persistently failed
		// push degrades the terminal reason to blocked — the local commit
		// keeps the bytes, but the server never heard the final state.
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
			if r.cfg.EventTap != nil {
				r.cfg.EventTap(j.wi, "push_failed", map[string]any{
					"phase": "seal", "reason": reason, "error": perr.Error(),
				})
			}
			finalReason = protocol.FinishBlocked
		}
	}
	// v1.5: the terminal close is SessionEnd — fenced on the session
	// hash. A STALE_SESSION refusal (this daemon was superseded between
	// the seal push and here) surfaces as the error below and leaves the
	// job SEALING, exactly like a hub outage: never a silent half-seal.
	// The status vocabulary is wrapped|failed|blocked — plus "paused",
	// which the CLI pause path passes through (merge amend: paused routes
	// to the M3 pause transition; the session is KEPT and clean_pause=true
	// rides the same atomic fenced update — the ONLY pause producer is
	// the operator CLI, so it is always a clean pause).
	if err := r.hub.SessionEnd(j.wi, api.SessionEndRequest{
		SessionID:  j.sessionID,
		Status:     finalReason,
		CleanPause: finalReason == api.StatusPaused,
	}); err != nil {
		return fmt.Errorf("runtime: %s: session-end failed (job left sealing): %w", j.wi, err)
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
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: j.wi, Reason: finalReason})
	return nil
}

// pauseOp is the operator pause close (v1.5 review Finding 2, MAJOR):
// the M3 pause transition WITH record retention. The old contract
// (a CLI pause sealed the capsule read-only, transitioned DONE and
// dropped the record) destroyed the session secret locally — nobody
// could ever attach again, `pf resume` printed a promise no restart
// could fulfill, and a plain claim answered ALREADY_HELD. The new
// contract:
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
func (r *Runtime) pauseOp(ctx context.Context, j *job, alreadyPushed bool) error {
	if err := j.transition(StatePaused); err != nil {
		return err
	}
	j.mu.Lock()
	j.stopReason = protocol.FinishPaused
	j.sealedPush = alreadyPushed
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
	if !alreadyPushed {
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
	}
	if err := r.hub.SessionEnd(j.wi, api.SessionEndRequest{
		SessionID: j.sessionID,
		Status:    api.StatusPaused,
		// always true: the operator CLI is the only pause producer (M3)
		CleanPause: true,
	}); err != nil {
		return fmt.Errorf("runtime: %s: session-end(paused) failed (record left paused; the next restart reconciles): %w", j.wi, err)
	}
	// The observe release (Finding 4): stops the monitor loop, prunes the
	// latestSpawn entry, forgets the ledger. The retained record keeps
	// its bookkeeping only.
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: j.wi, Reason: protocol.FinishPaused})
	_ = ctx
	return nil
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

// CapsuleDirFor returns the capsule directory of a WI: the live job's
// own recorded dir (routed by its project at claim time, polyforge#113);
// "" when the WI holds no live job — a route caller for a non-live WI
// has no capsule to address.
func (r *Runtime) CapsuleDirFor(wi string) string {
	id := r.Resolve(wi)
	if id == "" {
		id = wi
	}
	j := r.jobOf(id)
	if j == nil {
		return ""
	}
	return j.capsuleDir
}
