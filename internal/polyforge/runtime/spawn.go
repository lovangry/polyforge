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
// harness configuration (global skills, context files and prompt
// templates). The agent's only instruction of record is the initial prompt
// injected at derivation; a capsule's materialized step-agent definitions
// are a different resource family and stay reachable.
//
// --no-extensions is DELIBERATELY ABSENT and must stay absent (the v0
// supervisor's live-tested baseline, v0/internal/mainagent/supervisor.go:
// it silences the operator's extension UI noise but also removes the
// harness-native subagent tool — which is extension-provided — and with
// it the whole step-agent dispatch path; the harness has no built-in
// subagent facility to fall back on). The residual exposure of
// extension-registered harness background tasks is closed by instruction
// instead (the executor rules forbid a WI agent from using any harness
// background-task mechanism).
var piIsolationFlags = []string{
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
	// SessionHandle is the PUBLIC session fingerprint (B3): injected as
	// PF_SESSION_HANDLE. The SECRET session id never rides SpawnParams
	// at all — it must not reach the capsule journal, the env or argv.
	SessionHandle   string
	// SpawnN is this derivation's ordinal (B2): the per-WI monotonic
	// counter value AFTER this derivation (state.json spawn_n + 1
	// locally; capsule spawn-journal max + 1 across a fresh clone).
	// Injected as PF_SPAWN_N — the agent's flush fence identity.
	SpawnN         int
	TranscriptPath string // resume source; empty for a fresh derivation
	Resume         bool   // attach to TranscriptPath instead of a new session
	InitialPrompt  string
	CgroupPath     string // per-WI cgroup v2 dir; "" = layer 0 unavailable
	// InstanceID is THIS derivation's fence token for the v1→v1.5
	// transition: the session fingerprint ("#N" appended after
	// re-derivations, mirroring the v1 attempt#N shape). It rides the
	// spawn journal and RTMsg.AttemptID; the cleanup commit deletes the
	// protocol field and this collapses to SpawnN.
	InstanceID string
	// Model pins this derivation's harness model: the effective candidate
	// of the bundle's defaults model_candidates chain (F4/P6). "" = no
	// chain: the harness adapter's default stays in effect. Carried as
	// SPAWN ARGV, not capsule settings — see buildCmd for the measured
	// reasoning.
	Model   string
	Journal func(pid int, startedAt string) error
}

// AgentHandle is the supervised face over one derived agent process.
type AgentHandle interface {
	PID() int
	StartedAt() string
	Alive() bool
	Done() bool
	Terminate(bound time.Duration) error
	SendControl(m protocol.ControlMsg) error
	// WaitControlAck awaits the agent's ack of exactly the control
	// message m, matching on (method, correlation) — not method alone, so
	// concurrent waiters (stop notice, answer, probe) can never steal each
	// other's acks.
	WaitControlAck(ctx context.Context, m protocol.ControlMsg, bound time.Duration) error
}

// Spawner derives agent processes; tests substitute fakes that spawn real
// but trivial child processes.
type Spawner interface {
	Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error)
}

// piSpawner is the production spawner for the pi harness. It holds the
// config BY POINTER (see DefaultSpawner): fields are read at spawn time
// from the shared config, never from a copy.
type piSpawner struct {
	cfg *Config
}

func newPiSpawner(cfg *Config) *piSpawner {
	return &piSpawner{cfg: cfg}
}

// recordTaps wires the runtime Config's RecordTap into the process's
// stdout tap chain: the production daemon binds RecordTap to its observe
// adapter, so EVERY raw stdout record the spawner reads also reaches
// the Observe lane's stream monitoring. Without the binding the record
// stream (and with it the whole detection face) would be blind in
// production.
func (s *piSpawner) recordTaps(p SpawnParams) []func(string) {
	if s.cfg.RecordTap == nil {
		return nil
	}
	wi, tap := p.WorkItemID, s.cfg.RecordTap
	return []func(string){func(line string) { tap(wi, []byte(line)) }}
}

func (s *piSpawner) bin() string {
	if s.cfg.PiBin != "" {
		return s.cfg.PiBin
	}
	return "pi"
}

// buildCmd constructs (without starting) the argv/env/cwd of one
// derivation. Exported shape for the isolation regression: argv carries
// the transport first, then the isolation flags, then the resume session,
// then — when the bundle pinned a candidate chain — the model pin.
//
// WHY ARGV, NOT CAPSULE-LOCAL PROJECT SETTINGS (measured, F4/P6): the
// harness's project-level settings file (<CapsuleDir>/.pi/settings.json)
// is NOT disabled by --no-context-files (that flag only stops the
// agent-instruction context files' discovery) — it is gated by PROJECT
// TRUST instead. In rpc
// mode the harness has no UI, and with the default trust policy ("ask"),
// no --approve override and no saved decision, protected project settings
// are SKIPPED ENTIRELY (verified live: a bogus defaultModel in the
// capsule's .pi/settings.json was ignored; harness source: the trust
// resolver answers false when it cannot prompt). Even when trusted and
// loaded, a settings defaultModel that fails to resolve SILENTLY falls
// back to automatic selection, while argv --model fails LOUDLY — and a
// pinned bundle candidate that cannot resolve is exactly the visible
// infrastructure failure the recovery engine should see. So the pin
// rides argv.
//
// Residual settings pollution, deliberately accepted and documented: the
// operator's agent-directory settings (per-model max_tokens-style
// overrides, thinking budgets, models.json entries) remain inherited —
// relocating the agent dir would also relocate the operator's PROVIDER
// CREDENTIALS (they live in the same directory), leaving the WI agent
// unable to call any model; copying them into the capsule is forbidden.
// The argv pin removes the defaultModel inheritance vector; every other
// operator preference keeps applying at the harness's own defaults. No
// max_tokens is set here — the pinned model's own default is the
// conservative choice (an invented cap is exactly the v0 settings-
// pollution failure shape, from the other side).
func (s *piSpawner) buildCmd(p SpawnParams) *exec.Cmd {
	args := []string{"--mode", "rpc"}
	args = append(args, piIsolationFlags...)
	if p.Resume {
		args = append(args, "--session", p.TranscriptPath)
	}
	if p.Model != "" {
		// the candidate ids are provider-qualified "<provider>/<model>",
		// exactly the form the harness's --model pattern accepts
		args = append(args, "--model", p.Model)
	}
	cmd := exec.Command(s.bin(), args...)
	cmd.Dir = p.CapsuleDir // cwd = capsule root
	additions := []string{
		// DEAD MARKER: the harness IGNORES this variable for transcript
		// placement (its session store answers to its own *_SESSION_DIR
		// name); kept only for harness-internal use — the daemon-side
		// compensation lives in pitranscript.go (F3).
		"PI_SESSION_DIR=" + filepath.Join(p.CapsuleDir, capsule.DirSession),
		"PF_WI_ID=" + p.WorkItemID, // layer-4 cleanup marker (deliberate addition)
		// v1.5 B2/B3 identity: the PUBLIC session handle and the monotonic
		// derivation counter — the agent's flush fence keys. The SECRET
		// session id is deliberately ABSENT (it must never reach agent
		// env; it lives only in state.json and the one-shot askpass), and
		// so are the deleted PF_SESSION_ID / PF_GENERATION / PF_INSTANCE_ID.
		"PF_SESSION_HANDLE=" + p.SessionHandle,
		"PF_SPAWN_N=" + strconv.Itoa(p.SpawnN),
	}
	if s.cfg.SocketPath != "" {
		// deliberate addition, same mechanism as PF_WI_ID: the env sanitize
		// point strips every inherited PF_*, so the daemon's UDS path (the
		// agent's flush(reason) proxy, §五) is handed back explicitly.
		additions = append(additions, "PF_DAEMON_SOCKET="+s.cfg.SocketPath)
	}
	cmd.Env = newEnvSanitizer(s.cfg.EnvSecrets...).childEnv(additions...)
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
	proc := newProc(cmd, stdin, stdout, s.recordTaps(p)...)
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
// runtime-state/spawn-journal.jsonl. v1.5 (contract §4.3): the line gains
// the additive spawn_n field (the derivation ordinal — the last line
// carries the latest derivation, so a fresh clone on another machine
// re-seeds its counter from the max) and the public session_handle. The
// v1 synthetic session_id value is DROPPED: in v1.5 that name means the
// SECRET, which must never land in the capsule (B3).
func writeSpawnJournal(p SpawnParams, pid int, startedAt string, instance string) error {
	entry := map[string]any{
		"at":            nowStamp(),
		"work_item":     p.WorkItemID,
		"instance":      instance,
		"pid":           pid,
		"started_at":    startedAt,
		"session_handle": p.SessionHandle,
		"spawn_n":       p.SpawnN,
		"resume":        p.Resume,
		"transcript":    p.TranscriptPath,
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

// maxSpawnNFromJournal answers the highest spawn_n the capsule's spawn
// journal records (0 when absent/unreadable): the B2 fresh-clone source —
// a takeover or attach that cloned this capsule from the bare repo
// continues the derivation counter where the capsule's history left off.
func maxSpawnNFromJournal(capsuleDir string) int {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.DirRuntimeState, "spawn-journal.jsonl"))
	if err != nil {
		return 0
	}
	max := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			SpawnN int `json:"spawn_n"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.SpawnN > max {
			max = e.SpawnN
		}
	}
	return max
}

// ---------------------------------------------------------------------------
// Proc: the supervised handle.

const (
	maxControlLineBytes = 4 << 20
)

var errControlTooLong = fmt.Errorf("runtime: control line exceeded the bounded reader limit")

// proc wraps one derived harness process.
type proc struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	startedAt string
	taps      []func(string) // stdout record taps (the observe adapter binding)
	acks      ackHub

	mu   sync.Mutex // guards stdin writes
	pend sync.Mutex // guards pending ids (own lock: stdin writes may block)

	pending map[string]bool
	exited  chan struct{}
	exitErr error
}

func newProc(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, taps ...func(string)) *proc {
	p := &proc{
		cmd:       cmd,
		stdin:     stdin,
		startedAt: nowStamp(),
		pending:   map[string]bool{},
		acks:      newAckHub(),
		exited:    make(chan struct{}),
		taps:      taps,
	}
	go func() {
		p.exitErr = cmd.Wait()
		close(p.exited)
	}()
	go func() {
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
				fmt.Fprintf(os.Stderr, "runtime: control line over %d bytes dropped: %v\n", maxControlLineBytes, errControlTooLong)
				continue // keep draining as bulk, never block the child
			}
			for _, t := range p.taps {
				t(line)
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

// WaitControlAck awaits the agent's ack of exactly m, bounded like every
// control wait. The match is (method, correlation): the correlation token
// of m is the question id (pf/answer) or the nonce (pf/probe) when those
// are set, otherwise the numeric Seq — and the ack satisfies the wait by
// echoing that token (Ref for string tokens, Seq for numeric ones).
func (p *proc) WaitControlAck(ctx context.Context, m protocol.ControlMsg, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	qctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	w := p.acks.register(m)
	defer p.acks.unregister(w)
	if ack := p.acks.claim(w); ack != nil {
		return ackOutcome(m, *ack)
	}
	select {
	case ack := <-w.ch:
		return ackOutcome(m, ack)
	case <-qctx.Done():
		return fmt.Errorf("runtime: no ack for %s: %w", m.Method, qctx.Err())
	case <-p.exited:
		return fmt.Errorf("runtime: no ack for %s: agent exited", m.Method)
	}
}

func ackOutcome(m protocol.ControlMsg, ack protocol.ControlAck) error {
	if ack.OK {
		return nil
	}
	return fmt.Errorf("runtime: agent refused %s: %s", m.Method, ack.Error)
}

// ackTokenOf is the correlation token a control message carries:
// pf/answer → the question id, pf/probe → the nonce, everything else →
// the numeric seq.
func ackTokenOf(m protocol.ControlMsg) string {
	switch m.Method {
	case protocol.ControlAnswer:
		if m.ID != "" {
			return m.ID
		}
	case protocol.ControlProbe:
		if m.Nonce != "" {
			return m.Nonce
		}
	}
	return strconv.FormatInt(m.Seq, 10)
}

// ackMatches decides whether an incoming ack answers the message m: the
// method must agree and the echoed token must agree (Ref for the string
// tokens, Seq for the numeric one). A method whose message carries no
// correlation at all (Seq 0) matches its tokenless ack.
func ackMatches(m protocol.ControlMsg, ack protocol.ControlAck) bool {
	if ack.Method != m.Method {
		return false
	}
	token := ackTokenOf(m)
	if ack.Ref != "" {
		return ack.Ref == token
	}
	return strconv.FormatInt(ack.Seq, 10) == token
}

// classify sorts one stdout line: response records repeating OUR command
// ids are observed (success=false is logged, never an ack) and dropped;
// message_end records carrying an assistant message are the ack carrier —
// their text is scanned line-by-line for the closed ack shape and every
// match is ROUTED to the waiter whose (method, correlation) it answers;
// everything else is bulk.
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

// controlCandidate routes one ack-shaped line: a JSON object carrying a
// control method of the closed vocabulary is delivered to the waiter it
// answers — by (method, correlation), so concurrent waiters never steal
// each other's acks. Refusals (ok=false) are delivered too: the waiter
// must learn its method was refused, not time out on a dropped line. An
// ack that answers no live waiter (a late echo of a dead wait) is
// dropped.
func (p *proc) controlCandidate(line string) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "{") {
		return
	}
	var ack protocol.ControlAck
	if json.Unmarshal([]byte(s), &ack) != nil || ack.Method == "" {
		return
	}
	switch ack.Method {
	case protocol.ControlAnswer, protocol.ControlStopNotice, protocol.ControlProbe:
		// closed vocabulary
	default:
		return
	}
	p.acks.deliver(ack)
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
	// Setpgid: the child IS the group leader.
	return killGroup(p.cmd.Process.Pid, bound)
}

// ---------------------------------------------------------------------------
// The ack hub: per-waiter ack routing. The review's dual-key fix: a single
// FIFO let concurrent waiters consume each other's lines and never matched
// the echoed correlation. Each waiter registers under its own control
// message; an incoming ack is handed to the FIRST waiter whose (method,
// correlation) it satisfies. An ack that races ahead of its waiter —
// SendControl returns before WaitControlAck parks the wait, and a fast
// agent may ack in between — parks in the hub's bounded orphan backlog
// and is claimed by the waiter at registration, so nothing is lost on
// that path either.

const maxAckBacklog = 16 // bounds both the per-waiter and orphan backlogs

// ackHub routes control acks to the waiter that asked for them.
type ackHub struct {
	mu      sync.Mutex
	next    uint64
	waiters map[uint64]*ackWaiter
	orphans []protocol.ControlAck // acks that arrived before their waiter
}

func newAckHub() ackHub {
	return ackHub{waiters: map[uint64]*ackWaiter{}}
}

// ackWaiter is one outstanding WaitControlAck.
type ackWaiter struct {
	id      uint64
	msg     protocol.ControlMsg
	ch      chan protocol.ControlAck
	backlog []protocol.ControlAck
}

// register parks one waiter for m. The returned waiter's channel is
// buffered 1; an ack that raced ahead of the registration is served
// first through claim.
func (h *ackHub) register(m protocol.ControlMsg) *ackWaiter {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	w := &ackWaiter{id: h.next, msg: m, ch: make(chan protocol.ControlAck, 1)}
	h.waiters[w.id] = w
	return w
}

func (h *ackHub) unregister(w *ackWaiter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.waiters, w.id)
}

// claim hands w one ack answering its message: its channel (already
// delivered) first, then its backlog, then the hub's orphan backlog (the
// SendControl→WaitControlAck race window).
func (h *ackHub) claim(w *ackWaiter) *protocol.ControlAck {
	select {
	case ack := <-w.ch:
		return &ack
	default:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(w.backlog) > 0 {
		ack := w.backlog[0]
		w.backlog = w.backlog[1:]
		return &ack
	}
	for i, orphan := range h.orphans {
		if ackMatches(w.msg, orphan) {
			h.orphans = append(h.orphans[:i], h.orphans[i+1:]...)
			ack := orphan
			return &ack
		}
	}
	return nil
}

// deliver routes one parsed ack to the first waiter it answers. A waiter
// with a full channel parks the ack in its bounded backlog; an ack that
// answers nobody YET parks in the bounded orphan backlog (its waiter may
// be one SendControl-return away); beyond the bound it is dropped.
func (h *ackHub) deliver(ack protocol.ControlAck) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.waiters {
		if !ackMatches(w.msg, ack) {
			continue
		}
		select {
		case w.ch <- ack:
		default:
			if len(w.backlog) < maxAckBacklog {
				w.backlog = append(w.backlog, ack)
			}
		}
		return
	}
	if len(h.orphans) < maxAckBacklog {
		h.orphans = append(h.orphans, ack)
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
