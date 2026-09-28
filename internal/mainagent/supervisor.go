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
// The wire protocol between supervisor and harness is line-delimited JSON on
// the process stdio. The pi adapter speaks the REAL pi RPC protocol (pi
// docs: RPC mode — `pi --mode rpc`): stdin carries one JSON command object
// per line ({"id":..,"type":"prompt","message":..}), stdout carries
// protocol records only (response records for our command ids plus session
// events; assistant text arrives inside message records, never as raw
// lines), diagnostics go to stderr and stdin EOF is a graceful shutdown.
// The daemon→agent pf/* control methods ride prompt commands whose message
// text is the control JSON line VERBATIM; the agent acks inside its
// assistant message text, and the supervisor extracts ack lines from the
// message records. Tests drive this against a scripted fake harness
// (test/fakepi reproduces the same record shapes).
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

// controlEnvelope is the daemon→agent control JSON line — the
// transport-neutral payload the agent-side pf-execute parses (SKILL.md
// §12). Under the pi adapter it never touches the wire by itself: it is
// carried VERBATIM as the text of a prompt command (piCommand below), so
// the agent receives it as a user message whose text is exactly this
// line. The jsonrpc key is historical (the closed two-sided line shape);
// both sides parse method/params, and neither may rename it alone.
type controlEnvelope struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// piCommand is one stdin command of the REAL pi RPC protocol (pi docs:
// RPC Commands — prompt). Every command MAY carry a string id; the
// matching response record repeats it, which is how the supervisor
// correlates failures.
type piCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"` // "prompt"
	Message string `json:"message"`
	// StreamingBehavior "steer" on control prompts: pi rejects a bare
	// prompt while the agent is streaming, and steer queues the message
	// for delivery after the current assistant turn — exactly the
	// next-safe-boundary discipline the control contract demands. The
	// initial prompt needs none (the agent is idle at derivation).
	StreamingBehavior string `json:"streamingBehavior,omitempty"`
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

// nextCommandID mints the string id of one pi RPC command (the response
// record repeats it; the supervisor correlates failures by it).
func nextCommandID() string {
	return fmt.Sprintf("pf-%d", atomic.AddInt64(&msgID, 1))
}

// Adapter is a harness-specific launcher.
type Adapter interface {
	// Name is the harness name (pi | claude).
	Name() string
	// BuildCmd constructs the argv/env/stdio for one derivation.
	BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error)
	// InitialMessage returns the first stdin write delivering the
	// pf-execute instruction (pi: the session's first prompt command;
	// claude rides the system prompt instead, so this returns nil).
	InitialMessage(p SpawnParams) (*piCommand, error)
	// FrameControl returns the stdin payload for ONE daemon→agent control
	// line. line is the transport-neutral control JSON (controlEnvelope
	// serialized); id is the command id for correlation. The pi adapter
	// wraps the line in a prompt command (the agent receives it as a user
	// message whose text is the line VERBATIM); the claude adapter writes
	// the line itself.
	FrameControl(id, line string) any
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

func (a *PiAdapter) InitialMessage(p SpawnParams) (*piCommand, error) {
	// The pf-execute instruction is the session's FIRST prompt of the RPC
	// session (04 §6.1a). The agent is idle at derivation, so no
	// streamingBehavior is needed (and none may appear: the initial prompt
	// is the envelope regression-pinned shape).
	return &piCommand{ID: "pf-init", Type: "prompt", Message: p.InitialPrompt}, nil
}

// FrameControl implements Adapter: one control line rides a prompt command
// whose message text is the control JSON VERBATIM — the agent parses the
// user message text exactly as it parsed the raw line. steer keeps the
// command legal while the agent is mid-run and delivers it at the agent's
// next turn boundary (the safe-boundary contract of SKILL.md §12).
func (a *PiAdapter) FrameControl(id, line string) any {
	return piCommand{ID: id, Type: "prompt", Message: line, StreamingBehavior: "steer"}
}

// ---------------------------------------------------------------------------
// Claude adapter (04 §6.1a): claude -p, stable session id + streaming;
// crash continuation resumes the same session.
//
// UNVERIFIED: this adapter's stdio protocol has never been exercised
// against the real cc harness (out of scope for the pi-transport rework;
// see FrameControl). Do not take any of its wire behavior as measured.

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

func (a *ClaudeAdapter) InitialMessage(p SpawnParams) (*piCommand, error) {
	return nil, nil // system-prompt channel: no first-prompt message
}

// FrameControl implements Adapter: the claude control line is written as
// itself. UNVERIFIED: like the rest of the claude stdio contract, this
// framing has never been exercised against the real cc harness — the
// entire adapter (argv, session resume, ack shapes on stdout) is out of
// scope until a real-harness smoke runs; treat every behavior here as a
// placeholder that may need its own transport rework.
func (a *ClaudeAdapter) FrameControl(id, line string) any {
	return line
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
	// pending tracks the command ids this supervisor sent to the child's
	// RPC face: a response record repeating one of them is OBSERVED (a
	// success=false failure is logged to stderr, never treated as an ack)
	// and dropped from the map. Guarded by pendMu — its OWN lock, never mu:
	// the stdout reader observes responses with it while writeStdin holds
	// mu across a potentially BLOCKING stdin write; sharing mu would let a
	// child that fills its stdout pipe while not reading stdin deadlock
	// both goroutines on each other.
	pending map[string]bool
	pendMu  sync.Mutex
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

// lineQueue is the bounded LOSSLESS control-channel buffer: lines in the
// CLOSED control vocabulary (isControlLine) are queued and never dropped
// while the queue is open; crossing maxControlQueueLines closes the queue
// typed with ErrControlChannelOverflow (see the bounds comment for why
// closing loudly beats dropping). Bulk (non-control) output is NOT
// buffered here: the stdout reader drops it, keeping memory bounded.
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

// isControlMethod reports whether m is one of the four pf/* control
// methods — the CLOSED vocabulary of the stdio control channel.
func isControlMethod(m string) bool {
	switch m {
	case MethodQuiesce, MethodResume, MethodStopNotice, MethodAnswer:
		return true
	}
	return false
}

// isControlLine classifies one stdout line against the CLOSED control
// vocabulary (SK-6/CR-4, tightened in fix round 2): a line is control ONLY
// when it carries the EXACT ack shape pf-execute §12 defines,
// {"method":X,"result":{"ack":X}} with X one of the four pf/* methods
// (method and result.ack must name the SAME method). Everything else —
// free text, progress streams, bulk JSON that merely carries a "method"
// field naming one of the four methods (a noisy harness spoofing
// Quiesce/Resume/NotifyStop/DeliverAnswer), acks whose result.ack does not
// echo the method, method-only lines without the ack result — is BULK
// output. Two properties closed the spoof holes of the previous
// classifier, which admitted any line whose method named one of the four
// (flooding the bounded control queue) and let awaitAck accept it as an
// ack: the agent never initiates control traffic (pf-execute §12: "You
// never initiate messages on this channel; you only ack"), so nothing
// legitimate is lost by demanding the exact two-sided ack shape.
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
	if !isControlMethod(probe.Method) {
		return false
	}
	// The exact ack shape: the acknowledged method is echoed in
	// result.ack, byte-identical with the line's own method field.
	ack, hasAck := probe.Result["ack"].(string)
	return hasAck && ack == probe.Method
}

// ---------------------------------------------------------------------------
// Stdout record-stream classification (REAL pi RPC protocol).

// stdoutRecord is the probe shape for one stdout protocol record. Under
// the real protocol stdout is RESERVED for protocol records: response
// records for our command ids, session events (agent/turn/message/tool
// activity) and extension-UI records. The supervisor only needs three of
// them: response (failure observation), message_end (assistant text —
// the ONLY carrier of acks) and the type field to tell records from the
// claude adapter's raw-line legacy stream.
type stdoutRecord struct {
	Type    string          `json:"type"`
	ID      *string         `json:"id"`      // response records repeat the command id (null = not ours)
	Command string          `json:"command"` // "prompt" on response records
	Success *bool           `json:"success"`
	ErrText string          `json:"error"`
	Message json.RawMessage `json:"message"` // message_start/message_end
}

// assistantMessage is the message object inside a message_end record
// (pi docs: JSON Event Stream — message_end is the authoritative final
// message). Assistant content is an array of typed blocks; a bare string
// is tolerated defensively.
type assistantMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// classifyStdoutLine sorts ONE raw stdout line under the real protocol:
//
//   - a response record repeating one of OUR command ids is OBSERVED (a
//     success=false failure is logged to stderr — a rejected prompt
//     command, e.g. a malformed control line — but NEVER treated as an
//     ack) and dropped as bulk;
//   - a message_end record carrying an ASSISTANT message is the ack
//     carrier: its text is extracted and scanned line-by-line for the
//     closed ack shape (isControlLine); ack lines enter the bounded
//     lossless ctrl queue, every other extracted line is bulk;
//   - every other record (agent/turn events, message_update deltas,
//     extension-UI noise, responses for foreign ids) is BULK and dropped
//     with the existing bookkeeping;
//   - a line that is not a protocol record at all (no JSON object with a
//     type field) falls back to the legacy RAW-line classifier. Real pi
//     never emits such a line (stdout is reserved for records); the
//     fallback exists for the claude adapter, whose stdio contract is
//     UNVERIFIED and whose acks arrive as raw lines.
func (p *Proc) classifyStdoutLine(line string) {
	var rec stdoutRecord
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Type == "" {
		// Not a protocol record: legacy raw-line classification (claude
		// path). Bulk or ack, never a response observation.
		p.classifyCandidateLine(line)
		return
	}
	switch rec.Type {
	case "response":
		p.observeResponse(rec)
		atomic.AddInt64(&p.bulkDropped, 1)
	case "message_end":
		text, ok := assistantText(rec.Message)
		if !ok {
			atomic.AddInt64(&p.bulkDropped, 1)
			return
		}
		// The extracted text is scanned line-by-line with the SAME bounds
		// as raw lines: an ack may never be silently dropped, and an
		// overlong extracted line closes the control path typed
		// (ErrControlLineTooLong) exactly like an overlong raw line.
		for _, l := range strings.Split(text, "\n") {
			p.classifyCandidateLine(strings.TrimSuffix(l, "\r"))
		}
	default:
		// Session events, message_update deltas, extension-UI records,
		// responses for command ids we never sent: bulk.
		atomic.AddInt64(&p.bulkDropped, 1)
	}
}

// classifyCandidateLine runs ONE candidate line (raw or extracted from a
// message record) through the closed control vocabulary: an ack-shaped
// line enters the bounded lossless queue (with the overflow close),
// everything else is bulk. An extracted line beyond maxControlLineBytes
// closes the control path typed — the same ErrControlLineTooLong
// semantics the bounded reader enforces on raw lines.
func (p *Proc) classifyCandidateLine(line string) {
	if len(line) > maxControlLineBytes {
		reason := fmt.Errorf("%w: limit is %d bytes", ErrControlLineTooLong, maxControlLineBytes)
		p.markUnhealthy(reason)
		p.ctrl.close(reason)
		fmt.Fprintf(os.Stderr, "mainagent: control line exceeded %d bytes; control path CLOSED, agent marked unhealthy\n", maxControlLineBytes)
		return
	}
	if !isControlLine(line) {
		atomic.AddInt64(&p.bulkDropped, 1)
		return
	}
	if !p.ctrl.push(line) {
		reason := p.ctrl.reason()
		if reason == nil {
			reason = ErrControlChannelOverflow
		}
		p.markUnhealthy(reason)
		fmt.Fprintf(os.Stderr, "mainagent: control queue bound (%d) hit; control path CLOSED, agent marked unhealthy: %v\n", maxControlQueueLines, reason)
	}
}

// observeResponse handles one response record. A record repeating one of
// OUR command ids is the command's outcome: success=false is logged to
// stderr (a rejected command — the control line never reached the agent,
// so the pending ack wait will surface the failure through its bound) and
// NEVER treated as an ack. A success record, and every record for a
// foreign or absent id, is silently dropped by the caller as bulk.
func (p *Proc) observeResponse(rec stdoutRecord) {
	if rec.ID == nil || *rec.ID == "" {
		return // parse failures and foreign commands: not ours to observe
	}
	p.pendMu.Lock()
	_, ours := p.pending[*rec.ID]
	delete(p.pending, *rec.ID)
	p.pendMu.Unlock()
	if !ours {
		return
	}
	if rec.Success != nil && !*rec.Success {
		fmt.Fprintf(os.Stderr, "mainagent: pi command %s (%s) failed: %s\n", *rec.ID, orDefault(rec.Command, "?"), orDefault(rec.ErrText, "unknown error"))
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// assistantText extracts the assistant message text from the message
// object of one message_end record. ok=false means the record does not
// carry an assistant message (user/system echoes, malformed payloads):
// the caller drops it as bulk. Content is a typed-block array
// ([{"type":"text","text":...}]); a bare string is tolerated.
// message_end is the AUTHORITATIVE final message (pi docs), so acks are
// scanned here exactly once — never in the message_update deltas, which
// stream the same text piecemeal (scanning both would queue one ack
// twice and let a stale duplicate satisfy a LATER wait for the same
// method). The tradeoff is ack latency: an ack is visible when its
// message completes, not while it streams; the pf-execute contract acks
// at safe boundaries with short messages, well inside the bounded waits.
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
	// Defensive: a bare string content.
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, true
	}
	return "", false
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

// writeStdin writes ONE framed stdin record (a JSON object + LF); a dead
// pipe reports the process as gone. An agent marked unhealthy (control
// path closed by overflow or an overlong line) fails fast with the typed
// reason instead of writing into a path whose acks can never arrive.
func (p *Proc) writeStdin(payload any) error {
	if r, ok := p.unhealthyReason(); ok {
		return fmt.Errorf("mainagent: agent marked unhealthy, control path closed: %w", r)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return fmt.Errorf("mainagent: agent stdio closed")
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := p.stdin.Write(append(blob, '\n')); err != nil {
		return fmt.Errorf("mainagent: agent not accepting control messages: %w", err)
	}
	// Correlation: register the command id of a pi RPC command so the
	// matching response record can be observed (failures are logged).
	// Registered AFTER the successful write (pendMu, never mu: see the
	// field comment) so the stdout reader can keep draining.
	if cmd, ok := payload.(piCommand); ok && cmd.ID != "" {
		p.pendMu.Lock()
		if p.pending == nil {
			p.pending = map[string]bool{}
		}
		p.pending[cmd.ID] = true
		p.pendMu.Unlock()
	}
	return nil
}

// SendControl delivers ONE daemon→agent control method: the control JSON
// line (controlEnvelope) is framed for the adapter's transport — under the
// pi adapter it rides a prompt command whose message text is the line
// VERBATIM, so the agent-side executor parses the user message text
// exactly as it parsed the raw line. The bounded lossless ctrl-queue
// semantics on the ack side are unchanged.
func (p *Proc) SendControl(method string, params map[string]any) error {
	line, err := json.Marshal(controlEnvelope{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return p.writeStdin(p.adapter.FrameControl(nextCommandID(), string(line)))
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
			Method string         `json:"method"`
			Result map[string]any `json:"result"`
		}
		// The EXACT closed ack shape only (fix round 2): a genuine ack
		// echoes the acknowledged method in BOTH its method field and
		// result.ack. A bulk line merely carrying the method name — the
		// noisy-harness spoof — has no result.ack and must never satisfy
		// the pending ack; an ack line for a DIFFERENT method keeps waiting.
		if json.Unmarshal([]byte(line), &ack) == nil && ack.Method == method {
			if echoed, ok := ack.Result["ack"].(string); ok && echoed == method {
				return nil
			}
		}
		// Other control traffic: keep waiting.
	}
}

// NotifyStop implements the graceful-shutdown stop-advancing notice
// (04 §6.5 step 3): the agent picks its own next safe boundary; the daemon
// never picks or skips steps on its behalf (00 §3.6).
func (p *Proc) NotifyStop(ctx context.Context) error {
	if err := p.SendControl(MethodStopNotice, map[string]any{"reason": "graceful_shutdown"}); err != nil {
		return err
	}
	return p.awaitAck(ctx, MethodStopNotice)
}

// Quiesce implements the freeze confirmation (05 §2.7 / 04 §6.4): the
// agent confirms no in-flight sync-surface writes and none will start.
func (p *Proc) Quiesce(ctx context.Context) error {
	if err := p.SendControl(MethodQuiesce, nil); err != nil {
		return err
	}
	return p.awaitAck(ctx, MethodQuiesce)
}

// Resume implements the freeze-release notification (05 §2.7): the write
// freeze window is over (the staged bytes are pinned), so the agent may
// resume sync-surface writes. Best-effort by design — an agent that died
// mid-window simply never acks.
func (p *Proc) Resume(ctx context.Context) error {
	if err := p.SendControl(MethodResume, nil); err != nil {
		return err
	}
	return p.awaitAck(ctx, MethodResume)
}

// DeliverAnswer relays an answer payload verbatim to the waiting agent
// (00 §3.4a: the daemon forwards, never parses) and WAITS for the agent's
// pf/answer ack before reporting success (SK-4/CR-3). The ack is the
// durability signal the two-sided contract already defines: pf-execute
// §12 commits the agent to "Ack after the relay is durably recorded" —
// the pending_answer persist happens BEFORE the ack. Returning success on
// the bare pipe write turned the window between the write and that atomic
// persist into a PERMANENT loss: the daemon had told the submitter the
// answer was applied, the answer_id idempotency (00 §3.4a) then blocks
// any re-delivery of the same answer, and an agent crash inside that
// window left the question pending forever with its answer gone. Awaiting
// the ack (bounded like every other control wait) closes the window: on
// timeout, agent death or a closed control path this reports FAILURE, the
// question stays pending on the agent's persisted state, and the answer
// stays re-deliverable — the same relay reaches a re-derived agent
// attached to the same capsule. A daemon-side pending-answer journal would
// add a second durable record for data the capsule already owns; the ack
// wait is the design-faithful smaller shape.
func (p *Proc) DeliverAnswer(relay daemon.AnswerRelay) error {
	b, err := json.Marshal(relay)
	if err != nil {
		return err
	}
	params := map[string]any{}
	json.Unmarshal(b, &params)
	if err := p.SendControl(MethodAnswer, params); err != nil {
		return err
	}
	return p.awaitAck(context.Background(), MethodAnswer)
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
	if !daemon.WaitForGroupEmpty(pgid, timeout, "mainagent") {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !daemon.WaitForGroupEmpty(pgid, 5*time.Second, "mainagent") {
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
	// Stdout reader: under the REAL pi protocol stdout is a RECORD STREAM
	// (classifyStdoutLine): assistant message records carry the ack lines
	// into the bounded LOSSLESS queue (an ack may never be dropped — a lost
	// pf/quiesce or pf/stop_notice ack becomes a freeze/boundary timeout,
	// 05 §2.7); response records for our command ids are observed for
	// failure; every other record is bulk and may be dropped when nobody
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
			proc.classifyStdoutLine(line)
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
		if serr := proc.writeStdin(*m); serr != nil {
			_ = proc.Terminate(5 * time.Second)
			return nil, fmt.Errorf("mainagent: initial instruction injection failed (DAEMON-503): %w", serr)
		}
	}
	return proc, nil
}
