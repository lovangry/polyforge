package mainagent

// rpcreal_test.go: regressions pinning the REAL pi RPC transport (the
// polyforge#31 transport rework). The old wire envelope
// ({"jsonrpc":"2.0","id":null,"method":"user/prompt","params":{...}}) only
// ever worked against the fakepi test double — the real pi answers it with
// {"id":null,"type":"response","success":false,"error":"Unknown command:
// undefined"} and then sits in epoll forever (measured live). These tests
// pin the measured protocol:
//
//   (a) the prompt command envelope (id/type/message, no jsonrpc key);
//   (b) ack extraction from assistant message records, including a
//       multi-line message with the ack line embedded (and never queued
//       twice when the same text also streams through message_update);
//   (c) control methods delivered as prompt commands whose message text is
//       the control JSON line VERBATIM;
//   (d) the daemon survives a chatty child emitting many non-assistant
//       records (bulk drop, agent healthy, ack still arrives);
//   (e) a success=false response record for OUR command id is observed
//       (logged) but never treated as an ack;
//   plus: the extracted-line length bound closes the control path typed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// ackMessageRecord builds one REAL message_end record whose multi-line
// assistant text embeds the ack line for method (regression b shape).
func ackMessageRecord(method string) string {
	return `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"first line before the ack\n` +
		`{\"method\":\"` + method + `\",\"result\":{\"ack\":\"` + method + `\"}}` +
		`\nlast line after the ack"}]}}`
}

// ---------------------------------------------------------------------------
// (a) the prompt command envelope.

func TestRegressionPromptCommandEnvelope(t *testing.T) {
	a := &PiAdapter{}
	m, err := a.InitialMessage(daemon.SpawnParams{InitialPrompt: "# pf-execute\nrun it"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("the initial message must be one JSON object: %v", err)
	}
	// The REAL envelope keys (measured live): id, type, message — and the
	// command type is "prompt".
	if probe["type"] != "prompt" {
		t.Fatalf(`the initial command must carry "type":"prompt", got %v`, probe["type"])
	}
	if probe["message"] != "# pf-execute\nrun it" {
		t.Fatalf(`the initial command must carry the instruction in "message", got %v`, probe["message"])
	}
	if s, _ := probe["id"].(string); s == "" {
		t.Fatal(`the initial command must carry a string "id"`)
	}
	// The OLD (wrong) envelope keys must be gone: real pi answers
	// jsonrpc/method envelopes with "Unknown command: undefined".
	if _, ok := probe["jsonrpc"]; ok {
		t.Fatal(`the prompt command must NOT carry a jsonrpc key (real pi rejects JSON-RPC envelopes)`)
	}
	if _, ok := probe["method"]; ok {
		t.Fatal(`the prompt command must NOT carry a method key`)
	}
	if _, ok := probe["params"]; ok {
		t.Fatal(`the prompt command must NOT carry a params key`)
	}
	// The initial prompt carries no streamingBehavior (the agent is idle
	// at derivation); only control prompts steer.
	if _, ok := probe["streamingBehavior"]; ok {
		t.Fatal(`the initial prompt must not carry streamingBehavior`)
	}
}

// ---------------------------------------------------------------------------
// (b) ack extraction from assistant message records.

// TestRegressionAckExtractedFromMultiLineMessage: the ack line embedded in
// a MULTI-LINE assistant message (the SKILL.md §12 transport: the agent
// acks inside its assistant text) must be extracted and satisfy the
// pending quiesce wait.
func TestRegressionAckExtractedFromMultiLineMessage(t *testing.T) {
	bin := writeHarness(t, "multilineackharness", "#!/bin/sh\n"+`
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '`+ackMessageRecord("pf/quiesce")+`'
      ;;
  esac
done
`)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_rmt1")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("an ack line embedded mid-message must be extracted: %v", err)
	}
	if proc := h.(*Proc); proc.Unhealthy() {
		t.Fatal("a legitimate multi-line ack message must never mark the agent unhealthy")
	}
}

// TestRegressionAckNotQueuedTwice: the same assistant text streaming
// through message_update deltas AND completed by message_end (the real
// event shape) must yield exactly ONE queued ack — a duplicate would let a
// LATER wait for the same method succeed on the stale copy. The harness
// acks only the FIRST pf/quiesce (marker file); the second Quiesce must
// time out.
func TestRegressionAckNotQueuedTwice(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "acked")
	bin := writeHarness(t, "deltaplusendharness", "#!/bin/sh\n"+`
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      if [ -f "$MARKER" ]; then printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"no more acks"}]}}'; exit 0; fi
      : > "$MARKER"
      printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"{\"method\":\"pf/quiesce\",\"result\":{\"ack\":\"pf/quiesce\"}}"}}'
      printf '%s\n' '`+ackMessageRecord("pf/quiesce")+`'
      ;;
  esac
done
`)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_rmt2")
	t.Setenv("MARKER", marker) // inherited by the child through os.Environ
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("first quiesce must be acked (message_end is authoritative): %v", err)
	}
	// Second quiesce: the harness exits without acking — any stale
	// duplicate of the first ack in the queue would wrongly satisfy it.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	if err := h.Quiesce(ctx2); err == nil {
		t.Fatal("the ack must be queued exactly once — a stale duplicate satisfied the second wait")
	}
}

// TestRegressionAssistantTextExtraction: unit drill of the extraction
// over the REAL message shapes (captured live from pi 0.87.1): typed-block
// arrays, multi-block concatenation, bare-string tolerance, non-assistant
// roles rejected.
func TestRegressionAssistantTextExtraction(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
		ok   bool
	}{
		{"assistant typed blocks", `{"role":"assistant","content":[{"type":"text","text":"hello"}]}`, "hello", true},
		{"assistant multi-block", `{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"thinking","text":"T"},{"type":"text","text":"b"}]}`, "ab", true},
		{"assistant string content", `{"role":"assistant","content":"plain string"}`, "plain string", true},
		{"user echo is not an ack carrier", `{"role":"user","content":[{"type":"text","text":"hello"}]}`, "", false},
		{"system echo is not an ack carrier", `{"role":"system","content":""}`, "", false},
	}
	for _, c := range cases {
		got, ok := assistantText(json.RawMessage(c.msg))
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
	if _, ok := assistantText(nil); ok {
		t.Error("an absent message object must not classify as assistant text")
	}
}

// ---------------------------------------------------------------------------
// (c) control methods delivered as prompt commands with the control JSON
// line VERBATIM as the message text.

func TestRegressionControlRidesPromptCommandVerbatim(t *testing.T) {
	recFile := filepath.Join(t.TempDir(), "stdin.log")
	ackQuiesce := ackMessageRecord("pf/quiesce")
	ackStop := ackMessageRecord("pf/stop_notice")
	ackAnswer := ackMessageRecord("pf/answer")
	bin := writeHarness(t, "recordingharness", "#!/bin/sh\n"+`
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$RECORD_FILE"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '`+ackQuiesce+`'
      ;;
    *pf/stop_notice*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '`+ackStop+`'
      ;;
    *pf/answer*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '`+ackAnswer+`'
      ;;
  esac
done
`)
	t.Setenv("RECORD_FILE", recFile)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_rmt3")
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
	if len(lines) != 4 { // initial prompt + 3 control prompts
		t.Fatalf("the harness must record the initial prompt and the three control prompts, got %d: %q", len(lines), string(raw))
	}
	// The initial prompt: a plain prompt command.
	var initCmd map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &initCmd); err != nil {
		t.Fatalf("initial write is not JSON: %q", lines[0])
	}
	if initCmd["type"] != "prompt" || initCmd["message"] != p.InitialPrompt {
		t.Fatalf("the initial write must be the pf-execute prompt command: %q", lines[0])
	}
	// The three control prompts: message text is the control line VERBATIM.
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
		var cmd struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
			Stream  string `json:"streamingBehavior"`
		}
		if err := json.Unmarshal([]byte(lines[i+1]), &cmd); err != nil {
			t.Fatalf("control write %d is not JSON: %q", i+1, lines[i+1])
		}
		if cmd.Type != "prompt" || cmd.ID == "" || cmd.Stream != "steer" {
			t.Fatalf("control write %d must be a steer prompt command with a correlation id, got %q", i+1, lines[i+1])
		}
		// VERBATIM: the message text equals the control line the daemon
		// serialized for this method, byte for byte.
		line, _ := json.Marshal(controlEnvelope{JSONRPC: "2.0", Method: w.method, Params: w.params})
		if cmd.Message != string(line) {
			t.Fatalf("control write %d message must be the control line VERBATIM:\n got %q\nwant %q", i+1, cmd.Message, string(line))
		}
		// The command's own envelope carries no jsonrpc key at the top
		// level (it lives only inside the message text).
		var top map[string]any
		_ = json.Unmarshal([]byte(lines[i+1]), &top)
		if _, bad := top["jsonrpc"]; bad {
			t.Fatalf("control write %d must not carry a top-level jsonrpc key: %q", i+1, lines[i+1])
		}
	}
}

// ---------------------------------------------------------------------------
// (d) the daemon survives a chatty child emitting many non-assistant
// records (bulk drop).

func TestRegressionChattyChildBulkRecords(t *testing.T) {
	bin := writeHarness(t, "chattyharness", "#!/bin/sh"+`
i=0
while [ $i -lt 4000 ]; do
  printf '%s\n' '{"type":"agent_start"}'
  printf '%s\n' '{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"chatter"}}'
  printf '%s\n' '{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"user echo"}]}}'
  printf '%s\n' '{"type":"extension_ui_request","id":"ui-1","method":"setStatus","statusKey":"tasks"}'
  i=$((i+1))
done
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '`+ackMessageRecord("pf/quiesce")+`'
      ;;
  esac
done
`)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_rmt4")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("a chatty non-assistant record flood must not break the control path: %v", err)
	}
	if proc.Unhealthy() {
		t.Fatal("non-assistant records are bulk: the agent must stay healthy")
	}
	if d := proc.bulkDropped; d < 16000 {
		t.Fatalf("every non-assistant record must be counted as dropped bulk, got %d", d)
	}
}

// ---------------------------------------------------------------------------
// (e) a success=false response record for OUR command id is observed, not
// acked on.

func TestRegressionResponseFailureObservedNotAcked(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "failed")
	bin := writeHarness(t, "failrespharness", "#!/bin/sh"+`
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      if [ ! -f "$MARKER" ]; then
        : > "$MARKER"
        # OUR command id fails (e.g. rejected mid-stream): observed, but
        # this is NOT an ack — the agent never saw the method.
        printf '{"id":"%s","type":"response","command":"prompt","success":false,"error":"boom: rejected"}\n' "$id"
        continue
      fi
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '`+ackMessageRecord("pf/quiesce")+`'
      ;;
  esac
done
`)
	t.Setenv("MARKER", marker)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_rmt5")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	// First quiesce: the command FAILED (success=false response for our
	// id) and no ack follows — the wait must run to its deadline and fail;
	// the failure response must not close the control path nor mark the
	// agent unhealthy.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := h.Quiesce(ctx); err == nil {
		t.Fatal("a success=false response record must NOT satisfy the pending ack")
	}
	if proc.Unhealthy() {
		t.Fatal("a failed response record is bulk observation, never an unhealthy marker")
	}
	// The control path survived: the second quiesce is genuinely acked.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := h.Quiesce(ctx2); err != nil {
		t.Fatalf("the control path must survive a failed response record: %v", err)
	}
	// The failed id (and the second, successful one) were consumed by the
	// observation; only pf-init lingers (this harness never answers the
	// initial prompt). An unconsumed failed id would leave two entries.
	proc.pendMu.Lock()
	pending := len(proc.pending)
	proc.pendMu.Unlock()
	if pending != 1 {
		t.Fatalf("observed response ids must be consumed from the pending set, %d entries left", pending)
	}
}

// ---------------------------------------------------------------------------
// The extracted-line bound: an assistant message whose text line exceeds
// maxControlLineBytes closes the control path typed — the SAME
// ErrControlLineTooLong semantics the bounded reader enforces on raw
// records (the "semantics applied to the extracted lines" contract).
// A giant assistant message trips the RAW bound first (the record itself
// is one overlong line — the memory guard fires before any extraction can
// happen), so the observable behavior is the identical typed close; the
// per-extracted-line check in classifyCandidateLine is the defensive
// second layer keeping the invariant true no matter which path a line
// arrives by.

func TestRegressionOverlongExtractedLineClosesControlPath(t *testing.T) {
	bin := writeHarness(t, "gianttextmessageharness", "#!/bin/sh"+`
big=$(head -c 5000000 /dev/zero | tr '\0' 'a')
printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}\n' "$big"
sleep 300
`)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_rmt6")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	waitForUnhealthy(t, proc)
	if r, ok := proc.unhealthyReason(); !ok || !errors.Is(r, ErrControlLineTooLong) {
		t.Fatalf("the overlong extracted line must close the control path typed ErrControlLineTooLong, got %v / %v", r, ok)
	}
}
