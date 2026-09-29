package runtime

// Shared test scaffolding: a scripted aihub, a scripted spawner that spawns
// REAL (trivial) child processes so death/kill semantics are exercised
// honestly, and small builders for jobs and capsule git repos.

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// ---------------------------------------------------------------------------
// fake aihub

type fakeHub struct {
	mu        sync.Mutex
	status    map[string]string
	lookupErr map[string]error
	claims    []aihubclient.ClaimRequest
	ends      []aihubclient.EndGenerationRequest
	gen       int64
}

func newFakeHub() *fakeHub {
	return &fakeHub{status: map[string]string{}, lookupErr: map[string]error{}, gen: 1}
}

func (h *fakeHub) Claim(req aihubclient.ClaimRequest) (*aihubclient.ClaimResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.claims = append(h.claims, req)
	h.gen++
	return &aihubclient.ClaimResponse{
		Claim: api.Claim{
			WorkItemID:   req.WorkItemID,
			AttemptID:    "ra_" + req.WorkItemID,
			Generation:   h.gen,
			BundleName:   "demo-bundle",
			BundleDigest: "deadbeef",
		},
		Credential: api.PushCredential{AttemptID: "ra_" + req.WorkItemID, Username: req.WorkItemID, Secret: "push-secret"},
		WorkItem:   api.WorkItem{ID: req.WorkItemID, ProjectName: "proj"},
	}, nil
}

func (h *fakeHub) EndGeneration(wi string, req aihubclient.EndGenerationRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err, ok := h.lookupErr[wi]; ok {
		return err
	}
	h.ends = append(h.ends, req)
	return nil
}

func (h *fakeHub) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err, ok := h.lookupErr[idOrSlug]; ok {
		return nil, err
	}
	st := h.status[idOrSlug]
	if st == "" {
		st = api.StatusRunning
	}
	return &api.WorkItem{ID: idOrSlug, Status: st, ProjectName: "proj"}, nil
}

func (h *fakeHub) endCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ends)
}

func (h *fakeHub) lastEnd() aihubclient.EndGenerationRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ends) == 0 {
		return aihubclient.EndGenerationRequest{}
	}
	return h.ends[len(h.ends)-1]
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
	cmd.Env = append(os.Environ(), "PF_WI_ID="+p.WorkItemID)
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

func newTestRuntime(t *testing.T, hub Aihub, sp Spawner) (*Runtime, chan protocol.RTMsg, chan protocol.OBMsg) {
	t.Helper()
	cfg := Config{
		MachineID:      "m-test",
		WorkspaceRoot:  filepath.Join(t.TempDir(), "ws"),
		DataDir:        filepath.Join(t.TempDir(), "data"),
		MaxResumeFails: 2,
		MaxRecoveries:  2,
		SendBound:      2 * time.Second,
	}
	toOB := make(chan protocol.RTMsg, 64)
	fromOB := make(chan protocol.OBMsg, 64)
	r := New(cfg, hub, sp, toOB, fromOB)
	return r, toOB, fromOB
}

func seedJob(r *Runtime, wi string) *job {
	capDir := filepath.Join(r.cfg.WorkspaceRoot, "capsules", wi)
	_ = os.MkdirAll(capDir, 0o755)
	j := &job{
		wi:         wi,
		project:    "proj",
		attempt:    "ra_" + wi,
		instance:   "ra_" + wi,
		generation: 1,
		state:      StateRunning,
		capsuleDir: capDir,
		git:        gitops.Opts{WorkItemID: wi, AttemptID: "ra_" + wi, CapsuleDir: capDir, GitDir: filepath.Join(r.cfg.DataDir, "git", wi+".git")},
		sessionID:  "sess_" + wi + "_1",
		transcript: filepath.Join(capDir, "session", "session-ra_"+wi+".jsonl"),
		bundleName: "demo-bundle",
		spawnN:     1,
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
		SessionID:      j.sessionID,
		TranscriptPath: j.transcript,
		Generation:     j.generation,
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

func recvRT(t *testing.T, ch chan protocol.RTMsg) protocol.RTMsg {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an RT message")
		return protocol.RTMsg{}
	}
}

func assertNoRT(t *testing.T, ch chan protocol.RTMsg) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected RT message: %+v", m)
	default:
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
	cmd.Env = append(os.Environ(), "PF_WI_ID="+wi)
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

func writeBytes(p string, b []byte) error {
	if b == nil {
		b = []byte{}
	}
	return os.WriteFile(p, b, 0o644)
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
	cmd.Env = append(os.Environ(), "PF_WI_ID="+wi)
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
	pid, err := strconvAtoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	return &orphaningAgent{handle: h, orphanPID: pid}, nil
}

func strconvAtoi(s string) (int, error) { return strconv.Atoi(s) }

// osCommand / osCmd / osReadDir: thin aliases keeping the cleanup tests'
// focus on the layer logic.
type osCmd = exec.Cmd

func osCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
func osReadDir(p string) ([]os.DirEntry, error)       { return os.ReadDir(p) }
