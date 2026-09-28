package mainagent

// claudereal_test.go: regressions pinning the REAL Claude Code CLI
// transport (polyforge#33 ground-truth verification). The old claude
// adapter spoke an invented protocol: a --session-id value the CLI
// rejects ("Invalid session ID. Must be a valid UUID."), a
// stream-json output mode the CLI refuses without --verbose in print
// mode, an invented CLAUDE_SESSION_DIR env var, control lines written
// as bare stdin text (in streaming-input mode those would be parsed as
// user-message records and fail), and a crash-continuation --resume of
// a non-UUID id. Measured live against claude 2.1.280 (2026-09-28):
//
//   - claude -p --input-format stream-json --output-format stream-json
//     --verbose is a PERSISTENT multi-turn session (one process, one
//     user-message record per stdin line, records on stdout, exit 0 on
//     stdin EOF);
//   - --session-id/--resume demand a UUID; a name-derived uuid5 pins
//     the conversation id and survives a real turn into a --resume;
//   - a zero-turn conversation is NOT resumable ("No conversation found",
//     exit 1);
//   - per-turn output: system/hook records, per-message assistant
//     records (role + typed content blocks; thinking-only messages
//     carry no text), one result record per turn whose result field
//     duplicates the last assistant text.
//
// These tests pin: (a) the argv and UUID mapping; (b) the initial
// kickoff user record; (c) control methods riding user-message records
// with the control line VERBATIM as content; (d) ack extraction from
// claude assistant records, never duplicated from the result record;
// (e) bulk classification of claude noise (hook/system records, user
// tool echoes, thinking-only messages); (f) error result records
// observed but never acked; (g) the crash-continuation --resume wiring
// (session-identity read-back → MarkContinuation); (h) an end-to-end
// drive of test/fakeclaude, the protocol-faithful test double,
// including the measured zero-turn resume failure.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// waitForFileLines waits until path carries at least n lines (a shell
// harness's first write races its caller's Terminate — the kill can land
// before the interpreter runs its first statement).
func waitForFileLines(t *testing.T, path string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && len(strings.Split(strings.TrimSpace(string(raw)), "\n")) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never carried %d lines", path, n)
}

// claudeAckAssistantRecord builds one REAL claude assistant record
// (measured shape) whose multi-line text embeds the ack line for
// method — the SKILL.md §12 transport under the claude wire.
func claudeAckAssistantRecord(method string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"at a safe boundary now\n` +
		`{\"method\":\"` + method + `\",\"result\":{\"ack\":\"` + method + `\"}}` +
		`\ncontinuing the step"}]}}`
}

// ---------------------------------------------------------------------------
// (a) argv + session-UUID mapping.

func TestClaudeAdapterArgvMeasured(t *testing.T) {
	a := &ClaudeAdapter{Bin: "/custom/claude"}
	cmd, err := a.BuildCmd(daemon.SpawnParams{
		CapsuleDir: "/caps", SessionID: "sess_wi_1_2", WorkItemID: "wi_1", InitialPrompt: "SYS",
	}, "/caps/harness/claude/session-data")
	if err != nil {
		t.Fatal(err)
	}
	sid := claudeSessionUUID("sess_wi_1_2")
	want := []string{"/custom/claude",
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // REQUIRED: the real CLI refuses stream-json without it in print mode
		"--session-id", sid,
		"--append-system-prompt", "SYS",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("claude argv:\n got %v\nwant %v", cmd.Args, want)
	}
	if cmd.Dir != "/caps" {
		t.Fatalf("claude cwd must be the capsule dir, got %q", cmd.Dir)
	}
	envHas := func(k string) bool {
		for _, e := range cmd.Env {
			if strings.HasPrefix(e, k+"=") {
				return true
			}
		}
		return false
	}
	if !envHas("PF_SESSION_ID") || !envHas("PF_WORK_ITEM_ID") {
		t.Fatalf("claude env must carry PF_SESSION_ID/PF_WORK_ITEM_ID: %v", cmd.Env)
	}
	// The invented CLAUDE_SESSION_DIR must be GONE (the real CLI ignores
	// it; there is no capsule-local session-dir env var on this CLI).
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "CLAUDE_SESSION_DIR=") {
			t.Fatal("CLAUDE_SESSION_DIR is an invented variable the real CLI ignores; it must not be set")
		}
	}
	// Crash continuation: --resume of the SAME derived conversation UUID,
	// and no --session-id alongside.
	a.MarkContinuation()
	cmd2, err := a.BuildCmd(daemon.SpawnParams{CapsuleDir: "/caps", SessionID: "sess_wi_1_2", InitialPrompt: "SYS"}, "d")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd2.Args, " ")
	if !contains(joined, "--resume "+sid) {
		t.Fatalf("continuation argv must resume the derived conversation UUID %s: %v", sid, cmd2.Args)
	}
	if contains(joined, "--session-id") {
		t.Fatalf("continuation argv must not pin a fresh --session-id: %v", cmd2.Args)
	}
}

func TestClaudeSessionUUIDShapeAndDeterminism(t *testing.T) {
	u1 := claudeSessionUUID("sess_wi_9_1")
	u2 := claudeSessionUUID("sess_wi_9_1")
	u3 := claudeSessionUUID("sess_wi_9_2")
	if u1 != u2 {
		t.Fatal("the UUID map must be deterministic: the same session id must always map to the same conversation UUID")
	}
	if u1 == u3 {
		t.Fatal("different session ids must map to different conversation UUIDs")
	}
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, id := range []string{"sess_wi_9_1", "sess_x", ""} {
		if !re.MatchString(claudeSessionUUID(id)) {
			t.Fatalf("claudeSessionUUID(%q) must be a valid RFC 4122 v5 UUID (the CLI validates the format)", id)
		}
	}
}

// ---------------------------------------------------------------------------
// (b) the initial kickoff user record.

func TestClaudeInitialMessageIsUserRecord(t *testing.T) {
	a := &ClaudeAdapter{}
	m, err := a.InitialMessage(daemon.SpawnParams{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("the kickoff must be one JSON user-message record: %v (%s)", err, raw)
	}
	if probe.Type != "user" || probe.Message.Role != "user" || probe.Message.Content != claudeKickoff {
		t.Fatalf("the kickoff must be a user record carrying the fixed kickoff line: %s", raw)
	}
	// The kickoff is NOT a control line: nothing to ack yet, and the
	// agent-side closed classifier must not see control vocabulary.
	if isControlLine(string(raw)) || isControlLine(claudeKickoff) {
		t.Fatalf("the kickoff must not look like a control line: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// (c) control methods ride user-message records with the control JSON
// line VERBATIM as the content.

func TestClaudeControlRidesUserMessageVerbatim(t *testing.T) {
	recFile := filepath.Join(t.TempDir(), "stdin.log")
	script := "#!/bin/sh\n" + `
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$RECORD_FILE"
  case "$line" in
    *pf/quiesce*)     printf '%s\n' '` + claudeAckAssistantRecord("pf/quiesce") + `' ;;
    *pf/stop_notice*) printf '%s\n' '` + claudeAckAssistantRecord("pf/stop_notice") + `' ;;
    *pf/answer*)      printf '%s\n' '` + claudeAckAssistantRecord("pf/answer") + `' ;;
  esac
done
`
	bin := writeHarness(t, "clauderecharness", script)
	t.Setenv("RECORD_FILE", recFile)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_cr1")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)

	relay := daemon.AnswerRelay{
		QuestionID: "q_1", AnswerID: "a_1", AnswerText: "yes",
		AnsweredAt: "2026-09-28T00:00:00Z", AnsweredBy: "u_x",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if err := h.NotifyStop(ctx); err != nil {
		t.Fatalf("notify stop: %v", err)
	}
	if err := h.DeliverAnswer(relay); err != nil {
		t.Fatalf("deliver answer: %v", err)
	}

	raw, rerr := os.ReadFile(recFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 { // kickoff + 3 control user records
		t.Fatalf("the harness must record the kickoff and the three control records, got %d: %q", len(lines), string(raw))
	}
	// The first write: the fixed kickoff user record.
	var kickoff struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &kickoff); err != nil || kickoff.Type != "user" ||
		kickoff.Message.Role != "user" || kickoff.Message.Content != claudeKickoff {
		t.Fatalf("the first stdin write must be the kickoff user record: %q", lines[0])
	}
	// The control writes: user records whose content is the control line
	// VERBATIM, byte for byte — the agent-side pf-execute parses the user
	// message text exactly as the pi-side parses its prompt-command text.
	expectParams := map[string]any{}
	b, _ := json.Marshal(relay)
	_ = json.Unmarshal(b, &expectParams)
	want := []struct {
		method string
		params map[string]any
	}{
		{MethodQuiesce, nil},
		{MethodStopNotice, map[string]any{"reason": "graceful_shutdown"}},
		{MethodAnswer, expectParams},
	}
	for i, w := range want {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(lines[i+1]), &rec); err != nil {
			t.Fatalf("control write %d is not JSON: %q", i+1, lines[i+1])
		}
		if rec.Type != "user" || rec.Message.Role != "user" {
			t.Fatalf("control write %d must be a user-message record, got %q", i+1, lines[i+1])
		}
		line, _ := json.Marshal(controlEnvelope{JSONRPC: "2.0", Method: w.method, Params: w.params})
		if rec.Message.Content != string(line) {
			t.Fatalf("control write %d content must be the control line VERBATIM:\n got %q\nwant %q", i+1, rec.Message.Content, string(line))
		}
		// No pi-RPC keys leaked into the claude record.
		var top map[string]any
		_ = json.Unmarshal([]byte(lines[i+1]), &top)
		if _, bad := top["id"]; bad {
			t.Fatalf("control write %d must not carry a pi command id: %q", i+1, lines[i+1])
		}
		if tv, _ := top["type"].(string); tv == "prompt" {
			t.Fatalf("control write %d must not carry the pi prompt type: %q", i+1, lines[i+1])
		}
	}
}

// ---------------------------------------------------------------------------
// (d) ack extraction from claude assistant records; the result record
// must never duplicate a queued ack.

func TestClaudeAckFromAssistantRecord(t *testing.T) {
	script := "#!/bin/sh\n" + `
while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*)
      printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"boundary check"}]}}'
      printf '%s\n' '` + claudeAckAssistantRecord("pf/quiesce") + `' ;;
  esac
done
`
	bin := writeHarness(t, "claudeackharness", script)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_cr2")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("an ack embedded in a claude assistant record must be extracted: %v", err)
	}
	if proc := h.(*Proc); proc.Unhealthy() {
		t.Fatal("a legitimate multi-line ack message must never mark the agent unhealthy")
	}
}

// TestClaudeAckNotDuplicatedByResultRecord: the per-turn result record's
// result field duplicates the turn's LAST assistant text (measured) —
// the same ack line therefore appears in BOTH records. Only the
// assistant record is the ack carrier; scanning the result record too
// would queue the ack twice and let a LATER wait for the same method
// succeed on the stale copy.
func TestClaudeAckNotDuplicatedByResultRecord(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "acked")
	ackRec := claudeAckAssistantRecord("pf/quiesce")
	// The result record carries the SAME multi-line text in its result
	// field, exactly as the real CLI does (measured).
	var resRec struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
	}
	_ = json.Unmarshal([]byte(ackRec), &resRec)
	resultRecord, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "result": resRec.Result,
		"is_error": false, "num_turns": 1, "result_index": 0,
	})
	script := "#!/bin/sh\n" + `
while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*)
      if [ -f "$MARKER" ]; then printf '%s\n' '{"type":"result","subtype":"success","result":"no more acks","is_error":false,"num_turns":1,"result_index":1}'; exit 0; fi
      : > "$MARKER"
      printf '%s\n' '` + ackRec + `'
      printf '%s\n' '` + string(resultRecord) + `'
      ;;
  esac
done
`
	bin := writeHarness(t, "claudeackduperharness", script)
	t.Setenv("MARKER", marker)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_cr3")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("the assistant record's ack must satisfy the first wait: %v", err)
	}
	// Second quiesce: the harness answers with a control-free result
	// record only — a stale duplicate (result-record copy) would wrongly
	// satisfy this wait.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	if err := h.Quiesce(ctx2); err == nil {
		t.Fatal("the result record must not queue a second copy of the ack — a stale duplicate satisfied the second wait")
	}
}

// ---------------------------------------------------------------------------
// (e) claude noise is bulk: hook/system records, user tool echoes,
// thinking-only assistant messages — under a flood, the ack still
// arrives and the agent stays healthy.

func TestClaudeChattyRecordFlood(t *testing.T) {
	script := "#!/bin/sh" + `
i=0
while [ $i -lt 4000 ]; do
  printf '%s\n' '{"type":"system","subtype":"hook_started","hook_id":"f","session_id":"00000000-0000-0000-0000-000000000000"}'
  printf '%s\n' '{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"chatter"}]}}'
  printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"chatter"}]}}'
  i=$((i+1))
done
while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*) printf '%s\n' '` + claudeAckAssistantRecord("pf/quiesce") + `' ;;
  esac
done
`
	bin := writeHarness(t, "claudechattyharness", script)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_cr4")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("a chatty claude record flood must not break the control path: %v", err)
	}
	if proc.Unhealthy() {
		t.Fatal("non-ack claude records are bulk: the agent must stay healthy")
	}
	if d := proc.bulkDropped; d < 12000 {
		t.Fatalf("every non-ack record must be counted as dropped bulk, got %d", d)
	}
}

// ---------------------------------------------------------------------------
// (f) an error result record is observed (logged) but never an ack and
// never an unhealthy marker; the control path survives it.

func TestClaudeErrorResultObservedNotAcked(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "failed")
	script := "#!/bin/sh" + `
while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*)
      if [ ! -f "$MARKER" ]; then
        : > "$MARKER"
        printf '%s\n' '{"type":"result","subtype":"error_during_execution","result":"","is_error":true,"num_turns":1,"result_index":0}'
        continue
      fi
      printf '%s\n' '` + claudeAckAssistantRecord("pf/quiesce") + `' ;;
  esac
done
`
	bin := writeHarness(t, "claudeerrresultharness", script)
	t.Setenv("MARKER", marker)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_cr5")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := h.Quiesce(ctx); err == nil {
		t.Fatal("an error_during_execution result record must NOT satisfy the pending ack")
	}
	if proc.Unhealthy() {
		t.Fatal("an error result record is an observation, never an unhealthy marker")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := h.Quiesce(ctx2); err != nil {
		t.Fatalf("the control path must survive an error result record: %v", err)
	}
}

// ---------------------------------------------------------------------------
// (g) crash continuation: the session-identity read-back marks the
// claude adapter, and the re-derivation resumes the SAME derived
// conversation UUID (--resume <uuid>, no fresh --session-id).

func TestClaudeContinuationResumesSameConversation(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	// One log line per invocation, with newlines squashed (the
	// --append-system-prompt value legitimately contains line breaks —
	// measured "$*" would otherwise split one invocation across lines).
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$(printf '%s' \"$*\" | tr '\\n' ' ')\" >> \"$ARGV_LOG\"\n" +
		`while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*) printf '%s\n' '` + claudeAckAssistantRecord("pf/quiesce") + `' ;;
  esac
done
`
	bin := writeHarness(t, "claudefreshharness", script)
	t.Setenv("ARGV_LOG", argvLog)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_cr6")
	h1, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileLines(t, argvLog, 1) // let the harness log its argv before the kill
	h1.Terminate(2 * time.Second)

	// Crash continuation with the SAME session id on the SAME capsule:
	// the identity read-back marks the adapter, and the second harness
	// invocation must carry --resume of the same conversation UUID.
	h2, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatalf("same-session re-derivation must be allowed: %v", err)
	}
	defer h2.Terminate(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h2.Quiesce(ctx); err != nil {
		t.Fatalf("the resumed agent must still ack control methods: %v", err)
	}

	raw, rerr := os.ReadFile(argvLog)
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("exactly two harness invocations expected, got %d: %q", len(lines), string(raw))
	}
	t.Logf("harness argv log: %q", lines)
	uuid := claudeSessionUUID("sess_cr6")
	if !contains(lines[0], "--session-id "+uuid) || contains(lines[0], "--resume") {
		t.Fatalf("first derivation must pin a fresh --session-id %s: %q", uuid, lines[0])
	}
	if !contains(lines[1], "--resume "+uuid) || contains(lines[1], "--session-id") {
		t.Fatalf("re-derivation must resume the same conversation UUID %s: %q", uuid, lines[1])
	}
}

// ---------------------------------------------------------------------------
// (h) end-to-end against test/fakeclaude (the protocol-faithful test
// double): full control round-trip, conversation persistence across a
// crash continuation, and the MEASURED zero-turn resume failure.

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakeclaude")
	cmd := exec.Command("go", "build", "-o", bin, "./test/fakeclaude")
	cmd.Dir = "../.." // module root (go test's cwd is the package dir)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test/fakeclaude: %v: %s", err, out)
	}
	return bin
}

func TestFakeClaudeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("go build of the fake in-test")
	}
	bin := buildFakeClaude(t)
	t.Setenv("FAKECLAUDE_STATE_DIR", t.TempDir())
	spawnLog := filepath.Join(t.TempDir(), "spawn.log")
	t.Setenv("FAKECLAUDE_SPAWN_LOG", spawnLog)
	s := NewSupervisor("pi-unused", bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_fc_1")

	// First derivation: fresh session, full control round-trip.
	h1, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h1.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce over the real claude record shapes: %v", err)
	}
	if err := h1.NotifyStop(ctx); err != nil {
		t.Fatalf("notify stop: %v", err)
	}
	relay := daemon.AnswerRelay{
		QuestionID: "q_fc", AnswerID: "a_fc", AnswerText: "A",
		AnsweredAt: "2026-09-28T00:00:00Z", AnsweredBy: "u_x",
	}
	if err := h1.DeliverAnswer(relay); err != nil {
		t.Fatalf("deliver answer: %v", err)
	}
	h1.Terminate(2 * time.Second)

	// Crash continuation: the conversation completed turns, so the
	// resumed fake finds it, proceeds and acks again.
	h2, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatalf("re-derivation: %v", err)
	}
	defer h2.Terminate(2 * time.Second)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := h2.Quiesce(ctx2); err != nil {
		t.Fatalf("the resumed conversation must ack: %v", err)
	}

	raw, rerr := os.ReadFile(spawnLog)
	if rerr != nil {
		t.Fatal(rerr)
	}
	spawns := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(spawns) != 2 {
		t.Fatalf("two fakeclaude invocations expected, got %d: %q", len(spawns), string(raw))
	}
	if !contains(spawns[0], " fresh ") || contains(spawns[0], " resume ") {
		t.Fatalf("first spawn must be fresh: %q", spawns[0])
	}
	if !contains(spawns[1], " resume ") || !contains(spawns[1], claudeSessionUUID("sess_fc_1")) {
		t.Fatalf("second spawn must resume the pinned conversation UUID: %q", spawns[1])
	}
}

// TestFakeClaudeZeroTurnResumeFails: the MEASURED corner — a conversation
// whose first turn never completed is NOT resumable; the resumed harness
// dies at startup ("No conversation found", exit 1) instead of serving
// control acks. This is the claude-side residual risk documented on the
// adapter (a crash before the first completed turn breaks the chain);
// the test pins that the failure is LOUD either way it manifests: as a
// dead handle (Wait errors) or as a spawn-time injection failure into
// the already-dying process.
func TestFakeClaudeZeroTurnResumeFails(t *testing.T) {
	if testing.Short() {
		t.Skip("go build of the fake in-test")
	}
	bin := buildFakeClaude(t)
	t.Setenv("FAKECLAUDE_STATE_DIR", t.TempDir())
	s := NewSupervisor("pi-unused", bin, "")
	p := newSpawnParams(t, capsule.AdapterClaude, "sess_fc_2")
	// Pre-existing session identity WITHOUT any completed turn: the
	// §3.6a crash-continuation shape whose conversation never got a turn
	// (the real CLI never persisted one).
	capc := &capsule.Capsule{Dir: p.CapsuleDir, WorkItemID: p.WorkItemID}
	if err := capc.WriteSessionIdentity(capsule.AdapterClaude, p.SessionID); err != nil {
		t.Fatal(err)
	}
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		// The kickoff write raced the harness's exit-1 death: a loud
		// spawn failure is an acceptable manifestation of the measured
		// "No conversation found" refusal.
		if !contains(err.Error(), "initial instruction injection failed") &&
			!contains(err.Error(), "not accepting control messages") {
			t.Fatalf("unexpected spawn error: %v", err)
		}
		return
	}
	if werr := h.(*Proc).Wait(); werr == nil {
		t.Fatal("resuming a zero-turn conversation must fail loudly (measured: exit 1, \"No conversation found\")")
	}
}
