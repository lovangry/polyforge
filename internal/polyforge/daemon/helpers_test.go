package daemon

// fakes + shared scaffolding for the daemon package tests: an in-process
// fake agent handle (the runtime lane's own tests exercise REAL process
// death/kill semantics; here the control paths are the subject), a fake
// aihub, and builders for a daemon under test with real UDS + real git.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// ---------------------------------------------------------------------------
// fake agent handle

type fakeAgent struct {
	mu         sync.Mutex
	done       bool
	terminated bool
	sent       []protocol.ControlMsg
	ackFn      func(method string) error
}

func (a *fakeAgent) PID() int          { return 0 } // runtime's kill guard no-ops on pid<=0
func (a *fakeAgent) StartedAt() string { return "" }
func (a *fakeAgent) Alive() bool       { return !a.isDone() }
func (a *fakeAgent) Done() bool        { return a.isDone() }
func (a *fakeAgent) isDone() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done
}
func (a *fakeAgent) Terminate(time.Duration) error {
	a.mu.Lock()
	a.done = true
	a.terminated = true
	a.mu.Unlock()
	return nil
}
func (a *fakeAgent) SendControl(m protocol.ControlMsg) error {
	a.mu.Lock()
	a.sent = append(a.sent, m)
	a.mu.Unlock()
	return nil
}
func (a *fakeAgent) WaitControlAck(ctx context.Context, m protocol.ControlMsg, bound time.Duration) error {
	a.mu.Lock()
	fn := a.ackFn
	a.mu.Unlock()
	if fn != nil {
		return fn(m.Method)
	}
	return nil
}

func (a *fakeAgent) sentMethods() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.sent))
	for _, m := range a.sent {
		out = append(out, m.Method)
	}
	return out
}

func (a *fakeAgent) sentAnswer(id, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.sent {
		if m.Method == protocol.ControlAnswer && m.ID == id && m.Answer == text {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// fake spawner

type fakeSpawner struct {
	mu      sync.Mutex
	calls   []runtime.SpawnParams
	spawned []*fakeAgent // one entry per Spawn, in order
}

func (s *fakeSpawner) Spawn(ctx context.Context, p runtime.SpawnParams) (runtime.AgentHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, p)
	a := &fakeAgent{}
	s.spawned = append(s.spawned, a)
	return a, nil
}

func (s *fakeSpawner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// agentOf returns the fake agent of the n-th spawn (0-based).
func (s *fakeSpawner) agentOf(n int) *fakeAgent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.spawned) {
		return nil
	}
	return s.spawned[n]
}

func (s *fakeSpawner) lastParams() runtime.SpawnParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return runtime.SpawnParams{}
	}
	return s.calls[len(s.calls)-1]
}

// ---------------------------------------------------------------------------
// fake aihub

type fakeHub struct {
	mu       sync.Mutex
	status   map[string]string
	claims   []aihubclient.ClaimRequest
	ends     []endCall
	queues   []api.WorkItem
	blockEnd chan struct{} // non-nil: EndGeneration parks until closed
	gen      int64
	// canonical maps an id-or-slug form ("proj#7") to the canonical id
	// (F2 route tests); an unmapped input echoes itself.
	canonical map[string]string
	// memDo, when non-nil, serves the generic Do calls the F6 memory
	// adapters make (search + memory create); nil answers a typed
	// refusal. The tests that exercise the memory lanes script it.
	memDo func(method, path string, body, out any) error
}

type endCall struct {
	WI  string
	Req aihubclient.EndGenerationRequest
}

func newFakeHub() *fakeHub {
	return &fakeHub{status: map[string]string{}, gen: 1}
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
	block := h.blockEnd
	h.mu.Unlock()
	if block != nil {
		<-block
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends = append(h.ends, endCall{WI: wi, Req: req})
	return nil
}

func (h *fakeHub) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := idOrSlug
	if c, ok := h.canonical[idOrSlug]; ok {
		id = c
	}
	st := h.status[id]
	if st == "" {
		st = api.StatusRunning
	}
	return &api.WorkItem{ID: id, Status: st, ProjectName: "proj"}, nil
}

func (h *fakeHub) Queue(project string) ([]api.WorkItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]api.WorkItem, 0, len(h.queues))
	out = append(out, h.queues...)
	return out, nil
}

// Do serves the generic typed-request face the F6 memory adapters use.
func (h *fakeHub) Do(method, path string, body, out any) error {
	h.mu.Lock()
	fn := h.memDo
	h.mu.Unlock()
	if fn == nil {
		return &api.Error{Code: api.ErrDaemonInternal, Detail: "fakeHub: Do call not scripted"}
	}
	return fn(method, path, body, out)
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
	return h.ends[len(h.ends)-1].Req
}

// ---------------------------------------------------------------------------
// git scaffolding: a bare "aihub" remote per work item + a seeded capsule

func setGitIdentity(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "pf-test"},
		{"GIT_AUTHOR_EMAIL", "pf-test@invalid"},
		{"GIT_COMMITTER_NAME", "pf-test"},
		{"GIT_COMMITTER_EMAIL", "pf-test@invalid"},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

// mkBare creates the aihub-side bare repo for wi so runtime's clone and
// every push succeed. Push options are advertised: every push from the
// gitops choke point carries --push-option=attempt-id=… (§六 备层).
func mkBare(t *testing.T, base, wi string) string {
	t.Helper()
	dir := filepath.Join(base, wi+".git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", dir, "config", "receive.advertisePushOptions", "true").CombinedOutput(); err != nil {
		t.Fatalf("git config advertisePushOptions: %v: %s", err, out)
	}
	return dir
}

// seedCapsuleGit pre-materializes a capsule worktree + external git-dir
// + transcript for a runtime state.json record (the restart test's input).
func seedCapsuleGit(t *testing.T, capsuleDir, gitDir, remote string) {
	t.Helper()
	if err := os.MkdirAll(capsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", capsuleDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(capsuleDir, ".git"), gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capsuleDir, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+capsuleDir)
	for _, args := range [][]string{
		{"checkout", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		cmd.Dir = capsuleDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// seedStateFile writes a runtime state.json record (the runtime lane's
// own format, reconstructed field-for-field for the restart test).
func seedStateFile(t *testing.T, dataDir, machineID string, rec map[string]any) {
	t.Helper()
	blob, err := json.MarshalIndent(map[string]any{
		"version":    1,
		"machine_id": machineID,
		"jobs":       []map[string]any{rec},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "runtime-state.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// daemon under test

type testDaemon struct {
	*Daemon
	hub     *fakeHub
	spawner *fakeSpawner
	client  *cli.DaemonClient
	sock    string
	runDone chan error
}

func newTestDaemon(t *testing.T, mutate func(*Config)) *testDaemon {
	t.Helper()
	setGitIdentity(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	data := filepath.Join(dir, "data")
	hubBase := filepath.Join(dir, "hub")
	for _, d := range []string{ws, data, hubBase} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hub := newFakeHub()
	sp := &fakeSpawner{}
	cfg := Config{
		MachineID:         "m-test",
		SocketPath:        filepath.Join(dir, "s.sock"),
		WorkspaceRoot:     ws,
		DataDir:           data,
		Project:           "proj",
		GitRemoteBase:     hubBase,
		MaxConcurrent:     4,
		Hub:               hub,
		Spawner:           sp,
		ObservePoll:       10 * time.Millisecond,
		DrainInterval:     10 * time.Millisecond,
		DispatchTick:      10 * time.Millisecond,
		NoticeAckBound:    100 * time.Millisecond,
		ShutdownFlushWait: 100 * time.Millisecond,
		ExitPoll:          10 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	td := &testDaemon{
		Daemon:  d,
		hub:     hub,
		spawner: sp,
		sock:    cfg.SocketPath,
		runDone: make(chan error, 1),
	}
	go func() { td.runDone <- d.Run(context.Background()) }()
	// wait for the socket to be live
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(td.sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	td.client = cli.NewDaemonClient(td.sock)
	t.Cleanup(func() {
		_ = d.Shutdown(context.Background())
		select {
		case <-td.runDone:
		case <-time.After(5 * time.Second):
		}
	})
	return td
}

// submitClaim submits one work item and waits for its job to go live.
func (td *testDaemon) submitClaim(t *testing.T, wi string) {
	t.Helper()
	mkBare(t, td.cfg.GitRemoteBase, wi)
	raw, err := td.client.Claim(wi, "normal")
	if err != nil {
		t.Fatalf("claim %s: %v", wi, err)
	}
	var sr cli.SubmitResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("submit answer undecodable: %v", err)
	}
	if !sr.Accepted || sr.WorkItemID != wi {
		t.Fatalf("submit answer = %+v, want accepted for %s", sr, wi)
	}
	td.waitLive(t, wi)
}

// waitLive blocks until the work item holds a runtime job.
func (td *testDaemon) waitLive(t *testing.T, wi string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := td.client.Status()
		if err == nil {
			var st cli.StatusResponse
			if json.Unmarshal(raw, &st) == nil {
				for _, j := range st.Jobs {
					if j.WorkItemID == wi {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never went live", wi)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitMonitored blocks until the observe route answers for the work item
// (the per-WI monitor is attached).
func (td *testDaemon) waitMonitored(t *testing.T, wi string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := td.client.Observe(wi, 0); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became monitored", wi)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
