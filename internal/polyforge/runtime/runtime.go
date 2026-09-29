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
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// Aihub is the server face Runtime needs. *aihubclient.Client satisfies it.
type Aihub interface {
	Claim(req aihubclient.ClaimRequest) (*aihubclient.ClaimResponse, error)
	EndGeneration(wi string, req aihubclient.EndGenerationRequest) error
	GetWorkItem(idOrSlug string) (*api.WorkItem, error)
}

// Config is the daemon-supplied runtime configuration.
type Config struct {
	MachineID      string        // registered machine identity
	WorkspaceRoot  string        // <root>/capsules/<wi> are the capsule worktrees
	DataDir        string        // state.json + external git-dirs live here
	GitRemoteBase  string        // e.g. https://aihub.example/git → <base>/<wi>.git
	WorkflowsDir   string        // daemon checkout's workflows/ (materialization source)
	Harness        string        // harness adapter name ("pi")
	PiBin          string        // harness binary (default "pi")
	CgroupRoot     string        // cgroup v2 root; empty ⇒ layer-0 unavailable
	MaxResumeFails int           // consecutive resume failures before fresh (default 2)
	MaxRecoveries  int           // consecutive FAILED recoveries before bounded fail (default 2)
	SendBound      time.Duration // bound on one RT→OB send (default 5s)
	// SocketPath is the daemon's UDS listen path, injected into every
	// spawned agent as PF_DAEMON_SOCKET so the agent can call
	// flush(reason) on the proxy (§五).
	SocketPath string
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
}

func (c *Config) fill() {
	if c.MaxResumeFails <= 0 {
		c.MaxResumeFails = 2
	}
	if c.MaxRecoveries <= 0 {
		c.MaxRecoveries = 2
	}
	if c.SendBound <= 0 {
		c.SendBound = 5 * time.Second
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
	// attempt is the aihub claim attempt id; instance is the current agent
	// lineage id used as the protocol attempt-id fence.
	attempt      string
	instance     string
	generation   int64
	state        JobState
	stopReason   string // sealed-in reason while SEALING (restart can finish the stop)
	capsuleDir   string
	git          gitops.Opts
	sessionID    string
	transcript   string
	bundleName   string
	bundleDigest string

	agent          AgentHandle
	agentPID       int
	agentStartedAt string

	spawnN       int
	recoverFails int // consecutive FAILED recoveries; reset on success
	resumeFails  int
	resumeDead   bool

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
		statePath: filepath.Join(cfg.DataDir, "runtime-state.json"),
	}
}

// DefaultSpawner returns the production pi-harness spawner.
func DefaultSpawner(cfg Config) Spawner {
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
	state, instance := j.snapshot()
	if msg.AttemptID != instance {
		return // stale handle: dropped (recovery-loop guard, §二)
	}
	switch msg.Kind {
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
		if err := r.Stop(ctx, msg.WorkItem, msg.Reason); err != nil {
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
	r.mu.Unlock()
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

func randomKey() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Start (§一 Runtime): claim → clone/init capsule → materialize → spawn →
// RTStarted.

// Start claims wi and runs it to a live agent (a plain claim). Idempotency
// rides on the claim idempotency key: a retry after a lost response
// re-claims the same attempt server-side. See StartIntent for the
// resume/takeover intents.
func (r *Runtime) Start(ctx context.Context, wi string) error {
	return r.StartIntent(ctx, wi, "claim")
}

// StartIntent claims wi with an explicit claim intent (claim|resume|
// takeover — the aihub claim-endpoint semantics differ per intent) and
// runs it to a live agent. The phase-3 daemon routes the CLI's
// submit/resume/takeover verbs here.
func (r *Runtime) StartIntent(ctx context.Context, wi, intent string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if j := r.jobOf(wi); j != nil {
		return fmt.Errorf("runtime: %s already active (state %s)", wi, j.state)
	}
	resp, err := r.hub.Claim(aihubclient.ClaimRequest{
		WorkItemID:     wi,
		Intent:         intent,
		MachineID:      r.cfg.MachineID,
		Owner:          r.cfg.MachineID,
		IdempotencyKey: randomKey(),
	})
	if err != nil {
		return err
	}
	c := resp.Claim
	j := &job{
		wi:         wi,
		project:    resp.WorkItem.ProjectName,
		attempt:    c.AttemptID,
		state:      StateRunning,
		capsuleDir: r.capsuleDir(wi),
		git: gitops.Opts{
			WorkItemID: wi,
			AttemptID:  c.AttemptID,
			CapsuleDir: r.capsuleDir(wi),
			GitDir:     r.gitDirFor(wi),
			RemoteURL:  r.remoteURLFor(wi),
			Username:   c.WorkItemID,
			Secret:     resp.Credential.Secret,
		},
		generation:   c.Generation,
		bundleName:   c.BundleName,
		bundleDigest: c.BundleDigest,
	}
	j.sessionID = fmt.Sprintf("sess_%s_%d", wi, c.Generation)
	j.transcript = filepath.Join(j.capsuleDir, capsule.DirSession, "session-"+c.AttemptID+".jsonl")

	if err := r.prepareCapsule(j); err != nil {
		return err
	}
	if err := r.materialize(j); err != nil {
		return err
	}
	if _, err := r.spawnAgent(ctx, j, false); err != nil {
		return err
	}
	r.mu.Lock()
	r.jobs[wi] = j
	r.mu.Unlock()
	if err := r.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: state persist failed: %v\n", wi, err)
	}
	pid := 0
	j.mu.Lock()
	pid = j.agentPID
	inst := j.instance
	j.mu.Unlock()
	r.send(protocol.RTMsg{Kind: protocol.RTStarted, AttemptID: inst, WorkItem: wi, AgentPID: pid})
	return nil
}

// prepareCapsule makes CapsuleDir a git worktree whose git-dir is EXTERNAL
// (ruling #2: agents never reach hooks/config/index). The aihub bare repo is
// cloned (an empty-history clone succeeds with a warning); the shared
// gitops.Clone puts the git-dir inside the worktree, so it is relocated to
// the external location right after, leaving a one-line .git pointer file.
// A leftover capsule of a different generation is quarantined, never
// reused in place.
func (r *Runtime) prepareCapsule(j *job) error {
	o := j.git
	pointer := filepath.Join(o.CapsuleDir, ".git")
	if isRegularFile(pointer) && dirExists(o.GitDir) {
		if gen := readCapsuleGeneration(o.CapsuleDir); gen == j.generation {
			return nil // same-generation husk from a crashed Start: reuse
		}
		if err := quarantineCapsule(o.CapsuleDir); err != nil {
			return err
		}
	}
	if dirExists(o.CapsuleDir) && !isRegularFile(pointer) {
		// unprepared husk or an older generation's sealed scene
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

func readCapsuleGeneration(capsuleDir string) int64 {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.CapsuleJSON))
	if err != nil {
		return -1
	}
	var idn capsule.Identity
	if json.Unmarshal(raw, &idn) != nil {
		return -1
	}
	return idn.Generation
}

// quarantineCapsule moves a stale/foreign capsule aside, never deleting it.
func quarantineCapsule(capsuleDir string) error {
	qdir := filepath.Join(filepath.Dir(capsuleDir), "_quarantine")
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(qdir, filepath.Base(capsuleDir)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	return os.Rename(capsuleDir, dst)
}

// materialize lays out the capsule: workflows/, agents/, capsule.json,
// session.json, .gitignore and the runtime-state/ tree. The bundle is
// copied read-only from the daemon checkout when configured.
func (r *Runtime) materialize(j *job) error {
	for _, d := range []string{
		capsule.DirAgents, capsule.DirArtifacts, capsule.DirLogs, capsule.DirRepos,
		capsule.DirRuntimeState, capsule.DirSession, capsule.DirWorkflows,
		filepath.Join(capsule.DirLogs, "session"),
	} {
		if err := os.MkdirAll(filepath.Join(j.capsuleDir, filepath.FromSlash(d)), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(j.capsuleDir, ".gitignore"), []byte(capsule.GitignoreContent), 0o644); err != nil {
		return err
	}
	idn := capsule.Identity{
		WorkItemID:   j.wi,
		CapsuleID:    "cap_" + j.wi,
		Project:      j.project,
		Generation:   j.generation,
		AttemptID:    j.attempt,
		BundleName:   j.bundleName,
		BundleDigest: j.bundleDigest,
		Repos:        map[string]string{},
		CreatedAt:    nowStamp(),
	}
	blob, _ := json.MarshalIndent(idn, "", "  ")
	if err := atomicWrite(filepath.Join(j.capsuleDir, capsule.CapsuleJSON), blob, 0o644); err != nil {
		return err
	}
	sess := capsule.Session{
		AttemptID:      j.attempt,
		Generation:     j.generation,
		Harness:        r.cfg.Harness,
		SessionID:      j.sessionID,
		TranscriptPath: j.transcript,
		UpdatedAt:      nowStamp(),
	}
	sblob, _ := json.MarshalIndent(sess, "", "  ")
	if err := atomicWrite(filepath.Join(j.capsuleDir, capsule.SessionJSON), sblob, 0o644); err != nil {
		return err
	}
	if r.cfg.WorkflowsDir == "" || j.bundleName == "" {
		return nil
	}
	src := filepath.Join(r.cfg.WorkflowsDir, j.bundleName)
	if !dirExists(src) {
		fmt.Fprintf(os.Stderr, "runtime: %s: bundle %s not in checkout; capsule materialized without it\n", j.wi, j.bundleName)
		return nil
	}
	dst := filepath.Join(j.capsuleDir, capsule.DirWorkflows, j.bundleName)
	if err := copyTree(src, dst); err != nil {
		return err
	}
	// step-agent definitions ride the bundle's agents/ subdir
	if dirExists(filepath.Join(src, "agents")) {
		return copyTree(filepath.Join(src, "agents"), filepath.Join(j.capsuleDir, capsule.DirAgents))
	}
	return nil
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
// Stop (§一): seal → final push → EndGeneration → RTFinished.

// Stop seals wi with a terminal reason (protocol.FinishWrapped | -Failed).
// The agent tree is torn down, the capsule's final state is committed and
// pushed, the generation is ended server-side, the scene is made read-only
// and Observe hears WIFinished. An EndGeneration failure leaves the job in
// SEALING (persisted with its reason) so a restart finishes the close —
// never a silent half-seal.
func (r *Runtime) Stop(ctx context.Context, wi, reason string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	j := r.jobOf(wi)
	if j == nil {
		return fmt.Errorf("runtime: %s: no active job", wi)
	}
	return r.stopOp(ctx, j, reason, false)
}

func (r *Runtime) stopOp(ctx context.Context, j *job, reason string, alreadySealing bool) error {
	if !alreadySealing {
		if err := j.transition(StateSealing); err != nil {
			return err
		}
		j.mu.Lock()
		j.stopReason = reason
		j.mu.Unlock()
		r.persist()
	}
	// Six-layer teardown: no writer survives into the sealed scene.
	r.killAgentTree(j)

	// Final push (flush(wrap) semantics, §五). The seq trailer is
	// gitops' business (derived in-lock). CommitPush is honest on every
	// path — including a clean tree, where it pushes the standing HEAD —
	// so a failed first push is retried directly via gitops.Push below.
	// A persistently failed push degrades the terminal reason to blocked
	// — the local commit keeps the bytes, but the server never heard the
	// final state.
	finalReason := reason
	_, _, perr := gitops.CommitPush(j.git, "seal: "+reason)
	for i := 0; i < 2 && perr != nil; i++ {
		_, perr = gitops.Push(j.git)
	}
	if perr != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: final push failed after retries: %v\n", j.wi, perr)
		finalReason = protocol.FinishBlocked
	}
	if err := r.hub.EndGeneration(j.wi, aihubclient.EndGenerationRequest{
		AttemptID:  j.attempt,
		Generation: j.generation,
		Status:     finalReason,
	}); err != nil {
		return fmt.Errorf("runtime: %s: end-generation failed (job left sealing): %w", j.wi, err)
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
