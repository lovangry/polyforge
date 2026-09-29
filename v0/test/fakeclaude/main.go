// Command fakeclaude is the dogfood stand-in for the real Claude Code
// CLI (`claude`, 2.1.280), whose stdio contract was measured live for
// polyforge#33. The supervisor's ClaudeAdapter spawns it as the
// persistent WI Agent process; it speaks the REAL streaming-input/
// stream-json wire protocol and acks the pf/* control methods the way
// the real agent's pf-execute loop would (ack line embedded in the
// assistant message text — the SKILL.md §12 transport).
//
// REAL-claude behaviors REPRODUCED here (verified live against
// claude 2.1.280, `claude -p --input-format stream-json
// --output-format stream-json --verbose`):
//
//   - argv: -p --input-format stream-json --output-format stream-json
//     --verbose are required, plus exactly one of --session-id <uuid> /
//     --resume <uuid>; --append-system-prompt <text> is accepted;
//     --session-id rejects any non-UUID value ("Invalid session ID.
//     Must be a valid UUID.");
//   - stdin: one user-message record per line,
//     {"type":"user","message":{"role":"user","content":"<text>"}};
//     stdin EOF exits cleanly (exit 0) — the -p process is persistent
//     across turns in streaming-input mode;
//   - stdout: one JSON record per line. Startup: system hook_started/
//     hook_response pairs plus a {"type":"system","subtype":"init"}
//     record (per turn, carrying session_id); per assistant message:
//     {"type":"assistant","message":{"role":"assistant",
//     "content":[{"type":"text"|"thinking",...}]}} — a message may be
//     thinking-only (no text block); per turn exactly one
//     {"type":"result","subtype":"success","result":<last assistant
//     text>,"is_error":false,"num_turns":1,"result_index":<n>} record;
//   - session persistence: the conversation becomes resumable only
//     after its FIRST turn completes; --resume of an unknown or
//     zero-turn session prints "No conversation found with session ID:
//     <uuid>", emits an error_during_execution result record and exits
//     1 (all measured).
//
// REAL-claude behaviors NOT reproduced (known deviations):
//
//   - no real model: a "turn" is a canned synchronous record burst
//     (no stream_event deltas — the supervisor does not pass
//     --include-partial-messages — no tool_use, no compaction, no
//     per-message uuid stability, no usage/cost accounting beyond the
//     fields the supervisor reads);
//   - conversation "storage" is one file per session UUID under
//     FAKECLAUDE_STATE_DIR (the real CLI keeps sessions under
//     CLAUDE_CONFIG_DIR keyed by cwd + id). The transcript-shaped
//     <uuid>.jsonl written beside it models WHAT the daemon's
//     seal-time terminal-state collection copies out of the real
//     CLI's default session location (03 §4.4 / 04 §6.2 A step 2:
//     read-only copy into harness/claude/session-data/); the flat
//     one-file-per-uuid layout is the fake's own simplification — the
//     daemon's search tolerates any subdirectory layout under the
//     configured root (internal/daemon/sessioncollect.go);
//   - the thinking-block key shape ({"type":"thinking",...}) was not
//     captured verbatim from the real output; only its OBSERVABLE
//     effect is reproduced (an assistant record with no text block);
//   - argv errors exit 2 here (the real CLI's exit code for flag
//     errors was not measured; the "No conversation found" path exits
//     1, which WAS measured);
//   - no capsule-side execution.json work: that is fakepi's dogfood
//     domain; fakeclaude is transport-only.
//
// Test-only helper (the dogfood scenarios build and exec it);
// `go build ./...` ships it as a main package but nothing in production
// imports or references it.
//
// Knobs (environment, set by the spawning scenario):
//
//	FAKECLAUDE_STATE_DIR  – conversation store (default:
//	                      $TMPDIR/fakeclaude-conversations)
//	FAKECLAUDE_SPAWN_LOG  – when set, append one
//	                      "<pid> <session-uuid> <fresh|resume> <ts>"
//	                      line per process start (spawn/continuation pins)
//	FAKECLAUDE_FLOOD=<n>  – emit <n> bulk records before each ack
//	                      (chatty-child drills)
//	FAKECLAUDE_EXIT       – when non-empty, exit(0) immediately after
//	                      the startup records (a harness that dies on
//	                      startup; crash drills)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Control-channel method names — byte-identical with the two-sided
// contract (internal/mainagent Method* constants / SKILL.md §12). Under
// the claude transport these arrive as the `content` of user-message
// records, and the ack line is embedded in a MULTI-LINE assistant
// message (the §12 transport note).
const (
	methodQuiesce    = "pf/quiesce"
	methodResume     = "pf/resume"
	methodStopNotice = "pf/stop_notice"
	methodAnswer     = "pf/answer"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// session captures the resolved argv identity.
type session struct {
	uuid      string
	resume    bool // --resume (continuation) vs --session-id (fresh)
	sysprompt string
	// noSlashCommands records --disable-slash-commands (the WI-agent
	// isolation profile's one cc flag): accepted, inert for the fake.
	noSlashCommands bool
}

func main() {
	s, code := parseArgs(os.Args[1:])
	if code != 0 {
		os.Exit(code)
	}
	logSpawn(s)
	emitStartup(s)
	if os.Getenv("FAKECLAUDE_EXIT") != "" {
		os.Exit(0)
	}
	if s.resume && !conversationExists(s.uuid) {
		// Measured: a zero-turn or unknown session is not resumable.
		fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", s.uuid)
		emitRecord(map[string]any{
			"type": "result", "subtype": "error_during_execution",
			"session_id": s.uuid, "is_error": true, "num_turns": 0,
			"result": "", "result_index": 0,
		})
		os.Exit(1)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	turn := 0
	for sc.Scan() {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "user" {
			continue // not a user-message record: ignore
		}
		turn++
		emitInit(s, turn)
		text := userText(msg.Message.Content)
		var ctl struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(text), &ctl) != nil {
			ctl.Method = "" // free text, not a control line
		}
		switch ctl.Method {
		case methodQuiesce, methodResume, methodStopNotice:
			ackTurn(s, turn, ctl.Method)
		case methodAnswer:
			ackTurn(s, turn, methodAnswer)
		default:
			plainTurn(s, turn, text)
		}
	}
	// stdin EOF: measured — the real -p streaming-input process exits 0.
	os.Exit(0)
}

// userText extracts the user message text: a bare string (the shape the
// supervisor writes, measured working) or a typed-block array
// ([{"type":"text","text":..}], the SDK shape).
func userText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// parseArgs enforces the REAL argv (measured): the streaming trio, the
// required --verbose, one session selector, and the system-prompt
// append. Violations fail loudly — a supervisor argv drift must fail
// the dogfood scenario, not pass silently.
func parseArgs(args []string) (*session, int) {
	s := &session{}
	sawP, sawIn, sawOut, sawVerbose := false, false, false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			sawP = true
		case "--input-format":
			if i+1 >= len(args) || args[i+1] != "stream-json" {
				fmt.Fprintf(os.Stderr, "Error: --input-format must be stream-json\n")
				return nil, 2
			}
			i++
			sawIn = true
		case "--output-format":
			if i+1 >= len(args) || args[i+1] != "stream-json" {
				fmt.Fprintf(os.Stderr, "Error: --output-format stream-json requires stream-json\n")
				return nil, 2
			}
			i++
			sawOut = true
		case "--verbose":
			sawVerbose = true
		case "--disable-slash-commands":
			// The birth isolation profile's one cc flag (polyforge#51,
			// claudeIsolationFlags): a REAL CLI 2.1.283 option ("Disable
			// all skills") the adapter now always passes. Accepted and
			// inert here — the fake has no skills to disable, and its job
			// is to pin that the argv the adapter builds is one the real
			// CLI accepts.
			s.noSlashCommands = true
		case "--session-id":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --session-id requires a value\n")
				return nil, 2
			}
			i++
			if !uuidRe.MatchString(args[i]) {
				// Measured error text (uuid5-less ids are rejected).
				fmt.Fprintf(os.Stderr, "Error: Invalid session ID. Must be a valid UUID.\n")
				return nil, 2
			}
			s.uuid = args[i]
			s.resume = false
		case "--resume":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --resume requires a value\n")
				return nil, 2
			}
			i++
			if !uuidRe.MatchString(args[i]) {
				fmt.Fprintf(os.Stderr, "Error: --resume requires a valid session ID or session title when used with --print.\n")
				return nil, 2
			}
			s.uuid = args[i]
			s.resume = true
		case "--append-system-prompt":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --append-system-prompt requires a value\n")
				return nil, 2
			}
			i++
			s.sysprompt = args[i]
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown option %q (fakeclaude pins the real claude argv)\n", args[i])
			return nil, 2
		}
	}
	if !sawP || !sawIn || !sawOut {
		fmt.Fprintf(os.Stderr, "Error: fakeclaude requires -p --input-format stream-json --output-format stream-json\n")
		return nil, 2
	}
	if !sawVerbose {
		// Measured: "When using --print, --output-format=stream-json
		// requires --verbose".
		fmt.Fprintf(os.Stderr, "Error: When using --print, --output-format=stream-json requires --verbose\n")
		return nil, 2
	}
	if s.uuid == "" {
		fmt.Fprintf(os.Stderr, "Error: one of --session-id/--resume is required\n")
		return nil, 2
	}
	return s, 0
}

// ---------------------------------------------------------------------------
// Record emission (measured shapes; see the header comment).

func emitRecord(rec map[string]any) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	fmt.Println(string(raw))
}

// emitStartup reproduces the measured startup: system hook records
// (before any turn) carrying the session id.
func emitStartup(s *session) {
	hookName := "SessionStart:startup"
	if s.resume {
		hookName = "SessionStart:resume" // measured on a --resume launch
	}
	emitRecord(map[string]any{"type": "system", "subtype": "hook_started",
		"hook_id": "fake-h1", "hook_name": hookName, "session_id": s.uuid})
	emitRecord(map[string]any{"type": "system", "subtype": "hook_response",
		"hook_id": "fake-h1", "hook_name": hookName, "session_id": s.uuid})
}

// emitInit reproduces the per-turn init record (measured: an init record
// opens every turn, fresh or resumed, carrying the session id).
func emitInit(s *session, turn int) {
	emitRecord(map[string]any{"type": "system", "subtype": "init",
		"cwd": wd(), "session_id": s.uuid, "model": "fakeclaude",
		"tools": []any{}, "turn": turn})
}

// emitAssistant reproduces one complete assistant record (measured):
// the message object carries role + typed content blocks.
func emitAssistant(s *session, blocks []any) {
	msg := map[string]any{"role": "assistant", "content": blocks}
	emitRecord(map[string]any{"type": "assistant", "message": msg,
		"session_id": s.uuid})
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// emitResult reproduces the per-turn result record (measured): exactly
// one per turn, result = the turn's LAST assistant text, result_index
// cumulative across the process's turns.
func emitResult(s *session, turn int, lastAssistantText string) {
	emitRecord(map[string]any{"type": "result", "subtype": "success",
		"result": lastAssistantText, "session_id": s.uuid,
		"is_error": false, "num_turns": 1, "result_index": turn - 1,
		"duration_ms": 1, "total_cost_usd": 0})
}

// flood emits n bulk records around the ack (chatty-child drills):
// hook noise, a user tool-result echo and a thinking-only assistant
// record — the measured non-ack shapes.
func flood(s *session, n int) {
	for i := 0; i < n; i++ {
		emitRecord(map[string]any{"type": "system", "subtype": "hook_started",
			"hook_id": "fake-flood", "session_id": s.uuid})
		emitRecord(map[string]any{"type": "user", "message": map[string]any{
			"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "fake", "content": "noise"}}}})
		emitAssistant(s, []any{map[string]any{"type": "thinking", "thinking": "noise"}})
	}
}

// ackTurn reproduces the control-ack transport (SKILL.md §12): the ack
// JSON line embedded in a MULTI-LINE assistant message — never a raw
// stdout line. A thinking-only record precedes it (measured shape), the
// result record repeats the assistant text in its result field (also
// measured — and the supervisor must NOT read acks from there), and the
// completed turn makes the conversation resumable.
func ackTurn(s *session, turn int, method string) {
	if n, err := strconv.Atoi(os.Getenv("FAKECLAUDE_FLOOD")); err == nil && n > 0 {
		flood(s, n)
	}
	emitAssistant(s, []any{map[string]any{"type": "thinking", "thinking": "checking for a safe boundary"}})
	text := "control method received; acking at a safe boundary\n" +
		`{"method":"` + method + `","result":{"ack":"` + method + `"}}` + "\nstanding by"
	emitAssistant(s, []any{textBlock(text)})
	emitResult(s, turn, text)
	persistConversation(s, turn)
}

// plainTurn answers a non-control user message (e.g. the kickoff) and
// makes the conversation resumable (measured: first COMPLETED turn).
func plainTurn(s *session, turn int, text string) {
	reply := "fakeclaude: message received; standing by for control methods."
	emitAssistant(s, []any{map[string]any{"type": "thinking", "thinking": "no control line here"}})
	emitAssistant(s, []any{textBlock(reply)})
	emitResult(s, turn, reply)
	persistConversation(s, turn)
}

// ---------------------------------------------------------------------------
// Conversation persistence: a conversation exists (is resumable) only
// after a turn completed in it (measured zero-turn rule). Beside the
// resumability marker, every completed turn also appends to a
// transcript-shaped <uuid>.jsonl — the session record the daemon's
// terminal-state collection later copies out of this default session
// location into the capsule (03 §4.4).

func stateDir() string {
	if d := os.Getenv("FAKECLAUDE_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "fakeclaude-conversations")
}

func conversationPath(uuid string) string {
	return filepath.Join(stateDir(), uuid+".json")
}

// transcriptPath is the <uuid>.jsonl session record at this harness's
// default session location — the file the daemon's seal-time collection
// looks for (named by the conversation UUID, exactly like the real CLI's
// session JSONL).
func transcriptPath(uuid string) string {
	return filepath.Join(stateDir(), uuid+".jsonl")
}

func conversationExists(uuid string) bool {
	_, err := os.Stat(conversationPath(uuid))
	return err == nil
}

func persistConversation(s *session, turn int) {
	_ = os.MkdirAll(stateDir(), 0o755)
	_ = os.WriteFile(conversationPath(s.uuid),
		[]byte(`{"status":"turn_completed"}`), 0o644)
	// Append one JSON record per completed turn: the transcript shape
	// (JSONL, one record per line) the real CLI persists and the daemon
	// collects read-only at end-generation.
	rec, err := json.Marshal(map[string]any{
		"type": "user", "session_id": s.uuid, "turn": turn,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(transcriptPath(s.uuid), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", rec)
}

// ---------------------------------------------------------------------------
// Knobs.

func logSpawn(s *session) {
	p := os.Getenv("FAKECLAUDE_SPAWN_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	mode := "fresh"
	if s.resume {
		mode = "resume"
	}
	fmt.Fprintf(f, "%d %s %s %s\n", os.Getpid(), s.uuid, mode, nowStamp())
}

func wd() string {
	d, _ := os.Getwd()
	return d
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
