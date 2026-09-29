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
//   - Claude: claude -p --input-format stream-json --output-format
//     stream-json --verbose — the REAL persistent streaming mode
//     (measured live, Claude Code CLI 2.1.280): one long-lived process,
//     one user-message record per stdin line, JSON protocol records on
//     stdout, graceful exit on stdin EOF. pf-execute is injected as an
//     appended system prompt plus one kickoff user message; crash
//     continuation resumes via claude --resume <session uuid>.
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
	"crypto/subtle"
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
	// MethodProbe: the watchdog CONFIRMATION probe (04 §6.2 runtime note,
	// stream-heartbeat revision / polyforge#48). Sent ONLY after the hang
	// window has already expired, as the last benefit of the doubt before
	// the kill: the agent acks it at its next safe boundary exactly like
	// every other control method, and an ack CANCELS the kill. It carries
	// no instruction — the agent is not asked to do anything, only to
	// prove its loop still turns.
	MethodProbe = "pf/probe"
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
	// pf-execute instruction. Pi wraps it as the session's first prompt
	// command; claude rides the system prompt for the instruction itself
	// (04 §6.1a) and returns a fixed kickoff user message — the real CLI
	// never begins a turn until a user message arrives on stdin
	// (measured), so the main loop needs one to start.
	InitialMessage(p SpawnParams) (any, error)
	// FrameControl returns the stdin payload for ONE daemon→agent control
	// line. line is the transport-neutral control JSON (controlEnvelope
	// serialized). Both v1 adapters carry it as the text of a user-facing
	// message — pi wraps it in a prompt command, claude in a
	// streaming-input user-message record — so the agent-side pf-execute
	// parses the user message text as the control line VERBATIM.
	FrameControl(id, line string) any
}

// ---------------------------------------------------------------------------
// Env sanitize point (04 §8): the ONE place every spawned WI Agent's
// environment is built.

// envSanitizer is the single sanitize point both adapters route their
// cmd.Env through. Design 04 §8: AIHub credentials must NEVER enter a WI
// Agent's env/cwd/argv — yet the daemon legitimately holds its machine
// api key (from --api-key, POLYFORGE_API_KEY or --api-key-file), and in
// the --api-key/POLYFORGE_API_KEY modes that key sits in the DAEMON'S OWN
// environment. The previous wholesale inherit (`append(os.Environ(),
// ...)`) therefore handed the key to every spawned agent VERBATIM — a
// literal §8 violation reachable by ONE operator mistake (`pf daemon
// --api-key <key>`; the pilot runs use --api-key-file, which is
// compliant). This closes it: the child env is built as
// (sanitized os.Environ) + (the capsule/session additions), where the
// sanitize is a BLACKLIST — deliberately not a whitelist, the harness
// needs PATH/HOME and its own config vars and must not be broken by an
// invented allowlist — stripping at minimum:
//
//   - every POLYFORGE_* variable (the polyforge/aihub credential
//     namespace: POLYFORGE_API_KEY, POLYFORGE_AIHUB_URL, ...);
//   - every PF_* variable;
//   - belt and braces, any variable whose VALUE equals the daemon's
//     currently-configured api key, so even a RENAMED variable (an
//     operator exporting the key under another name) cannot smuggle it.
type envSanitizer struct {
	// apiKey is the daemon's currently-configured machine api key,
	// captured ONCE at construction (constructor injection via
	// NewSupervisor — never parsed per spawn). Empty means no key is
	// configured: the exact-value filter is unarmed, the prefix blacklist
	// still applies.
	apiKey string
}

// newEnvSanitizer captures the daemon's api key for the lifetime of the
// supervisor (04 §8: one read, not one per spawn).
func newEnvSanitizer(apiKey string) *envSanitizer {
	return &envSanitizer{apiKey: apiKey}
}

// childEnv builds one spawned agent's environment: sanitized os.Environ
// followed by the adapter's capsule/session additions. The exact-value
// key filter is a single constant-time comparison per inherited entry
// (crypto/subtle.ConstantTimeCompare: no early-exit on a mismatched
// prefix), cheap enough to run over the whole environ on every spawn. A
// nil receiver is tolerated (adapters constructed bare in unit tests):
// the prefix blacklist still applies, only the key filter is unarmed.
func (e *envSanitizer) childEnv(additions ...string) []string {
	var key string
	if e != nil {
		key = e.apiKey
	}
	inherit := os.Environ()
	out := make([]string, 0, len(inherit)+len(additions))
	for _, kv := range inherit {
		name, val, _ := strings.Cut(kv, "=")
		// The polyforge/aihub credential namespace: everything the
		// daemon's own flag/env surface uses is POLYFORGE_* or PF_*.
		if strings.HasPrefix(name, "POLYFORGE_") || strings.HasPrefix(name, "PF_") {
			continue
		}
		// Belt and braces: a renamed variable whose VALUE is exactly the
		// configured key cannot carry it either.
		if key != "" && subtle.ConstantTimeCompare([]byte(val), []byte(key)) == 1 {
			continue
		}
		out = append(out, kv)
	}
	return append(out, additions...)
}

// ---------------------------------------------------------------------------
// Pi adapter (04 §6.1a): pi --mode rpc, session dir inside the Capsule.

type PiAdapter struct {
	// Bin overrides the pi executable path (defaults to "pi"; tests
	// substitute a fake).
	Bin string
	// env is the shared §8 env sanitizer (constructor-injected once by
	// NewSupervisor, shared with the claude adapter). Nil keeps the
	// prefix blacklist with the exact-value key filter unarmed.
	env *envSanitizer
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
	// Credentials never enter the env (04 §8): sanitized os.Environ plus
	// ONLY capsule path + session id additions, via the shared sanitizer.
	cmd.Env = a.env.childEnv(
		"PI_SESSION_DIR="+sessionDir,
		"PF_SESSION_ID="+p.SessionID,
		"PF_WORK_ITEM_ID="+p.WorkItemID,
		"PF_GENERATION="+fmt.Sprint(p.Generation),
	)
	return cmd, nil
}

func (a *PiAdapter) InitialMessage(p SpawnParams) (any, error) {
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
// Claude adapter (04 §6.1a): claude -p in streaming-input mode, stable
// session id + streaming; crash continuation resumes the same session.
//
// MEASURED against the real Claude Code CLI 2.1.280 (ground-truth probes,
// polyforge#33):
//
//   - `claude -p --input-format stream-json --output-format stream-json
//     --verbose` is a PERSISTENT multi-turn session: the process stays
//     alive across turns, one stdin line carries one user-message record
//     {"type":"user","message":{"role":"user","content":<string>}},
//     each turn streams JSON records to stdout, and stdin EOF exits
//     cleanly (exit 0). -p with the default text input is ONE-SHOT
//     (result record, process exits) and cannot host the §3.6a
//     persistent contract.
//   - `--output-format stream-json` in print mode REQUIRES --verbose
//     (the CLI refuses the combination without it).
//   - `--session-id <uuid>` pins the conversation id but demands a VALID
//     UUID ("sess_<wi>_<gen>" is rejected: "Invalid session ID. Must be a
//     valid UUID."); `--resume <uuid>` continues that conversation
//     (context retained, same id echoed). daemon.ClaudeSessionUUID maps the
//     polyforge session id onto a deterministic UUID so the §3.6a
//     identity rules ride through unchanged.
//   - A session with ZERO completed turns is NOT resumable: --resume of a
//     conversation that never finished a turn exits 1 with "No
//     conversation found with session ID: <uuid>". A crash before the
//     first turn completes therefore used to break the continuation
//     chain — the re-derived claude died at startup and the daemon's
//     dead-agent handling crash-looped on the same refusal forever. The
//     supervisor's zero-turn startup watch (Spawn) now catches the
//     refusal and falls back to a FRESH derivation under the SAME
//     harness session id (see the ZERO-TURN CRASH WINDOW paragraph
//     below). UNVERIFIED: whether a crash MID first turn leaves a
//     resumable conversation.
//   - Output record shapes (no --include-partial-messages): hook noise +
//     {"type":"system","subtype":"init",...,"session_id"} per turn,
//     {"type":"assistant","message":{"role":"assistant","content":
//     [{"type":"text"|"thinking",...}]}} per assistant message (a
//     message may be thinking-only — no text block), and ONE
//     {"type":"result","subtype":"success"|"error_during_execution",
//     "result":<last assistant text>,"is_error":<bool>,"num_turns",
//     "result_index"} per turn.
//
// Still UNVERIFIED against the real CLI (kept conservative):
//
//   - mid-turn control delivery: a user message arriving while a turn is
//     in flight (real pi queues it via steer; claude's streaming input is
//     documented to queue — not measured here);
//   - --append-system-prompt on --resume: the flag COMBINATION is
//     accepted (measured: --resume + --append-system-prompt + streaming
//     input exits 0 on a persisted conversation); the SEMANTICS are not
//     live-measured: the CLI records the system prompt on the
//     conversation's first request (system-prompt snapshot) and reuses
//     that record on later resumes, so a re-derived agent keeps the FIRST
//     generation's pf-execute text even when the daemon re-injects an
//     updated one (CLI help text, not live-measured);
//   - capsule-local session storage (00 §3.6a "session data placement"):
//     the real CLI writes conversations under CLAUDE_CONFIG_DIR (default
//     ~/.claude), which also owns credentials — relocating it into the
//     capsule would move auth material there (04 §8 forbids). v1 claude
//     session isolation therefore rides the pinned conversation UUID,
//     not a capsule-local directory.
//
// THE ZERO-TURN CRASH WINDOW is the cc twin of the daemon's
// spawn-prologue orphan window (R3N-F6, 04 §6.1): both are the sliver
// between derivation and the first durable record where a crash leaves
// nothing the continuation chain can re-attach to. The daemon's window
// is between the process start and the identity journal's fsync; this
// one is between the claude spawn and the first COMPLETED turn — the
// conversation only becomes resumable then (measured), so an agent
// crash inside that window leaves a session identity on disk whose
// conversation the CLI refuses to resume, and every re-derivation
// hits the same refusal. The reaction is adapter-internal, exactly as
// the journal callback is daemon-internal: when a continuation
// derivation observes the measured refusal (exit 1 with "No
// conversation found with session ID: <uuid>" before any assistant or
// successful result record), the supervisor falls back to a FRESH
// derivation under the SAME harness session id — the same conversation
// UUID pinned via --session-id (NOT --resume), the same initial
// instruction. This preserves 04 §6.1a's continuation semantics (same
// session identity, same generation, same machine — 03 §4.4's only
// reuse case): the fallback is shape-internal, and neither the
// supervisor's face nor the daemon needs to know it exists.

type ClaudeAdapter struct {
	Bin string
	// env is the shared §8 env sanitizer (constructor-injected once by
	// NewSupervisor, shared with the pi adapter). Nil keeps the prefix
	// blacklist with the exact-value key filter unarmed.
	env *envSanitizer
	// Resume marks a crash-continuation derivation: claude --resume <sid>.
	// Set by the supervisor's session-identity read-back (Spawn), never
	// by hand in production paths.
	Resume bool
	// freshFallback, armed by the supervisor's zero-turn refusal
	// fallback (ArmFreshFallback), re-arms the NEXT BuildCmd as a FRESH
	// derivation under the SAME session id: the same conversation UUID
	// via --session-id instead of --resume. One-shot — consumed by that
	// BuildCmd — because the supervisor arms it at most once per Spawn.
	freshFallback bool
	// lastSid is the conversation UUID of the most recent BuildCmd — the
	// id the measured refusal text names, so the startup classification
	// matches the refusal of OUR conversation, never a foreign one.
	lastSid string
}

func (a *ClaudeAdapter) Name() string { return capsule.AdapterClaude }

func (a *ClaudeAdapter) claudeBin() string {
	if a.Bin != "" {
		return a.Bin
	}
	return "claude"
}

// claudeUserMessage is one stdin record of the REAL claude streaming-input
// protocol (measured): {"type":"user","message":{"role":"user",
// "content":"<text>"}}. The daemon→agent control JSON line rides
// `content` VERBATIM — the agent-side pf-execute parses the user message
// text exactly as the pi-side parses its prompt-command text (the same
// §12 two-sided contract).
type claudeUserMessage struct {
	Type    string                `json:"type"` // "user"
	Message claudeUserMessageBody `json:"message"`
}

type claudeUserMessageBody struct {
	Role    string `json:"role"` // "user"
	Content string `json:"content"`
}

// claudeKickoff is the first-turn user message. The pf-execute instruction
// rides --append-system-prompt (04 §6.1a), but a claude -p session never
// begins a turn until a user message arrives on stdin (measured: a
// streaming-input session fed immediate EOF emits hook records and exits
// without a single turn), so the adapter kicks the main loop off with
// this fixed line. Deliberately NOT a control JSON line: nothing to ack.
const claudeKickoff = "pf-execute kickoff: begin the work-item main loop now, following the pf-execute instructions in your system prompt."

// ClaudeSessionUUID (daemon package) is the ONE harness-session-id →
// conversation-UUID mapping: the adapter's spawn argv and the daemon's
// seal-time terminal-state collection both call it — never a second
// mapping anywhere.
func (a *ClaudeAdapter) BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error) {
	sid := daemon.ClaudeSessionUUID(p.SessionID)
	a.lastSid = sid
	// The REAL persistent-session argv (measured): -p plus streaming input
	// and output; --verbose is REQUIRED for stream-json output in print
	// mode. One process per WI for its whole lifetime (00 §3.6a
	// persistence): each turn is one user-message record on stdin, exit
	// comes at stdin EOF.
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	switch {
	case a.freshFallback:
		// Zero-turn refusal fallback: a FRESH derivation under the SAME
		// harness session id — the same conversation UUID via --session-id,
		// NOT --resume — so 04 §6.1a continuation semantics survive (same
		// session identity, same generation, same machine). One-shot.
		args = append(args, "--session-id", sid)
		a.freshFallback = false
	case a.Resume:
		// Crash continuation: same session id, same machine, same
		// generation — the ONLY reuse case (03 §4.4).
		args = append(args, "--resume", sid)
	default:
		args = append(args, "--session-id", sid)
	}
	// pf-execute is injected as an APPENDED SYSTEM PROMPT (not a user
	// message that later traffic can wash away), per 04 §6.1a. On a
	// --resume derivation the CLI reuses the conversation's recorded
	// system-prompt snapshot (see the UNVERIFIED note above).
	args = append(args, "--append-system-prompt", p.InitialPrompt)
	cmd := exec.Command(a.claudeBin(), args...)
	cmd.Dir = p.CapsuleDir
	// Credentials never enter the env (04 §8): sanitized os.Environ plus
	// ONLY the session id additions, via the shared sanitizer. sessionDir
	// is deliberately UNUSED: the real CLI has no capsule-local session-dir
	// env (the old CLAUDE_SESSION_DIR was an invented variable the CLI
	// ignores); conversations live under CLAUDE_CONFIG_DIR keyed by the
	// pinned UUID.
	cmd.Env = a.env.childEnv(
		"PF_SESSION_ID="+p.SessionID,
		"PF_WORK_ITEM_ID="+p.WorkItemID,
	)
	return cmd, nil
}

func (a *ClaudeAdapter) InitialMessage(p SpawnParams) (any, error) {
	// The instruction rides the system prompt (04 §6.1a); this kickoff
	// user message is the turn that starts the main loop (see
	// claudeKickoff).
	m := claudeUserMessage{Type: "user"}
	m.Message.Role = "user"
	m.Message.Content = claudeKickoff
	return m, nil
}

// FrameControl implements Adapter: ONE control line rides one
// streaming-input user-message record whose content is the control JSON
// VERBATIM — the agent-side pf-execute parses the user message text
// exactly as the pi-side parses its prompt-command text (the same §12
// contract). The id is UNUSED: the real claude CLI has no command-id
// correlation (no response records to observe); per-turn outcomes arrive
// as result records instead.
func (a *ClaudeAdapter) FrameControl(id, line string) any {
	m := claudeUserMessage{Type: "user"}
	m.Message.Role = "user"
	m.Message.Content = line
	return m
}

// MarkContinuation implements the supervisor's optional continuation face:
// the capsule's session identity already existed with the matching id
// (03 §4.4's only reuse case), so the next BuildCmd resumes the same
// claude conversation instead of pinning a fresh one.
func (a *ClaudeAdapter) MarkContinuation() { a.Resume = true }

// claudeZeroTurnRefusalText is the MEASURED refusal line (claude 2.1.280,
// polyforge#33): --resume of a conversation that never completed a turn
// prints exactly this — naming the requested id — to stderr and exits 1,
// before serving any turn.
const claudeZeroTurnRefusalText = "No conversation found with session ID: "

// StartupRefusal implements zeroTurnFallback: it classifies the startup
// death of ONE continuation derivation as the measured zero-turn resume
// refusal — and ONLY that. The discriminator set is deliberately tight:
// a session that already served a turn is not the zero-turn case; the
// exit code must be the measured 1; and the stderr tail must carry the
// refusal text naming OUR conversation UUID (the CLI echoes the id it
// was asked to resume). Any other startup death — a network error, a
// flag error, a clean exit — stays what it is: a typed spawn failure the
// caller reports.
func (a *ClaudeAdapter) StartupRefusal(exitErr error, stderrTail string, servedTurn bool) bool {
	if servedTurn {
		return false
	}
	var ee *exec.ExitError
	if !errors.As(exitErr, &ee) || ee.ExitCode() != 1 {
		return false
	}
	return a.lastSid != "" && strings.Contains(stderrTail, claudeZeroTurnRefusalText+a.lastSid)
}

// ArmFreshFallback implements zeroTurnFallback: the NEXT BuildCmd pins
// the same conversation UUID via --session-id instead of --resume. The
// supervisor arms at most ONE fallback per Spawn.
func (a *ClaudeAdapter) ArmFreshFallback() { a.freshFallback = true }

// continuationAdapter is the supervisor's OPTIONAL adapter face for
// harnesses that re-attach to an EXISTING conversation on re-derivation
// (claude: --resume). The supervisor marks the adapter when the capsule's
// session identity already existed with the matching id and nothing else;
// how the harness continues the session — or ignores the mark, as pi does
// (its session dir carries the state) — is adapter-internal, so the
// supervisor face stays shape-agnostic (00 §3.6a).
type continuationAdapter interface{ MarkContinuation() }

// zeroTurnFallback is the supervisor's OPTIONAL adapter face for
// harnesses whose CONTINUATION derivations can be REFUSED at startup
// (claude: the measured zero-turn --resume refusal — a conversation that
// never completed a turn is not resumable, so the re-derived harness
// dies at launch; 04 §6.1a's zero-turn crash window). When the adapter
// implements this face, the supervisor watches a continuation
// derivation's startup window; on the adapter's own classification of
// the refusal it arms ONE fresh fallback derivation under the SAME
// session id. The supervisor face stays shape-agnostic (00 §3.6a): a
// harness without the face (pi) never gets the watch, and the daemon
// observes neither the refusal nor the fallback.
type zeroTurnFallback interface {
	continuationAdapter
	// StartupRefusal classifies the startup death of one continuation
	// derivation. exitErr is the process's Wait() outcome, stderrTail
	// the captured tail of the harness stderr, servedTurn whether the
	// stream proved ANY served turn (an assistant record or a non-error
	// result record) before the death — the measured refusal arrives
	// before any of those.
	StartupRefusal(exitErr error, stderrTail string, servedTurn bool) bool
	// ArmFreshFallback re-arms the NEXT BuildCmd as a FRESH derivation
	// under the SAME session id.
	ArmFreshFallback()
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
	// turnServed is closed (once) when the stdout stream classifies a
	// record proving the harness SERVED a turn — any assistant record,
	// or a result record that is not the turn's own error outcome. The
	// zero-turn startup watch (a continuation derivation of a
	// zeroTurnFallback adapter) uses it as the healthy signal that ends
	// the watch; the measured zero-turn resume refusal emits neither.
	turnServed chan struct{}
	turnOnce   sync.Once
	// stream is the polyforge#48 record-stream observation (lastRecordAt,
	// in-flight turn, infra incidents) the daemon's hang watchdog reads
	// through the AgentStreamProbe face. See streamheartbeat.go; nil on a
	// bare Proc built by a unit test, which every reader tolerates.
	stream *streamState
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

// maxStderrTail bounds the captured harness stderr tail. The zero-turn
// startup watch reads it for the measured refusal text, which sits at
// the END of the stream (right before the exit), so a tail buffer is
// sufficient — and a chatty harness cannot balloon it.
const maxStderrTail = 64 << 10

// stderrTail keeps the LAST max bytes written to it. The zero-turn
// startup watch needs the harness's final stderr lines (the measured
// refusal text) without an unbounded buffer; it is tee'd next to
// os.Stderr so the daemon's log face is unchanged.
type stderrTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

// Write implements io.Writer (tail-truncating).
func (t *stderrTail) Write(b []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	t.mu.Unlock()
	return len(b), nil
}

// String returns the captured tail.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

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
	case MethodQuiesce, MethodResume, MethodStopNotice, MethodAnswer, MethodProbe:
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
	Subtype string          `json:"subtype"` // claude system/result subtypes
	ID      *string         `json:"id"`      // response records repeat the command id (null = not ours)
	Command string          `json:"command"` // "prompt" on response records
	Success *bool           `json:"success"`
	ErrText string          `json:"error"`
	Message json.RawMessage `json:"message"`  // pi message_end / claude assistant
	IsError *bool           `json:"is_error"` // claude result records
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
//   - a claude assistant record (same inner message shape as pi's
//     message_end: role + typed content blocks, measured) is the claude
//     ack carrier, scanned the same way; a thinking-only record carries
//     no text block and stays bulk;
//   - a claude result record is the per-turn outcome: an error turn is
//     logged to stderr (the same failure observation as a pi
//     success=false response), never an ack — its result field duplicates
//     the turn's LAST assistant text (measured), so scanning it would
//     queue one ack twice;
//   - every other record (pi session events, message_update deltas,
//     claude system/hook noise, extension-UI records, responses for
//     foreign ids) is BULK and dropped with the existing bookkeeping;
//   - a line that is not a protocol record at all (no JSON object with a
//     type field) falls back to the legacy RAW-line classifier. Both
//     real harnesses emit records only (pi by protocol, claude
//     measured); the fallback stays as defense for unknown noise.
func (p *Proc) classifyStdoutLine(line string) {
	var rec stdoutRecord
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Type == "" {
		// Not a protocol record: legacy raw-line classification.
		// Bulk or ack, never a response observation. It still counts as
		// stream progress (the harness produced output) — the heartbeat
		// measures SILENCE, and a noisy non-record stream is not silent.
		if p.stream != nil {
			p.stream.noteRecord("")
		}
		p.classifyCandidateLine(line)
		return
	}
	// polyforge#48: the heartbeat and the infra-incident classification
	// ride this SAME pass — the reader already sees every record, so the
	// stream criterion costs two atomic stores and no extra plumbing.
	// Both run before the ack dispatch so a control-path close (overlong
	// line) cannot swallow the liveness observation that explains it.
	if p.stream != nil {
		p.stream.noteRecord(rec.Type)
		p.classifyIncident(rec)
	}
	switch rec.Type {
	case "response": // pi command outcome
		p.observeResponse(rec)
		atomic.AddInt64(&p.bulkDropped, 1)
	case "message_end": // pi authoritative assistant message
		text, ok := assistantText(rec.Message)
		if !ok {
			atomic.AddInt64(&p.bulkDropped, 1)
			return
		}
		// The extracted text is scanned line-by-line with the SAME bounds
		// as raw lines: an ack may never be silently dropped, and an
		// overlong extracted line closes the control path typed
		// (ErrControlLineTooLong) exactly like an overlong raw line.
		p.scanAssistantText(text)
	case "assistant": // claude assistant message (the ack carrier)
		p.markTurnServed() // ANY assistant record proves a served turn (even thinking-only)
		text, ok := assistantText(rec.Message)
		if !ok {
			atomic.AddInt64(&p.bulkDropped, 1)
			return
		}
		p.scanAssistantText(text)
	case "result": // claude per-turn outcome
		p.observeClaudeResult(rec)
		if !claudeResultFailed(rec) {
			// A COMPLETED turn: the healthy-signal evidence for the
			// zero-turn startup watch. The refusal path's own
			// error_during_execution result record is excluded by the
			// same claudeResultFailed test.
			p.markTurnServed()
		}
		atomic.AddInt64(&p.bulkDropped, 1)
	default:
		// Session events, message_update deltas, claude system/hook noise,
		// extension-UI records, responses for command ids we never sent:
		// bulk.
		atomic.AddInt64(&p.bulkDropped, 1)
	}
}

// scanAssistantText scans ONE extracted assistant text (pi message_end or
// claude assistant record) line-by-line through the closed control
// vocabulary with the same bounds as raw lines: an ack may never be
// silently dropped, and an overlong extracted line closes the control
// path typed (ErrControlLineTooLong) exactly like an overlong raw line.
func (p *Proc) scanAssistantText(text string) {
	for _, l := range strings.Split(text, "\n") {
		p.classifyCandidateLine(strings.TrimSuffix(l, "\r"))
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

// observeClaudeResult handles one claude per-turn result record. An
// error turn — is_error set or a non-success subtype, e.g. the measured
// "No conversation found with session ID: <uuid>" failure of a --resume
// derivation whose conversation never completed a turn, or an API
// failure — is logged to stderr, mirroring the pi response-failure
// observation: never an ack, never an unhealthy marker. The result field
// duplicates the turn's LAST assistant text (measured: identical to the
// final assistant record's text), so it is NOT scanned for acks — the
// assistant record already carried it, and scanning both would queue one
// ack twice.
func (p *Proc) observeClaudeResult(rec stdoutRecord) {
	if !claudeResultFailed(rec) {
		return
	}
	fmt.Fprintf(os.Stderr, "mainagent: claude turn failed (subtype=%s): see the harness stderr/session for detail\n", orDefault(rec.Subtype, "unknown"))
}

// claudeResultFailed reports whether one claude result record is an
// ERROR outcome — is_error set, or a subtype other than success. The
// measured zero-turn resume refusal and an API/network failure both
// carry this shape; the zero-turn startup watch must not mistake the
// refusal's own error record for a served turn.
func claudeResultFailed(rec stdoutRecord) bool {
	if rec.IsError != nil && *rec.IsError {
		return true
	}
	return rec.Subtype != "" && rec.Subtype != "success"
}

// markTurnServed records (once) that the harness stream proved a served
// turn; see the turnServed field comment. Tolerates a bare Proc (no
// watch armed) as a no-op.
func (p *Proc) markTurnServed() {
	if p.turnServed == nil {
		return
	}
	p.turnOnce.Do(func() { close(p.turnServed) })
}

// servedTurn reports whether a served turn was proven on the stream —
// the zero-turn startup watch's "the session already produced output"
// input.
func (p *Proc) servedTurn() bool {
	select {
	case <-p.turnServed:
		return true
	default:
		return false
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
	// field comment) so the stdout reader can keep draining. The initial
	// prompt arrives as *piCommand (InitialMessage returns any) while
	// control prompts arrive as piCommand values — both register.
	switch c := payload.(type) {
	case piCommand:
		if c.ID != "" {
			p.pendMu.Lock()
			p.pendMuRegister(c.ID)
			p.pendMu.Unlock()
		}
	case *piCommand:
		if c != nil && c.ID != "" {
			p.pendMu.Lock()
			p.pendMuRegister(c.ID)
			p.pendMu.Unlock()
		}
	}
	return nil
}

// pendMuRegister adds id to the pending set (caller holds pendMu).
func (p *Proc) pendMuRegister(id string) {
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	p.pending[id] = true
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
	return p.awaitAckBound(ctx, method, defaultAckBound)
}

// defaultAckBound is the control-ack window every method EXCEPT the two
// turn-scale ones uses. quiesce / resume / stop_notice are answered from
// the agent's boundary bookkeeping and were measured well inside it.
const defaultAckBound = 30 * time.Second

// answerAckBound is the pf/answer ack window (polyforge#48 item 5,
// folded from #49). The 30-second default was calibrated against the
// FAKE harness, which acks instantly. A real agent consuming an answer
// runs a whole model turn — the pilot of 2026-09-29 01:10 measured the
// ack arriving 45-90s after the daemon had already reported DAEMON-500,
// with the answer landing correctly afterwards. The window is therefore
// raised to turn scale, and a timeout is no longer error-grade:
// DeliverAnswer reports daemon.ErrAnswerAckPending, the two-phase
// "delivered, consumption pending" outcome.
//
// Operator override: POLYFORGE_ANSWER_ACK_WINDOW (a Go duration).
var answerAckBound = envDuration("POLYFORGE_ANSWER_ACK_WINDOW", 10*time.Minute)

// envDuration reads a Go duration from the environment, falling back to
// def on an absent or unparsable value (loudly for the latter: a typo in
// an operator override must not silently restore a default).
func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "mainagent: %s=%q is not a positive Go duration; using %s\n", name, raw, def)
		return def
	}
	return d
}

// awaitAckBound is awaitAck with an explicit bound (the ack windows are
// per-method since polyforge#48: see answerAckBound / probeAckBound).
func (p *Proc) awaitAckBound(ctx context.Context, method string, bound time.Duration) error {
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
//
// TWO-PHASE OUTCOME (polyforge#48 item 5, folded from #49). The wait is
// now turn-scale (answerAckBound) and its EXPIRY is separated from its
// FAILURE, because the pilot proved they are different facts:
//
//   - the stdin write failed, or the control path is closed/dead: the
//     relay never reached the agent — a real delivery failure, returned
//     as the typed error it is;
//   - the write succeeded but no ack arrived inside the window: delivery
//     is CONFIRMED and consumption is merely unobserved. The measured
//     case landed the answer 45-90s later. Reporting that as an error
//     told the operator the answer was lost when it was not.
//     daemon.ErrAnswerAckPending carries that distinction all the way to
//     the CLI, and keeps the ledger entry redeliverable so a crash
//     continuation re-delivers the same answer_id idempotently.
func (p *Proc) DeliverAnswer(relay daemon.AnswerRelay) error {
	b, err := json.Marshal(relay)
	if err != nil {
		return err
	}
	params := map[string]any{}
	json.Unmarshal(b, &params)
	if err := p.SendControl(MethodAnswer, params); err != nil {
		return err // the relay never reached the wire: a true delivery failure
	}
	err = p.awaitAckBound(context.Background(), MethodAnswer, answerAckBound)
	if err == nil {
		return nil
	}
	// Distinguish the ack WINDOW expiring (consumption pending) from the
	// control path dying under the wait (the agent will never consume it).
	if p.Unhealthy() || p.Done() {
		return err
	}
	if strings.HasPrefix(err.Error(), "mainagent: no ack for "+MethodAnswer) {
		fmt.Fprintf(os.Stderr, "mainagent: answer %s delivered, no consumption ack within %s (agent turn still running; not an error)\n", relay.AnswerID, answerAckBound)
		return daemon.ErrAnswerAckPending
	}
	return err
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

// Done is the NON-BLOCKING exit probe the daemon's runtime death watchdog
// polls once per dispatcher tick (watchdog.go, 04 §6.2 runtime note): the
// reaper closes `exited` the moment cmd.Wait() returns, so a true here is
// the authoritative exit signal with zero waiting. Additive to the
// AgentHandle face by design: the blocking Wait above and every existing
// agentexit.go consumer keep their semantics untouched.
func (p *Proc) Done() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
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
// for tests/fakes). apiKey is the daemon's currently-configured machine api
// key, captured ONCE here (04 §8: it must never reach a spawned agent, not
// even via a renamed env var) — both adapters share the one envSanitizer
// built from it.
func NewSupervisor(piBin, claudeBin, apiKey string) *Supervisor {
	env := newEnvSanitizer(apiKey)
	return &Supervisor{AdapterFor: func(name string) Adapter {
		switch name {
		case capsule.AdapterClaude:
			return &ClaudeAdapter{Bin: claudeBin, env: env}
		default:
			return &PiAdapter{Bin: piBin, env: env}
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
	resumed := false
	if idn, err := capc.ReadSessionIdentity(p.Adapter); err == nil && idn.SessionID != "" {
		if idn.SessionID != p.SessionID {
			return nil, fmt.Errorf("mainagent: session id conflict: local %s vs requested %s (new generation must not reuse an old session)", idn.SessionID, p.SessionID)
		}
		resumed = true
	} else {
		if err := capc.WriteSessionIdentity(p.Adapter, p.SessionID); err != nil {
			return nil, err
		}
	}
	// Crash-continuation mark (03 §4.4's only reuse case): an adapter whose
	// harness re-attaches to an existing conversation (claude --resume)
	// learns it here. The supervisor face stays shape-agnostic — pi
	// ignores the mark (its session dir carries the state).
	if resumed {
		if ca, ok := adapter.(continuationAdapter); ok {
			ca.MarkContinuation()
		}
	}

	// Derivation loop: at most TWO derivations. The first is the marked
	// one (a continuation re-attaches where the harness can). A
	// continuation derivation of a zeroTurnFallback adapter is watched
	// through its startup window (see derive); only the adapter's
	// classification of the MEASURED zero-turn resume refusal (claude:
	// the harness died at launch refusing --resume of a conversation
	// that never completed a turn) earns the second — a FRESH derivation
	// under the SAME harness session id (04 §6.1a semantics preserved:
	// same session identity, same generation, same machine).
	//
	// The session-identity read-back above deliberately does NOT re-run
	// for the fallback: the identity on disk is already OURS (that
	// read-back is what marked this a continuation), and re-reading it
	// would re-mark the adapter and loop the refusal. The fallback is
	// NOT a generation change and must not touch session.json at all —
	// one identity, one id, exactly as before.
	watch := resumed
	if _, ok := adapter.(zeroTurnFallback); !ok {
		watch = false // the harness has no refusal shape: nothing to watch
	}
	for {
		proc, refused, err := s.derive(adapter, p, sessionDir, watch)
		if err != nil {
			return nil, err
		}
		if !refused {
			return proc, nil
		}
		// refused is only ever set with the watch armed, which implies
		// the face; arming clears the watch, so the loop runs at most
		// twice (the fallback derivation is fresh and cannot be refused).
		adapter.(zeroTurnFallback).ArmFreshFallback()
		watch = false
	}
}

// zeroTurnWatchBound bounds the zero-turn startup watch of a
// continuation derivation (claude). The measured refusal is the
// harness's FIRST act after its startup records (the conversation-store
// check precedes any turn), so a healthy resumed derivation either
// serves its first turn or outlives the bound. Continuations are rare
// (crash re-derivation only), so this bound costs nothing on the fresh
// path; a refusal slower than the bound degrades to the pre-fallback
// behavior (a dead handle the daemon's liveness observation catches).
const zeroTurnWatchBound = 5 * time.Second

// derive derives ONE harness process for p — BuildCmd → start → identity
// journal → initial-instruction injection — and returns the supervised
// handle. watch arms the zero-turn startup watch; only a CONTINUATION
// derivation of a zeroTurnFallback adapter gets it. refused=true means
// the watched derivation died at startup with the adapter's measured
// zero-turn resume refusal: the caller arms exactly one fresh fallback
// derivation under the same session id. Any OTHER watched startup death
// is a typed spawn failure (a network error must never silently become
// a fresh session); an unwatched derivation behaves exactly as before
// (early deaths surface through the injection write or the daemon's
// liveness observation).
func (s *Supervisor) derive(adapter Adapter, p SpawnParams, sessionDir string, watch bool) (*Proc, bool, error) {
	cmd, err := adapter.BuildCmd(p, sessionDir)
	if err != nil {
		return nil, false, err
	}
	// Process group: the agent is its own group leader so Terminate can
	// collect step agents / command children with one signal (04 §7.1).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, false, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	// The zero-turn startup watch needs the harness's stderr tail (the
	// measured refusal text) — captured next to the daemon's log face,
	// bounded so a chatty harness cannot balloon it. Unwatched
	// derivations keep the direct stderr pass-through unchanged.
	var tail *stderrTail
	if watch {
		tail = &stderrTail{max: maxStderrTail}
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	} else {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("mainagent: harness start failed (DAEMON-503): %w", err)
	}
	proc := &Proc{
		cmd: cmd, adapter: adapter, sessionID: p.SessionID,
		startedAt: time.Now().UTC().Format(time.RFC3339Nano),
		stdin:     stdin, ctrl: newLineQueue(), exited: make(chan struct{}),
		turnServed: make(chan struct{}),
		stream:     &streamState{},
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
	// above is already running, so no corpse is left behind). The
	// zero-turn fallback derivation journals its OWN pid through the same
	// callback — the journal row must name the process that is LIVE when
	// Spawn returns, never the refused one it replaced.
	if p.OnProcessStarted != nil {
		if jerr := p.OnProcessStarted(cmd.Process.Pid, proc.startedAt); jerr != nil {
			_ = proc.Terminate(5 * time.Second)
			return nil, false, fmt.Errorf("mainagent: process-identity journaling failed (process torn down): %w", jerr)
		}
	}
	// Inject the initial stdin write (04 §6.1a): pi gets the pf-execute
	// instruction as the session's FIRST prompt; claude carries the
	// instruction in its system prompt and writes the fixed kickoff user
	// message. Any failure here must not leak a running half-initialized
	// agent: the process group is torn down BEFORE the error is returned.
	m, ierr := adapter.InitialMessage(p)
	if ierr != nil {
		_ = proc.Terminate(5 * time.Second)
		return nil, false, fmt.Errorf("mainagent: initial instruction build failed (DAEMON-503): %w", ierr)
	}
	if m != nil {
		if serr := proc.writeStdin(m); serr != nil {
			if !watch {
				_ = proc.Terminate(5 * time.Second)
				return nil, false, fmt.Errorf("mainagent: initial instruction injection failed (DAEMON-503): %w", serr)
			}
			// Under the watch, a failed kickoff write is most likely the
			// startup death itself (the pipe closed as the harness died —
			// the measured refusal races the injection). Let the watch
			// classify instead of failing blind: a refusal becomes the
			// fresh fallback, anything else the typed spawn failure.
		}
	}
	if !watch {
		return proc, false, nil
	}
	// Zero-turn startup watch (continuation derivations only, 04 §6.1a's
	// zero-turn crash window): race the harness's startup against
	// (i) its death — the measured refusal arrives before any served
	// turn; (ii) the first served turn (the healthy signal: a --resume
	// the harness accepted is already past its conversation-store
	// check); and (iii) the bound, after which the derivation is taken
	// as healthy exactly as before.
	select {
	case <-proc.exited:
		// Startup death: classify. The refusal earns exactly one fresh
		// fallback; anything else stays a typed spawn failure.
		ra := adapter.(zeroTurnFallback) // the watch implies the face
		if ra.StartupRefusal(proc.exitErr, tail.String(), proc.servedTurn()) {
			// Collect the refused derivation's residue BEFORE the fallback
			// derivation starts — the same 04 §7.1 step 4 discipline:
			// proven group-empty before re-derivation.
			_ = proc.Terminate(5 * time.Second)
			return nil, true, nil
		}
		_ = proc.Terminate(5 * time.Second)
		st := tail.String()
		if len(st) > 1024 {
			st = st[len(st)-1024:] // keep the typed failure readable
		}
		return nil, false, fmt.Errorf("mainagent: harness continuation died at startup (DAEMON-503): %v; stderr tail: %q", proc.exitErr, st)
	case <-proc.turnServed:
		// The resumed harness served a turn: the conversation existed and
		// was accepted — healthy; the watch ends.
		return proc, false, nil
	case <-time.After(zeroTurnWatchBound):
		// Still alive and quiet past the bound: healthy as far as this
		// watch can prove; behave exactly as the unwatched path.
		return proc, false, nil
	}
}
