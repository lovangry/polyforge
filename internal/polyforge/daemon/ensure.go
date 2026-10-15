package daemon

// ensure.go: `pf daemon ensure` (polyforge#174) — the ONLY way the daemon
// is started on a work machine. tools/install.sh calls it after an
// install or upgrade; the harness adapters (pi's pf-chat extension, the
// Claude Code SessionStart hook) call it when a human's session starts or
// resumes, which is what brings the daemon back after a container or
// machine restart (tmux does not survive one).
//
//   - Idempotent: a daemon answering GET /status on the configured socket
//     means nothing to do (exit 0).
//   - Otherwise a tmux session (supervisor.SessionName) runs the restart
//     loop: `pf daemon <canonical argv>`; a non-zero exit (crash, panic,
//     kill) restarts it after 5s, a clean exit (0: `pf ops shutdown`,
//     SIGTERM/SIGINT — the graceful sequence) ENDS the loop and the
//     session, so `pf ops shutdown` stops the daemon for good. ensure
//     waits until the socket answers. The daemon ignores SIGHUP (nohup):
//     killing the tmux session (a fallback for a stuck loop) never kills
//     the daemon itself.
//   - An flock on <data-dir>/ensure.lock serializes concurrent calls (a
//     pi-web restart resumes many sessions at once): the holder starts at
//     most one loop; a caller that does not get the lock waits for the
//     holder to finish and answers by the socket, starting nothing.
//     tools/install.sh holds the same lock across an upgrade (stop →
//     swap), so an adapter's ensure in that window starts nothing.
//   - A step agent (PF_WI_ID set) is refused outright.
//   - Without tmux it falls back to one unsupervised detached start and
//     says so loudly.
//
// The canonical argv is the one tools/install.sh used to launch with:
// --config, -machine-id (the recorded <data-dir>/daemon-id, else
// m-<short hostname>), -data-dir, -socket (the config's daemon_socket)
// and -workflows-dir <data-dir>/release/current/workflows. The harness
// root keeps its default (<data-dir>, polyforge#171); auto and the job
// cap stay the daemon's persisted settings (never forced on argv).

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/supervisor"
)

// ensureLockFile is the flock file serializing concurrent ensure calls.
const ensureLockFile = "ensure.lock"

// daemonLogFile is the daemon's log under its data dir (appended; the
// loop rotates it to daemon.log.1 past logRotateBytes).
const (
	daemonLogFile  = "daemon.log"
	logRotateBytes = 10 << 20
)

// restartDelay is the pause before a failed daemon is restarted.
const restartDelay = 5

// ensurePlan is everything one ensure run decided before acting.
type ensurePlan struct {
	pfBin        string
	configPath   string
	dataDir      string
	socket       string
	workflowsDir string
	machineID    string
	home         string
	timeout      time.Duration
	// refuse, when set, is why no daemon may be started (checked after
	// the socket probe: a live daemon is "nothing to do" regardless)
	refuse error
}

// daemonArgv is the canonical `pf daemon` argv (without the binary).
func (p *ensurePlan) daemonArgv() []string {
	return []string{"daemon",
		"--config", p.configPath,
		"-machine-id", p.machineID,
		"-data-dir", p.dataDir,
		"-socket", p.socket,
		"-workflows-dir", p.workflowsDir,
	}
}

func (p *ensurePlan) logPath() string { return filepath.Join(p.dataDir, daemonLogFile) }

// Ensure executes `pf daemon ensure [--config P] [--workflows-dir D]
// [--timeout D]` and returns the exit code: 0 the daemon answers on its
// socket, 1 it does not (or ensure refused to start it), 2 usage.
func Ensure(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pf daemon ensure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "shared config file (default ~/.config/polyforge/cli.toml)")
	workflowsDir := fs.String("workflows-dir", "", "workflows dir the daemon runs (default <data-dir>/release/current/workflows)")
	timeout := fs.Duration("timeout", 30*time.Second, "how long to wait for the daemon socket to answer")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pf daemon ensure: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if os.Getenv("PF_WI_ID") != "" {
		fmt.Fprintln(stderr, "pf daemon ensure: refused inside a step agent (PF_WI_ID is set): only a human's session starts the daemon")
		return 1
	}
	plan, err := planEnsure(*cfgPath, *workflowsDir, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "pf daemon ensure: %v\n", err)
		return 1
	}
	return plan.run(stdout, stderr)
}

// planEnsure resolves the canonical launch from the config and the
// environment. A config without a workspace mapping is refused here: the
// daemon would refuse to start, and a restart loop around it would only
// spin.
func planEnsure(cfgPath, workflowsDir string, timeout time.Duration) (*ensurePlan, error) {
	if cfgPath == "" {
		cfgPath = cli.DefaultConfigPath()
	}
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}
	shared, err := cli.LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	dataDir := os.Getenv("POLYFORGE_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share", "polyforge")
	}
	p := &ensurePlan{
		configPath: cfgPath,
		dataDir:    dataDir,
		socket:     shared.DaemonSocket,
		home:       home,
		timeout:    timeout,
	}
	p.workflowsDir = workflowsDir
	if p.workflowsDir == "" {
		p.workflowsDir = filepath.Join(dataDir, "release", "current", "workflows")
	}
	if abs, err := filepath.Abs(p.workflowsDir); err == nil {
		p.workflowsDir = abs
	}
	p.machineID = ensureMachineID(dataDir)
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot locate the pf binary: %w", err)
	}
	p.pfBin = exe
	if shared.Mapping.Empty() {
		p.refuse = errNoMapping{cfgPath}
	}
	return p, nil
}

// errNoMapping is the refusal for a config without a [workspace] section.
type errNoMapping struct{ path string }

func (e errNoMapping) Error() string {
	return fmt.Sprintf("not starting the daemon: %s has no [workspace.\"<path>\"] mapping, so the daemon would refuse to start — add one, check it with `pf doctor`, then run `pf daemon ensure` again", e.path)
}

var hostSanitizeRe = regexp.MustCompile(`[^a-z0-9-]`)

// ensureMachineID answers the recorded daemon id, else m-<short host>
// sanitized exactly as tools/install.sh always did.
func ensureMachineID(dataDir string) string {
	if raw, err := os.ReadFile(filepath.Join(dataDir, daemonIDFile)); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	return "m-" + hostSanitizeRe.ReplaceAllString(strings.ToLower(host), "-")
}

// daemonAnswers probes GET /status on the socket (short bound).
func daemonAnswers(sock string) (string, bool) {
	c := cli.NewDaemonClient(sock)
	c.HTTP.Timeout = 3 * time.Second
	raw, err := c.Status()
	if err != nil {
		return "", false
	}
	var st cli.StatusResponse
	_ = json.Unmarshal(raw, &st)
	return st.MachineID, true
}

func (p *ensurePlan) run(stdout, stderr io.Writer) int {
	if id, ok := daemonAnswers(p.socket); ok {
		fmt.Fprintf(stdout, "daemon %s answers on %s\n", id, p.socket)
		return 0
	}
	if p.refuse != nil {
		fmt.Fprintf(stderr, "pf daemon ensure: %v\n", p.refuse)
		return 1
	}
	if err := os.MkdirAll(p.dataDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "pf daemon ensure: %v\n", err)
		return 1
	}
	lock, err := os.OpenFile(filepath.Join(p.dataDir, ensureLockFile), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "pf daemon ensure: %v\n", err)
		return 1
	}
	defer lock.Close() // closing the fd drops the flock
	deadline := time.Now().Add(p.timeout)
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		// Another ensure is starting the daemon: wait for it to finish,
		// then answer by the socket — never start a second loop.
		fmt.Fprintln(stdout, "another `pf daemon ensure` is starting the daemon; waiting for it")
		for syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		return p.verdict(stdout, stderr)
	}
	// Holder: the daemon may have come up while we waited for the lock.
	if id, ok := daemonAnswers(p.socket); ok {
		fmt.Fprintf(stdout, "daemon %s answers on %s\n", id, p.socket)
		return 0
	}
	if st, err := os.Stat(p.workflowsDir); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "pf daemon ensure: not starting the daemon: the workflows dir %s is missing (run tools/install.sh, or pass --workflows-dir)\n", p.workflowsDir)
		return 1
	}
	if err := p.start(stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "pf daemon ensure: %v\n", err)
		return 1
	}
	for time.Now().Before(deadline) {
		if _, ok := daemonAnswers(p.socket); ok {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return p.verdict(stdout, stderr)
}

// verdict answers the exit code by the socket.
func (p *ensurePlan) verdict(stdout, stderr io.Writer) int {
	if id, ok := daemonAnswers(p.socket); ok {
		fmt.Fprintf(stdout, "daemon %s answers on %s\n", id, p.socket)
		return 0
	}
	fmt.Fprintf(stderr, "pf daemon ensure: the daemon does not answer on %s after %s; check %s (`pf doctor` names config problems)%s\n",
		p.socket, p.timeout, p.logPath(), logTail(p.logPath(), 10))
	return 1
}

// start launches the supervision loop in tmux (or, without tmux, one
// unsupervised daemon).
func (p *ensurePlan) start(stdout, stderr io.Writer) error {
	running, err := supervisor.LoopRunning()
	if errors.Is(err, supervisor.ErrNoTmux) {
		fmt.Fprintf(stderr, "WARNING: tmux is not installed: starting the daemon WITHOUT supervision — a crash is not restarted until the next `pf daemon ensure`; install tmux and run `pf ops shutdown`, then `pf daemon ensure`\n")
		return p.startDetached()
	}
	if running {
		// the loop is between two iterations (or its daemon is still
		// booting): it starts the daemon itself — never a second loop
		fmt.Fprintf(stdout, "supervision loop (tmux session %s) present; waiting for its daemon\n", supervisor.SessionName)
		return nil
	}
	out, err := supervisor.Tmux("new-session", "-d", "-s", supervisor.SessionName, "-c", p.home, "sh", "-c", p.loopScript())
	if err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, strings.TrimSpace(out))
	}
	fmt.Fprintf(stdout, "started the daemon supervision loop (tmux session %s; log %s)\n", supervisor.SessionName, p.logPath())
	return nil
}

// loopScript is the sh program the tmux session runs. Its environment is
// the TMUX SERVER's (its global environment, captured when the server
// started, plus the client's update-environment variables), not the
// ensure caller's: PATH and HOME are therefore pinned to the caller's,
// and the markers of a harness session (TMUX*, every CLAUDE*, PF_CHAT_ID,
// PF_WI_ID) are dropped so the daemon — and the step agents it spawns —
// never inherit them. Everything else (e.g. ANTHROPIC_*) is whatever the
// tmux server carries.
func (p *ensurePlan) loopScript() string {
	argv := []string{"nohup", shQuote(p.pfBin)}
	for _, a := range p.daemonArgv() {
		argv = append(argv, shQuote(a))
	}
	log := shQuote(p.logPath())
	var b strings.Builder
	fmt.Fprintf(&b, "export PATH=%s HOME=%s %s=%s\n", shQuote(os.Getenv("PATH")), shQuote(p.home), supervisor.MarkerEnv, shQuote("tmux:"+supervisor.SessionName))
	b.WriteString("unset TMUX TMUX_PANE PF_CHAT_ID PF_WI_ID\n")
	b.WriteString("for v in $(env | sed -n 's/^\\(CLAUDE[A-Za-z0-9_]*\\)=.*/\\1/p'); do unset \"$v\"; done\n")
	fmt.Fprintf(&b, "cd %s 2>/dev/null || cd /\n", shQuote(p.home))
	b.WriteString("while :; do\n")
	fmt.Fprintf(&b, "  if [ \"$(wc -c < %s 2>/dev/null || echo 0)\" -gt %d ]; then mv -f %s %s.1; fi\n", log, logRotateBytes, log, log)
	fmt.Fprintf(&b, "  echo \"pf daemon ensure loop: starting the daemon at $(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)\" >> %s\n", log)
	fmt.Fprintf(&b, "  %s < /dev/null >> %s 2>&1\n", strings.Join(argv, " "), log)
	b.WriteString("  rc=$?\n")
	fmt.Fprintf(&b, "  if [ \"$rc\" -eq 0 ]; then echo \"pf daemon ensure loop: the daemon exited cleanly; the loop ends\" >> %s; break; fi\n", log)
	fmt.Fprintf(&b, "  echo \"pf daemon ensure loop: the daemon failed (exit $rc); restarting in %ds\" >> %s\n", restartDelay, log)
	fmt.Fprintf(&b, "  sleep %d\n", restartDelay)
	b.WriteString("done\n")
	return b.String()
}

// startDetached starts one daemon in its own session (no tmux).
func (p *ensurePlan) startDetached() error {
	logf, err := os.OpenFile(p.logPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(p.pfBin, p.daemonArgv()...)
	cmd.Dir = p.home
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// shQuote single-quotes s for sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// logTail answers the last n lines of the log (empty when unreadable).
func logTail(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ""
	}
	if len(raw) > 64<<10 {
		raw = raw[len(raw)-64<<10:]
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "\n  " + strings.Join(lines, "\n  ")
}
