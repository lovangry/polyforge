package test

// harness_test.go: the shared E2E + fault-injection harness (polyforge#11
// E2E/FAULT-INJECTION lane). Boots the REAL components in-process — the
// Core HTTP face (internal/server over a real SQLite store), the Daemon
// (internal/daemon) with its real UDS operations face, the real
// internal/cli clients driving both faces, the real publish/recover
// packages over the real HTTP client — plus the test-only fakes the
// design leaves injectable (the WI-Agent spawner, 04 §6.1 step 9).
//
// Hermetic by construction: every env lives under t.TempDir(), every
// socket is a Unix domain socket in the temp dir, the Core listens on
// loopback only. No network beyond loopback/UDS, no external state.
//
// This package is test-only: it is never imported by production code
// (the same rule as internal/core/testsupport) and lives entirely in
// *_test.go files so `go build ./...` ships nothing from it.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/cli"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/daemon"
	"github.com/silgrid/polyforge/internal/server"
	"github.com/silgrid/polyforge/internal/sync/aihub"
	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/publish"
	"github.com/silgrid/polyforge/internal/sync/recover"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// ---------------------------------------------------------------------------
// Core env: the real AIHub face on loopback.

// coreEnv is one real Core: migrated SQLite store, registered service
// (secret-epoch machinery), seeded identities, the demo workflow bundle,
// and the full HTTP mux behind an httptest listener.
type coreEnv struct {
	t         *testing.T
	ts        *httptest.Server
	db        *store.DB
	dataDir   string
	workerKey string // api key of usr_worker (writer)
	adminKey  string // api key of usr_admin (global admin)
}

var keySeq int

// mintKey inserts an api key for one seeded user and returns the
// plaintext (the same convention as the server's authenticate(): the
// stored hash is hex(sha256(plaintext))).
func (e *coreEnv) mintKey(user string) string {
	e.t.Helper()
	keySeq++
	plain := fmt.Sprintf("pfk_e2e_%s_%d_%d", user, os.Getpid(), keySeq)
	sum := sha256.Sum256([]byte(plain))
	if _, err := e.db.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,NULL,?3)`,
		fmt.Sprintf("key_%s_%d", user, keySeq), user, hex.EncodeToString(sum[:])); err != nil {
		e.t.Fatal(err)
	}
	return plain
}

func newCoreEnv(t *testing.T) *coreEnv {
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
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	e := &coreEnv{t: t, ts: ts, db: db, dataDir: dir}
	e.workerKey = e.mintKey("usr_worker")
	e.adminKey = e.mintKey("usr_admin")
	return e
}

// client returns a Daemon-shaped aihub client bound to one key.
func (e *coreEnv) client(key string) *aihub.Client { return aihub.New(e.ts.URL, key) }

// pf returns the real pf CLI's Core client (internal/cli) bound to a key.
func (e *coreEnv) pf(key string) *cli.Client { return cli.NewClient(e.ts.URL, key) }

// newWI creates an execution work item (demo bundle, empty repo
// inventory) through the real CLI client, as `pf file create` would.
func (e *coreEnv) newWI(t *testing.T, goal string) string {
	t.Helper()
	raw, err := e.pf(e.workerKey).CreateWorkItem(&cli.CreateWorkItemRequest{
		Project: "p1", Goal: goal, BundleName: ptr("demo"),
		RepoInventory: []cli.RepoDecl{}, RepoInventoryBase: []cli.RepoBaseDecl{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		t.Fatalf("create wi: %v %s", err, raw)
	}
	return out.ID
}

// rawGET is an authenticated GET returning the raw body (faces the
// aihub client does not mirror, e.g. row-39 repo-inventory).
func (e *coreEnv) rawGET(t *testing.T, key, path string) []byte {
	t.Helper()
	req, err := http.NewRequest("GET", e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	return raw
}

// ---- Core-side facts, read straight from the authority (the DB). ----

func (e *coreEnv) wiStatus(wi string) string {
	e.t.Helper()
	var status string
	if err := e.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wi).Scan(&status); err != nil {
		e.t.Fatal(err)
	}
	return status
}

// headRow returns the Core HEAD pointer (seq, manifest_sha).
func (e *coreEnv) headRow(wi string) (int64, string) {
	e.t.Helper()
	var seq sql.NullInt64
	var sha sql.NullString
	if err := e.db.QueryRow(`SELECT h.seq, r.manifest_sha FROM capsule_heads h
		JOIN capsule_revisions r ON r.work_item_id=h.work_item_id AND r.seq=h.seq
		WHERE h.work_item_id=?1`, wi).Scan(&seq, &sha); err == sql.ErrNoRows {
		return -1, ""
	} else if err != nil {
		e.t.Fatal(err)
	}
	return seq.Int64, sha.String
}

// headGeneration returns capsule_heads.generation (the publishing
// generation of the current HEAD), or -1 when no HEAD exists.
func (e *coreEnv) headGeneration(wi string) int64 {
	e.t.Helper()
	var gen sql.NullInt64
	if err := e.db.QueryRow(`SELECT generation FROM capsule_heads WHERE work_item_id=?1`, wi).Scan(&gen); err == sql.ErrNoRows {
		return -1
	} else if err != nil {
		e.t.Fatal(err)
	}
	return gen.Int64
}

func (e *coreEnv) eventCount(wi, typ string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM wi_events WHERE work_item_id=?1 AND event_type=?2`,
		wi, typ).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// genEnds maps generation -> end_reason ("" while the generation is
// still active). The whole close story of one WI in one read.
func (e *coreEnv) genEnds(wi string) map[int64]string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT generation, end_reason FROM owner_generations WHERE work_item_id=?1 ORDER BY generation`, wi)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var gen int64
		var reason sql.NullString
		if err := rows.Scan(&gen, &reason); err != nil {
			e.t.Fatal(err)
		}
		out[gen] = reason.String
	}
	return out
}

// signToken reconstructs the generation token the Service would sign for
// (wi, generation, machine): hex(HMAC-SHA256(secret, "wi|gen|machine"))
// over the current epoch's secret file (internal/core/service signToken).
// The fault-injection drills use it to speak "as the daemon's lost
// memory" — the token the dead process held and no component persists.
func (e *coreEnv) signToken(wi string, gen int64, machine string) string {
	e.t.Helper()
	var v string
	if err := e.db.QueryRow(`SELECT value FROM core_meta WHERE key='current_secret_epoch'`).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join(e.dataDir, "service.secret."+v))
	if err != nil {
		e.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s|%d|%s", wi, gen, machine)
	return hex.EncodeToString(mac.Sum(nil))
}

// casPath is the Core-side on-disk CAS block location (internal/core/
// capsule Store.casPath) — the fault drills flip bytes here.
func (e *coreEnv) casPath(wi, sha string) string {
	return filepath.Join(e.dataDir, "capsules", wi, "rev", "_cas", sha[:2], sha)
}

// ---------------------------------------------------------------------------
// WI-Agent spawner fake: the design's injectable boundary (04 §6.1 step
// 9). A real child process (sleep) stands in for the agent so the
// SO_PEERCRED pid checks have a real distinct pid to match against.

type fakeAgent struct {
	t         *testing.T
	pid       int
	startedAt string
	child     *exec.Cmd
	dead      bool

	mu      sync.Mutex
	answers []daemon.AnswerRelay
	// onAnswer mirrors the WI Agent's consumption contract (03 §4.2 D9):
	// archive the answer and clear pending_question in the SAME atomic
	// write. Tests install it to make the fake consume like the real one.
	onAnswer func(daemon.AnswerRelay)
	// onNotifyStop, when set, replaces the default acknowledgment — used
	// to park a close at its boundary notice for race/close drills.
	onNotifyStop func()
}

func (a *fakeAgent) PID() int          { return a.pid }
func (a *fakeAgent) StartedAt() string { return a.startedAt }
func (a *fakeAgent) Alive() bool       { return !a.dead }

// GroupEmpty: the fake models a clean exit with no residual group.
func (a *fakeAgent) GroupEmpty() bool { return a.dead }

func (a *fakeAgent) NotifyStop(ctx context.Context) error {
	if a.onNotifyStop != nil {
		a.onNotifyStop()
		return nil
	}
	return nil
}
func (a *fakeAgent) Quiesce(ctx context.Context) error { return nil }
func (a *fakeAgent) Resume(ctx context.Context) error  { return nil }
func (a *fakeAgent) DeliverAnswer(rel daemon.AnswerRelay) error {
	a.mu.Lock()
	a.answers = append(a.answers, rel)
	hook := a.onAnswer
	a.mu.Unlock()
	if hook != nil {
		hook(rel)
	}
	return nil
}
func (a *fakeAgent) Terminate(timeout time.Duration) error {
	a.dead = true
	if a.child != nil && a.child.Process != nil {
		a.child.Process.Kill()
		a.child.Wait()
	}
	return nil
}

// relays returns every answer relay the fake agent received (for the
// five-field verbatim assertions).
func (a *fakeAgent) relays() []daemon.AnswerRelay {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]daemon.AnswerRelay, len(a.answers))
	copy(out, a.answers)
	return out
}

type fakeSpawner struct {
	t      *testing.T
	mu     sync.Mutex
	spawns []daemon.SpawnParams
	agents []*fakeAgent
}

func newFakeSpawner(t *testing.T) *fakeSpawner {
	return &fakeSpawner{t: t}
}

func (s *fakeSpawner) Spawn(ctx context.Context, p daemon.SpawnParams) (daemon.AgentHandle, error) {
	// A real distinct child pid: SO_PEERCRED proxies must be able to tell
	// the agent apart from the (same-pid) test caller.
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		return nil, err
	}
	s.t.Cleanup(func() {
		if child.Process != nil {
			child.Process.Kill()
			child.Wait()
		}
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &fakeAgent{t: s.t, pid: child.Process.Pid, startedAt: time.Now().UTC().Format(time.RFC3339Nano), child: child}
	s.spawns = append(s.spawns, p)
	s.agents = append(s.agents, a)
	return a, nil
}

func (s *fakeSpawner) last() *fakeAgent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.agents) == 0 {
		return nil
	}
	return s.agents[len(s.agents)-1]
}

// ---------------------------------------------------------------------------
// Daemon env: one machine's daemon (the `pf daemon` subcommand body,
// driven in-process here) over the real Core.

// daemonEnv is one machine: its own home (workspace root, daemon data
// dir, daemon checkout of skills/), a real Daemon wired to the Core at
// baseURL (the real face, or a fault-injection proxy in front of it),
// and its UDS operations face.
type daemonEnv struct {
	t        *testing.T
	core     *coreEnv
	machine  string
	apiKey   string
	home     string // <temp>/home: ws/, daemon/, skills/, workflows/
	cfg      daemon.Config
	d        *daemon.Daemon
	spawner  *fakeSpawner
	socket   string
	pfClient *cli.DaemonClient
}

// newMachineEnv assembles one machine's daemon. baseURL lets a drill
// front the Core with a fault-injection proxy (the daemon sees only an
// ordinary AIHub URL).
func newMachineEnv(t *testing.T, core *coreEnv, machine, apiKey, baseURL string) *daemonEnv {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{
		filepath.Join(home, "skills", "pf-execute"),
		filepath.Join(home, "workflows"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"),
		[]byte("# pf-execute\nDrive the workflow step by step.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spawner := newFakeSpawner(t)
	cfg := daemon.Config{
		AIHubURL: baseURL, APIKey: apiKey, MachineID: machine,
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		DrainProject:  "p1",
		Adapter:       "pi",
		Version:       "e2e-test",
	}
	d, err := daemon.New(cfg, spawner)
	if err != nil {
		t.Fatal(err)
	}
	return &daemonEnv{
		t: t, core: core, machine: machine, apiKey: apiKey, home: home,
		cfg: cfg, d: d, spawner: spawner,
		socket: filepath.Join(home, "polyforge.sock"),
	}
}

// newDaemonEnv is the common case: a worker machine straight on the Core.
func newDaemonEnv(t *testing.T, core *coreEnv) *daemonEnv {
	return newMachineEnv(t, core, "mac_a", core.workerKey, core.ts.URL)
}

// serve starts the UDS operations face on this machine's socket.
func (e *daemonEnv) serve() {
	e.t.Helper()
	api, err := daemon.NewAPIServer(e.d, e.socket)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		// The pooled CLI client's idle keep-alive conns are left OPEN on
		// purpose: APIServer.Close drains them (polyforge#11 FIX-ROUND,
		// inference 4 — accepted conns stay poller-registered since
		// SO_PEERCRED no longer flips them into blocking mode), so every
		// drill here also regression-pins that a live pooled session
		// does not hang the face's shutdown.
		api.Close()
	})
	go func() { _ = api.Serve() }()
	waitUDS(e.t, e.socket)
}

// pf returns the real pf CLI's daemon client on this machine's socket.
// One pooled client per machine: its idle keep-alive conns are
// deliberately left open at cleanup — APIServer.Close handles them
// (polyforge#11 FIX-ROUND, inference 4), so the drills pin that a live
// pooled CLI session no longer hangs the face's shutdown.
func (e *daemonEnv) pf() *cli.DaemonClient {
	if e.pfClient == nil {
		e.pfClient = cli.NewDaemonClient(e.socket)
	}
	return e.pfClient
}

// wsRoot is the machine's capsule workspace root.
func (e *daemonEnv) wsRoot() string { return filepath.Join(e.home, "ws") }

// state loads this machine's daemon/state.json.
func (e *daemonEnv) state() *daemon.State {
	e.t.Helper()
	st, err := daemon.NewStateStore(e.cfg.DataDir).Load()
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// openCapsule opens the live capsule of wi on this machine.
func (e *daemonEnv) openCapsule(wi string) (*capsule.Capsule, *capsule.Identity) {
	e.t.Helper()
	return openCapsule(e.t, e.wsRoot(), wi)
}

// claimWorkingJob runs the full 04 §6.1 sequence for wi and makes the WI
// Agent "work" one step (a written execution.json); the caller decides
// when to publish it.
func (e *daemonEnv) claimWorkingJob(wi string) (*capsule.Capsule, *fakeAgent) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.d.Submit(daemon.SubmitIntent{WorkItemID: wi, Intent: "claim"}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		e.t.Fatal(err)
	}
	agent := e.spawner.last()
	if agent == nil {
		e.t.Fatal("no WI Agent was derived")
	}
	capc, _ := e.openCapsule(wi)
	writeExecution(e.t, capc, wi, 1, "code", 1)
	return capc, agent
}

// ---------------------------------------------------------------------------
// Capsule work helpers (what the WI Agent does between publishes).

func openCapsule(t *testing.T, wsRoot, wi string) (*capsule.Capsule, *capsule.Identity) {
	t.Helper()
	capc, idn, err := capsule.Open(wsRoot, wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	return capc, idn
}

// writeExecution persists a minimal running execution state (the capsule
// write side of "the agent advanced a step").
func writeExecution(t *testing.T, capc *capsule.Capsule, wi string, gen int64, step string, version int64) {
	t.Helper()
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: wi, Generation: gen,
		CurrentStep: step, WorkflowStatus: capsule.StatusRunning,
		Steps:   map[string]*capsule.Step{step: {Status: capsule.StepInProgress}},
		Version: version,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeCapsuleFile(t *testing.T, capc *capsule.Capsule, rel, content string) {
	t.Helper()
	p := filepath.Join(capc.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// parkNeedsInput parks the current step on an open pending_question (the
// 03 §4.2 needs_input shape) and returns it.
func parkNeedsInput(t *testing.T, capc *capsule.Capsule, wi string, gen int64, questionID string) *capsule.PendingQuestion {
	t.Helper()
	q := &capsule.PendingQuestion{QuestionID: questionID, AskedAt: daemon.NowStamp(), Content: "Which design should step " + questionID + " take?"}
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: wi, Generation: gen,
		CurrentStep: "code", WorkflowStatus: capsule.StatusRunning,
		Steps:   map[string]*capsule.Step{"code": {Status: capsule.StepInProgress, PendingQuestion: q}},
		Version: 2,
	}); err != nil {
		t.Fatal(err)
	}
	return q
}

// teachAnswerConsumption installs the real WI Agent's answer consumption
// contract (03 §4.2 D9) on the fake: archive the answer and clear
// pending_question in the SAME atomic write.
func teachAnswerConsumption(agent *fakeAgent, capc *capsule.Capsule, t *testing.T) {
	agent.onAnswer = func(rel daemon.AnswerRelay) {
		cur, err := capc.LoadExecution()
		if err != nil || cur == nil {
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
		if err := capc.SaveExecution(cur); err != nil {
			t.Logf("answer consumption write failed: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Raw UDS calls (for the shapes the typed CLI client cannot express:
// forged X-PF-User, error-envelope pinning).

// udsBody issues one request against a daemon UDS face and returns the
// status plus the raw body. out, when non-nil, receives the decoded
// JSON body on any status.
func udsBody(t *testing.T, socket, method, path string, body any, out any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, "http://daemon"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	// One request per connection: the raw client must not leave an idle
	// keep-alive conn behind (see serve()'s cleanup note).
	req.Close = true
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return conn, nil }}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	tr.CloseIdleConnections()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("uds %s %s: undecodable body %s", method, path, raw)
		}
	}
	return resp.StatusCode, raw
}

// errEnvelope decodes {"error":{code,reason?,detail}} — the typed error
// shape shared by both faces (reason exists on the Core face only).
type errEnvelope struct {
	Error struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	} `json:"error"`
}

func udsErr(t *testing.T, socket, method, path string, body any, hdr map[string]string) (int, errEnvelope) {
	t.Helper()
	code, raw := udsBody(t, socket, method, path, body, nil, hdr)
	var env errEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
		t.Fatalf("uds %s %s: no typed error envelope in %s", method, path, raw)
	}
	return code, env
}

func waitUDS(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon UDS face %s never came up: %v", socket, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitUntil polls cond until true or the deadline (bounded busy-wait for
// the asynchronous daemon sequences).
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Recovery-site helpers: consuming claim/force-takeover credentials the
// way the 04 §6.1 spawn sequence does, at component level. These exist
// because the ONLY way a recovery side can hold a generation token is
// consuming the claim-family response directly (02 §6.2); the drills use
// them wherever a component-level path is the faithful one.

// pubRemote adapts the aihub client to publish.Remote.
type pubRemote struct{ c *aihub.Client }

func (r *pubRemote) PutBlock(ctx context.Context, wi string, seq int64, sha string, block []byte, token string) error {
	return r.c.PutBlock(wi, seq, sha, block, token)
}
func (r *pubRemote) PostManifest(ctx context.Context, wi string, m revision.Manifest, sha, token string) (int64, error) {
	return r.c.PostManifest(wi, m, sha, token)
}
func (r *pubRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.c.GetHead(wi)
}

// recRemote adapts the aihub client to recover.Remote (the row-19
// revisions listing is requested with include=ops by the client).
type recRemote struct{ c *aihub.Client }

func (r *recRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.c.GetHead(wi)
}
func (r *recRemote) ListRevisions(ctx context.Context, wi string, since int64) ([]revision.RevisionMeta, error) {
	return r.c.ListRevisions(wi, since)
}
func (r *recRemote) GetManifest(ctx context.Context, wi string, seq int64) (revision.Manifest, error) {
	return r.c.GetManifest(wi, seq)
}
func (r *recRemote) GetBlock(ctx context.Context, wi, sha string) ([]byte, error) {
	return r.c.GetBlock(wi, sha)
}

// bootstrapFromCore assembles the capsule_bootstrap payload from the
// Core read faces — the same fields the claim-family response carries
// (bundle reference + repo inventory, 02 §8). Used where a drill builds a
// recovery site from credentials it already holds instead of replaying a
// claim-family response (the force-takeover response itself carries the
// payload since the polyforge#11 FIX-ROUND D1 fix).
func bootstrapFromCore(ctx context.Context, t *testing.T, e *coreEnv, key, wi string) *capsule.Bootstrap {
	t.Helper()
	client := e.client(key)
	detail, err := client.GetWI(wi)
	if err != nil {
		t.Fatal(err)
	}
	bundleName, _ := detail["bundle_name"].(string)
	bundleDigest, _ := detail["bundle_digest"].(string)
	var inv struct {
		RepoInventory []capsule.RepoEntry `json:"repo_inventory"`
	}
	if err := json.Unmarshal(e.rawGET(t, key, "/api/v1/work-items/"+wi+"/repo-inventory"), &inv); err != nil {
		t.Fatal(err)
	}
	if bundleName == "" {
		t.Fatalf("wi detail carries no bundle_name: %v", detail)
	}
	return &capsule.Bootstrap{
		WorkItemID: wi, BundleName: bundleName, BundleDigest: bundleDigest,
		RepoInventory: inv.RepoInventory,
	}
}

// site is a machine's execution scene rebuilt from claim-family
// credentials: capsule + seeded publish state + a real Publisher over
// the real Core face.
type site struct {
	t       *testing.T
	core    *coreEnv
	client  *aihub.Client
	wi      string
	machine string
	gen     int64
	token   string
	cap     *capsule.Capsule
	pub     *publish.Publisher
	dataDir string
	wsRoot  string
}

// rebuildSite consumes claim-family credentials (generation + token)
// exactly as the 04 §6.1 spawn sequence would: capsule create (with its
// quarantine-or-reuse), inventory checkout, remote site reconstruction
// (05 §7.4, row-19 include=ops replay), publish-state seeding.
func rebuildSite(ctx context.Context, t *testing.T, e *coreEnv, key, machine, wi string, gen int64, token, wsRoot, dataDir string) *site {
	t.Helper()
	client := e.client(key)
	boot := bootstrapFromCore(ctx, t, e, key, wi)
	capc, res, err := capsule.Create(wsRoot, wi, gen, machine, boot.BundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Quarantined != "" {
		t.Logf("capsule create quarantined a prior scene: %s", res.Quarantined)
	}
	if err := capc.CheckoutInventory(boot); err != nil {
		t.Fatalf("inventory checkout: %v", err)
	}
	rec, err := recover.Recover(ctx, capc, &recRemote{c: client})
	if err != nil {
		t.Fatalf("remote site reconstruction: %v", err)
	}
	pub := publish.New(&pubRemote{c: client}, freeze.NewManager(), freeze.NewStager(dataDir), publish.NewStore(dataDir))
	if rec.Head != nil {
		if err := pub.SeedFromRecovery(wi, gen, rec.Head, rec.LiveFiles, rec.BytesDigest, rec.WorktreeDigest); err != nil {
			t.Fatal(err)
		}
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	return &site{
		t: t, core: e, client: client, wi: wi, machine: machine,
		gen: gen, token: token, cap: capc, pub: pub, dataDir: dataDir, wsRoot: wsRoot,
	}
}

func (s *site) work(rel, content string) {
	writeCapsuleFile(s.t, s.cap, rel, content)
}

// publish runs one real full-scan publish (block -> manifest -> HEAD).
func (s *site) publish(ctx context.Context) *publish.Result {
	s.t.Helper()
	res, err := s.pub.Publish(ctx, publish.Request{
		WorkItemID: s.wi, CapsuleDir: s.cap.Dir, Generation: s.gen, Token: s.token,
		Conditions: freeze.Conditions{ProcessGroupDead: func() bool { return true }},
	})
	if err != nil {
		s.t.Fatalf("site publish: %v", err)
	}
	return res
}

// finalFlush runs the 05 §6.1 generation-end flush and returns the
// evidence the end-generation call must carry (02 §6.4).
func (s *site) finalFlush(ctx context.Context) *aihub.FlushEvidence {
	s.t.Helper()
	fe, err := s.pub.FinalFlush(ctx, publish.FlushRequest{
		WorkItemID: s.wi, CapsuleDir: s.cap.Dir, Generation: s.gen, Token: s.token,
		Conditions: freeze.Conditions{ProcessGroupDead: func() bool { return true }},
	})
	if err != nil {
		s.t.Fatalf("site final flush: %v", err)
	}
	return fe
}

// endGeneration closes the generation with daemon-style evidence.
func (s *site) endGeneration(ctx context.Context, reason string, fe *aihub.FlushEvidence) error {
	return s.client.EndGeneration(s.wi, reason, "", s.token, fe)
}

// ---------------------------------------------------------------------------
// holdProxy: a fault-injection proxy in front of the Core face that
// PARKS every end-generation request (hijacks the connection) until the
// drill kills it — the wire-level stand-in for "the daemon process died
// between the final flush and the end-generation call".

func newHoldProxy(t *testing.T, up http.Handler) (*httptest.Server, chan net.Conn) {
	t.Helper()
	held := make(chan net.Conn, 4)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/end-generation") {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "proxy cannot hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			held <- conn
			// Hold the request until the drill closes the conn (the
			// process-death boundary); the blocked read unblocks on close
			// and the handler goroutine exits without leaking.
			buf := make([]byte, 1)
			_, _ = conn.Read(buf)
			return
		}
		up.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, held
}

// ---------------------------------------------------------------------------
// Small shared utilities.

func ptr(s string) *string { return &s }

// strconv64 formats an int64 (manifest paths etc.).
func strconv64(n int64) string { return strconv.FormatInt(n, 10) }

// capsuleCreate / recoverReplay / statFile / flipByte / readFileIfExists:
// tiny raw-file helpers shared by the fault drills.
func capsuleCreate(wsRoot, wi string, gen int64, machine, bundleDigest string) (*capsule.Capsule, *capsule.CreateResult, error) {
	return capsule.Create(wsRoot, wi, gen, machine, bundleDigest)
}

func recoverReplay(ctx context.Context, capc *capsule.Capsule, client *aihub.Client) (*recover.Result, error) {
	return recover.Recover(ctx, capc, &recRemote{c: client})
}

func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

// flipByte XORs one byte of a regular file in place (at-rest corruption).
func flipByte(path string, off int64) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		return false
	}
	b[0] ^= 0x40
	_, err = f.WriteAt(b[:], off)
	return err == nil
}

// readFileIfExists reads one file or reports the miss.
func readFileIfExists(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
}

// mustBeAPIError asserts err is the typed Core envelope with the given
// code (and optional reason substring) and returns it.
func mustBeAPIError(t *testing.T, err error, code, reasonPart string) *aihub.APIError {
	t.Helper()
	ae, ok := err.(*aihub.APIError)
	if !ok {
		t.Fatalf("expected *aihub.APIError{%s}, got %T: %v", code, err, err)
	}
	if ae.Code != code {
		t.Fatalf("expected code %s, got %s (reason=%s detail=%s)", code, ae.Code, ae.Reason, ae.Detail)
	}
	if reasonPart != "" && !strings.Contains(ae.Reason, reasonPart) {
		t.Fatalf("expected reason containing %q, got %q (detail=%s)", reasonPart, ae.Reason, ae.Detail)
	}
	return ae
}

// daemonFinish decodes a pause/wrap route body into the typed outcome.
func daemonFinish(t *testing.T, raw []byte) daemon.FinishOutcome {
	t.Helper()
	var out daemon.FinishOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("finish outcome undecodable: %s", raw)
	}
	return out
}

// readyQueueView is the flat two-array scheduler response as the CLI
// surfaces it (raw pass-through; decoded here for assertions).
type readyQueueView struct {
	Project    string `json:"project"`
	Executable []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"executable"`
	Blocked []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"blocked"`
}

func readyQueue(t *testing.T, e *coreEnv) readyQueueView {
	t.Helper()
	raw, err := e.pf(e.workerKey).ReadyQueue("p1")
	if err != nil {
		t.Fatal(err)
	}
	var q readyQueueView
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatalf("ready queue undecodable: %s", raw)
	}
	return q
}

func (q readyQueueView) hasExecutable(id string) bool {
	for _, x := range q.Executable {
		if x.ID == id {
			return true
		}
	}
	return false
}

func (q readyQueueView) hasBlocked(id string) bool {
	for _, x := range q.Blocked {
		if x.ID == id {
			return true
		}
	}
	return false
}
