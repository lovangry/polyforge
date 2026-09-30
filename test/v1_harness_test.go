package test

// v1_harness_test.go: the v1 end-to-end dogfood harness. Mirrors the v0
// usage_* discipline: build the REAL binaries (aihub, pf, fakepi) once,
// exec them as real OS processes, and assert against real wire/git/
// process state only — aihub as a process on a free loopback port, the
// daemon as a `pf daemon` process on its own UDS, the pf CLI as the real
// binary, fakepi as the WI-agent stand-in the daemon derives via
// --pi-bin, and the capsule git chain verified with real git against
// the aihub's on-disk bare repos.
//
// Hermetic: every test gets its own temp install (aihub data dir, daemon
// data dir + workspace, socket, plan dir), a free loopback port, and
// cleanups that kill every process it started — even on failure. No
// default ports, no shared state, no network beyond loopback.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// ---------------------------------------------------------------------------
// Binary build cache: one build per test-binary run, shared by every
// v1_s* scenario.

var (
	v1BinOnce sync.Once
	v1BinDir  string
	v1BinErr  error
)

// v1Root is the module root (the tests run with cwd = test/).
func v1Root(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// v1Bins builds aihub, pf and fakepi into a shared temp dir. When
// PFV1_COVER_BINS is set, the binaries are built INSTRUMENTED
// (-cover -coverpkg=./internal/...) and GOCOVERDIR is pointed at
// <dir>/covdata so every spawned aihub/daemon/CLI/agent process emits
// its coverage there — the manual instrumented round for the e2e
// coverage report (merge with `go tool covdata textfmt`).
func v1Bins(t *testing.T) string {
	t.Helper()
	v1BinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pfv1-bins")
		if err != nil {
			v1BinErr = err
			return
		}
		build := exec.Command("go", "build", "-o", dir+"/",
			"./cmd/aihub", "./cmd/pf", "./test/fakepi", "./tools/v0-export")
		if os.Getenv("PFV1_COVER_BINS") != "" {
			build = exec.Command("go", "build", "-cover", "-coverpkg=./...",
				"-o", dir+"/", "./cmd/aihub", "./cmd/pf", "./test/fakepi", "./tools/v0-export")
			covDir := filepath.Join(dir, "covdata")
			if err := os.MkdirAll(covDir, 0o755); err != nil {
				v1BinErr = err
				return
			}
			os.Setenv("GOCOVERDIR", covDir) // every spawned process inherits it
		}
		build.Dir = v1Root(t)
		build.Env = append(os.Environ(), "GOWORK=off")
		if out, err := build.CombinedOutput(); err != nil {
			v1BinErr = fmt.Errorf("build binaries: %v: %s", err, out)
			return
		}
		v1BinDir = dir
	})
	if v1BinErr != nil {
		t.Fatalf("binary build cache: %v", v1BinErr)
	}
	return v1BinDir
}

// ---------------------------------------------------------------------------
// One real aihub install + the pf CLI face.

const v1Project = "e2e"

// v1Install is a scratch install of the real system.
type v1Install struct {
	t          *testing.T
	binDir     string
	root       string
	dataDir    string // aihub data dir (bare repos under dataDir/git)
	url        string
	port       int
	adminKey   string
	aihubLog   string
	aihubCmd   *exec.Cmd
	client     *aihubclient.Client
	planDir    string
	pfPath     string
	fakepiPath string
}

// newV1Install boots a real aihub on a free loopback port, harvests the
// first-boot bootstrap admin key, and creates the e2e project.
func newV1Install(t *testing.T) *v1Install {
	return newV1InstallRepos(t, nil)
}

// taskRemote is one local fake task-repo remote: a real git repo (one
// commit on main) the daemon can clone over file:// — no network.
type taskRemote struct {
	dir  string
	url  string
	head string
}

func newTaskRemote(t *testing.T) taskRemote {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "init")
	head := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	return taskRemote{dir: dir, url: "file://" + dir, head: head}
}

// newV1InstallTaskRepo boots the standard install with the e2e project
// configured with one local fake task-repo remote, so a scenario can
// exercise the claim→clone lane (repos land in the capsule before the
// agent spawns).
func newV1InstallTaskRepo(t *testing.T) (*v1Install, taskRemote) {
	t.Helper()
	remote := newTaskRemote(t)
	inst := newV1InstallRepos(t, []api.ProjectRepo{{Name: "demo-src", URL: remote.url}})
	return inst, remote
}

// newV1InstallRepos is newV1Install with an explicit task-repo inventory
// on the e2e project (nil = none).
func newV1InstallRepos(t *testing.T, repos []api.ProjectRepo) *v1Install {
	t.Helper()
	bins := v1Bins(t)
	inst := &v1Install{
		t:          t,
		binDir:     bins,
		root:       v1Root(t),
		dataDir:    t.TempDir(),
		aihubLog:   filepath.Join(t.TempDir(), "aihub.log"),
		planDir:    t.TempDir(),
		pfPath:     filepath.Join(bins, "pf"),
		fakepiPath: filepath.Join(bins, "fakepi"),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inst.port = ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	inst.url = "http://127.0.0.1:" + strconv.Itoa(inst.port)

	logf, err := os.Create(inst.aihubLog)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(bins, "aihub"),
		"--data-dir", inst.dataDir, "--listen", "127.0.0.1:"+strconv.Itoa(inst.port))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inst.aihubCmd = cmd
	t.Cleanup(func() {
		if cmd.Process != nil {
			// SIGTERM first: the server's own graceful path (also flushes
			// any instrumented-coverage data at exit); SIGKILL fallback.
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		logf.Close()
	})
	inst.waitHTTP(t)
	inst.adminKey = v1HarvestBootstrapKey(t, inst.aihubLog)
	inst.client = aihubclient.New(inst.url, inst.adminKey)
	if _, err := inst.client.CreateProject(aihubclient.ProjectCreateRequest{
		Name: v1Project, Description: "v1 e2e scratch project", Repos: repos,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return inst
}

// waitHTTP polls the aihub TCP face until it answers.
func (u *v1Install) waitHTTP(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(u.port))
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("aihub never came up on %d: %v", u.port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// v1HarvestBootstrapKey reads the one-time bootstrap admin key the aihub
// prints on the first boot of an empty deployment.
func v1HarvestBootstrapKey(t *testing.T, logPath string) string {
	t.Helper()
	marker := "one-time api key (shown once, store it now): "
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(logPath)
		if err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if i := strings.Index(line, marker); i >= 0 {
					key := strings.TrimSpace(line[i+len(marker):])
					if key != "" {
						return key
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no bootstrap admin key in the aihub log:\n%s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stopAihub terminates the REAL aihub process: SIGTERM (the server's
// own graceful shutdown path) with a hard-kill fallback. Idempotent.
// The data dir survives, so the bare repos and the whole hub state are
// exactly what the restart re-opens.
func (u *v1Install) stopAihub() {
	if u.aihubCmd == nil || u.aihubCmd.Process == nil {
		return
	}
	_ = u.aihubCmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = u.aihubCmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = u.aihubCmd.Process.Kill()
		<-done
	}
	u.aihubCmd = nil
}

// restartAihub re-execs the aihub on the SAME data dir and port (the
// outage-scenario recovery lever) and waits for its HTTP face. The data
// dir persists across stopAihub, so the bare repos the agents push to
// keep their history and their pre-receive seq bookkeeping.
func (u *v1Install) restartAihub() {
	u.t.Helper()
	if u.aihubCmd != nil && u.aihubCmd.Process != nil {
		u.t.Fatal("restartAihub: aihub still running; stopAihub first")
	}
	logf, err := os.OpenFile(u.aihubLog, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		u.t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(u.binDir, "aihub"),
		"--data-dir", u.dataDir, "--listen", "127.0.0.1:"+strconv.Itoa(u.port))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		u.t.Fatal(err)
	}
	u.aihubCmd = cmd
	u.t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		logf.Close()
	})
	u.waitHTTP(u.t)
}

// pf runs the REAL pf CLI against this install (env overrides carry the
// configuration; no config file is involved). wantExit < 0 means the
// command must succeed.
func (u *v1Install) pf(t *testing.T, args ...string) string {
	t.Helper()
	return u.pfExit(t, -1, "", args...)
}

// pfCombined is pf returning stdout+stderr (for error-path pins).
func (u *v1Install) pfCombined(t *testing.T, wantExit int, sock string, args ...string) string {
	t.Helper()
	cmd := exec.Command(u.pfPath, args...)
	cmd.Env = append(os.Environ(),
		"PF_SERVER="+u.url,
		"PF_API_KEY="+u.adminKey,
		"PF_USER_ID=usr_root",
		"PF_PROJECT="+v1Project,
	)
	if sock != "" {
		cmd.Env = append(cmd.Env, "PF_DAEMON_SOCKET="+sock)
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	if wantExit < 0 && code != 0 {
		t.Fatalf("pf %v: exit %d (output %q)", args, code, out.String())
	}
	if wantExit >= 0 && code != wantExit {
		t.Fatalf("pf %v: exit %d, want %d (output %q)", args, code, wantExit, out.String())
	}
	return out.String()
}

func (u *v1Install) pfExit(t *testing.T, wantExit int, sock string, args ...string) string {
	t.Helper()
	cmd := exec.Command(u.pfPath, args...)
	cmd.Env = append(os.Environ(),
		"PF_SERVER="+u.url,
		"PF_API_KEY="+u.adminKey,
		"PF_USER_ID=usr_root",
		"PF_PROJECT="+v1Project,
	)
	if sock != "" {
		cmd.Env = append(cmd.Env, "PF_DAEMON_SOCKET="+sock)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	if wantExit < 0 && code != 0 {
		t.Fatalf("pf %v: exit %d (stdout %q stderr %q)", args, code, stdout.String(), stderr.String())
	}
	if wantExit >= 0 && code != wantExit {
		t.Fatalf("pf %v: exit %d, want %d (stdout %q stderr %q)", args, code, wantExit, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// newWI files one execution ticket (bundle quick-fix, no human session
// required) and scripts its agent with the given plan actions.
// resumeActions, when non-nil, script the crash-continuation derivation
// (the recovery engine's resume spawn).
func (u *v1Install) newWI(t *testing.T, goal string, actions, resumeActions []map[string]any) string {
	t.Helper()
	raw, err := u.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project:    v1Project,
		Goal:       goal,
		BundleName: strPtr("quick-fix"),
		Attrs:      map[string]any{"requires_human_session": false},
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &wi); err != nil {
		t.Fatalf("create work item answer: %v (%s)", err, raw)
	}
	u.setPlanFull(t, wi.ID, actions, resumeActions)
	return wi.ID
}

// setPlan (re)writes the plan file the fake agent reads at process start.
func (u *v1Install) setPlan(t *testing.T, wi string, actions []map[string]any) {
	t.Helper()
	u.setPlanFull(t, wi, actions, nil)
}

func (u *v1Install) setPlanFull(t *testing.T, wi string, actions, resumeActions []map[string]any) {
	t.Helper()
	if actions == nil {
		actions = []map[string]any{}
	}
	body := map[string]any{"actions": actions}
	if resumeActions != nil {
		body["resume_actions"] = resumeActions
	}
	blob, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.planDir, wi+".json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// wiStatus reads the aihub-side work item status.
func (u *v1Install) wiStatus(t *testing.T, wi string) string {
	t.Helper()
	item, err := u.client.GetWorkItem(wi)
	if err != nil {
		t.Fatalf("get work item %s: %v", wi, err)
	}
	return item.Status
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// The daemon as a real process.

// v1Daemon is one `pf daemon` OS process.
type v1Daemon struct {
	t          *testing.T
	inst       *v1Install
	cmd        *exec.Cmd
	logPath    string
	sock       string
	dataDir    string
	workspace  string
	machineID  string
	extraEnv   []string
	extraArgs  []string // extra `pf daemon` flags appended to the argv
	client     *cli.DaemonClient
	waitedExit chan error
	serverURL  string // --server override (optServerURL)
}

type daemonOpt func(*v1Daemon)

func optMachineID(id string) daemonOpt {
	return func(d *v1Daemon) { d.machineID = id }
}

// optObserveWindows injects the observe supervision windows through the
// real `pf daemon` flags (#12). A zero duration omits its flag, leaving
// that window at the production default.
func optObserveWindows(hang, idle, probe, answerAck time.Duration) daemonOpt {
	return func(d *v1Daemon) {
		for _, f := range []struct {
			name string
			val  time.Duration
		}{
			{"--hang-window", hang},
			{"--idle-window", idle},
			{"--probe-timeout", probe},
			{"--answer-ack-window", answerAck},
		} {
			if f.val > 0 {
				d.extraArgs = append(d.extraArgs, f.name, f.val.String())
			}
		}
	}
}

// optMode pre-seeds the daemon state file with an initial mode (read at
// boot; used to start straight in drain).
func optMode(mode string) daemonOpt {
	return func(d *v1Daemon) { d.extraEnv = append(d.extraEnv, "__PFV1_MODE__="+mode) }
}

// optServerURL points the daemon's aihub API face (--server) at url
// instead of the install itself — the s17 hold-proxy front. In that
// case git pushes are pinned DIRECT to the install via --git-remote-base
// so the proxy only ever sees the API client's traffic. Without the opt
// the daemon args are exactly today's.
func optServerURL(url string) daemonOpt {
	return func(d *v1Daemon) { d.serverURL = url }
}

// shortTmp makes a short temp path (unix socket paths are bound to ~104
// bytes; t.TempDir() paths can exceed that with nested test names).
func shortTmp(parent, prefix string) string {
	d, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		panic("shortTmp: " + err.Error())
	}
	return d
}

// newV1Daemon prepares (without starting) a daemon process bound to its
// own socket, data dir and workspace.
func newV1Daemon(t *testing.T, inst *v1Install, opts ...daemonOpt) *v1Daemon {
	t.Helper()
	sockBase := shortTmp("/tmp", "pfv1sock")
	sock := filepath.Join(sockBase, "d.sock")
	dataDir := t.TempDir()
	workspace := t.TempDir()
	d := &v1Daemon{
		t:         t,
		inst:      inst,
		sock:      sock,
		dataDir:   dataDir,
		workspace: workspace,
		machineID: "m-e2e",
		logPath:   filepath.Join(t.TempDir(), "daemon.log"),
	}
	for _, o := range opts {
		o(d)
	}
	// drain mode pre-seed: the daemon-state.json the boot loader reads
	mode := ""
	for _, kv := range d.extraEnv {
		if strings.HasPrefix(kv, "__PFV1_MODE__=") {
			mode = strings.TrimPrefix(kv, "__PFV1_MODE__=")
		}
	}
	if mode != "" {
		st := map[string]any{"version": 1, "machine_id": d.machineID, "mode": mode, "max_concurrent_jobs": 4}
		blob, _ := json.Marshal(st)
		if err := os.WriteFile(filepath.Join(dataDir, "daemon-state.json"), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { d.stop() })
	return d
}

// start launches the daemon process and waits until its UDS face answers.
func (d *v1Daemon) start() {
	d.t.Helper()
	logf, err := os.Create(d.logPath)
	if err != nil {
		d.t.Fatal(err)
	}
	// --server: the install itself, or the test's hold-proxy front
	// (optServerURL) — in the latter case git pushes are pinned direct
	// to the install so only the API client is proxied.
	serverURL := d.inst.url
	gitRemoteBase := ""
	if d.serverURL != "" {
		serverURL = d.serverURL
		gitRemoteBase = d.inst.url + "/git"
	}
	args := []string{
		"daemon",
		"--socket", d.sock,
		"--server", serverURL,
		"--api-key", d.inst.adminKey,
		"--machine-id", d.machineID,
		"--data-dir", d.dataDir,
		"--workspace-root", d.workspace,
		"--project", v1Project,
		"--workflows-dir", filepath.Join(d.inst.root, "workflows"),
		"--executor-prompt", filepath.Join(d.inst.root, "skills", "pf-execute", "SKILL.md"),
		"--pi-bin", d.inst.fakepiPath,
	}
	if gitRemoteBase != "" {
		args = append(args, "--git-remote-base", gitRemoteBase)
	}
	args = append(args, d.extraArgs...)
	cmd := exec.Command(d.inst.pfPath, args...)
	cmd.Env = append(os.Environ(),
		"FAKEPI_PLANS="+d.inst.planDir,
		"GIT_AUTHOR_NAME=pf-e2e",
		"GIT_AUTHOR_EMAIL=pf-e2e@invalid",
		"GIT_COMMITTER_NAME=pf-e2e",
		"GIT_COMMITTER_EMAIL=pf-e2e@invalid",
	)
	for _, kv := range d.extraEnv {
		if strings.HasPrefix(kv, "__PFV1_MODE__=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Dir = d.dataDir
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	d.cmd = cmd
	d.waitedExit = make(chan error, 1)
	go func() { d.waitedExit <- cmd.Wait() }()
	t := d.t
	t.Cleanup(func() {
		_ = logf.Close()
		if t.Failed() {
			t.Logf("daemon log tail (%s):\n%s", d.logPath, d.logTail())
		}
	})

	// Wait for the UDS face.
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := net.Dial("unix", d.sock)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case werr := <-d.waitedExit:
			d.t.Fatalf("daemon exited during startup: %v (log tail: %s)", werr, d.logTail())
		default:
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("daemon UDS never came up on %s (log tail: %s)", d.sock, d.logTail())
		}
		time.Sleep(25 * time.Millisecond)
	}
	d.client = cli.NewDaemonClient(d.sock)
}

// logTail returns the last ~4KB of the daemon log for failure messages.
func (d *v1Daemon) logTail() string {
	raw, err := os.ReadFile(d.logPath)
	if err != nil {
		return fmt.Sprintf("(unreadable log: %v)", err)
	}
	if len(raw) > 4096 {
		raw = raw[len(raw)-4096:]
	}
	return string(raw)
}

// stop terminates the daemon process: SIGTERM first (the graceful path
// when the process is mid-run), SIGKILL fallback. Idempotent.
func (d *v1Daemon) stop() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.waitedExit:
	case <-time.After(10 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.waitedExit
	}
	d.cmd = nil
}

// crash kills the daemon process hard (SIGKILL) — the crash-restart
// scenarios need the agents to survive.
func (d *v1Daemon) crash() {
	d.t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		d.t.Fatal("crash: daemon not running")
	}
	_ = d.cmd.Process.Kill()
	<-d.waitedExit
	d.cmd = nil
}

// exitedWithin reports whether the daemon process exited within bound
// (the graceful-shutdown contract: Run returns once the sequence ends).
// The exit is consumed here: a later stop() becomes a no-op.
func (d *v1Daemon) exitedWithin(bound time.Duration) bool {
	if d.cmd == nil || d.cmd.Process == nil {
		return true // already collected (e.g. by a previous call)
	}
	select {
	case <-d.waitedExit:
		d.cmd = nil
		return true
	case <-time.After(bound):
		return false
	}
}

// pf runs the real pf CLI against THIS daemon's socket (must succeed).
func (d *v1Daemon) pf(t *testing.T, args ...string) string {
	t.Helper()
	return d.inst.pfExit(t, -1, d.sock, args...)
}

// pfCode runs the real pf CLI against THIS daemon's socket with an
// expected exit code; returns combined stdout+stderr.
func (d *v1Daemon) pfCode(t *testing.T, wantExit int, args ...string) string {
	t.Helper()
	return d.inst.pfCombined(t, wantExit, d.sock, args...)
}

// status reads GET /status.
func (d *v1Daemon) status(t *testing.T) cli.StatusResponse {
	t.Helper()
	if d.client == nil {
		t.Fatal("daemon not started")
	}
	raw, err := d.client.Status()
	if err != nil {
		t.Fatalf("daemon status: %v (log tail: %s)", err, d.logTail())
	}
	var st cli.StatusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("status undecodable: %v (%s)", err, raw)
	}
	return st
}

// observe reads the JobView projection for wi.
func (d *v1Daemon) observe(t *testing.T, wi string) *protocol.JobView {
	t.Helper()
	raw, err := d.client.Observe(wi, 0)
	if err != nil {
		t.Fatalf("observe %s: %v (log tail: %s)", wi, err, d.logTail())
	}
	var jv protocol.JobView
	if err := json.Unmarshal(raw, &jv); err != nil {
		t.Fatalf("observe %s undecodable: %v (%s)", wi, err, raw)
	}
	return &jv
}

// agentPIDOf returns the recorded agent pid of wi (0 when not running).
func (d *v1Daemon) agentPIDOf(t *testing.T, wi string) int {
	t.Helper()
	for _, j := range d.status(t).Jobs {
		if j.WorkItemID == wi {
			return j.AgentPID
		}
	}
	return 0
}

// capsuleDir is this daemon's capsule root for wi.
func (d *v1Daemon) capsuleDir(wi string) string {
	return filepath.Join(d.workspace, "capsules", wi)
}

// eventsOf reads the daemon's durable events.log and returns the entries
// of one work item.
func (d *v1Daemon) eventsOf(t *testing.T, wi string) []protocol.EventEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.dataDir, "events.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("events.log: %v", err)
	}
	var out []protocol.EventEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e protocol.EventEntry
		if json.Unmarshal([]byte(line), &e) == nil && (e.WI == wi || e.WI == "") {
			if e.WI == wi {
				out = append(out, e)
			}
		}
	}
	return out
}

// eventKinds is the ordered kind list of one work item's events.
func (d *v1Daemon) eventKinds(t *testing.T, wi string) []string {
	t.Helper()
	var kinds []string
	for _, e := range d.eventsOf(t, wi) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// ---------------------------------------------------------------------------
// Pollers.

// waitUntil polls cond every 100ms until it holds or the bound expires;
// the failure message carries desc so a red is diagnosable from the log.
func waitUntil(t *testing.T, bound time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout (%v): %s", bound, desc)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitWIStatus polls the aihub-side status of wi.
func (u *v1Install) waitWIStatus(t *testing.T, wi, status string, bound time.Duration) {
	t.Helper()
	waitUntil(t, bound, fmt.Sprintf("work item %s never reached status %q (now %q)", wi, status, u.wiStatus(t, wi)),
		func() bool { return u.wiStatus(t, wi) == status })
}

// waitForEvent polls the daemon's durable events.log until one event of
// the given kind appears for wi, and returns it.
func waitForEvent(t *testing.T, d *v1Daemon, wi, kind string, bound time.Duration) protocol.EventEntry {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		for _, e := range d.eventsOf(t, wi) {
			if e.Kind == kind {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: no %q event for %s (saw %v)", kind, wi, d.eventKinds(t, wi))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Real-git assertions against the aihub's on-disk bare repos.

// bareRepo is the aihub bare repo path of one work item.
func (u *v1Install) bareRepo(wi string) string {
	return filepath.Join(u.dataDir, "git", wi+".git")
}

// gitRun runs git with a bounded output capture.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=pf-e2e", "GIT_AUTHOR_EMAIL=pf-e2e@invalid",
		"GIT_COMMITTER_NAME=pf-e2e", "GIT_COMMITTER_EMAIL=pf-e2e@invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir %s): %v: %s", args, dir, err, out)
	}
	return string(out)
}

// bareCommitCount is the number of commits on the bare repo's main
// (0 when the repo is absent or still unborn).
func (u *v1Install) bareCommitCount(t *testing.T, wi string) int {
	t.Helper()
	if _, err := os.Stat(filepath.Join(u.bareRepo(wi), "HEAD")); err != nil {
		return 0
	}
	cmd := exec.Command("git", "--git-dir", u.bareRepo(wi), "rev-list", "--count", "main")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "ambiguous argument") ||
			strings.Contains(string(out), "unknown revision") {
			return 0 // unborn branch: the chain has not started yet
		}
		t.Fatalf("rev-list count: %v: %s", err, out)
	}
	n, perr := strconv.Atoi(strings.TrimSpace(string(out)))
	if perr != nil {
		t.Fatalf("rev-list count %q: %v", out, perr)
	}
	return n
}

// waitBareCommits waits until the server-side chain holds at least n
// commits on main.
func (u *v1Install) waitBareCommits(t *testing.T, wi string, n int, bound time.Duration) {
	t.Helper()
	waitUntil(t, bound, fmt.Sprintf("bare repo of %s never reached %d commits (has %d)", wi, n, u.bareCommitCount(t, wi)),
		func() bool { return u.bareCommitCount(t, wi) >= n })
}

// capsuleCommitCount is the LOCAL chain length of one capsule: commits
// on main of the daemon-side external git dir (<dataDir>/git/<wi>.git).
// A failed push leaves its commit here — the backpressure scenarios'
// catch-up baseline (what the server-side bare repo must reach once the
// aihub is back).
func capsuleCommitCount(t *testing.T, dataDir, wi string) int {
	t.Helper()
	gitDir := filepath.Join(dataDir, "git", wi+".git")
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		return 0
	}
	cmd := exec.Command("git", "--git-dir", gitDir, "rev-list", "--count", "main")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "ambiguous argument") ||
			strings.Contains(string(out), "unknown revision") {
			return 0 // unborn branch: the chain has not started yet
		}
		t.Fatalf("capsule rev-list count: %v: %s", err, out)
	}
	n, perr := strconv.Atoi(strings.TrimSpace(string(out)))
	if perr != nil {
		t.Fatalf("capsule rev-list count %q: %v", out, perr)
	}
	return n
}

// bareSeqTrailers returns the Polyforge-Seq trailer values of main, in
// commit order (oldest first).
func (u *v1Install) bareSeqTrailers(t *testing.T, wi string) []int64 {
	t.Helper()
	if u.bareCommitCount(t, wi) == 0 {
		return nil
	}
	out := gitRun(t, u.bareRepo(wi), "log", "--reverse", "--format=%(trailers:key=Polyforge-Seq,valueonly)", "main")
	var seqs []int64
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			t.Fatalf("seq trailer %q: %v", line, err)
		}
		seqs = append(seqs, n)
	}
	return seqs
}

// bareHasPath reports whether the server-side chain's main tip carries
// the given path (the capsule revision chain content check).
func (u *v1Install) bareHasPath(t *testing.T, wi, path string) bool {
	t.Helper()
	if u.bareCommitCount(t, wi) == 0 {
		return false
	}
	out, err := func() (string, error) {
		cmd := exec.Command("git", "--git-dir", u.bareRepo(wi), "ls-tree", "--name-only", "-r", "main", path)
		cmd.Env = os.Environ()
		b, err := cmd.CombinedOutput()
		return string(b), err
	}()
	if err != nil {
		t.Fatalf("ls-tree: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out)) != ""
}

// bareFileAtMain dumps one file of the server-side chain's main tip.
func (u *v1Install) bareFileAtMain(t *testing.T, wi, path string) string {
	t.Helper()
	return gitRun(t, u.bareRepo(wi), "show", "main:"+path)
}

// ---------------------------------------------------------------------------
// Fake-agent journal + spawn journal readers (capsule-side ledgers).

// fakeJournal parses logs/fakepi.jsonl of one capsule.
func fakeJournal(t *testing.T, capsuleDir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capsuleDir, "logs", "fakepi.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("fakepi journal: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// spawnJournal parses runtime-state/spawn-journal.jsonl of one capsule.
func spawnJournal(t *testing.T, capsuleDir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capsuleDir, "runtime-state", "spawn-journal.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("spawn journal: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Orphan scan: no process may survive carrying a work item's identity.

// orphanPIDs lists live pids whose environ carries PF_WI_ID=<wi> (the
// layer-4 cleanup marker every derived agent is born with).
func orphanPIDs(wi string) []int {
	marker := "PF_WI_ID=" + wi
	var out []int
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(raw), "\x00") {
			if kv == marker {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// killOrphans hard-kills any surviving process carrying the work item's
// env marker (crash-scenario hygiene: a crashed daemon's agents are
// deliberately left alive by the scenario and must not leak past it).
func killOrphans(wi string) {
	for _, pid := range orphanPIDs(wi) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// httpStatusOf is a one-shot HTTP helper for raw wire assertions.
func httpStatusOf(method, url, user, secret string) (int, http.Header, string) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return -1, nil, err.Error()
	}
	if user != "" {
		req.SetBasicAuth(user, secret)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return -1, nil, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, string(body)
}
