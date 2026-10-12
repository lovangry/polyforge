package runtime

// Shared test scaffolding: a scripted aihub, a scripted spawner that spawns
// REAL (trivial) child processes so death/kill semantics are exercised
// honestly, and small builders for jobs and capsule git repos.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/mapping"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// ---------------------------------------------------------------------------
// fake aihub (v1.5 session model): a tiny in-memory hub. `held` maps a
// canonical wi → the session SECRET currently holding it ("" = unheld);
// SessionVerify compares against it, SessionClaim installs,
// SessionEnd clears (terminal) or keeps (paused) it, SessionAttach
// re-opens a paused+not-clean-pause row. `status` scripts the WI status
// verify answers; `lookupErr` scripts outages per wi.

type fakeHub struct {
	mu         sync.Mutex
	status     map[string]string
	held       map[string]string // wi → session secret ("" = unheld)
	cleanPause map[string]bool
	lookupErr  map[string]error
	claims     []api.SessionClaimRequest
	attaches   []api.SessionAttachRequest
	ends       []api.SessionEndRequest
	end        map[string]error  // wi → end-error scripting (per wi)
	digest     string            // claim-time bundle digest (default "deadbeef")
	repos      []api.ProjectRepo // claim-served task-repo inventory (nil = none)
	// canonical maps an id-or-slug form (e.g. "proj#7") to the canonical
	// work-item id the claim answer carries (F2 tests); nil entry = the
	// input echoes itself (the pre-F2 behavior).
	canonical map[string]string
	items     map[string]*api.WorkItem // canonical id → claim answer record (F1 tests)
	// keyClaims implements the aihub idempotency contract for tests (D4/
	// B1): a repeated claim with the SAME IdempotencyKey hands back the
	// SAME claim answer (same session, same bundle pin) instead of
	// opening a new one.
	keyClaims map[string]*api.SessionClaimResponse
}

func newFakeHub() *fakeHub {
	return &fakeHub{
		status:     map[string]string{},
		held:       map[string]string{},
		cleanPause: map[string]bool{},
		lookupErr:  map[string]error{},
		end:        map[string]error{},
		keyClaims:  map[string]*api.SessionClaimResponse{},
	}
}

// hold scripts the hub: wi is held by sessionID in the given status.
func (h *fakeHub) hold(wi, sessionID, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[wi] = sessionID
	if status != "" {
		h.status[wi] = status
	}
}

func (h *fakeHub) canonicalID(idOrSlug string) string {
	if c, ok := h.canonical[idOrSlug]; ok {
		return c
	}
	return idOrSlug
}

func (h *fakeHub) itemFor(id string) api.WorkItem {
	item := api.WorkItem{ID: id, ProjectName: "proj"}
	if h.items != nil {
		if it, ok := h.items[id]; ok && it != nil {
			item = *it
		}
	}
	return item
}

func (h *fakeHub) claimResponse(wi, sessionID string) *api.SessionClaimResponse {
	digest := h.digest
	if digest == "" {
		digest = "deadbeef"
	}
	return &api.SessionClaimResponse{
		Claim: api.SessionClaim{
			WorkItemID:         wi,
			SessionFingerprint: api.SessionFingerprint(sessionID),
			BundleName:         "demo-bundle",
			BundleDigest:       digest,
			Repos:              h.repos,
		},
		WorkItem: h.itemFor(wi),
	}
}

func (h *fakeHub) SessionClaim(req api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.claims = append(h.claims, req)
	// idempotent replay (the aihub contract): same key → same answer,
	// provided the cached session is still the live one (D4)
	if req.IdempotencyKey != "" {
		if prev, ok := h.keyClaims[req.IdempotencyKey]; ok {
			if prev.Claim.SessionFingerprint == api.SessionFingerprint(req.SessionID) {
				return prev, nil
			}
			return nil, &api.Error{Code: "CORE-403", Detail: "STALE_SESSION: idempotency cache holds another session"}
		}
	}
	id := h.canonicalID(req.WorkItemID)
	if cur := h.held[id]; cur != "" && cur != req.SessionID {
		return nil, &api.Error{Code: "CORE-409", Detail: "ALREADY_HELD"}
	}
	resp := h.claimResponse(id, req.SessionID)
	if req.IdempotencyKey != "" {
		h.keyClaims[req.IdempotencyKey] = resp
	}
	h.held[id] = req.SessionID
	h.status[id] = api.StatusRunning
	return resp, nil
}

// forceEnd scripts the aihub-side force-end-session recovery door: the
// held session is dropped and the WI returns to queued, so the next plain
// claim succeeds.
func (h *fakeHub) forceEnd(wi string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(wi)
	h.held[id] = ""
	h.status[id] = api.StatusQueued
	h.cleanPause[id] = false
}

func (h *fakeHub) SessionAttach(wi string, req api.SessionAttachRequest) (*api.SessionClaimResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attaches = append(h.attaches, req)
	id := h.canonicalID(wi)
	if err, ok := h.lookupErr[id]; ok {
		return nil, err
	}
	if h.held[id] != req.SessionID {
		return nil, &api.Error{Code: "CORE-403", Detail: "STALE_SESSION"}
	}
	if h.status[id] != api.StatusPaused || h.cleanPause[id] {
		return nil, &api.Error{Code: "CORE-409", Detail: "NOT_ATTACHABLE"}
	}
	h.status[id] = api.StatusRunning
	return h.claimResponse(id, req.SessionID), nil
}

func (h *fakeHub) SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(wi)
	if err, ok := h.lookupErr[id]; ok {
		return nil, err
	}
	st := h.status[id]
	if st == "" {
		st = api.StatusRunning
	}
	out := &api.SessionVerifyResponse{WorkItemID: id, Status: st, CleanPause: h.cleanPause[id]}
	if h.held[id] == sessionID && sessionID != "" {
		out.Matches = true
	} else if h.held[id] != "" {
		out.CurrentFingerprint = api.SessionFingerprint(h.held[id])
	}
	return out, nil
}

func (h *fakeHub) SessionEnd(wi string, req api.SessionEndRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(wi)
	if err, ok := h.lookupErr[id]; ok {
		return err
	}
	if err, ok := h.end[id]; ok {
		return err
	}
	// fenced: another session's row is not ours to end
	if cur := h.held[id]; cur != "" && cur != req.SessionID {
		return &api.Error{Code: "CORE-403", Detail: "STALE_SESSION"}
	}
	h.ends = append(h.ends, req)
	switch req.Status {
	case api.StatusWrapped, api.StatusFailed, api.StatusBlocked, api.StatusCanceled:
		h.held[id] = "" // terminal + blocked clear the session (M2)
	default:
		// paused keeps the session (the design's paused-with-session)
	}
	h.status[id] = req.Status
	return nil
}

func (h *fakeHub) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(idOrSlug)
	if err, ok := h.lookupErr[id]; ok {
		return nil, err
	}
	st := h.status[id]
	if st == "" {
		st = api.StatusRunning
	}
	item := h.itemFor(id)
	item.Status = st
	return &item, nil
}

func (h *fakeHub) endCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ends)
}

func (h *fakeHub) lastEnd() api.SessionEndRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ends) == 0 {
		return api.SessionEndRequest{}
	}
	return h.ends[len(h.ends)-1]
}

func (h *fakeHub) claimCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.claims)
}

// ---------------------------------------------------------------------------
// scripted spawner: records every call and spawns a real `sleep` child as
// the "agent" (own process group, PF_WI_ID marker env) so kill semantics
// are real.

type scriptedSpawner struct {
	mu         sync.Mutex
	calls      []SpawnParams
	failResume int
	failFresh  int
}

func (s *scriptedSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	s.mu.Lock()
	s.calls = append(s.calls, p)
	fr, ff := s.failResume, s.failFresh
	if p.Resume && fr > 0 {
		s.failResume--
		s.mu.Unlock()
		return nil, fmt.Errorf("scripted resume spawn failure")
	}
	if !p.Resume && ff > 0 {
		s.failFresh--
		s.mu.Unlock()
		return nil, fmt.Errorf("scripted fresh spawn failure")
	}
	s.mu.Unlock()

	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PF_WI_ID="+p.WorkItemID, "PF_DAEMON_ID=m-test")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// fidelity with the production spawner: the identity journal runs
	// before Spawn answers (its failure tears the fresh process down).
	if p.Journal != nil {
		startedAt := nowStamp()
		if jerr := p.Journal(cmd.Process.Pid, startedAt); jerr != nil {
			_ = killGroup(cmd.Process.Pid, cleanupBound)
			return nil, fmt.Errorf("scripted spawn journal failed: %w", jerr)
		}
	}
	return newProc(cmd, stdin, stdout), nil
}

func (s *scriptedSpawner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptedSpawner) call(i int) SpawnParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

// ---------------------------------------------------------------------------
// builders

func setGitIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "pf-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "pf-test@invalid")
	t.Setenv("GIT_COMMITTER_NAME", "pf-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "pf-test@invalid")
}

// mappingFor builds the tests' one-workspace plan: the given workspace
// mapping the named projects (default "proj" — the fakes' project).
func mappingFor(ws string, projects ...string) *mapping.Plan {
	if len(projects) == 0 {
		projects = []string{"proj"}
	}
	p, err := mapping.Build([]mapping.Entry{{Workspace: ws, Projects: projects}})
	if err != nil {
		panic(err)
	}
	return p
}

// testWorkspace answers the tests' single mapped workspace (project
// "proj") — the unseal/quarantine probes' base.
func testWorkspace(r *Runtime) string {
	ws, err := r.cfg.Mapping.WorkspaceFor("proj")
	if err != nil {
		panic(err)
	}
	return ws
}

// testCapsuleDir derives the capsule dir of wi under the test runtime's
// mapping (the tests' single project "proj") — the unit replacement for
// the deleted single-workspace capsuleDir.
func testCapsuleDir(r *Runtime, wi string) string {
	ws, err := r.cfg.Mapping.WorkspaceFor("proj")
	if err != nil {
		panic(err)
	}
	return filepath.Join(ws, "capsules", wi)
}

func newTestRuntime(t *testing.T, hub Aihub, sp Spawner) (*Runtime, *supervise.Queue[protocol.RTMsg], *supervise.Queue[protocol.OBMsg]) {
	t.Helper()
	cfg := Config{
		MachineID: "m-test",
		Mapping:   mappingFor(filepath.Join(t.TempDir(), "ws")),
		DataDir:   filepath.Join(t.TempDir(), "data"),
		// polyforge#106: the driver is the only engine — the rig scripts it
		// through the single engine seam (a derivation is a Spawn call).
		AgentStarter:  scriptedEngineStarter(sp),
		MaxRecoveries: 2,
		// Test episodes are separated by handleOB calls, not real time:
		// the entry-rate storm guard's window is collapsed to nothing so
		// every recovery models a fresh episode (the storm tests override
		// this with a real window to drive the guard itself).
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := supervise.NewQueue[protocol.RTMsg]()
	fromOB := supervise.NewQueue[protocol.OBMsg]()
	r := New(cfg, hub, sp, toOB, fromOB)
	return r, toOB, fromOB
}

// fakeSession mints a deterministic per-WI session SECRET for tests
// (64-hex, the real shape — sha256 of the wi id).
func fakeSession(wi string) string {
	sum := sha256.Sum256([]byte("test-session:" + wi))
	return hex.EncodeToString(sum[:])
}

// scriptedEngineStarter binds the runtime's single engine seam to the
// test's spawner (polyforge#106: the driver is the only engine, so the
// in-process rig scripts it — a derivation is one Spawn call, the same
// shape the daemon tests' fake engine uses).
func scriptedEngineStarter(sp Spawner) AgentStartFunc {
	return func(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
		p := SpawnParams{
			WorkItemID:    in.WI,
			CapsuleDir:    in.CapsuleDir,
			SessionHandle: in.SessionHandle,
			SpawnN:        int(in.SpawnN),
			InitialPrompt: "engine",
		}
		h, err := sp.Spawn(ctx, p)
		if err != nil {
			return nil, err
		}
		// Like the real engine, journal the derivation: the capsule spawn
		// journal is the B2 fence ledger every derivation appends to.
		token := fmt.Sprintf("%s#%d", in.SessionHandle, in.SpawnN-1)
		if jerr := writeSpawnJournal(p, h.PID(), h.StartedAt(), token); jerr != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: scripted engine journal: %v\n", in.WI, jerr)
		}
		return h, nil
	}
}

func seedJob(r *Runtime, wi string) *job {
	capDir := testCapsuleDir(r, wi)
	_ = os.MkdirAll(capDir, 0o755)
	sess := fakeSession(wi)
	fp := api.SessionFingerprint(sess)
	j := &job{
		wi:                 wi,
		project:            "proj",
		sessionID:          sess,
		sessionFingerprint: fp,
		state:              StateRunning,
		capsuleDir:         capDir,
		git:                gitops.Opts{WorkItemID: wi, Fingerprint: fp, Username: wi, Secret: sess, CapsuleDir: capDir, GitDir: filepath.Join(r.cfg.DataDir, "git", wi+".git")},
		transcript:         sessionPathFor(capDir, fp),
		bundleName:         "demo-bundle",
		spawnN:             1,
	}
	r.mu.Lock()
	r.jobs[wi] = j
	r.mu.Unlock()
	return j
}

func installOldAgent(t *testing.T, sp Spawner, j *job) AgentHandle {
	t.Helper()
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:     j.wi,
		CapsuleDir:     j.capsuleDir,
		SessionHandle:  j.sessionFingerprint,
		SpawnN:         j.spawnN + 1,
		TranscriptPath: j.transcript,
		InitialPrompt:  "initial",
	})
	if err != nil {
		t.Fatalf("installOldAgent: %v", err)
	}
	j.mu.Lock()
	j.agent = h
	j.agentPID = h.PID()
	j.agentStartedAt = h.StartedAt()
	j.mu.Unlock()
	return h
}

func writeTranscript(t *testing.T, j *job) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(j.transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.transcript, []byte("{\"type\":\"message_end\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initGitCapsule gives j a real capsule worktree with an external git-dir
// and a local bare repo as its push remote, so CommitPush succeeds. The
// bare advertises push options: every push from the gitops choke point
// carries --push-option=attempt-id=… (§六 备层).
func initGitCapsule(t *testing.T, j *job) string {
	t.Helper()
	setGitIdentity(t)
	bare := filepath.Join(t.TempDir(), "hub.git")
	if out, err := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", bare, "config", "receive.advertisePushOptions", "true").CombinedOutput(); err != nil {
		t.Fatalf("git config advertisePushOptions: %v: %s", err, out)
	}
	if err := os.MkdirAll(j.git.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	j.git.RemoteURL = bare
	j.git.Username = j.wi
	j.git.Secret = "push-secret"
	if err := gitops.InitLocal(j.git); err != nil {
		t.Fatalf("gitops.InitLocal: %v", err)
	}
	if err := gitops.ExternalizeGitDir(j.git); err != nil {
		t.Fatalf("gitops.ExternalizeGitDir: %v", err)
	}
	// one initial commit so rev-list/push have a HEAD to stand on
	if err := os.WriteFile(filepath.Join(j.capsuleDir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "init")
	cmd.Env = j.git.Env()
	cmd.Dir = j.capsuleDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initial commit: %v: %s", err, out)
	}
	return bare
}

func bareHasMain(t *testing.T, bare string) bool {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", "refs/heads/main").Output()
	return err == nil && len(out) > 0
}

// ---------------------------------------------------------------------------
// assertions

func recvRT(t *testing.T, q *supervise.Queue[protocol.RTMsg]) protocol.RTMsg {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if m, ok := q.Pop(); ok {
			return m
		}
		select {
		case <-q.Notify():
		case <-deadline:
			t.Fatal("timed out waiting for an RT message")
			return protocol.RTMsg{}
		}
	}
}

func assertNoRT(t *testing.T, q *supervise.Queue[protocol.RTMsg]) {
	t.Helper()
	if m, ok := q.Pop(); ok {
		t.Fatalf("unexpected RT message: %+v", m)
	}
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for procExists(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func mustBeAlive(t *testing.T, pid int) {
	t.Helper()
	if !procExists(pid) {
		t.Fatalf("pid %d unexpectedly dead", pid)
	}
}

// unseal re-writable bits so t.TempDir cleanup does not fight a sealed
// scene.
func unseal(t *testing.T, root string) {
	t.Helper()
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(p, 0o755)
		} else if info.Mode().IsRegular() {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
}

// spawnSleeper starts a plain sleeper as its own group leader (with the
// WI's env marker) without going through the scripted spawner, so restart
// tests can count real derivations.
func spawnSleeper(t *testing.T, wi string) (pid int, startedAt string) {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PF_WI_ID="+wi, "PF_DAEMON_ID=m-test")
	startedAt = nowStamp()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, startedAt
}

func writeStateFile(t *testing.T, r *Runtime, recs []*jobRecord) {
	t.Helper()
	blob, err := json.MarshalIndent(&stateFile{Version: 1, MachineID: r.cfg.MachineID, Jobs: recs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(r.statePath, blob, stateFilePerm); err != nil {
		t.Fatal(err)
	}
}

func loadRecords(t *testing.T, r *Runtime) []*jobRecord {
	t.Helper()
	st, err := loadState(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	return st.Jobs
}

var errLookupBoom = fmt.Errorf("aihub unreachable (scripted)")

func timeAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(5 * time.Second)
		close(ch)
	}()
	return ch
}

// orphaningAgent is an "agent" whose leader dies immediately after forking
// a long-lived child: the child survives in the leader's process group as
// the orphan the six-layer cleanup must still collect.
type orphaningAgent struct {
	handle    AgentHandle
	orphanPID int
}

func spawnOrphaningSleeper(wi string) (*orphaningAgent, error) {
	dir, err := os.MkdirTemp("", "pf-orphan-")
	if err != nil {
		return nil, err
	}
	pidFile := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $! > "+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PF_WI_ID="+wi, "PF_DAEMON_ID=m-test")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := newProc(cmd, stdin, stdout)
	// the leader exits on its own; wait for it and read the orphan's pid
	deadline := time.Now().Add(5 * time.Second)
	for !h.Done() {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("orphaning leader did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return nil, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	return &orphaningAgent{handle: h, orphanPID: pid}, nil
}

// writeSpawnJournal appends one journal line in the driver adapter's
// shape (the production writer lives in the driver; tests only need the
// ledger it leaves behind).
func writeSpawnJournal(p SpawnParams, pid int, startedAt string, spawnToken string) error {
	blob, err := json.Marshal(map[string]any{
		"at":             nowStamp(),
		"work_item":      p.WorkItemID,
		"spawn_token":    spawnToken,
		"pid":            pid,
		"started_at":     startedAt,
		"session_handle": p.SessionHandle,
		"spawn_n":        p.SpawnN,
		"resume":         p.Resume,
		"transcript":     p.TranscriptPath,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(p.CapsuleDir, capsule.DirRuntimeState, capsule.SpawnJournalFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(blob, '\n'))
	return err
}

func newProc(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, taps ...func(string)) *proc {
	return newProcOpts(cmd, stdin, stdout, procOpts{taps: taps})
}
