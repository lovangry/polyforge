package test

// usage_helpers_test.go: the REAL-BINARY dogfood harness (polyforge#17
// real-usage simulation round). Unlike harness_test.go (which boots the
// real components IN-PROCESS), these helpers exec the REAL BUILT
// BINARIES — cmd/aihub and cmd/pf (the merged work-machine binary: one-shot
// verbs plus the `daemon` subcommand, 00 §3.2 deployment-unit merge) — as
// actual operating-system processes, exactly the way an operator runs
// them, plus test/fakepi as the stand-in harness binary the daemon
// derives as the WI Agent (--pi-bin override, the injectable boundary of
// 04 §6.1 step 9).
//
// Hermetic: every test gets its own temp install (aihub data dir + daemon
// home + socket + XDG config home), a free loopback port, and cleanups
// that kill every process it started — even on failure. No default ports,
// no shared state, no network beyond loopback.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Binary build cache: built once per test-binary run, shared by all
// usage_* tests (building four binaries per test would dominate runtime).

var (
	usageBinOnce sync.Once
	usageBinDir  string
	usageBinErr  error
)

// usageBins builds aihub, pf and fakepi into a shared temp dir (three
// build targets: the work-machine binary is one file — the daemon runs
// as the `pf daemon` subcommand, not a separate binary).
func usageBins(t *testing.T) string {
	t.Helper()
	usageBinOnce.Do(func() {
		// Manual instrumented round (covdata measurement): pre-built
		// binaries (go build -cover -coverpkg=./...) replace the fresh
		// build, so the spawned aihub/pf/fakepi subprocesses emit covdata
		// into the caller's GOCOVERDIR (inherited via the environment).
		// Opt-in only: unset in every ordinary test run.
		if dir := os.Getenv("PF_USAGE_BIN_DIR"); dir != "" {
			usageBinDir = dir
			return
		}
		dir, err := os.MkdirTemp("", "pf17-dogfood-bins")
		if err != nil {
			usageBinErr = err
			return
		}
		// go test's cwd is the package dir (test/): build module targets.
		cmd := exec.Command("go", "build", "-o", dir+"/",
			"./cmd/aihub", "./cmd/pf", "./test/fakepi")
		cmd.Dir = ".." // module root
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			usageBinErr = fmt.Errorf("build binaries: %v: %s", err, out)
			return
		}
		usageBinDir = dir
	})
	if usageBinErr != nil {
		t.Fatalf("binary build cache: %v", usageBinErr)
	}
	return usageBinDir
}

// ---------------------------------------------------------------------------
// One real install: aihub process + admin key + XDG config + the pf CLI.

// usageInstall is a scratch install of the real system under temp dirs.
type usageInstall struct {
	t            *testing.T
	binDir       string
	dataDir      string
	url          string
	port         int
	adminKey     string
	aihubLog     string
	aihubCmd     *exec.Cmd
	xdgHome      string // XDG_CONFIG_HOME: <xdg>/polyforge/cli.toml
	cfgPath      string
	pfPath       string
	socketDir    string
	skillsDir    string // module skills/ checkout (daemon --skills-dir)
	workflowsDir string // module workflows/ checkout (daemon --workflows-dir, fix round 2: materialization source)
}

// newUsageInstall boots a real aihub on a free loopback port with its own
// data dir, harvests the FIRST-BOOT bootstrap admin key (the real-usage
// fix under test: a fresh install must be operable at all), writes the
// pf CLI's default config file, and creates the "usage" project.
func newUsageInstall(t *testing.T) *usageInstall {
	t.Helper()
	bins := usageBins(t)
	inst := &usageInstall{
		t:         t,
		binDir:    bins,
		dataDir:   t.TempDir(),
		aihubLog:  filepath.Join(t.TempDir(), "aihub.log"),
		xdgHome:   t.TempDir(),
		pfPath:    filepath.Join(bins, "pf"),
		socketDir: t.TempDir(),
	}
	// A free loopback port for the aihub HTTP face.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inst.port = ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	inst.url = "http://127.0.0.1:" + strconv.Itoa(inst.port)

	// The workflows dir: the module's real reference bundle.
	wf, err := filepath.Abs(filepath.Join("..", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	skills, err := filepath.Abs(filepath.Join("..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	inst.skillsDir = skills
	inst.workflowsDir = wf

	logf, err := os.Create(inst.aihubLog)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(bins, "aihub"),
		"--data-dir", inst.dataDir, "--listen", "127.0.0.1:"+strconv.Itoa(inst.port), "--workflows-dir", wf)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inst.aihubCmd = cmd
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		logf.Close()
	})

	// Wait for the HTTP face; harvest the bootstrap admin key from the log.
	inst.waitHTTP(t)
	inst.adminKey = harvestBootstrapKey(t, inst.aihubLog)

	// The CLI's DEFAULT config path (no --config flag anywhere): the
	// documented single location $XDG_CONFIG_HOME/polyforge/cli.toml.
	if err := os.MkdirAll(filepath.Join(inst.xdgHome, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst.cfgPath = filepath.Join(inst.xdgHome, "polyforge", "cli.toml")
	body := fmt.Sprintf("server = %q\napi_key = %q\ndaemon_socket = %q\nuser_id = \"usr_root\"\nproject = \"usage\"\n",
		inst.url, inst.adminKey, filepath.Join(inst.socketDir, "polyforge.sock"))
	if err := os.WriteFile(inst.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed the project through the real CLI (management verbs).
	inst.pf(t, "projects", "create", "--name", "usage")
	return inst
}

// waitHTTP polls the aihub TCP face until it answers.
func (u *usageInstall) waitHTTP(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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

// harvestBootstrapKey reads the one-time bootstrap admin key the aihub
// prints on the first boot of an empty deployment (cmd/aihub fix).
func harvestBootstrapKey(t *testing.T, logPath string) string {
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

// pf runs the REAL pf CLI against this install's default config path.
// Returns stdout; fails the test on a non-zero exit unless wantExit >= 0
// names the expected code.
func (u *usageInstall) pf(t *testing.T, args ...string) string {
	t.Helper()
	return u.pfExit(t, -1, args...)
}

// pfExit is pf with an expected exit code (use for error-path pins).
// wantExit < 0 means "must succeed".
func (u *usageInstall) pfExit(t *testing.T, wantExit int, args ...string) string {
	t.Helper()
	cmd := exec.Command(u.pfPath, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+u.xdgHome)
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
		t.Fatalf("pf %v: exit %d (stderr %q)", args, code, stderr.String())
	}
	if wantExit >= 0 && code != wantExit {
		t.Fatalf("pf %v: exit %d, want %d (stderr %q)", args, code, wantExit, stderr.String())
	}
	if code != 0 {
		// Error-path calls read the typed message from stderr.
		return strings.TrimSpace(stderr.String())
	}
	return stdout.String()
}

// jsonUnmarshalStrict decodes exactly one JSON value from s (trailing
// newline tolerated) — the CLI's --json mode must emit the server body
// as-is and nothing else on stdout.
func jsonUnmarshalStrict(s string, out any) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("empty output")
	}
	return json.Unmarshal([]byte(s), out)
}

// ---------------------------------------------------------------------------
// Real machine: one `pf daemon` process (+ its WI Agents) over the install.

// usageMachine is one real daemon process with its own machine identity,
// home, socket and api key.
type usageMachine struct {
	t        *testing.T
	inst     *usageInstall
	id       string
	userID   string
	apiKey   string
	home     string
	socket   string
	spawnLog string
	keyFile  string
	cmd      *exec.Cmd
	logPath  string
}

// newUsageMachine registers a machine through the REAL CLI (the row-21
// operator verb), writes its one-time key to a 0600 file, and keeps it.
func (u *usageInstall) newUsageMachine(t *testing.T, machineID string) *usageMachine {
	t.Helper()
	raw := u.pf(t, "--json", "machines", "register", machineID, "--version", "usage-test", "--display-name", machineID)
	var reg struct {
		UserID string `json:"user_id"`
		APIKey string `json:"api_key"`
	}
	if err := jsonUnmarshalStrict(raw, &reg); err != nil || reg.APIKey == "" {
		t.Fatalf("machines register: bad answer %q (%v)", raw, err)
	}
	m := &usageMachine{
		t: t, inst: u, id: machineID, userID: reg.UserID, apiKey: reg.APIKey,
		home:     t.TempDir(),
		spawnLog: filepath.Join(t.TempDir(), "spawns.log"),
		logPath:  filepath.Join(t.TempDir(), "daemon.log"),
	}
	// The daemon home needs the workflows/skills checkout dirs configured
	// via flags; the socket lives in the install's socket dir.
	m.socket = filepath.Join(u.socketDir, machineID+".sock")
	keyFile := filepath.Join(m.home, "machine.key")
	if err := os.WriteFile(keyFile, []byte(m.apiKey), 0o600); err != nil {
		t.Fatal(err)
	}
	m.keyFile = keyFile
	// Data-plane reads (progress read-through, the daemon-side observe
	// layer, the terminal precheck) need project membership — the M3
	// operator duty of 02 §4.10 (machine keys claim, membership reads).
	u.pf(t, "projects", "members", "add", "usage", "--user", m.userID, "--role", "writer")
	return m
}

// start launches the REAL daemon: the `pf daemon` subcommand of the
// merged work-machine binary. mode knobs (FAKEPI_MODE=ask) are
// inherited by the WI Agent child (04 §6.1a env).
func (m *usageMachine) start(extraEnv ...string) {
	m.startWithArgs(nil, extraEnv...)
}

// startWithArgs is start() with extra daemon argv. The Batch wave
// scenarios need `--project`: the daemon's recommendation face
// (mode(get_recommendation)) reads the ready queue of the CONFIGURED
// project, and a daemon started without it cannot serve a wave at all.
func (m *usageMachine) startWithArgs(extraArgs []string, extraEnv ...string) {
	m.t.Helper()
	if m.cmd != nil {
		m.t.Fatal("machine daemon already started")
	}
	logf, err := os.Create(m.logPath)
	if err != nil {
		m.t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(m.inst.binDir, "pf"), append([]string{"daemon",
		"--aihub-url", m.inst.url,
		"--api-key-file", m.keyFile,
		"--machine-id", m.id,
		"--home", m.home,
		"--socket", m.socket,
		"--pi-bin", filepath.Join(m.inst.binDir, "fakepi"),
		"--skills-dir", m.inst.skillsDir,
		"--workflows-dir", m.inst.workflowsDir,
	}, extraArgs...)...)
	cmd.Env = append(append(os.Environ(), extraEnv...),
		"FAKEPI_SPAWN_LOG="+m.spawnLog,
	)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		m.t.Fatal(err)
	}
	m.cmd = cmd
	m.t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		m.killOrphanAgents()
		logf.Close()
	})
	m.waitSocket()
}

// waitSocket polls the daemon's UDS face.
func (m *usageMachine) waitSocket() {
	m.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("unix", m.socket)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("daemon %s face never came up: %v", m.id, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnCount returns the number of WI Agent processes the daemon derived
// (the fake harness journals every process start).
func (m *usageMachine) spawnCount() int {
	raw, err := os.ReadFile(m.spawnLog)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// spawnSession returns the session id of the n-th (1-based) spawn.
func (m *usageMachine) spawnSession(n int) string {
	m.t.Helper()
	raw, err := os.ReadFile(m.spawnLog)
	if err != nil {
		m.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if n < 1 || n > len(lines) {
		m.t.Fatalf("spawn %d not recorded (have %d lines)", n, len(lines))
	}
	f := strings.Fields(lines[n-1])
	if len(f) < 2 {
		m.t.Fatalf("spawn line undecodable: %q", lines[n-1])
	}
	return f[1]
}

// agentPIDs returns the pids the fake harness journals (all derived
// agents of this machine, across daemon lives).
func (m *usageMachine) agentPIDs() []int {
	raw, err := os.ReadFile(m.spawnLog)
	if err != nil {
		return nil
	}
	var out []int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}

// killOrphanAgents reaps any WI Agent process this machine's daemons left
// alive (crash drills deliberately orphan them).
func (m *usageMachine) killOrphanAgents() {
	for _, pid := range m.agentPIDs() {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
}

// kill9 SIGKILLs the daemon process (the crash drills; no shutdown path).
func (m *usageMachine) kill9() {
	m.t.Helper()
	if m.cmd == nil || m.cmd.Process == nil {
		m.t.Fatal("no daemon to kill")
	}
	if err := m.cmd.Process.Kill(); err != nil {
		m.t.Fatalf("kill -9: %v", err)
	}
	m.cmd.Wait()
	m.cmd = nil
}

// daemonLog dumps the daemon's log on failure.
func (m *usageMachine) daemonLog() string {
	raw, _ := os.ReadFile(m.logPath)
	return string(raw)
}

// pfOn runs the CLI against THIS machine's socket (config socket override).
func (m *usageMachine) pfOn(args ...string) string {
	m.t.Helper()
	return m.inst.pfExit(m.t, -1, append([]string{"--socket", m.socket}, args...)...)
}

// pfOnExit is pfOn with an expected exit code.
func (m *usageMachine) pfOnExit(wantExit int, args ...string) string {
	m.t.Helper()
	return m.inst.pfExit(m.t, wantExit, append([]string{"--socket", m.socket}, args...)...)
}

// ---------------------------------------------------------------------------
// Common small pins.

// wiCreate files one execution work item via the CLI and returns its id.
func (u *usageInstall) wiCreate(t *testing.T, goal string) string {
	t.Helper()
	raw := u.pf(t, "--json", "file", "create", "--project", "usage", "--goal", goal, "--bundle", "feature-full")
	var out struct {
		ID string `json:"id"`
	}
	if err := jsonUnmarshalStrict(raw, &out); err != nil || out.ID == "" {
		t.Fatalf("file create: %q (%v)", raw, err)
	}
	return out.ID
}

// wiStatus reads the work item status through the real CLI.
func (u *usageInstall) wiStatus(t *testing.T, wi string) string {
	t.Helper()
	raw := u.pf(t, "--json", "file", "show", wi)
	var d struct {
		Status string `json:"status"`
	}
	if err := jsonUnmarshalStrict(raw, &d); err != nil {
		t.Fatalf("file show %s: %q", wi, raw)
	}
	return d.Status
}

// readyQueueIDs returns the executable ids of the ready queue via the CLI.
func (u *usageInstall) readyQueueIDs(t *testing.T) []string {
	t.Helper()
	raw := u.pf(t, "--json", "queue", "--ready")
	var q struct {
		Executable []struct {
			ID string `json:"id"`
		} `json:"executable"`
	}
	if err := jsonUnmarshalStrict(raw, &q); err != nil {
		t.Fatalf("queue --ready: %q", raw)
	}
	ids := make([]string, 0, len(q.Executable))
	for _, e := range q.Executable {
		ids = append(ids, e.ID)
	}
	return ids
}

// waitUntilUse polls cond with a bounded deadline (shared shape with the
// in-process harness but a local name to keep the file self-contained).
func waitUntilUse(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// runPfWithXDG runs the real pf CLI with an explicit XDG_CONFIG_HOME
// (config-path experiments) and returns stdout (failing the test on a
// non-zero exit).
func runPfWithXDG(t *testing.T, inst *usageInstall, xdg string, args ...string) string {
	t.Helper()
	cmd := execPf(inst, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+xdg)
	return runPfCmd(t, cmd, args...)
}

// execPf builds the exec.Cmd for the real pf binary with default env.
func execPf(inst *usageInstall, args ...string) *exec.Cmd {
	return exec.Command(inst.pfPath, args...)
}

// runPfCmd runs the command, failing the test on non-zero exit; returns
// stdout (error paths read stderr via pfExit instead).
func runPfCmd(t *testing.T, cmd *exec.Cmd, args ...string) string {
	t.Helper()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("pf %v: exit %d (stderr %q)", args, ee.ExitCode(), stderr.String())
		}
		t.Fatalf("pf %v: %v", args, err)
	}
	return stdout.String()
}

// filepathJoin is filepath.Join, re-exposed for the scenario files.
func filepathJoin(parts ...string) string { return filepath.Join(parts...) }

// pfIO runs the real pf CLI and returns (stdout, stderr) without failing
// on the exit code (scenario files assert on the streams themselves).
func (u *usageInstall) pfIO(args ...string) (string, string) {
	cmd := exec.Command(u.pfPath, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+u.xdgHome)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	return stdout.String(), stderr.String()
}

// procSignal0 probes a real process with signal 0 (nil = alive).
func procSignal0(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}

// writeFileUsage writes a small file with parent dirs (agent-side work).
func writeFileUsage(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// killAgentUsage SIGKILLs one real agent process (the crash drills).
func killAgentUsage(t *testing.T, pid int) {
	t.Helper()
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}

// daemonExitedStatus holds each reaped daemon child's ACTUAL process
// exit status, keyed by the *exec.Cmd: the Wait4 probe below REAPS the
// child on the observing call, so the status must be captured at that
// moment — it cannot be read afterwards (a later Wait would only ECHILD).
var daemonExitedStatus = map[*exec.Cmd]int{}

// daemonExitedUsage reports whether the daemon child has exited (a
// zombie counts as exited — the test process has not reaped it yet; the
// kill/kill-0 probe cannot tell the two apart). Once exit is observed
// the child is reaped and its ACTUAL exit status is recorded in
// daemonExitedStatus: callers must assert it (daemonAssertExit0Usage)
// instead of treating "exited" as enough — the process-level exit-0
// guarantee of the shutdown paths is exactly what a supervisor
// (systemd Restart=on-failure) reads.
func daemonExitedUsage(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	if _, reaped := daemonExitedStatus[cmd]; reaped {
		return true // already observed and reaped: keep answering exited
	}
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WNOHANG, nil)
	if err != nil {
		return false // ECHILD etc.: treat as still running (probe below)
	}
	if pid == cmd.Process.Pid {
		daemonExitedStatus[cmd] = ws.ExitStatus() // <0 on signal death
		return true
	}
	return false
}

// daemonAssertExit0Usage asserts the reaped daemon child's ACTUAL exit
// status is 0 — the exit-0 guarantee of BOTH graceful shutdown paths
// (signal-driven and ops-face). Before the RH-1 fix main fell through
// to the CLI verb dispatcher after runDaemon returned, printing
// `unknown command "daemon"` and exiting 2; a signal death records a
// negative status. Either way this assertion fails, so that regression
// can never pass again.
func daemonAssertExit0Usage(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if !daemonExitedUsage(cmd) {
		t.Fatal("daemon child has not exited yet")
	}
	if code := daemonExitedStatus[cmd]; code != 0 {
		t.Fatalf("daemon process exit status = %d, want 0 (graceful shutdown exit-0 guarantee; <0 = signal death)", code)
	}
}
