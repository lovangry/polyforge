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
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// Aihub is the server face Runtime needs (v1.5 session model: the
// generation-era Claim/EndGeneration are replaced by the session
// endpoints). *aihubclient.Client satisfies it.
type Aihub interface {
	SessionClaim(req api.SessionClaimRequest) (*api.SessionClaimResponse, error)
	ForceTakeover(wi string, req api.SessionClaimRequest) (*api.SessionClaimResponse, error)
	SessionAttach(wi string, req api.SessionAttachRequest) (*api.SessionClaimResponse, error)
	SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error)
	SessionEnd(wi string, req api.SessionEndRequest) error
	GetWorkItem(idOrSlug string) (*api.WorkItem, error)
}

// Config is the daemon-supplied runtime configuration.
type Config struct {
	MachineID     string // registered machine identity
	WorkspaceRoot string // <root>/capsules/<wi> are the capsule worktrees
	DataDir       string // state.json + external git-dirs live here
	GitRemoteBase string // e.g. https://aihub.example/git → <base>/<wi>.git
	WorkflowsDir  string // daemon checkout's workflows/ (materialization source)
	Harness       string // harness adapter name ("pi")
	PiBin         string // harness binary (default "pi")
	// HarnessSessionsRoot overrides the harness's default session store
	// (~/.pi/agent/sessions) for the live-transcript locator (F3): the
	// production default is right; tests and relocated installs point
	// it at a scratch tree. Empty = the default.
	HarnessSessionsRoot string
	CgroupRoot          string        // cgroup v2 root; empty ⇒ layer-0 unavailable
	MaxResumeFails      int           // consecutive resume failures before fresh (default 2)
	MaxRecoveries       int           // consecutive FAILED recoveries before bounded fail (default 2)
	SendBound           time.Duration // bound on one RT→OB send (default 5s)
	// SocketPath is the daemon's UDS listen path, injected into every
	// spawned agent as PF_DAEMON_SOCKET so the agent can call
	// flush(reason) on the proxy (§五).
	SocketPath string
	// EventTap, when non-nil, receives runtime-sourced diagnostic events
	// (kind "push_failed" on a seal push that failed after its retries)
	// so they land in the daemon's durable events log next to the
	// observe lane's own entries. The daemon binds it to the observe
	// EventLog; nil (tests) disables it.
	EventTap func(wi, kind string, body map[string]any)
	// ExecutorPromptFile, when set, is prepended verbatim to every agent's
	// initial prompt (the executor skill shipped with the daemon release).
	ExecutorPromptFile string
	// EnvSecrets are configured secret values (machine api key, push
	// credential) the spawned agents' environment must never carry, even
	// under a renamed variable.
	EnvSecrets []string
	// RecordTap, when non-nil, receives every raw stdout record line the
	// production spawner reads (one harness record per delivery, verbatim
	// including the trailing newline stripped). The phase-3 daemon binds it
	// to its observe adapter so the Observe lane's stream monitoring sees
	// the record stream; nil (tests, non-pi spawners) disables it.
	RecordTap func(wi string, line []byte)
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
	// (the pi spawner); it decides the F7 discovery-copy directory.
	if c.Harness == "" {
		c.Harness = "pi"
	}
	if c.MaxResumeFails <= 0 {
		c.MaxResumeFails = 2
	}
	if c.MaxRecoveries <= 0 {
		c.MaxRecoveries = 2
	}
	if c.SendBound <= 0 {
		c.SendBound = 5 * time.Second
	}
	if c.RecallTopK <= 0 {
		c.RecallTopK = defaultRecallTopK
	}
}

// Runtime is the per-daemon WI lifecycle manager.
type Runtime struct {
	cfg     Config
	hub     Aihub
	spawner Spawner
	toOB    chan<- protocol.RTMsg
	fromOB  <-chan protocol.OBMsg

	mu   sync.Mutex
	jobs map[string]*job
	// aliases maps every NON-canonical id form the runtime has ever been
	// asked to run (a "<project>#<seq>" slug, most often) to the
	// canonical work-item id the claim resolved it to. In-memory only:
	// Resolve's unknown-input answer is "", and the daemon's resolver
	// falls back to aihub for those (a restart re-learns on demand).
	aliases map[string]string

	// opMu serializes the mutating operations (Start/Stop/recover/Restart):
	// the message loop and the daemon's direct calls share it, so the state
	// machine never sees two drivers at once.
	opMu      sync.Mutex
	statePath string
}

// job is one WI's runtime bookkeeping.
type job struct {
	wi      string
	project string
	// sessionID is the v1.5 SECRET (B3): 64-hex minted at claim, the git
	// push password. It never reaches the capsule, agent env, argv or
	// any log/event surface — only state.json and the one-shot askpass.
	sessionID string
	// sessionFingerprint is the PUBLIC handle (api.SessionFingerprint).
	sessionFingerprint string
	// instance is the derivation fence token carried on RTMsg.AttemptID
	// during the v1→v1.5 transition: the fingerprint for the first
	// derivation, "fingerprint#N" for later ones (the v1 attempt#N
	// shape, keyed on the spawn counter). The cleanup commit deletes the
	// protocol field and this collapses to SpawnN.
	instance string
	state      JobState
	stopReason string // sealed-in reason while SEALING (restart can finish the stop)
	// sealedPush records that the final capsule state was ALREADY
	// committed and pushed by the flush(wrap) half of the dual close
	// (the flaw fix): the stopOp half then skips its own seal push. It is
	// set when the SEALING transition records the reason and persisted
	// with it, so a crash between the flush's push and SessionEnd keeps
	// the restart's stop finisher from re-pushing either.
	sealedPush bool
	capsuleDir string
	git        gitops.Opts // Username=wi, Secret=session_id (v1.5 §“gitops call sites”)
	transcript string
	// liveTranscript is the file the CURRENT derivation appends to: the
	// harness-store path a fresh pi derivation really writes (the harness
	// ignores PI_SESSION_DIR), or — for a resumed derivation — the very
	// file --session points at. Empty while not yet located (the observe
	// lane re-locates at its sync points; the recovery engine at its
	// resume decision).
	liveTranscript string
	bundleName     string
	bundleDigest   string
	// item is the claim answer's work-item snapshot (the F1 materializer
	// input). In-memory only: the durable copy is the capsule's
	// runtime-state/work-item.json, which recovery re-derives from aihub
	// (GetWorkItem) when a legacy capsule predates it.
	item *api.WorkItem
	// repos is the claim-served task-repo inventory (api.Claim.Repos):
	// materialize clones each entry into repos/<name> before the agent
	// starts. In-memory only — recovery never re-materializes, so it is
	// not part of the persisted job record.
	repos []api.ProjectRepo

	agent          AgentHandle
	agentPID       int
	agentStartedAt string

	spawnN       int
	recoverFails int // consecutive FAILED recoveries; reset on success
	resumeFails  int
	resumeDead   bool
	// cleanPause mirrors the hub's operator-pause flag; always false
	// while this daemon owns the job (see jobRecord.CleanPause).
	cleanPause bool
	// candidates is the bundle's defaults model_candidates chain (F4):
	// spawnAgent pins the derivation to candidates[candidateOrdinal]. The
	// ordinal advances ONLY on provider-class recovery (INV-1g; see
	// recover.go advanceCandidate) and is persisted in the job record.
	candidates       []string
	candidateOrdinal int

	mu sync.Mutex
}

// New builds a Runtime. toOB receives RT→Observe messages, fromOB carries
// Observe→Runtime messages; both sides speak only common/protocol types.
func New(cfg Config, hub Aihub, spawner Spawner, toOB chan<- protocol.RTMsg, fromOB <-chan protocol.OBMsg) *Runtime {
	cfg.fill()
	return &Runtime{
		cfg:       cfg,
		hub:       hub,
		spawner:   spawner,
		toOB:      toOB,
		fromOB:    fromOB,
		jobs:      map[string]*job{},
		aliases:   map[string]string{},
		statePath: filepath.Join(cfg.DataDir, "runtime-state.json"),
	}
}

// DefaultSpawner returns the production pi-harness spawner. It takes the
// runtime config BY POINTER: the spawner reads its fields at spawn time
// from the shared config, so any field bound after assembly (RecordTap
// included) is seen by the spawner.
func DefaultSpawner(cfg *Config) Spawner {
	return newPiSpawner(cfg)
}

// Run is the OB-message loop: it consumes Observe messages until ctx ends.
// Every message is fenced by attempt-id; the fencing and per-state drops
// live here (§二: attempt-id ≠ current ⇒ dropped; SEALING drops a
// same-attempt WINeedsRecovery).
func (r *Runtime) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-r.fromOB:
			if !ok {
				return nil
			}
			r.handleOB(ctx, msg)
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
	state, fence := j.snapshot()
	if msg.AttemptID != fence {
		return // stale handle: dropped (recovery-loop guard, §二)
	}
	switch msg.Kind {
	case protocol.OBSuperseded:
		// v1.5 M4: the observe lane's periodic session revalidation
		// learned the WI is held elsewhere. NOT a recovery: the new owner
		// owns the work. Kill the tree, quarantine the capsule, drop the
		// record — and make NO hub transition (fencing-on-use already
		// rejects our writes; a SessionEnd would be refused as stale
		// anyway, and a blocked verdict would sabotage the new owner).
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
		r.recoverOp(ctx, j, msg.Reason)
	case protocol.OBCompleted:
		if state != StateRunning {
			return
		}
		// The terminal vocabulary (wrapped|failed|blocked) passes through
		// VERBATIM: flush(wrap)'s blocked outcome (3 failed seals) must
		// reach aihub as blocked, not be silently upgraded to wrapped.
		//
		// wrapped|failed additionally mean the monitor's flushWrap has
		// ALREADY committed and pushed the final state ("wrap: <status>",
		// its EvPublished) — the dual-path close's other half, this very
		// Stop, must not push a second time: the attempt credential is
		// revoked once the sealed state settles, and a redundant seal push
		// fails against it (the real-machine push_failed: a successful
		// wrap push followed one second later by a seal push refused with
		// GIT-500). The blocked vocabulary is the exception: the flush
		// itself failed to push, so the stop retries the push before
		// degrading — exactly the pre-existing semantics.
		sealedByFlush := msg.Reason != protocol.FinishBlocked
		if err := r.stopWith(ctx, msg.WorkItem, msg.Reason, sealedByFlush); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: stop after completion failed: %v\n", msg.WorkItem, err)
		}
	}
}

// send delivers one RT→Observe message, bounded so a wedged Observe cannot
// wedge the Runtime (the message would only ever be re-derived anyway).
func (r *Runtime) send(msg protocol.RTMsg) {
	select {
	case r.toOB <- msg:
	case <-time.After(r.cfg.SendBound):
		fmt.Fprintf(os.Stderr, "runtime: %s: RT message %s dropped (observe channel full)\n", msg.WorkItem, msg.Kind)
	}
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
	return atomicWrite(r.statePath, blob, stateFilePerm)
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

func (r *Runtime) capsuleDir(wi string) string {
	return filepath.Join(r.cfg.WorkspaceRoot, "capsules", wi)
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

// harnessSessionsRoot answers the configured harness session store.
func (r *Runtime) harnessSessionsRoot() string {
	if r.cfg.HarnessSessionsRoot != "" {
		return r.cfg.HarnessSessionsRoot
	}
	return HarnessSessionsRootDefault()
}

// locateLive locates the live transcript of a derivation born at
// spawnAt (the F3 face over the shared helper; "" = not there yet or
// never — a typed warning condition, never an error).
func (r *Runtime) locateLive(capsuleDir string, spawnAt time.Time) string {
	return LocateLiveTranscript(r.harnessSessionsRoot(), capsuleDir, spawnAt)
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

// Start claims wi and runs it to a live agent (a plain claim). Idempotency
// rides on the claim idempotency key: a retry after a lost response
// re-claims the same session server-side (D4). See StartIntent for the
// resume/takeover intents.
func (r *Runtime) Start(ctx context.Context, wi string) error {
	return r.StartIntent(ctx, wi, "claim")
}

// StartIntent claims wi with an explicit claim intent (claim|resume|
// takeover) and runs it to a live agent. v1.5 maps the intents onto the
// session endpoints: "takeover" → ForceTakeover (an atomic session
// swap, a NEW secret), everything else → the CAS SessionClaim (a paused
// WI answers NOT_CLAIMABLE until the hub side grows an operator-unpause
// transition — the honest refusal, never a guess; contract complaint
// filed). The secret is minted HERE and journaled durably BEFORE the
// call (B1): a crash between journal and call is recoverable by replay,
// a crash after the call by verify — the ambiguous-claim window is
// closed from both sides.
func (r *Runtime) StartIntent(ctx context.Context, wi, intent string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if j := r.jobOf(wi); j != nil {
		return fmt.Errorf("runtime: %s already active (state %s)", wi, j.state)
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
	var resp *api.SessionClaimResponse
	if intent == "takeover" {
		req.Reason = "operator takeover"
		resp, err = r.hub.ForceTakeover(wi, req)
	} else {
		resp, err = r.hub.SessionClaim(req)
	}
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
		// an already-live job (same spelling or canonical): the start is a
		// duplicate — for a Restart replay of the claiming journal this
		// means the original start registered the job before the crash
		// (the state.json record reconciles it), so only the journal is stale.
		r.clearClaiming(wi)
		return fmt.Errorf("runtime: %s already active (state %s)", id, existing.state)
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
		instance:           fp, // derivation 1's fence token
		state:              StateRunning,
		capsuleDir:         r.capsuleDir(id),
		git: gitops.Opts{
			WorkItemID: id,
			// the push-option backup layer's value until Lane C renames
			// the option: the session fingerprint (contract §1.6).
			AttemptID:  fp,
			CapsuleDir: r.capsuleDir(id),
			GitDir:     r.gitDirFor(id),
			RemoteURL:  r.remoteURLFor(id),
			Username:   id,
			Secret:     sessionID, // v1.5: the session secret IS the push password
		},
		bundleName:   c.BundleName,
		bundleDigest: c.BundleDigest,
		repos:        c.Repos,
		item:         &resp.WorkItem,
	}
	j.transcript = sessionPathFor(j.capsuleDir, fp)

	if err := r.prepareCapsule(j); err != nil {
		return r.abandonClaim(j, wi, err, "prepare capsule: "+err.Error())
	}
	// B2 spawn_n source: a FRESHLY CLAIMED capsule may carry another
	// session's spawn journal (the takeover/attach clone path — the
	// journal rides capsule git), so the derivation counter continues
	// where the capsule's history left off; a brand-new capsule answers
	// 0 and the first derivation is #1.
	j.mu.Lock()
	j.spawnN = maxSpawnNFromJournal(j.capsuleDir)
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
	if _, err := r.spawnAgent(ctx, j, false, ""); err != nil {
		return r.abandonClaim(j, wi, err, "spawn agent: "+err.Error())
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
	inst := j.instance
	spawnN := j.spawnN
	j.mu.Unlock()
	r.send(protocol.RTMsg{
		Kind:      protocol.RTStarted,
		AttemptID: inst,
		SpawnN:    spawnN,
		WorkItem:  id,
		AgentPID:  pid,
		// the secret rides in-process to the observe lane only (its M4
		// session revalidation needs it); never a log, never the capsule.
		SessionID: j.sessionID,
	})
	return nil
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
		fmt.Fprintf(os.Stderr, "runtime: %s: session-end(blocked) after failed start failed (claiming entry kept for the next restart): %v\n", j.wi, cerr)
		return origErr
	}
	r.clearClaiming(submitted)
	return origErr
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
// gitignored: it deliberately does NOT travel with a takeover clone), so:
//
//	amarker == this session's fingerprint → this session's own
//	  crashed-Start husk (or attach-resume scene): reuse in place;
//	anything else (another session's scene, a sealed close, a pre-v1.5
//	  capsule with no marker) → quarantine, never reuse.
func (r *Runtime) prepareCapsule(j *job) error {
	o := j.git
	pointer := filepath.Join(o.CapsuleDir, ".git")
	if isRegularFile(pointer) && dirExists(o.GitDir) {
		if readSessionMarker(o.CapsuleDir) == j.sessionFingerprint {
			return nil // this session's own crashed-Start husk: reuse
		}
		if err := quarantineCapsule(o.CapsuleDir); err != nil {
			return err
		}
	}
	if dirExists(o.CapsuleDir) && !isRegularFile(pointer) {
		// unprepared husk or another session's sealed scene
		if err := quarantineCapsule(o.CapsuleDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(o.GitDir), 0o755); err != nil {
		return err
	}
	if err := gitops.Clone(o); err != nil {
		// No usable remote (or a torn local state): fall back to a purely
		// local init — gitops owns the single implementation of the
		// init/externalize pair (the old gitops.Init was removed: it
		// wrote the .git pointer over a live directory).
		if lerr := gitops.InitLocal(o); lerr != nil {
			return fmt.Errorf("runtime: %s: capsule clone failed: %w", j.wi, err)
		}
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
// daemon-private, gitignored, so it never travels with a takeover clone
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
func quarantineCapsule(capsuleDir string) error {
	if !dirExists(capsuleDir) {
		return nil // nothing to move (a capsule never materialized)
	}
	ts := strconv.FormatInt(time.Now().UnixNano(), 36)
	return os.Rename(capsuleDir, capsuleDir+".quarantined."+ts)
}

// materialize lays out the capsule: workflows/, agents/, .pi/agents/,
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
	// BaseCommit semantics (aligned with v0, multi-repo shape): the
	// per-repo claim-time baselines live in the Repos map above and are
	// authoritative; BaseCommit stays EMPTY rather than singling out one
	// repo's SHA as "the" baseline — with several task repos there is no
	// single main repo to pick, and any choice would silently lie.
	// v1.5: the generation/attempt lineage fields stay at their zero
	// values (the capsule Identity shape is frozen in common/); session
	// ownership lives in the daemon-private sync/session-handle marker.
	idn := capsule.Identity{
		WorkItemID:   j.wi,
		CapsuleID:    "cap_" + j.wi,
		Project:      j.project,
		BundleName:   j.bundleName,
		BundleDigest: j.bundleDigest,
		BaseCommit:   "",
		Repos:        repoSHAs,
		CreatedAt:    nowStamp(),
	}
	blob, _ := json.MarshalIndent(idn, "", "  ")
	if err := atomicWrite(filepath.Join(j.capsuleDir, capsule.CapsuleJSON), blob, 0o644); err != nil {
		return err
	}
	// v1.5: the session ownership marker — this session's fingerprint,
	// under gitignored sync/ so a takeover clone re-stamps its own.
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
	if err := atomicWrite(filepath.Join(j.capsuleDir, capsule.SessionJSON), sblob, 0o644); err != nil {
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
	// (materialize.go): the authority copy lands under workflows/<bundle>
	// and agents/, the harness DISCOVERY copies under .pi/agents/, and
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
// (a legacy pre-F1 capsule, or a torn scene): the in-memory claim
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
	return atomicWrite(filepath.Join(j.capsuleDir, filepath.FromSlash(capsule.WorkItemJSON)), blob, 0o644)
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
		in, ierr := os.Open(p)
		if ierr != nil {
			return ierr
		}
		defer in.Close()
		if werr := atomicWriteFileMode(target, in, 0o444); werr != nil {
			return werr
		}
		return nil
	})
}

func atomicWriteFileMode(dst string, in io.Reader, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".mtmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// ---------------------------------------------------------------------------
// Stop (§一): seal → final push → SessionEnd → RTFinished.

// Stop seals wi with a terminal reason (protocol.FinishWrapped | -Failed,
// plus "paused" from the CLI pause route). The agent tree is torn down,
// the capsule's final state is committed and pushed, the session is ended
// server-side (SessionEnd, fenced on our secret), the scene is made
// read-only and Observe hears WIFinished. A SessionEnd failure leaves the
// job in SEALING (persisted with its reason) so a restart finishes the
// close — never a silent half-seal.
func (r *Runtime) Stop(ctx context.Context, wi, reason string) error {
	return r.stopWith(ctx, wi, reason, false)
}

// stopWith is Stop plus the dual-close seal knowledge: sealedByFlush
// names a close whose final capsule state the agent's flush(wrap) path
// has ALREADY committed and pushed, so stopOp skips its own seal push
// (see handleOB's OBCompleted branch). The CLI close paths pass false
// there: their seal push is the only one that ever ran.
func (r *Runtime) stopWith(ctx context.Context, wi, reason string, sealedByFlush bool) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	j := r.resolveJob(wi)
	if j == nil {
		return fmt.Errorf("runtime: %s: no active job", wi)
	}
	return r.stopOp(ctx, j, reason, false, sealedByFlush)
}

// supersedeOp is the OBSuperseded response (v1.5 M4 + m3): the periodic
// session revalidation learned this WI is held by another daemon. The
// other owner now owns the work, so this daemon makes NO hub transition
// (fencing-on-use rejects our writes; a SessionEnd would be refused as
// STALE and a blocked verdict would sabotage the new owner), runs NO
// recovery, and instead: kills the local tree, quarantines the capsule
// in place (capsules/<id>.quarantined.<ts>), drops the state.json record
// and emits the owner-visible event — the answer ledger did not travel,
// so "answers may be lost; question re-opened" (m3).
func (r *Runtime) supersedeOp(ctx context.Context, j *job, detail string) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	// The job may have settled (Stop finished, bounded fail) between
	// handleOB's fetch and this lock: a gone job needs nothing.
	if r.jobOf(j.wi) != j {
		return
	}
	r.killAgentTree(j)
	if err := quarantineCapsule(j.capsuleDir); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: superseded capsule quarantine failed (scene left in place): %v\n", j.wi, err)
	}
	r.dropJob(j)
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", j.wi, err)
	}
	if r.cfg.EventTap != nil {
		r.cfg.EventTap(j.wi, "session_superseded", map[string]any{
			"session_fingerprint": j.sessionFingerprint,
			"note":               "answers may be lost; question re-opened",
			"detail":             detail,
		})
	}
	_ = ctx
}

func (r *Runtime) stopOp(ctx context.Context, j *job, reason string, alreadySealing bool, sealedByFlush bool) error {
	if !alreadySealing {
		if err := j.transition(StateSealing); err != nil {
			return err
		}
		j.mu.Lock()
		j.stopReason = reason
		// Persisted with the reason: a daemon crash between the flush's
		// push and SessionEnd must not make the restart's stop finisher
		// re-push either.
		j.sealedPush = sealedByFlush
		j.mu.Unlock()
		r.persist()
	}
	// Six-layer teardown: no writer survives into the sealed scene.
	r.killAgentTree(j)

	// Final push (flush(wrap) semantics, §五) — UNLESS the flush path
	// already sealed: then the final state is committed, pushed and
	// published, the transcript rode that push, and a second push can
	// only fail against the revoked attempt credential (the real-machine
	// push_failed one second after a successful wrap push). Only the
	// read-only seal and the terminal close remain for this half of the
	// dual path.
	finalReason := reason
	if !sealedByFlush {
		// F3: the dead derivation's live transcript rides the seal commit
		// (C1) — sync before the push, best-effort.
		r.syncTranscriptBeforeSeal(j)

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
	// which the CLI pause path passes through (contract complaint: §1.5
	// has no pause transition; see the lane report).
	if err := r.hub.SessionEnd(j.wi, api.SessionEndRequest{
		SessionID: j.sessionID,
		Status:    finalReason,
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
	r.send(protocol.RTMsg{Kind: protocol.RTFinished, AttemptID: j.instance, WorkItem: j.wi, Reason: finalReason})
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
