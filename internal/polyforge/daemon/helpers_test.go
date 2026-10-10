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
	"github.com/silgrid/polyforge/internal/polyforge/mapping"
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
// fake aihub (v1.5 session model): the daemon-facing subset of the hub —
// session claim/verify/attach/end plus the queue and the generic Do the
// memory lanes proxy through. `held` maps wi → session secret; an UNHELD
// running WI answers verify matches=true for ANY secret (the reconciling
// daemon's own record), which is exactly what the seeded-record restart
// tests need; a HELD wi answers matches only for its own secret.

type fakeHub struct {
	mu        sync.Mutex
	status    map[string]string
	held      map[string]string
	claims    []api.SessionClaimRequest
	takeovers []api.SessionClaimRequest
	ends      []endCall
	attaches  []api.SessionAttachRequest
	// verifies records the wi of every SessionVerify call (the M4
	// revalidation seam the -revalidate-every flag test pins).
	verifies []string
	// resumeBlockeds/terminateBlockeds record the wi of every §6.4 owner
	// door call (polyforge#116): the resume door (resume-blocked route)
	// and the terminate door (wrap --fail on a parked workflow).
	resumeBlockeds    []string
	terminateBlockeds []string
	queues            []api.WorkItem
	blockEnd          chan struct{} // non-nil: SessionEnd parks until closed
	// canonical maps an id-or-slug form ("proj#7") to the canonical id
	// (F2 route tests); an unmapped input echoes itself.
	canonical map[string]string
	// memDo, when non-nil, serves the generic Do calls the F6 memory
	// adapters make (search + memory create); nil answers a typed
	// refusal. The tests that exercise the memory lanes script it.
	memDo func(method, path string, body, out any) error
	// queueErr, when non-nil, is what Queue answers instead of the
	// seeded list — the drain degraded-signal test scripts denials with
	// it.
	queueErr error
	// heartbeats/heartbeatErr are the M3 liveness seam: heartbeats
	// records every Heartbeat payload the daemon POSTed; heartbeatErr,
	// when non-nil, is what Heartbeat answers instead.
	// when non-nil, fails every call (the backoff test's fault
	// injector); heartbeatAttempts counts every call including failures
	// (the disabled-mode test asserts on attempts, not successes). All
	// guarded by mu.
	heartbeats        []aihubclient.MachineHeartbeat
	heartbeatErr      error
	heartbeatAttempts int
	// shipLedgerCalls/shipLedgers are the ship-ledger seam
	// (polyforge#123, P1b): every RecordShipLedger call lands here in
	// call order; ledgerErr, when non-nil, fails every call (the typed
	// ship-failure test's fault injector).
	shipLedgerCalls []string
	shipLedgers     map[string][]api.ShipLedgerEntry
	ledgerErr       error
}

type endCall struct {
	WI  string
	Req api.SessionEndRequest
}

func newFakeHub() *fakeHub {
	return &fakeHub{status: map[string]string{}, held: map[string]string{}}
}

func (h *fakeHub) canonicalID(in string) string {
	if c, ok := h.canonical[in]; ok {
		return c
	}
	return in
}

func (h *fakeHub) SessionClaim(req api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.claims = append(h.claims, req)
	id := h.canonicalID(req.WorkItemID)
	h.held[id] = req.SessionID
	h.status[id] = api.StatusRunning
	return &api.SessionClaimResponse{
		Claim: api.SessionClaim{
			WorkItemID:         id,
			SessionFingerprint: api.SessionFingerprint(req.SessionID),
			BundleName:         "demo-bundle",
			BundleDigest:       "deadbeef",
		},
		WorkItem: api.WorkItem{ID: id, ProjectName: "proj"},
	}, nil
}

func (h *fakeHub) ForceTakeover(wi string, req api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	h.mu.Lock()
	h.takeovers = append(h.takeovers, req)
	h.mu.Unlock()
	return h.SessionClaim(req)
}

func (h *fakeHub) SessionAttach(wi string, req api.SessionAttachRequest) (*api.SessionClaimResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attaches = append(h.attaches, req)
	id := h.canonicalID(wi)
	h.status[id] = api.StatusRunning
	return &api.SessionClaimResponse{
		Claim:    api.SessionClaim{WorkItemID: id, SessionFingerprint: api.SessionFingerprint(req.SessionID)},
		WorkItem: api.WorkItem{ID: id, ProjectName: "proj"},
	}, nil
}

func (h *fakeHub) SessionVerify(wi, sessionID string) (*api.SessionVerifyResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.verifies = append(h.verifies, h.canonicalID(wi))
	id := h.canonicalID(wi)
	st := h.status[id]
	if st == "" {
		st = api.StatusRunning
	}
	out := &api.SessionVerifyResponse{WorkItemID: id, Status: st}
	if cur := h.held[id]; cur != "" {
		if cur == sessionID {
			out.Matches = true
		} else {
			out.CurrentFingerprint = api.SessionFingerprint(cur)
		}
	} else if st == api.StatusRunning && sessionID != "" {
		// unheld-but-running: the reconciling record's own session (the
		// seeded-restart tests' shape)
		out.Matches = true
	}
	return out, nil
}

func (h *fakeHub) SessionEnd(wi string, req api.SessionEndRequest) error {
	h.mu.Lock()
	block := h.blockEnd
	h.mu.Unlock()
	if block != nil {
		<-block
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(wi)
	h.ends = append(h.ends, endCall{WI: id, Req: req})
	switch req.Status {
	case api.StatusWrapped, api.StatusFailed, api.StatusBlocked:
		h.held[id] = ""
	}
	h.status[id] = req.Status
	return nil
}

func (h *fakeHub) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(idOrSlug)
	st := h.status[id]
	if st == "" {
		st = api.StatusRunning
	}
	return &api.WorkItem{ID: id, Status: st, ProjectName: "proj"}, nil
}

// ResumeBlockedWorkItem / TerminateBlockedWorkItem are the §6.4 owner
// doors (polyforge#116): the resume door returns the WI to claimable
// (queued, session cleared), the terminate door lands failed. Both
// record the transition in the status map; the returned view is unused
// by the routes (only the error matters).
func (h *fakeHub) ResumeBlockedWorkItem(idOrSlug string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(idOrSlug)
	h.status[id] = api.StatusQueued
	delete(h.held, id)
	h.resumeBlockeds = append(h.resumeBlockeds, id)
	return json.RawMessage(`{"id":"` + id + `","status":"` + api.StatusQueued + `"}`), nil
}

func (h *fakeHub) TerminateBlockedWorkItem(idOrSlug string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.canonicalID(idOrSlug)
	h.status[id] = api.StatusFailed
	delete(h.held, id)
	h.terminateBlockeds = append(h.terminateBlockeds, id)
	return json.RawMessage(`{"id":"` + id + `","status":"` + api.StatusFailed + `"}`), nil
}

func (h *fakeHub) Queue(project string) ([]api.WorkItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.queueErr != nil {
		return nil, h.queueErr
	}
	out := make([]api.WorkItem, 0, len(h.queues))
	out = append(out, h.queues...)
	return out, nil
}

// Heartbeat records the M3 liveness POSTs; heartbeats carries the received
// payloads (inspection seam for the heartbeat tests) and heartbeatErr, if
// set, fails every call (the backoff test's fault injector).
func (h *fakeHub) Heartbeat(req aihubclient.MachineHeartbeat) (*aihubclient.MachineHeartbeatResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heartbeatAttempts++
	if h.heartbeatErr != nil {
		return nil, h.heartbeatErr
	}
	h.heartbeats = append(h.heartbeats, req)
	return &aihubclient.MachineHeartbeatResult{MachineID: req.MachineID, UserID: "mac_test"}, nil
}

func (h *fakeHub) setHeartbeatErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heartbeatErr = err
}

func (h *fakeHub) heartbeatsSnapshot() []aihubclient.MachineHeartbeat {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]aihubclient.MachineHeartbeat, len(h.heartbeats))
	copy(out, h.heartbeats)
	return out
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

// RecordShipLedger records the ship bridge's ledger calls (polyforge#123,
// P1b seam): every (wi, entries) lands in shipLedgers in call order;
// ledgerErr, when non-nil, is what the call answers instead (the typed
// ship-failure test scripts it).
func (h *fakeHub) RecordShipLedger(wi string, req api.ShipLedgerRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ledgerErr != nil {
		return h.ledgerErr
	}
	if h.shipLedgers == nil {
		h.shipLedgers = map[string][]api.ShipLedgerEntry{}
	}
	h.shipLedgerCalls = append(h.shipLedgerCalls, wi)
	h.shipLedgers[wi] = append(h.shipLedgers[wi], req.Repos...)
	return nil
}

func (h *fakeHub) verifyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.verifies)
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
	return h.ends[len(h.ends)-1].Req
}

func (h *fakeHub) lastEndWI() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ends) == 0 {
		return ""
	}
	return h.ends[len(h.ends)-1].WI
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

// fakeEngineStarter binds a scripted execution engine: the driver is
// the only engine (polyforge#106) and the config knob only swaps the
// starter, so the in-process tests script the engine through it. The
// starter delegates to the job's own spawner (the config knob), so the
// fake-agent handles and spawn-call assertions keep working — including
// for tests that swap cfg.Spawner for a wrapping variant.
func fakeEngineStarter() func(ctx context.Context, in runtime.AgentStartInput) (runtime.AgentHandle, error) {
	return func(ctx context.Context, in runtime.AgentStartInput) (runtime.AgentHandle, error) {
		return in.Spawner.Spawn(ctx, runtime.SpawnParams{
			WorkItemID:    in.WI,
			CapsuleDir:    in.CapsuleDir,
			SessionHandle: in.SessionHandle,
			SpawnN:        int(in.SpawnN),
			InitialPrompt: "engine",
		})
	}
}

// mappingFor builds the daemon tests' one-workspace plan: ws mapping the
// fakes' project "proj" (polyforge#113 — the Config carries the mapping,
// never a single workspace or project scope).
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
		MachineID:  "m-test",
		SocketPath: filepath.Join(dir, "s.sock"),
		// The driver is the only engine (polyforge#106); these tests bind
		// a FAKE engine starter that delegates to the fake spawner so the
		// scripted WI-Agent semantics (spawn call params, agent handles)
		// stay assertable without a real driver.
		Mapping:           mappingFor(ws),
		DataDir:           data,
		GitRemoteBase:     hubBase,
		MaxConcurrent:     4,
		Hub:               hub,
		Spawner:           sp,
		AgentStarter:      fakeEngineStarter(),
		ObservePoll:       10 * time.Millisecond,
		DrainInterval:     10 * time.Millisecond,
		DispatchTick:      10 * time.Millisecond,
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
		_ = d.Shutdown(context.Background(), DirectShutdownCause())
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
