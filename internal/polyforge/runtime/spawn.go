// spawn.go: the WI-agent spawner for the pi harness. Ports the v0
// supervisor's LIVE-TESTED RPC protocol layer: stdin carries one JSON
// command object per line ({"id":..,"type":"prompt","message":..}), stdout
// is a record stream where the assistant message_end record is the ack
// carrier, response records correlate command ids, and daemon→agent control
// lines ride prompt commands whose message text is the control JSON
// VERBATIM. Isolation, env sanitization and the spawn journal land here.
package runtime

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// piIsolationFlags is the WI-agent birth isolation profile: the named flag
// set that keeps a spawned agent from inheriting the OPERATOR's global
// harness configuration (global extensions, skills, context files and
// prompt templates). The agent's only instruction of record is the initial
// prompt injected at derivation; a capsule's materialized step-agent
// definitions are a different resource family and stay reachable.
var piIsolationFlags = []string{
	"--no-extensions",
	"--no-skills",
	"--no-context-files",
	"--no-prompt-templates",
}

// piCommand is one stdin command of the real pi RPC protocol. Every
// command MAY carry a string id; the matching response record repeats it.
// streamingBehavior "steer" queues a control prompt for delivery after the
// current assistant turn — the safe-boundary discipline the control
// contract demands; the initial prompt needs none (the agent is idle at
// derivation).
type piCommand struct {
	ID                string `json:"id,omitempty"`
	Type              string `json:"type"` // "prompt"
	Message           string `json:"message"`
	StreamingBehavior string `json:"streamingBehavior,omitempty"`
}

// stdout record probe shapes: response records repeat our command ids;
// message_end is the authoritative final assistant message (the ack
// carrier — message_update deltas stream the same text piecemeal and are
// never scanned, so one ack is never queued twice).
type stdoutRecord struct {
	Type    string          `json:"type"`
	Command string          `json:"command"` // "prompt" on response records
	ID      *string         `json:"id"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Message json.RawMessage `json:"message"`
}

type assistantMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ---------------------------------------------------------------------------
// Env sanitize point: the ONE place every spawned agent's environment is
// built. AIHub credentials must never reach a WI agent's env/cwd/argv, so
// the inherited environ is filtered (blacklist, not an invented allowlist —
// the harness needs PATH/HOME and its own config vars): every POLYFORGE_*
// and PF_* variable is stripped, and belt-and-braces, any variable whose
// VALUE equals a configured secret is stripped too, so even a RENAMED
// variable cannot smuggle it.

type envSanitizer struct {
	secrets []string // configured secret values (api key, push credential)
}

func newEnvSanitizer(secrets ...string) *envSanitizer {
	var kept []string
	for _, s := range secrets {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return &envSanitizer{secrets: kept}
}

func (e *envSanitizer) childEnv(additions ...string) []string {
	var secrets []string
	if e != nil {
		secrets = e.secrets
	}
	out := make([]string, 0, len(os.Environ())+len(additions))
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "POLYFORGE_") || strings.HasPrefix(name, "PF_") {
			continue
		}
		leak := false
		for _, s := range secrets {
			if subtle.ConstantTimeCompare([]byte(val), []byte(s)) == 1 {
				leak = true
				break
			}
		}
		if !leak {
			out = append(out, kv)
		}
	}
	return append(out, additions...)
}

// ---------------------------------------------------------------------------
// SpawnParams / AgentHandle

// SpawnParams describes one agent derivation.
type SpawnParams struct {
	WorkItemID     string
	CapsuleDir     string
	SessionID      string
	TranscriptPath string // resume source; empty for a fresh derivation
	Resume         bool   // attach to TranscriptPath instead of a new session
	Generation     int64
	InitialPrompt  string
	CgroupPath     string // per-WI cgroup v2 dir; "" = layer 0 unavailable
	Journal        func(pid int, startedAt string) error
}

// AgentHandle is the supervised face over one derived agent process.
type AgentHandle interface {
	PID() int
	StartedAt() string
	Alive() bool
	Done() bool
	Terminate(bound time.Duration) error
	SendControl(m protocol.ControlMsg) error
	// WaitControlAck awaits the agent's ack of one control method.
	WaitControlAck(ctx context.Context, method string, bound time.Duration) error
}

// Spawner derives agent processes; tests substitute fakes that spawn real
// but trivial child processes.
type Spawner interface {
	Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error)
}

// piSpawner is the production spawner for the pi harness.
type piSpawner struct {
	cfg Config
}

func newPiSpawner(cfg Config) *piSpawner {
	return &piSpawner{cfg: cfg}
}

func (s *piSpawner) bin() string {
	if s.cfg.PiBin != "" {
		return s.cfg.PiBin
	}
	return "pi"
}

// buildCmd constructs (without starting) the argv/env/cwd of one
// derivation. Exported shape for the isolation regression: argv carries
// the transport first, then the isolation flags, then the resume session.
func (s *piSpawner) buildCmd(p SpawnParams) *exec.Cmd {
	args := []string{"--mode", "rpc"}
	args = append(args, piIsolationFlags...)
	if p.Resume {
		args = append(args, "--session", p.TranscriptPath)
	}
	cmd := exec.Command(s.bin(), args...)
	cmd.Dir = p.CapsuleDir // cwd = capsule root
	cmd.Env = newEnvSanitizer(s.cfg.EnvSecrets...).childEnv(
		"PI_SESSION_DIR="+filepath.Join(p.CapsuleDir, capsule.DirSession),
		"PF_WI_ID="+p.WorkItemID, // layer-4 cleanup marker (deliberate addition)
		"PF_SESSION_ID="+p.SessionID,
		"PF_GENERATION="+strconv.FormatInt(p.Generation, 10),
	)
	return cmd
}

// Spawn derives one persistent agent process: process group (the agent is
// its own group leader so the cleanup layers can collect step agents and
// command children), cgroup enrollment (best-effort layer-0), identity
// journal BEFORE the spawn returns, then the initial prompt as the
// session's first command.
func (s *piSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	if p.InitialPrompt == "" {
		return nil, fmt.Errorf("runtime: spawn without an initial prompt")
	}
	cmd := s.buildCmd(p)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runtime: harness start failed: %w", err)
	}
	proc := newProc(cmd, stdin, stdout)
	// Layer-0 enrollment: move the agent (and with it every descendant) into
	// the per-WI cgroup. Unavailable cgroups degrade silently — layers 1-4
	// remain the kill path.
	if p.CgroupPath != "" {
		if err := os.WriteFile(filepath.Join(p.CgroupPath, "cgroup.procs"),
			[]byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: cgroup enrollment unavailable, layer 0 degrades: %v\n", p.WorkItemID, err)
		}
	}
	// Identity journal before Spawn returns: a crash between derivation and
	// the runtime's persisted record must never leave an undiscoverable
	// orphan. Its failure tears the fresh process down.
	if p.Journal != nil {
		if jerr := p.Journal(cmd.Process.Pid, proc.startedAt); jerr != nil {
			_ = proc.Terminate(5 * time.Second)
			return nil, fmt.Errorf("runtime: spawn journal failed (process torn down): %w", jerr)
		}
	}
	if err := proc.writeCommand(piCommand{ID: "pf-init", Type: "prompt", Message: p.InitialPrompt}); err != nil {
		_ = proc.Terminate(5 * time.Second)
		return nil, fmt.Errorf("runtime: initial prompt injection failed (process torn down): %w", err)
	}
	return proc, nil
}

// writeSpawnJournal appends one spawn identity line to the capsule's
// runtime-state/spawn-journal.jsonl.
func writeSpawnJournal(p SpawnParams, pid int, startedAt string, instance string) error {
	entry := map[string]any{
		"at":         nowStamp(),
		"work_item":  p.WorkItemID,
		"instance":   instance,
		"pid":        pid,
		"started_at": startedAt,
		"session_id": p.SessionID,
		"resume":     p.Resume,
		"transcript": p.TranscriptPath,
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := filepath.Join(p.CapsuleDir, capsule.DirRuntimeState, "spawn-journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(blob, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ---------------------------------------------------------------------------
// Proc: the supervised handle.

const (
	maxControlLineBytes  = 4 << 20
	maxControlQueueLines = 1024
)

var (
	errControlOverflow = fmt.Errorf("runtime: control queue overflow (harness floods control lines)")
	errControlTooLong  = fmt.Errorf("runtime: control line exceeded the bounded reader limit")
)

// proc wraps one derived harness process.
type proc struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	startedAt string

	mu   sync.Mutex // guards stdin writes
	pend sync.Mutex // guards pending ids (own lock: stdin writes may block)

	pending map[string]bool
	ctrl    *lineQueue
	exited  chan struct{}
	exitErr error
}

func newProc(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) *proc {
	p := &proc{
		cmd:       cmd,
		stdin:     stdin,
		startedAt: nowStamp(),
		pending:   map[string]bool{},
		ctrl:      newLineQueue(),
		exited:    make(chan struct{}),
	}
	go func() {
		p.exitErr = cmd.Wait()
		close(p.exited)
	}()
	go func() {
		defer p.ctrl.close(nil)
		br := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, tooLong, rerr := readBoundedLine(br, maxControlLineBytes)
			if rerr != nil {
				if rerr != io.EOF {
					fmt.Fprintf(os.Stderr, "runtime: harness stdout read failed: %v\n", rerr)
				}
				return
			}
			if tooLong {
				p.ctrl.closeTyped(errControlTooLong)
				fmt.Fprintf(os.Stderr, "runtime: control line over %d bytes; control path closed\n", maxControlLineBytes)
				continue // keep draining as bulk, never block the child
			}
			p.classify(line)
		}
	}()
	return p
}

func (p *proc) PID() int          { return p.cmd.Process.Pid }
func (p *proc) StartedAt() string { return p.startedAt }

func (p *proc) Done() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

func (p *proc) Alive() bool {
	if p.Done() {
		return false
	}
	return p.cmd.Process != nil && p.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// writeCommand frames one stdin JSON command.
func (p *proc) writeCommand(c piCommand) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return fmt.Errorf("runtime: agent stdio closed")
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := p.stdin.Write(append(blob, '\n')); err != nil {
		return fmt.Errorf("runtime: agent not accepting commands: %w", err)
	}
	if c.ID != "" {
		p.pend.Lock()
		p.pending[c.ID] = true
		p.pend.Unlock()
	}
	return nil
}

// SendControl delivers one daemon→agent control method (§五 stdio channel:
// answer / stop_notice / probe only): the protocol.ControlMsg JSON line is
// the message text of a prompt command VERBATIM — the agent parses the user
// message text exactly as the raw line. steer keeps the command legal while
// a turn is in flight and delivers at the next boundary.
func (p *proc) SendControl(m protocol.ControlMsg) error {
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return p.writeCommand(piCommand{
		ID: nextCommandID(), Type: "prompt",
		Message: string(line), StreamingBehavior: "steer",
	})
}

// WaitControlAck awaits the agent's ack (protocol.ControlAck echoing the
// method with ok=true), bounded like every control wait.
func (p *proc) WaitControlAck(ctx context.Context, method string, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	qctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		line, ok := p.ctrl.next(qctx)
		if !ok {
			if qctx.Err() != nil {
				return fmt.Errorf("runtime: no ack for %s: %w", method, qctx.Err())
			}
			if r := p.ctrl.reason(); r != nil {
				return fmt.Errorf("runtime: no ack for %s: control path closed: %w", method, r)
			}
			return fmt.Errorf("runtime: no ack for %s: agent output closed", method)
		}
		var ack protocol.ControlAck
		if json.Unmarshal([]byte(line), &ack) == nil && ack.Method == method {
			if ack.OK {
				return nil
			}
			return fmt.Errorf("runtime: agent refused %s: %s", method, ack.Error)
		}
		// other control traffic: keep waiting
	}
}

// classify sorts one stdout line: response records repeating OUR command
// ids are observed (success=false is logged, never an ack) and dropped;
// message_end records carrying an assistant message are the ack carrier —
// their text is scanned line-by-line for the closed ack shape; everything
// else is bulk.
func (p *proc) classify(line string) {
	var rec stdoutRecord
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Type == "" {
		return // non-record noise: bulk
	}
	switch rec.Type {
	case "response":
		p.observeResponse(rec)
	case "message_end":
		text, ok := assistantText(rec.Message)
		if !ok {
			return
		}
		for _, l := range strings.Split(text, "\n") {
			p.controlCandidate(l)
		}
	}
}

func (p *proc) observeResponse(rec stdoutRecord) {
	if rec.ID == nil || *rec.ID == "" {
		return
	}
	p.pend.Lock()
	_, ours := p.pending[*rec.ID]
	delete(p.pending, *rec.ID)
	p.pend.Unlock()
	if ours && rec.Success != nil && !*rec.Success {
		fmt.Fprintf(os.Stderr, "runtime: harness command %s failed: %s\n", *rec.ID, rec.Error)
	}
}

// controlCandidate queues one ack-shaped line: a JSON object carrying a
// control method and ok=true. Anything else is bulk.
func (p *proc) controlCandidate(line string) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "{") {
		return
	}
	var ack protocol.ControlAck
	if json.Unmarshal([]byte(s), &ack) != nil || ack.Method == "" || !ack.OK {
		return
	}
	switch ack.Method {
	case protocol.ControlAnswer, protocol.ControlStopNotice, protocol.ControlProbe:
		// closed vocabulary
	default:
		return
	}
	if !p.ctrl.push(s) {
		p.ctrl.closeTyped(errControlOverflow)
	}
}

func assistantText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var m assistantMessage
	if json.Unmarshal(raw, &m) != nil || m.Role != "assistant" {
		return "", false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String(), true
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, true
	}
	return "", false
}

var msgID int64

func nextCommandID() string {
	return "pf-" + strconv.FormatInt(atomic.AddInt64(&msgID, 1), 10)
}

// Terminate kills the WHOLE process group and proves it empty: SIGTERM to
// -pgid, bounded wait for the group (not merely the leader), SIGKILL, wait
// again.
func (p *proc) Terminate(bound time.Duration) error {
	if p.cmd.Process == nil {
		return nil
	}
	pgid := p.cmd.Process.Pid // Setpgid: the child IS the group leader
	if err := killGroup(pgid, bound); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bounded lossless control queue: an ack may never be silently dropped (a
// lost ack turns every later control wait into a timeout), so the queue is
// bounded rather than lossy and crossing the bound closes it LOUDLY.

type lineQueue struct {
	mu     sync.Mutex
	notify chan struct{}
	items  []string
	closed bool
	err    error
}

func newLineQueue() *lineQueue { return &lineQueue{notify: make(chan struct{}, 1)} }

func (q *lineQueue) push(line string) bool {
	q.mu.Lock()
	if q.closed || len(q.items) >= maxControlQueueLines {
		q.closed = true
		if q.err == nil {
			q.err = errControlOverflow
		}
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, line)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

func (q *lineQueue) close(err error) {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.err = err
	}
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *lineQueue) closeTyped(err error) { q.close(err) }

func (q *lineQueue) reason() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}

func (q *lineQueue) next(ctx context.Context) (string, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			line := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return line, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-q.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// readBoundedLine reads one newline-terminated line, never accumulating
// more than max bytes of CONTENT: an overlong line is drained and reported
// as tooLong so a hostile harness cannot balloon the reader's memory.
func readBoundedLine(br *bufio.Reader, max int) (line string, tooLong bool, err error) {
	const termSlack = 2
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			if len(buf)+len(chunk) > max+termSlack {
				for rerr == bufio.ErrBufferFull {
					chunk, rerr = br.ReadSlice('\n')
				}
				return "", true, nil
			}
			buf = append(buf, chunk...)
			continue
		}
		if rerr != nil {
			if rerr == io.EOF && len(buf)+len(chunk) > 0 {
				if len(buf)+len(chunk) > max {
					return "", true, nil
				}
				buf = append(buf, chunk...)
				return strings.TrimRight(string(buf), "\r"), false, nil
			}
			return "", false, rerr
		}
		buf = append(buf, chunk...)
		content := buf
		if len(content) > 0 && content[len(content)-1] == '\n' {
			content = content[:len(content)-1]
			if len(content) > 0 && content[len(content)-1] == '\r' {
				content = content[:len(content)-1]
			}
		}
		if len(content) > max {
			return "", true, nil
		}
		return strings.TrimRight(string(content), "\r"), false, nil
	}
}
