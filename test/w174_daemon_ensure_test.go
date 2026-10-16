package test

// w174 (polyforge#174): `pf daemon ensure` — the only way the daemon is
// started — driven end to end with the REAL pf binary, the REAL daemon
// and a REAL tmux on a PRIVATE server (TMUX_TMPDIR is a scratch dir and
// TMUX is unset: the operator's tmux server and sessions are never
// reachable). HOME, the data dir, the socket and the config are scratch;
// the aihub server is a dead address (the daemon serves its socket
// without it). Every test kills its tmux server and daemons on cleanup;
// the package's process-hygiene check fails the run on any leak.
//
// Pinned: idempotence (a live daemon is left alone), the canonical argv
// and the supervision marker, concurrency (N simultaneous ensures → one
// loop, one daemon), the loop reviving a killed daemon within ~5s, a
// graceful shutdown (`pf ops shutdown`, SIGTERM) ending the loop with the
// daemon, a killed loop session (install.sh's fallback for a leftover
// loop) leaving its daemon running for a graceful stop, the no-mapping
// refusal and the no-tmux fallback.

import (
	"bytes"
	"context"
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
)

// w174Env is one hermetic machine: scratch HOME/data/config, private tmux.
type w174Env struct {
	pf      string
	home    string
	data    string
	sock    string
	cfg     string
	tmuxDir string
	path    string
}

// w174NewEnv lays out a machine with a real [workspace] mapping and the
// release layout install.sh maintains (release/current -> v0, workflows/).
func w174NewEnv(t *testing.T, withMapping bool) *w174Env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	e := &w174Env{pf: filepath.Join(v1Bins(t), "pf"), home: t.TempDir(), tmuxDir: t.TempDir(), path: os.Getenv("PATH")}
	e.data = filepath.Join(e.home, ".local", "share", "polyforge")
	e.sock = filepath.Join(e.data, "daemon.sock")
	e.cfg = filepath.Join(e.home, ".config", "polyforge", "cli.toml")
	ws := filepath.Join(e.home, "ws")
	for _, d := range []string{ws, filepath.Dir(e.cfg), filepath.Join(e.data, "release", "v0", "workflows")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("v0", filepath.Join(e.data, "release", "current")); err != nil {
		t.Fatal(err)
	}
	body := "server = \"http://127.0.0.1:1\"\napi_key = \"w174key\"\ndaemon_socket = \"" + e.sock + "\"\n"
	if withMapping {
		body += "\n[workspace.\"" + ws + "\"]\nprojects = [\"e2e\"]\n"
	}
	if err := os.WriteFile(e.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.teardown() })
	return e
}

// env answers the process environment of every command of this machine.
func (e *w174Env) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		switch k {
		case "TMUX", "TMUX_PANE", "HOME", "TMUX_TMPDIR", "POLYFORGE_DATA_DIR", "PATH", "XDG_CONFIG_HOME",
			// anything that could point a run at the real harness dirs or
			// the real daemon / machine
			"PI_CODING_AGENT_DIR", "CLAUDE_CONFIG_DIR", "POLYFORGE_HARNESS_ROOT", "POLYFORGE_OPERATOR_AGENT_DIR",
			"POLYFORGE_MACHINE_ID", "POLYFORGE_DISPLAY_NAME", "XDG_DATA_HOME", "XDG_STATE_HOME":
			continue
		}
		// PF_*: socket, server, key, and install.sh's PF_BIN / PF_HOST /
		// PF_INSTALL_FORCE (a test sets what it needs explicitly)
		if strings.HasPrefix(k, "PF_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+e.home, "TMUX_TMPDIR="+e.tmuxDir, "POLYFORGE_DATA_DIR="+e.data,
		"PATH="+e.path, "XDG_CONFIG_HOME="+filepath.Join(e.home, ".config"))
}

// ensure runs `pf daemon ensure --config <cfg>` and answers (exit, stdout, stderr).
func (e *w174Env) ensure(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.pf, append([]string{"daemon", "ensure", "--config", e.cfg}, extra...)...)
	cmd.Env = e.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf daemon ensure: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errb.String()
}

// tmux runs tmux against this machine's private server.
func (e *w174Env) tmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Env = e.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sessions lists the private tmux server's sessions.
func (e *w174Env) sessions() []string {
	out, err := e.tmux("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// daemons answers the pids of `pf daemon` processes serving this data dir.
func (e *w174Env) daemons() []int {
	var pids []int
	ents, _ := os.ReadDir("/proc")
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		argv := w174Argv(pid)
		if len(argv) < 2 || argv[1] != "daemon" || contains(argv, "ensure") {
			continue
		}
		if contains(argv, e.data) {
			if st, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "stat")); err == nil && w174Zombie(string(st)) {
				continue
			}
			pids = append(pids, pid)
		}
	}
	return pids
}

func w174Argv(pid int) []string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

func w174Zombie(stat string) bool {
	i := strings.LastIndex(stat, ")")
	return i > 0 && len(stat) > i+2 && stat[i+2] == 'Z'
}

// answers reports whether the daemon serves GET /status on the socket.
func (e *w174Env) answers() bool {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", e.sock)
		}}}
	resp, err := c.Get("http://daemon/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// shutdown asks the daemon for its graceful shutdown.
func (e *w174Env) shutdown(t *testing.T) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", e.sock)
		}}}
	resp, err := c.Post("http://daemon/shutdown", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /shutdown: %v", err)
	}
	resp.Body.Close()
}

// teardown kills the private tmux server, then every daemon of this
// machine, and waits until they are gone.
func (e *w174Env) teardown() {
	_, _ = e.tmux("kill-server")
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids := e.daemons()
		if len(pids) == 0 || time.Now().After(deadline) {
			return
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// oneDaemon waits until exactly one daemon of this machine answers and
// answers its pid.
func (e *w174Env) oneDaemon(t *testing.T, bound time.Duration) int {
	t.Helper()
	var pids []int
	waitUntil(t, bound, "exactly one answering daemon", func() bool {
		pids = e.daemons()
		return len(pids) == 1 && e.answers()
	})
	return pids[0]
}

func TestW174EnsureIdempotentCanonicalArgv(t *testing.T) {
	e := w174NewEnv(t, true)
	code, out, errOut := e.ensure(t)
	if code != 0 {
		t.Fatalf("first ensure: exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if !e.answers() {
		t.Fatal("ensure returned 0 but the socket does not answer")
	}
	if s := e.sessions(); len(s) != 1 || s[0] != "polyforge-daemon" {
		t.Fatalf("tmux sessions = %v, want exactly [polyforge-daemon]", s)
	}
	pid := e.oneDaemon(t, 5*time.Second)

	// the canonical argv install.sh used, and the supervision marker
	argv := strings.Join(w174Argv(pid), " ")
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	for _, want := range []string{
		"daemon --config " + e.cfg,
		"-machine-id m-",
		"-data-dir " + e.data,
		"-socket " + e.sock,
		"-workflows-dir " + filepath.Join(e.data, "release", "current", "workflows"),
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("daemon argv %q lacks %q", argv, want)
		}
	}
	for _, banned := range []string{"-auto", "-max-concurrent-jobs", "-workspace", "--project"} {
		if strings.Contains(argv, banned) {
			t.Fatalf("daemon argv %q carries %q", argv, banned)
		}
	}
	environ, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if !bytes.Contains(environ, []byte("POLYFORGE_SUPERVISOR=tmux:polyforge-daemon\x00")) {
		t.Fatal("the loop's daemon lacks the POLYFORGE_SUPERVISOR marker")
	}
	if bytes.Contains(environ, []byte("\x00TMUX=")) {
		t.Fatal("the loop's daemon inherited TMUX (its steps would reach the tmux server)")
	}

	// second ensure: nothing to do
	start := time.Now()
	code, out, _ = e.ensure(t)
	if code != 0 || !strings.Contains(out, "answers on") {
		t.Fatalf("second ensure: exit %d stdout %q", code, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("second ensure took %s: a live daemon must be a fast no-op", time.Since(start))
	}
	if again := e.oneDaemon(t, 2*time.Second); again != pid {
		t.Fatalf("second ensure replaced the daemon: pid %d -> %d", pid, again)
	}
	if s := e.sessions(); len(s) != 1 {
		t.Fatalf("second ensure changed the tmux sessions: %v", s)
	}
}

func TestW174EnsureConcurrentStartsOneLoopOneDaemon(t *testing.T) {
	e := w174NewEnv(t, true)
	const n = 6
	codes := make([]int, n)
	outs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, o, eo := e.ensure(t)
			codes[i], outs[i] = c, o+eo
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 0 {
			t.Fatalf("ensure #%d: exit %d (%s)", i, c, outs[i])
		}
	}
	if s := e.sessions(); len(s) != 1 || s[0] != "polyforge-daemon" {
		t.Fatalf("tmux sessions = %v, want exactly one loop", s)
	}
	e.oneDaemon(t, 5*time.Second)
	// settle: no second daemon appears afterwards either
	time.Sleep(1500 * time.Millisecond)
	if pids := e.daemons(); len(pids) != 1 {
		t.Fatalf("daemons after settling = %v, want one", pids)
	}
	started := 0
	for _, o := range outs {
		if strings.Contains(o, "started the daemon supervision loop") {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("%d ensure calls started a loop, want exactly 1:\n%s", started, strings.Join(outs, "\n---\n"))
	}
}

func TestW174LoopRestartsKilledDaemon(t *testing.T) {
	e := w174NewEnv(t, true)
	if code, out, errOut := e.ensure(t); code != 0 {
		t.Fatalf("ensure: exit %d %s %s", code, out, errOut)
	}
	pid := e.oneDaemon(t, 5*time.Second)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	waitUntil(t, 15*time.Second, "the loop never revived the killed daemon", func() bool {
		pids := e.daemons()
		return len(pids) == 1 && pids[0] != pid && e.answers()
	})
	// ~5s pause + the daemon's own boot
	if took := time.Since(killed); took < 4*time.Second || took > 12*time.Second {
		t.Fatalf("revived after %s, want ~5s (the loop's sleep)", took)
	}
	if s := e.sessions(); len(s) != 1 {
		t.Fatalf("tmux sessions = %v", s)
	}
}

// TestW174GracefulStopEndsTheLoop: `pf ops shutdown` (and SIGTERM) under
// the loop exits 0, which ends the loop: no daemon, no session, nothing
// revived after the restart pause.
func TestW174GracefulStopEndsTheLoop(t *testing.T) {
	e := w174NewEnv(t, true)
	for _, how := range []string{"pf ops shutdown", "SIGTERM"} {
		if code, out, errOut := e.ensure(t); code != 0 {
			t.Fatalf("ensure: exit %d %s %s", code, out, errOut)
		}
		pid := e.oneDaemon(t, 5*time.Second)
		if how == "SIGTERM" {
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		} else {
			cmd := exec.Command(e.pf, "--config", e.cfg, "ops", "shutdown")
			cmd.Env = e.env()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("pf ops shutdown: %v %s", err, out)
			}
		}
		waitUntil(t, 30*time.Second, how+": the daemon and its loop did not end", func() bool {
			return len(e.daemons()) == 0 && len(e.sessions()) == 0
		})
		time.Sleep(7 * time.Second) // longer than the restart pause
		if pids, s := e.daemons(), e.sessions(); len(pids) != 0 || len(s) != 0 {
			t.Fatalf("%s: revived after a clean exit (daemons %v, sessions %v)", how, pids, s)
		}
		log, _ := os.ReadFile(filepath.Join(e.data, "daemon.log"))
		if !strings.Contains(string(log), "exited cleanly; the loop ends") {
			t.Fatalf("%s: the loop did not log its clean end:\n%s", how, log)
		}
		_ = os.Remove(filepath.Join(e.data, "daemon.log"))
	}
}

// TestW174KilledLoopLeavesDaemonForGracefulStop pins install.sh's
// fallback for a leftover loop: killing the loop's tmux session leaves
// the daemon running (it ignores the hangup); the graceful shutdown then
// ends it and nothing revives it.
func TestW174KilledLoopLeavesDaemonForGracefulStop(t *testing.T) {
	e := w174NewEnv(t, true)
	if code, out, errOut := e.ensure(t); code != 0 {
		t.Fatalf("ensure: exit %d %s %s", code, out, errOut)
	}
	pid := e.oneDaemon(t, 5*time.Second)
	if out, err := e.tmux("kill-session", "-t", "=polyforge-daemon"); err != nil {
		t.Fatalf("kill-session: %v %s", err, out)
	}
	time.Sleep(1500 * time.Millisecond)
	if pids := e.daemons(); len(pids) != 1 || pids[0] != pid || !e.answers() {
		t.Fatalf("killing the loop took the daemon down (daemons %v, answers %v): it must keep running for the graceful stop", pids, e.answers())
	}
	e.shutdown(t)
	waitUntil(t, 30*time.Second, "the daemon did not exit after the graceful shutdown", func() bool { return len(e.daemons()) == 0 })
	time.Sleep(7 * time.Second) // longer than the loop's pause
	if pids := e.daemons(); len(pids) != 0 {
		t.Fatalf("a daemon came back after the loop was stopped: %v", pids)
	}
}

func TestW174EnsureRefusesWithoutMapping(t *testing.T) {
	e := w174NewEnv(t, false)
	code, _, errOut := e.ensure(t)
	if code != 1 || !strings.Contains(errOut, "[workspace") {
		t.Fatalf("ensure without a mapping: exit %d stderr %q, want 1 naming the mapping", code, errOut)
	}
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("a loop was started for a config the daemon refuses: %v", s)
	}
	if pids := e.daemons(); len(pids) != 0 {
		t.Fatalf("a daemon was started: %v", pids)
	}
}

func TestW174EnsureWithoutTmuxStartsOneUnsupervisedDaemon(t *testing.T) {
	e := w174NewEnv(t, true)
	// a PATH carrying the daemon's tools but no tmux
	bin := t.TempDir()
	for _, tool := range []string{"git", "sh"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.path = bin
	code, out, errOut := e.ensure(t)
	if code != 0 {
		t.Fatalf("ensure without tmux: exit %d %s %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "WARNING: tmux is not installed") {
		t.Fatalf("no loud warning without tmux: stderr %q", errOut)
	}
	e.oneDaemon(t, 5*time.Second)
	e.path = os.Getenv("PATH")
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("tmux sessions without tmux on PATH: %v", s)
	}
}
