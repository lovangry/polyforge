package daemon

// Integration tests against the REAL internal/server: the full 04 §6.1
// claim→capsule→WI-Agent sequence, the 04 §6.5 graceful-shutdown sequence
// with a real final flush + end-generation pause accepted by the Core's
// flush-evidence validator, and the 04 §5.1 operations face over a real
// Unix domain socket (including the SO_PEERCRED proxy refusal).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/server"
)

// integAgent is a fully controllable fake WI Agent: it acknowledges
// control methods and can be killed to simulate a crash.
type integAgent struct {
	pid       int
	startedAt string
	quiesced  chan struct{}
	stopped   chan struct{}
	dead      bool
	child     *exec.Cmd
	answers   []AnswerRelay
	// onAnswer mirrors the real WI Agent's consumption contract (03 §4.2
	// D9): the resume-write archives the answer and clears pending_question
	// in the SAME atomic write.
	onAnswer func(AnswerRelay)
	// onNotifyStop, when set, replaces the channel push in NotifyStop —
	// used to park a close at its boundary wait for race tests.
	onNotifyStop func()
}

func (a *integAgent) PID() int          { return a.pid }
func (a *integAgent) StartedAt() string { return a.startedAt }
func (a *integAgent) Alive() bool       { return !a.dead }
func (a *integAgent) NotifyStop(ctx context.Context) error {
	if a.onNotifyStop != nil {
		a.onNotifyStop()
		return nil
	}
	select {
	case a.stopped <- struct{}{}:
	default:
	}
	return nil
}
func (a *integAgent) Quiesce(ctx context.Context) error {
	select {
	case a.quiesced <- struct{}{}:
	default:
	}
	return nil
}
func (a *integAgent) DeliverAnswer(rel AnswerRelay) error {
	a.answers = append(a.answers, rel)
	if a.onAnswer != nil {
		a.onAnswer(rel)
	}
	return nil
}
func (a *integAgent) Terminate(timeout time.Duration) error {
	a.dead = true
	if a.child != nil && a.child.Process != nil {
		a.child.Process.Kill()
		a.child.Wait()
	}
	return nil
}

type integSpawner struct {
	mu     chan struct{}
	spawns []SpawnParams
	agents []*integAgent
}

func newIntegSpawner() *integSpawner {
	return &integSpawner{mu: make(chan struct{}, 1)}
}

func (s *integSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	s.spawns = append(s.spawns, p)
	// A real distinct child pid: the SO_PEERCRED guard must be able to
	// tell the agent apart from the (same-pid) test caller.
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		return nil, err
	}
	a := &integAgent{
		pid:       child.Process.Pid,
		startedAt: NowStamp(),
		quiesced:  make(chan struct{}, 1),
		stopped:   make(chan struct{}, 1),
		child:     child,
	}
	s.agents = append(s.agents, a)
	return a, nil
}

type denv struct {
	t         *testing.T
	ts        *serverURL
	db        *store.DB
	d         *Daemon
	spawner   *integSpawner
	wi        string
	dataDir   string
	socket    string
	api       *APIServer
	writerKey string
}

type serverURL struct{ url string }

func newDaemonEnv(t *testing.T) *denv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := service.Register(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.MakeDemoBundle(dir); err != nil {
		t.Fatal(err)
	}
	srv := server.New(db, dir, filepath.Join(dir, "workflows"), svc)
	ts := startTestServer(t, srv)

	// Machine key for the daemon.
	plain := "pfk_daemon_it"
	sum := sha256HexOf(plain)
	if _, err := db.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES ('key_dmn','usr_worker',NULL,?1)`, sum); err != nil {
		t.Fatal(err)
	}

	// Create the execution ticket.
	body := `{"project":"p1","goal":"daemon it","bundle_name":"demo","repo_inventory":[],"repo_inventory_base":[]}`
	req, _ := http.NewRequest("POST", ts.url+"/api/v1/work-items", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create wi: %d %s", resp.StatusCode, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &created)

	// Daemon under test, with a fake spawner and a real pf-execute file in
	// the "checkout".
	home := filepath.Join(dir, "home")
	os.MkdirAll(filepath.Join(home, "skills", "pf-execute"), 0o755)
	os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\nDrive the workflow.\n"), 0o644)
	os.MkdirAll(filepath.Join(home, "workflows"), 0o755)

	spawner := newIntegSpawner()
	cfg := Config{
		AIHubURL: ts.url, APIKey: plain, MachineID: "mac_a",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		DrainProject:  "p1",
		Adapter:       "pi",
		Version:       "it",
	}
	d, err := New(cfg, spawner)
	if err != nil {
		t.Fatal(err)
	}
	return &denv{
		t: t, ts: &serverURL{ts.url}, db: db, d: d, spawner: spawner,
		wi: created.ID, dataDir: dir,
		socket: filepath.Join(dir, "polyforge.sock"),
	}
}

func startTestServer(t *testing.T, h http.Handler) *serverURL {
	t.Helper()
	ts := startHTTPServer(h)
	t.Cleanup(ts.close)
	return &serverURL{url: ts.url}
}

type httpTS struct {
	url   string
	close func()
}

func startHTTPServer(h http.Handler) *httpTS {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	return &httpTS{url: "http://" + ln.Addr().String(), close: func() { srv.Close() }}
}

func sha256HexOf(s string) string {
	// Same convention as the server's authenticate(): hex(sha256(plaintext))
	// compared against api_keys.key_hash.
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestDaemonClaimSequenceAndShutdown: the whole 04 §6.1 walk against the
// real Core, then the 04 §6.5 graceful shutdown with a real final flush
// whose evidence the real Core validator accepts.
func TestDaemonClaimSequenceAndShutdown(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()

	// Submit + spawn (04 §6.1 steps 1–11 through the real claim path).
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	spawned, err := e.d.SpawnNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !spawned {
		t.Fatal("job must spawn into a free slot")
	}
	if len(e.spawner.spawns) != 1 {
		t.Fatal("exactly one persistent WI Agent must be derived (INV-14)")
	}
	p := e.spawner.spawns[0]
	// Capsule created with layout + harness dirs + session identity.
	capc, idn, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}
	if idn.Status != capsule.StatusReady || idn.Generation != 1 {
		t.Fatalf("capsule identity: %+v", idn)
	}
	if _, err := os.Stat(filepath.Join(capc.Dir, ".pi", "agents")); err != nil {
		t.Fatal(".pi/agents must exist from capsule creation")
	}
	sess, err := capc.ReadSessionIdentity("pi")
	if err != nil || sess.SessionID != p.SessionID || !strings.HasPrefix(p.SessionID, "sess_"+e.wi+"_1") {
		t.Fatalf("session identity: %+v %v (params %+v)", sess, err, p.SessionID)
	}
	// pf-execute injected as the initial instruction.
	if !strings.Contains(p.InitialPrompt, "# pf-execute") || !strings.Contains(p.InitialPrompt, "bundle_name: demo") {
		t.Fatal("initial instruction must carry the injected pf-execute + bundle reference")
	}
	// Daemon state recorded the job (04 §4.1) and no pending intent remains.
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st.FindJob(e.wi)
	if j == nil || j.MainAgentPID == 0 || j.HarnessSessionID != p.SessionID || len(st.PendingJobs) != 0 {
		t.Fatalf("state after spawn: %+v pending=%d", j, len(st.PendingJobs))
	}

	// Teach the fake agent the consumption contract: on answer delivery it
	// performs the same atomic write a real WI Agent would (archive the
	// answer, clear pending_question).
	e.spawner.agents[0].onAnswer = func(rel AnswerRelay) {
		if err := capc.MakeWritable(); err != nil {
			return
		}
		cur, lerr := capc.LoadExecution()
		if lerr != nil || cur == nil {
			return
		}
		step := cur.Steps[cur.CurrentStep]
		if step == nil {
			return
		}
		step.Answers = append(step.Answers, capsule.ArchivedAnswer{
			QuestionID: rel.QuestionID, AnswerText: rel.AnswerText,
			AnsweredAt: rel.AnsweredAt, AnsweredBy: rel.AnsweredBy,
		})
		step.PendingQuestion = nil
		cur.Version++
		capc.SaveExecution(cur)
	}

	// The WI Agent "works": writes execution.json, the daemon publishes on
	// its publish_request (04 §6.4 / 05 §6 full flow against real Core).
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	execJSON := `{"capsule_id":"` + capc.ID() + `","work_item_id":"` + e.wi + `","generation":1,"current_step":"code","workflow_status":"running","steps":{},"version":1}`
	if err := capc.SaveExecution(execOf(e.wi, 1, "code", execJSON)); err != nil {
		t.Fatal(err)
	}
	out, err := e.d.PublishRequest(ctx, e.wi, "step_completed")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Published || out.HeadSeq != 0 {
		t.Fatalf("publish_request: %+v", out)
	}

	// Graceful shutdown (04 §6.5): latch, boundary, final flush (with the
	// session transcript archive riding the last revision), pause.
	e.spawner.agents[0].dead = true // simulate the agent having exited at its boundary
	if err := e.d.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// The WI is paused on the real Core with evidence-validated pause.
	var status string
	if err := e.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, e.wi).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "paused" {
		t.Fatalf("wi status after shutdown-pause: %s", status)
	}
	// HEAD exists (the flush published) and daemon state is clean.
	st2, _ := NewStateStore(e.d.cfg.DataDir).Load()
	if len(st2.Jobs) != 0 {
		t.Fatalf("jobs must be cleared before exit: %+v", st2.Jobs)
	}
	// The capsule is sealed read-only.
	if fi, err := os.Stat(capc.Dir); err == nil && fi.Mode().Perm()&0o222 != 0 {
		t.Fatal("capsule must be sealed read-only after shutdown")
	}
}

// execOf builds a minimal execution state for the capsule write helper.
func execOf(wi string, gen int64, step, raw string) *capsule.Execution {
	// Reuse the raw JSON verbatim: decode into the struct so Save applies
	// its own stamps.
	var e capsule.Execution
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		panic(err)
	}
	return &e
}

// TestDaemonAPIServerFace: the 04 §5.1 operations face over a real UDS:
// status, observe ring, answer relay (403/409/idempotent), mode transitions,
// shutdown gating, and the SO_PEERCRED proxy refusal for a non-agent peer.
func TestDaemonAPIServerFace(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()

	c := &udsClient{path: e.socket}

	// status: bookkeeping only (no execution state — INV-D1).
	var st map[string]any
	if code, err := c.do("GET", "/status", nil, &st); err != nil || code != 200 {
		t.Fatalf("status: %d %v", code, err)
	}
	if st["mode"] != ModeInteractive {
		t.Fatalf("status mode: %v", st)
	}

	// answer against a step with an open question.
	capc, _, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	question := &capsule.PendingQuestion{QuestionID: "q_abc123", AskedAt: NowStamp(), Content: "Which design?"}
	exec := &capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: e.wi, Generation: 1,
		CurrentStep: "grill", WorkflowStatus: capsule.StatusRunning,
		Steps:   map[string]*capsule.Step{"grill": {Status: capsule.StepInProgress, PendingQuestion: question}},
		Version: 1,
	}
	if err := capc.SaveExecution(exec); err != nil {
		t.Fatal(err)
	}
	// Consumption contract on the fake agent: archive + clear, one write.
	e.spawner.agents[0].onAnswer = func(rel AnswerRelay) {
		if err := capc.MakeWritable(); err != nil {
			return
		}
		cur, lerr := capc.LoadExecution()
		if lerr != nil || cur == nil {
			return
		}
		step := cur.Steps[cur.CurrentStep]
		if step == nil {
			return
		}
		step.Answers = append(step.Answers, capsule.ArchivedAnswer{
			QuestionID: rel.QuestionID, AnswerText: rel.AnswerText,
			AnsweredAt: rel.AnsweredAt, AnsweredBy: rel.AnsweredBy,
		})
		step.PendingQuestion = nil
		cur.Version++
		capc.SaveExecution(cur)
	}
	relay := map[string]any{
		"question_id": "q_abc123", "answer_id": "a_1", "answer_text": "Option B",
		"answered_at": NowStamp(), "answered_by": "usr_worker",
	}
	var out map[string]any
	// Identity mismatch → DAEMON-403 (00 §3.4a): the authenticated caller
	// (X-PF-User) differs from answered_by.
	relayWrong := map[string]any{}
	for k, v := range relay {
		relayWrong[k] = v
	}
	relayWrong["answered_by"] = "usr_other"
	code, err := c.doAs("POST", "/jobs/"+e.wi+"/answer", relayWrong, &out, "usr_worker")
	if err != nil || code != 403 {
		t.Fatalf("identity mismatch: %d %v", code, err)
	}
	// First submission applies (caller identity matches answered_by).
	if code, err := c.doAs("POST", "/jobs/"+e.wi+"/answer", relay, &out, "usr_worker"); err != nil || code != 200 {
		t.Fatalf("answer: %d %v", code, err)
	}
	// Same answer_id replay: idempotent 200.
	if code, err := c.doAs("POST", "/jobs/"+e.wi+"/answer", relay, &out, "usr_worker"); err != nil || code != 200 {
		t.Fatalf("replay: %d %v", code, err)
	}
	// New answer_id against the consumed question → DAEMON-409.
	relay2 := map[string]any{}
	for k, v := range relay {
		relay2[k] = v
	}
	relay2["answer_id"] = "a_2"
	if code, err := c.doAs("POST", "/jobs/"+e.wi+"/answer", relay2, &out, "usr_worker"); err != nil || code != 409 {
		t.Fatalf("consumed question: %d %v", code, err)
	}

	// observe: ring + snapshot.
	if code, err := c.do("GET", "/jobs/"+e.wi+"/observe?after=0", nil, &out); err != nil || code != 200 {
		t.Fatalf("observe: %d %v", code, err)
	}

	// mode: drain then single claim refused with DAEMON-409 (mode
	// exclusion, NOT shutdown), abort-drain ok.
	if code, err := c.do("POST", "/mode", map[string]any{"action": "drain"}, &out); err != nil || code != 200 {
		t.Fatalf("drain: %d %v", code, err)
	}
	if code, err := c.do("POST", "/jobs/wi_other9/claim", nil, &out); err != nil || code != 409 {
		t.Fatalf("drain must refuse single claim: %d %v", code, err)
	}
	if code, err := c.do("POST", "/mode", map[string]any{"action": "abort-drain"}, &out); err != nil || code != 200 {
		t.Fatalf("abort-drain: %d %v", code, err)
	}
	// max_concurrent_jobs validation.
	if code, err := c.do("POST", "/mode", map[string]any{"action": "set", "max_concurrent_jobs": 0}, &out); err != nil || code != 400 {
		t.Fatalf("bad concurrency: %d %v", code, err)
	}

	// proxy_call from a NON-agent peer (this test process): the SO_PEERCRED
	// pid does not match any job's main_agent_pid → DAEMON-403, and an
	// off-whitelist operation is refused even before that.
	if code, err := c.do("POST", "/proxy_call", map[string]any{
		"work_item_id": e.wi, "operation": "rotate_secret", "payload": map[string]any{},
	}, &out); err != nil || code != 403 {
		t.Fatalf("off-whitelist op: %d %v", code, err)
	}
	if code, err := c.do("POST", "/proxy_call", map[string]any{
		"work_item_id": e.wi, "operation": "read_workflow_ref", "payload": map[string]any{},
	}, &out); err != nil || code != 403 {
		t.Fatalf("non-agent peer: %d %v", code, err)
	}

	// shutdown: latch; further submissions → DAEMON-410.
	if code, err := c.do("POST", "/shutdown", nil, &out); err != nil || code != 200 {
		t.Fatalf("shutdown: %d %v", code, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !e.d.modes.ShuttingDown() {
			if time.Now().After(deadline) {
				t.Fatal("shutdown sequence did not converge")
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		break
	}
	if code, err := c.do("POST", "/jobs/wi_other9/claim", nil, &out); err != nil || code != 410 {
		t.Fatalf("post-shutdown submit: %d %v body=%v", code, err, out)
	}
}

// udsClient is a minimal HTTP-over-UDS client for the face tests.
type udsClient struct{ path string }

func (c *udsClient) do(method, path string, body any, out any) (int, error) {
	return c.doAs(method, path, body, out, "")
}

func (c *udsClient) doAs(method, path string, body any, out any, user string) (int, error) {
	var rd io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = strings.NewReader(string(blob))
	}
	req, err := http.NewRequest(method, "http://daemon"+path, rd)
	if err != nil {
		return 0, err
	}
	if user != "" {
		req.Header.Set("X-PF-User", user)
	}
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return conn, nil }}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if uerr := json.Unmarshal(raw, out); uerr != nil {
			return resp.StatusCode, fmt.Errorf("body: %s", raw)
		}
	}
	return resp.StatusCode, nil
}

var _ = capsule.StatusRunning
