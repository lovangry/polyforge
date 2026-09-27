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
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
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
	mu        sync.Mutex
	cmd       *exec.Cmd
	adapter   Adapter
	sessionID string
	startedAt string
	stdin     io.WriteCloser
	outCh     chan string
	exited    chan struct{}
	exitErr   error
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
func (p *Proc) send(m rpcMessage) error {
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

// awaitAck waits for one stdout line acknowledging the pending method
// (bounded by ctx; the pf-execute main loop's contract is to ack control
// methods at its next safe boundary).
func (p *Proc) awaitAck(ctx context.Context, method string) error {
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-p.outCh:
			if !ok {
				return fmt.Errorf("mainagent: agent output closed")
			}
			var ack struct {
				Result map[string]any `json:"result"`
				Method string         `json:"method"`
			}
			if json.Unmarshal([]byte(line), &ack) == nil && (ack.Method == method || ack.Result["ack"] == method) {
				return nil
			}
			// Other traffic (step output streams): keep waiting.
		case <-time.After(time.Until(deadline)):
			return fmt.Errorf("mainagent: no ack for %s", method)
		}
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

// Terminate kills the WHOLE process group (04 §6.2 path B / §7.1 residue):
// SIGTERM to -pgid, bounded wait, then SIGKILL.
func (p *Proc) Terminate(timeout time.Duration) error {
	if p.cmd.Process == nil {
		return nil
	}
	pgid := p.cmd.Process.Pid // Setpgid: the child IS the group leader
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.exited:
		return nil
	case <-time.After(timeout):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("mainagent: process group %d refused to die", pgid)
		}
	}
	return nil
}

// Exposed for the supervisor's respawn decisions.
func (p *Proc) Wait() error {
	<-p.exited
	return p.exitErr
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
		stdin:     stdin, outCh: make(chan string, 64), exited: make(chan struct{}),
	}
	// Stdout reader: every line lands in the ack channel (streamed output
	// the supervisor does not interpret beyond pf/* acks).
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			select {
			case proc.outCh <- sc.Text():
			default: // ring semantics: control acks matter, bulk output may drop
			}
		}
	}()
	go func() {
		proc.exitErr = cmd.Wait()
		close(proc.exited)
	}()
	// Inject the pf-execute initial instruction (04 §6.1a): pi gets it as
	// the session's FIRST prompt, claude already carries it in the system
	// prompt.
	if m, ierr := adapter.InitialMessage(p); ierr == nil && m != nil {
		if serr := proc.send(*m); serr != nil {
			return proc, fmt.Errorf("mainagent: initial instruction injection failed: %w", serr)
		}
	}
	return proc, nil
}
