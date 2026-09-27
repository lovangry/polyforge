// Package mainagent implements the persistent WI Agent supervisor
// (00 §3.6/§3.6a, 04 §6.1a, INV-14): ONE long-lived harness process per
// work item, derived by the Daemon, supervised via standard process
// handles (pid, pgid, exit code, stdio pipes), re-derived on crash with the
// SAME session id only in the same-machine/same-generation continuation
// case, and never restarted per step.
//
// v1 harness adapters (00 §3.6a minimal contract):
//   - Pi:     pi --mode rpc, Capsule-local session dir, pf-execute injected
//     as the FIRST prompt of the RPC session.
//   - Claude: claude -p with a stable session id and streaming output,
//     pf-execute injected as an appended system prompt; crash continuation
//     resumes via claude --resume <session id>.
//
// The wire protocol between supervisor and harness is line-delimited JSON
// on the process stdio (pi --mode rpc speaks JSON-RPC 2.0 over stdio); the
// supervisor sends pf/* control methods and treats any ack line as the
// confirmation. Tests drive this against a scripted fake harness.
package mainagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// SpawnParams re-exports the daemon's spawn contract (one shape).
type SpawnParams = daemon.SpawnParams

// rpcMessage is one line of the stdio control channel.
type rpcMessage struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      *int64         `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// The stdio control channel method names (the WI-Agent side of the
// protocol this supervisor speaks). These names are a TWO-SIDED contract:
// skills/pf-execute/SKILL.md declares the agent's half and must name each
// method BYTE-IDENTICALLY. TestPfExecuteSkillDeclaresControlChannel in
// skills_contract_test.go is the drift guard — change a name here and the
// test fails until the skill file matches.
const (
	// MethodQuiesce: freeze confirmation (05 §2.7 / 04 §6.4). The agent
	// acks that no in-flight sync-surface writes exist and none will
	// start until released.
	MethodQuiesce = "pf/quiesce"
	// MethodResume: the freeze-release notification (05 §2.7 release
	// step). The staging pin is complete — the agent may resume
	// sync-surface writes. Paired with MethodQuiesce; byte-identical with
	// the SKILL.md closed method set.
	MethodResume = "pf/resume"
	// MethodStopNotice: the stop-advancing notice (04 §6.5 step 3). The
	// agent picks its own next safe boundary; the daemon never picks or
	// skips steps on its behalf (00 §3.6).
	MethodStopNotice = "pf/stop_notice"
	// MethodAnswer: the Interactive answer relay (00 §3.4a). Params carry
	// the five-field AnswerRelay verbatim, bound by question_id.
	MethodAnswer = "pf/answer"
)

var msgID int64

func nextMsgID() int64 {
	msgID++
	return msgID
}

// Adapter is a harness-specific launcher.
type Adapter interface {
	// Name is the harness name (pi | claude).
	Name() string
	// BuildCmd constructs the argv/env/stdio for one derivation.
	BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error)
	// InitialMessage returns the first stdio message delivering the
	// pf-execute instruction (pi: first prompt; claude rides the system
	// prompt instead, so this returns nil).
	InitialMessage(p SpawnParams) (*rpcMessage, error)
}

// ---------------------------------------------------------------------------
// Pi adapter (04 §6.1a): pi --mode rpc, session dir inside the Capsule.

type PiAdapter struct {
	// Bin overrides the pi executable path (defaults to "pi"; tests
	// substitute a fake).
	Bin string
}

func (a *PiAdapter) Name() string { return capsule.AdapterPi }

func (a *PiAdapter) piBin() string {
	if a.Bin != "" {
		return a.Bin
	}
	return "pi"
}

func (a *PiAdapter) BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error) {
	cmd := exec.Command(a.piBin(), "--mode", "rpc")
	cmd.Dir = p.CapsuleDir // capsule-local session dir: pi resolves sessions under cwd
	cmd.Env = append(os.Environ(),
		"PI_SESSION_DIR="+sessionDir,
		"PF_SESSION_ID="+p.SessionID,
		"PF_WORK_ITEM_ID="+p.WorkItemID,
		"PF_GENERATION="+fmt.Sprint(p.Generation),
	)
	// Credentials never enter the env (04 §8): only capsule path + session id.
	return cmd, nil
}

func (a *PiAdapter) InitialMessage(p SpawnParams) (*rpcMessage, error) {
	return &rpcMessage{
		JSONRPC: "2.0", ID: nil, Method: "user/prompt",
		Params: map[string]any{"text": p.InitialPrompt},
	}, nil
}

// ---------------------------------------------------------------------------
// Claude adapter (04 §6.1a): claude -p, stable session id + streaming;
// crash continuation resumes the same session.

type ClaudeAdapter struct {
	Bin string
	// Resume marks a crash-continuation derivation: claude --resume <sid>.
	Resume bool
}

func (a *ClaudeAdapter) Name() string { return capsule.AdapterClaude }

func (a *ClaudeAdapter) claudeBin() string {
	if a.Bin != "" {
		return a.Bin
	}
	return "claude"
}

func (a *ClaudeAdapter) BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error) {
	args := []string{"-p", "--output-format", "stream-json"}
	if a.Resume {
		// Crash continuation: same session id, same machine, same
		// generation — the ONLY reuse case (03 §4.4).
		args = append(args, "--resume", p.SessionID)
	} else {
		args = append(args, "--session-id", p.SessionID)
	}
	// pf-execute is injected as an APPENDED SYSTEM PROMPT (not a user
	// message that later traffic can wash away), per 04 §6.1a.
	args = append(args, "--append-system-prompt", p.InitialPrompt)
	cmd := exec.Command(a.claudeBin(), args...)
	cmd.Dir = p.CapsuleDir
	cmd.Env = append(os.Environ(),
		"CLAUDE_SESSION_DIR="+sessionDir,
		"PF_SESSION_ID="+p.SessionID,
		"PF_WORK_ITEM_ID="+p.WorkItemID,
	)
	return cmd, nil
}

func (a *ClaudeAdapter) InitialMessage(p SpawnParams) (*rpcMessage, error) {
	return nil, nil // system-prompt channel: no first-prompt message
}

// ---------------------------------------------------------------------------
// Proc: the supervised handle over one derived agent process.

type Proc struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	adapter     Adapter
	sessionID   string
	startedAt   string
	stdin       io.WriteCloser
	ctrl        *lineQueue // bounded lossless control/ack path (see lineQueue)
	bulkDropped int64      // bulk (non-control) lines dropped when nobody consumes them
	exited      chan struct{}
	exitErr     error
	// unhealthy holds the typed reason the agent was marked unhealthy
	// (control queue overflow, overlong control line): set once, read by
	// send/awaitAck so every later control call fails fast and LOUDLY
	// instead of waiting out its bound against a closed path.
	unhealthy atomic.Value // error
}

// Control-path bounds (R2N-9). Acks can never be silently dropped — a
// lost pf/quiesce or pf/stop_notice ack turns every freeze/boundary wait
// into a timeout (05 §2.7 / 04 §6.5) — so the queue is bounded rather than
// lossy: no legitimate harness emits more than maxControlQueueLines
// control-shaped lines within one bounded ack wait (a handful is the
// protocol's whole vocabulary), and hitting the bound is itself PROOF of a
// flooding/compromised harness. The design-faithful shape at that point is
// to close the control path LOUDLY — every future ack fails fast with the
// typed reason and the agent is marked unhealthy (the daemon's freeze/close
// then fails honestly instead of hanging) — never to silently drop the one
// ack that might be awaited.
const (
	// maxControlQueueLines bounds the control queue before it is closed.
	maxControlQueueLines = 1024
	// maxControlLineBytes bounds ONE control line; bufio.Scanner's silent
	// ErrTooLong death is replaced by a bounded reader that surfaces the
	// overlong line, closes the control path typed and keeps draining bulk
	// so the child can never block on its stdout.
	maxControlLineBytes = 4 << 20
)

// ErrControlChannelOverflow marks a harness that flooded more than
// maxControlQueueLines control-shaped lines: the control path is closed
// and the agent marked unhealthy.
var ErrControlChannelOverflow = errors.New("mainagent: control queue overflow (harness floods control-shaped lines)")

// ErrControlLineTooLong marks a single control-shaped line beyond
// maxControlLineBytes: the control path is closed and the agent marked
// unhealthy.
var ErrControlLineTooLong = errors.New("mainagent: control line exceeded the bounded reader limit")

// lineQueue is the bounded LOSSLESS control-channel buffer: lines
// classified as potential control acks are queued and never dropped while
// the queue is open; crossing maxControlQueueLines closes the queue typed
// with ErrControlChannelOverflow (see the bounds comment for why closing
// loudly beats dropping). Bulk (non-control) output is NOT buffered here:
// the stdout reader drops it, keeping memory bounded.
type lineQueue struct {
	mu     sync.Mutex
	notify chan struct{}
	items  []string
	closed bool
	err    error // typed close reason (overflow/overlong); nil = clean EOF
}

func newLineQueue() *lineQueue { return &lineQueue{notify: make(chan struct{}, 1)} }

// push appends one line, or closes the queue typed when the bound is hit.
// ok=false means the queue was closed (by this push or earlier).
func (q *lineQueue) push(line string) (ok bool) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	if len(q.items) >= maxControlQueueLines {
		q.closed = true
		q.err = fmt.Errorf("%w: %d lines queued", ErrControlChannelOverflow, len(q.items))
		q.mu.Unlock()
		select {
		case q.notify <- struct{}{}:
		default:
		}
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

// close ends the queue with a reason (nil = clean end of output). Already
// queued lines remain readable: an ack that arrived before the failure is
// still delivered to its waiter.
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

// reason returns the typed close reason, if any.
func (q *lineQueue) reason() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}

// next pops one line, waiting until ctx ends (an empty string with ok=false
// means the queue is drained and closed).
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

// isControlLine classifies one stdout line: JSON objects carrying a
// "method" field, or a "result" object with an ack marker, are potential
// control acks (the two-sided contract shape); everything else — free text,
// progress streams, non-ack JSON — is bulk output.
func isControlLine(line string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "{") {
		return false
	}
	var probe struct {
		Method string         `json:"method"`
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal([]byte(s), &probe) != nil {
		return false
	}
	if probe.Method != "" {
		return true
	}
	_, hasAck := probe.Result["ack"]
	return hasAck
}

// PID implements daemon.AgentHandle.
func (p *Proc) PID() int { return p.cmd.Process.Pid }

// StartedAt implements daemon.AgentHandle (the pid-reuse guard input).
func (p *Proc) StartedAt() string { return p.startedAt }

// Alive reports process liveness (standard handle observation, 00 §3.6).
func (p *Proc) Alive() bool {
	select {
	case <-p.exited:
		return false
	default:
	}
	return p.cmd.Process != nil && p.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// send writes one control line; a dead pipe reports the process as gone.
// An agent marked unhealthy (control path closed by overflow or an
// overlong line) fails fast with the typed reason instead of writing into
// a path whose acks can never arrive.
func (p *Proc) send(m rpcMessage) error {
	if r, ok := p.unhealthyReason(); ok {
		return fmt.Errorf("mainagent: agent marked unhealthy, control path closed: %w", r)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return fmt.Errorf("mainagent: agent stdio closed")
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := p.stdin.Write(append(blob, '\n')); err != nil {
		return fmt.Errorf("mainagent: agent not accepting control messages: %w", err)
	}
	return nil
}

// awaitAck waits for one CONTROL line acknowledging the pending method
// (bounded by ctx; the pf-execute main loop's contract is to ack control
// methods at its next safe boundary). Control lines are buffered on the
// lossless path, so concurrent bulk output on the same stdout can never
// push the ack out of the window.
func (p *Proc) awaitAck(ctx context.Context, method string) error {
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	qctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		line, ok := p.ctrl.next(qctx)
		if !ok {
			if qctx.Err() != nil {
				if qctx.Err() == context.DeadlineExceeded {
					return fmt.Errorf("mainagent: no ack for %s", method)
				}
				return qctx.Err()
			}
			// The control path ENDED: a typed reason (overflow/overlong)
			// is surfaced LOUDLY — the old code reported a silent
			// "agent output closed" for a Scanner that had died on an
			// overlong line (R2N-9).
			if r := p.ctrl.reason(); r != nil {
				return fmt.Errorf("mainagent: no ack for %s: control path closed: %w", method, r)
			}
			return fmt.Errorf("mainagent: agent output closed")
		}
		var ack struct {
			Result map[string]any `json:"result"`
			Method string         `json:"method"`
		}
		if json.Unmarshal([]byte(line), &ack) == nil && (ack.Method == method || ack.Result["ack"] == method) {
			return nil
		}
		// Other control traffic: keep waiting.
	}
}

// NotifyStop implements the graceful-shutdown stop-advancing notice
// (04 §6.5 step 3): the agent picks its own next safe boundary; the daemon
// never picks or skips steps on its behalf (00 §3.6).
func (p *Proc) NotifyStop(ctx context.Context) error {
	m := rpcMessage{JSONRPC: "2.0", Method: MethodStopNotice, Params: map[string]any{"reason": "graceful_shutdown"}}
	if err := p.send(m); err != nil {
		return err
	}
	return p.awaitAck(ctx, MethodStopNotice)
}

// Quiesce implements the freeze confirmation (05 §2.7 / 04 §6.4): the
// agent confirms no in-flight sync-surface writes and none will start.
func (p *Proc) Quiesce(ctx context.Context) error {
	m := rpcMessage{JSONRPC: "2.0", Method: MethodQuiesce}
	if err := p.send(m); err != nil {
		return err
	}
	return p.awaitAck(ctx, MethodQuiesce)
}

// Resume implements the freeze-release notification (05 §2.7): the write
// freeze window is over (the staged bytes are pinned), so the agent may
// resume sync-surface writes. Best-effort by design — an agent that died
// mid-window simply never acks.
func (p *Proc) Resume(ctx context.Context) error {
	m := rpcMessage{JSONRPC: "2.0", Method: MethodResume}
	if err := p.send(m); err != nil {
		return err
	}
	return p.awaitAck(ctx, MethodResume)
}

// DeliverAnswer relays an answer payload verbatim to the waiting agent
// (00 §3.4a: the daemon forwards, never parses).
func (p *Proc) DeliverAnswer(relay daemon.AnswerRelay) error {
	b, err := json.Marshal(relay)
	if err != nil {
		return err
	}
	params := map[string]any{}
	json.Unmarshal(b, &params)
	return p.send(rpcMessage{JSONRPC: "2.0", Method: MethodAnswer, Params: params})
}

// Terminate kills the WHOLE process group and proves it empty (04 §6.2
// path B / §7.1 residue): SIGTERM to -pgid, bounded wait for the GROUP to
// be empty — not merely for the leader to exit (a child that ignores
// SIGTERM keeps writing otherwise) — then SIGKILL to -pgid and a second
// bounded wait. Works for an already-dead leader too: the group id
// persists while any member lives, so the residue collection still runs.
func (p *Proc) Terminate(timeout time.Duration) error {
	if p.cmd.Process == nil {
		return nil
	}
	pgid := p.cmd.Process.Pid // Setpgid: the child IS the group leader
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if !waitForGroupEmpty(pgid, timeout) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !waitForGroupEmpty(pgid, 5*time.Second) {
			return fmt.Errorf("mainagent: process group %d refused to die", pgid)
		}
	}
	return nil
}

// GroupEmpty implements daemon.AgentHandle (05 §2.7 shortcut condition ①):
// the whole process group — the agent and every step agent / command child
// — is verified empty, not just the top-level pid. FAILS CLOSED: an
// unverifiable group (unreadable procfs) is reported as NON-empty and the
// typed error is logged — the dead-writers shortcut is never granted on
// an assumption.
func (p *Proc) GroupEmpty() bool {
	if p.cmd.Process == nil {
		return true
	}
	empty, err := daemon.ProcessGroupEmpty(p.cmd.Process.Pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mainagent: process group %d emptiness unverifiable, failing closed: %v\n", p.cmd.Process.Pid, err)
		return false
	}
	return empty
}

// waitForGroupEmpty polls the group-emptiness check until it holds or the
// bound elapses. An unverifiable group fails closed (false + loud log),
// same as daemon.ProcessGroupEmpty's contract.
func waitForGroupEmpty(pgid int, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		empty, err := daemon.ProcessGroupEmpty(pgid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mainagent: process group %d emptiness unverifiable, failing closed: %v\n", pgid, err)
			return false
		}
		if empty {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exposed for the supervisor's respawn decisions.
func (p *Proc) Wait() error {
	<-p.exited
	return p.exitErr
}

// markUnhealthy records the typed reason the control path died; the first
// reason wins (the overflow that closed the path, not a later symptom).
func (p *Proc) markUnhealthy(reason error) {
	if p.unhealthy.Load() == nil {
		p.unhealthy.Store(reason)
	}
}

// unhealthyReason reports the unhealthy marker, if set.
func (p *Proc) unhealthyReason() (error, bool) {
	v := p.unhealthy.Load()
	if v == nil {
		return nil, false
	}
	r, _ := v.(error)
	return r, true
}

// Unhealthy reports whether the agent was marked unhealthy (control path
// closed by queue overflow or an overlong control line, R2N-9). An
// unhealthy agent is still ALIVE — the caller (the daemon's freeze/close
// path) decides collection — but no control message can reach it.
func (p *Proc) Unhealthy() bool {
	_, bad := p.unhealthyReason()
	return bad
}

// readBoundedLine reads one newline-terminated line through br, never
// accumulating more than max bytes of line CONTENT (the terminator is
// framing, not content): a line longer than the bound is drained and
// reported as tooLong (the caller closes the control path typed), so a
// hostile harness cannot balloon the reader's memory. A trailing \r is
// trimmed like bufio.Scanner's ScanLines did.
//
// R3N-F9: the bound is enforced on COMPLETED lines too. The old code only
// checked size on ErrBufferFull, so a final fragment ending in a newline
// (or EOF) was appended without checking the accumulated length — a
// completed line over the limit evaded ErrControlLineTooLong whenever it
// fit the reader's buffer. The accumulation guard keeps a 2-byte slack
// for a possible "\r\n" terminator so an at-limit line is never
// false-flagged before its terminator is seen.
//
// R4N-4: the WHOLE terminator — "\r\n" as well as the lone "\n" — is
// framing. The old completed-line check subtracted only the "\n", so a
// line of exactly max content bytes + "\r\n" was wrongly rejected (the
// CR counted as content). The full terminator is stripped before the
// bound check; the 2-byte accumulation slack stays for safety, but a
// completed line's content is measured without the CR. The CR may sit at
// the end of an earlier accumulated chunk (a buffer boundary can split
// "\r\n"), so the terminator is stripped from the COMBINED line, never
// from the last chunk alone.
func readBoundedLine(br *bufio.Reader, max int) (line string, tooLong bool, err error) {
	const termSlack = 2 // a pending terminator may be "\r\n"
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			if len(buf)+len(chunk) > max+termSlack {
				// Drain the remainder of the overlong line so the stream
				// stays framed for whatever follows.
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
				// Final unterminated line: deliver it (Scanner parity) —
				// after the SAME content bound check as terminated lines.
				if len(buf)+len(chunk) > max {
					return "", true, nil
				}
				buf = append(buf, chunk...)
				return strings.TrimRight(string(buf), "\r"), false, nil
			}
			return "", false, rerr
		}
		// Completed (newline-terminated) line: R3N-F9 — the accumulated
		// payload is checked here too, not only on the ErrBufferFull path.
		// R4N-4: the FULL terminator ("\r\n" or the lone "\n") is framing,
		// not content, so a line of exactly max content bytes + "\r\n"
		// stays legal; the strip happens on the combined line so a
		// terminator split across the accumulation boundary is handled.
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

// ---------------------------------------------------------------------------
// Supervisor: derives agents per WI, owns the adapter choice.

type Supervisor struct {
	// AdapterFor resolves the adapter for a spawn; the daemon's config
	// names pi|claude, tests substitute fakes.
	AdapterFor func(name string) Adapter
}

// NewSupervisor wires the two v1 adapters (with optional binary overrides
// for tests/fakes).
func NewSupervisor(piBin, claudeBin string) *Supervisor {
	return &Supervisor{AdapterFor: func(name string) Adapter {
		switch name {
		case capsule.AdapterClaude:
			return &ClaudeAdapter{Bin: claudeBin}
		default:
			return &PiAdapter{Bin: piBin}
		}
	}}
}

// Spawn derives one persistent WI Agent process (daemon.AgentSpawner).
func (s *Supervisor) Spawn(ctx context.Context, p SpawnParams) (daemon.AgentHandle, error) {
	adapter := s.AdapterFor(p.Adapter)
	if adapter == nil {
		return nil, fmt.Errorf("mainagent: unknown adapter %q", p.Adapter)
	}
	capc := &capsule.Capsule{Dir: p.CapsuleDir, WorkItemID: p.WorkItemID, Generation: p.Generation}
	sessionDir := capc.SessionDataDir(p.Adapter)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return nil, err
	}
	// Session identity: written ONCE on first derivation; a crash
	// continuation of the same generation finds the SAME id on disk
	// (03 §4.4's only reuse case) — the daemon already computed it, this
	// read-back is the consistency guard.
	if idn, err := capc.ReadSessionIdentity(p.Adapter); err == nil && idn.SessionID != "" {
		if idn.SessionID != p.SessionID {
			return nil, fmt.Errorf("mainagent: session id conflict: local %s vs requested %s (new generation must not reuse an old session)", idn.SessionID, p.SessionID)
		}
	} else {
		if err := capc.WriteSessionIdentity(p.Adapter, p.SessionID); err != nil {
			return nil, err
		}
	}

	cmd, err := adapter.BuildCmd(p, sessionDir)
	if err != nil {
		return nil, err
	}
	// Process group: the agent is its own group leader so Terminate can
	// collect step agents / command children with one signal (04 §7.1).
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
		return nil, fmt.Errorf("mainagent: harness start failed (DAEMON-503): %w", err)
	}
	proc := &Proc{
		cmd: cmd, adapter: adapter, sessionID: p.SessionID,
		startedAt: time.Now().UTC().Format(time.RFC3339Nano),
		stdin:     stdin, ctrl: newLineQueue(), exited: make(chan struct{}),
	}
	// Stdout reader: TWO paths. Lines that look like control acks go to
	// the bounded LOSSLESS queue (an ack may never be dropped — a lost
	// pf/quiesce or pf/stop_notice ack becomes a freeze/boundary timeout,
	// 05 §2.7); all other output is bulk and may be dropped when nobody
	// consumes it, keeping memory bounded under a flooding harness.
	// R2N-9: bufio.Scanner is GONE — its overlong-line death was silent
	// (sc.Err ignored, control path just stops). The bounded reader below
	// surfaces the overlong line: the control path is closed typed, the
	// agent marked unhealthy and the failure logged; reading CONTINUES as
	// bulk so a chatty child can never block on its own stdout.
	go func() {
		defer proc.ctrl.close(nil)
		br := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, tooLong, rerr := readBoundedLine(br, maxControlLineBytes)
			if rerr != nil {
				if rerr != io.EOF {
					fmt.Fprintf(os.Stderr, "mainagent: harness stdout read failed: %v\n", rerr)
				}
				return
			}
			if tooLong {
				reason := fmt.Errorf("%w: limit is %d bytes", ErrControlLineTooLong, maxControlLineBytes)
				proc.markUnhealthy(reason)
				proc.ctrl.close(reason)
				fmt.Fprintf(os.Stderr, "mainagent: control line exceeded %d bytes; control path CLOSED, agent marked unhealthy\n", maxControlLineBytes)
				continue // keep draining as bulk (never block the child)
			}
			if isControlLine(line) {
				if !proc.ctrl.push(line) {
					reason := proc.ctrl.reason()
					if reason == nil {
						reason = ErrControlChannelOverflow
					}
					proc.markUnhealthy(reason)
					fmt.Fprintf(os.Stderr, "mainagent: control queue bound (%d) hit; control path CLOSED, agent marked unhealthy: %v\n", maxControlQueueLines, reason)
				}
			} else {
				atomic.AddInt64(&proc.bulkDropped, 1) // ring semantics for bulk output
			}
		}
	}()
	go func() {
		proc.exitErr = cmd.Wait()
		close(proc.exited)
	}()
	// R3N-F6: journal the process identity BEFORE Spawn returns (and before
	// the first instruction is injected, so a journaled agent has at most
	// received nothing yet). A crash between the successful derivation and
	// the daemon's formal jobs[] record used to leave a live agent no
	// restart could discover (no pid anywhere durable), so the orphan
	// requeue spawned a SECOND agent over the live one, overwriting
	// unpublished local work. The callback lets the daemon persist
	// (pid, started-at) into the pending_jobs[] row first; its failure
	// fails the spawn and tears the process down (the reaper goroutine
	// above is already running, so no corpse is left behind).
	if p.OnProcessStarted != nil {
		if jerr := p.OnProcessStarted(cmd.Process.Pid, proc.startedAt); jerr != nil {
			_ = proc.Terminate(5 * time.Second)
			return nil, fmt.Errorf("mainagent: process-identity journaling failed (process torn down): %w", jerr)
		}
	}
	// Inject the pf-execute initial instruction (04 §6.1a): pi gets it as
	// the session's FIRST prompt, claude already carries it in the system
	// prompt. Any failure here must not leak a running half-initialized
	// agent: the process group is torn down BEFORE the error is returned.
	m, ierr := adapter.InitialMessage(p)
	if ierr != nil {
		_ = proc.Terminate(5 * time.Second)
		return nil, fmt.Errorf("mainagent: initial instruction build failed (DAEMON-503): %w", ierr)
	}
	if m != nil {
		if serr := proc.send(*m); serr != nil {
			_ = proc.Terminate(5 * time.Second)
			return nil, fmt.Errorf("mainagent: initial instruction injection failed (DAEMON-503): %w", serr)
		}
	}
	return proc, nil
}
