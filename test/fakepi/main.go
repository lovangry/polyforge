// Command fakepi is the dogfood stand-in for the real `pi --mode rpc`
// harness binary (04 §6.1a). The REAL `pf daemon` subcommand spawns it
// as the persistent WI Agent process; it speaks the REAL pi RPC wire
// protocol (pi docs: RPC mode / RPC Commands / JSON Event Stream) and does
// capsule-side work the way the real agent's pf-execute loop would:
// atomic runtime-state/execution.json writes, one artifact, needs_input
// question ask and answer consumption (archive + clear pending_question
// in the SAME atomic write, 03 §4.2 D9).
//
// REAL-pi behaviors REPRODUCED here (verified live against pi 0.87.1,
// `pi --mode rpc`):
//
//   - stdin: one JSON command object per line; prompt commands
//     {"id":..,"type":"prompt","message":..} are the only commands
//     accepted (real pi answers anything else with a success=false
//     response, e.g. "Unknown command: undefined" for a JSON-RPC
//     envelope — the bug this fake used to encode);
//   - stdout: response records {"id","type":"response","command":"prompt",
//     "success":true} echo the command id, emitted BEFORE the run events;
//   - assistant text rides session events — message_start,
//     message_update (text_start/text_delta/text_end inside
//     assistantMessageEvent) and the authoritative message_end whose
//     message.content is a typed-block array [{"type":"text","text":..}]
//     (this is the record the supervisor's ack extraction consumes);
//   - control methods arrive as prompt commands whose message text is the
//     control JSON line VERBATIM; the ack line is embedded in a
//     MULTI-LINE assistant message (the SKILL.md §12 transport);
//   - agent lifecycle records agent_start/turn_start/turn_end/agent_end,
//     and agent_settled — including the startup agent_settled real pi
//     emits for a fresh session before any prompt;
//   - stdout is reserved for protocol records; diagnostics go to stderr.
//
// REAL-pi behaviors NOT reproduced (known deviations):
//
//   - stdin EOF does NOT exit: the process parks alive. Real pi shuts
//     down on EOF, but the dogfood crash drills (usage_s5/s6) pin the
//     kill -9'd daemon's WI Agent SURVIVING (04 §7.1 alive-reattach),
//     which requires ignoring the closed pipe;
//   - no real model: the "run" behind a prompt is a canned synchronous
//     event burst (no compaction, retry, queue_update, tool or
//     extension-UI records, no session files, no failure responses for
//     prompts issued mid-"streaming" — fakepi never streams);
//   - the initial pf-execute instruction is not interpreted: the
//     capsule-side effects are the FAKEPI_MODE scripts below.
//
// Test-only helper (the usage_* dogfood scenarios build and exec it);
// `go build ./...` ships it as a main package but nothing in production
// imports or references it.
//
// Knobs (environment, set by the spawning scenario):
//
//	FAKEPI_MODE  idle  – write a running execution.json, then wait (default)
//	               ask  – same, then park the step on an open pending_question
//	FAKEPI_SPAWN_LOG – when set, append one "<pid> <session_id> <started_at>"
//	               line per process start (spawn-count / session-reuse pins).
//	FAKEPI_EXIT  – when non-empty, exit(0) immediately (a harness that
//	               dies on startup; crash drills).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Control-channel method names — byte-identical with the two-sided
// contract (internal/mainagent Method* constants / SKILL.md §12). Under
// the pi transport these arrive as the TEXT of prompt commands.
const (
	methodQuiesce    = "pf/quiesce"
	methodResume     = "pf/resume"
	methodStopNotice = "pf/stop_notice"
	methodAnswer     = "pf/answer"
)

// execPath is the capsule-side execution state file (cwd is the capsule
// dir; the daemon spawns with cmd.Dir = capsule dir).
const execPath = "runtime-state/execution.json"

// The persisted needs_input question this agent asks (03 §4.2 D8).
const askedQuestionID = "q_dg0001"

func main() {
	logSpawn()
	if os.Getenv("FAKEPI_EXIT") != "" {
		os.Exit(0)
	}
	mode := os.Getenv("FAKEPI_MODE")
	switch mode {
	case "", "idle":
		writeRunningExecution(nil)
		_ = os.MkdirAll("artifacts", 0o755)
		_ = os.WriteFile("artifacts/work.md", []byte("fakepi work\n"), 0o644)
	case "ask":
		writeRunningExecution(&pendingQuestion{
			QuestionID: askedQuestionID,
			AskedAt:    nowStamp(),
			Content:    "Which design should the code step take: A or B?",
		})
	default:
		fmt.Fprintf(os.Stderr, "fakepi: unknown FAKEPI_MODE %q\n", mode)
		os.Exit(1)
	}

	// RPC startup: real pi emits agent_settled for the fresh session BEFORE
	// any prompt arrives (measured live).
	emitRecord(map[string]any{"type": "agent_settled"})

	// The RPC command loop (REAL protocol): stdin carries one JSON command
	// object per line; only prompt commands are meaningful here. The
	// control methods ride prompt commands whose message text is the
	// control JSON line VERBATIM — parsed exactly as the old raw lines
	// were. Every prompt is answered by a response record echoing the
	// command id, then — for control methods — a full event burst whose
	// assistant message embeds the ack line.
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var cmd struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &cmd) != nil || cmd.Type != "prompt" {
			continue // not a prompt command: ignore (real pi answers unknown
			// commands with a success=false response; nothing here needs that)
		}
		emitRecord(map[string]any{
			"id": cmd.ID, "type": "response", "command": "prompt", "success": true,
		})
		var ctl struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(cmd.Message), &ctl) != nil {
			ctl.Method = "" // free text, not a control line
		}
		switch ctl.Method {
		case methodQuiesce, methodResume, methodStopNotice:
			ackViaAssistantMessage(ctl.Method)
		case methodAnswer:
			consumeAnswer([]byte(cmd.Message))
			ackViaAssistantMessage(methodAnswer)
		default:
			// The initial pf-execute instruction: a plain assistant turn
			// (no ack line — nothing to ack yet).
			emitAssistantTurn("fakepi: instruction received; standing by for control methods.")
		}
	}
	// stdin EOF (e.g. the daemon died): park alive — the crash drills need
	// the WI Agent to survive the parent's kill -9. A bare `select {}`
	// trips the Go runtime's all-goroutines-asleep deadlock detector the
	// moment the control loop ends, killing the agent; an armed timer
	// never does.
	for {
		time.Sleep(time.Hour)
	}
}

// ackViaAssistantMessage acks one control method the way the REAL
// transport contract defines (SKILL.md §12 transport note): the ack JSON
// line is embedded in a MULTI-LINE assistant message — never written as a
// raw stdout line.
func ackViaAssistantMessage(method string) {
	text := "control method received; acking at a safe boundary\n" +
		`{"method":"` + method + `","result":{"ack":"` + method + `"}}` + "\nstanding by"
	emitAssistantTurn(text)
}

// emitAssistantTurn streams one complete assistant message as the REAL
// event sequence (pi docs: JSON Event Stream): agent/turn lifecycle,
// message_start, message_update deltas (text_start / text_delta /
// text_end inside assistantMessageEvent) and the authoritative message_end
// carrying the full typed-block content array. The burst ends with
// agent_end + agent_settled (quiescence).
func emitAssistantTurn(text string) {
	block := map[string]any{"type": "text", "text": text}
	content := []any{block}
	message := map[string]any{"role": "assistant", "content": content}
	usage := map[string]any{
		"input": 0, "output": len(text), "cacheRead": 0, "cacheWrite": 0,
		"totalTokens": len(text),
		"cost":        map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0},
	}
	firstDelta := text
	if len(firstDelta) > 16 { // two deltas: the ack line may straddle chunks
		firstDelta = text[:16]
	}
	emitRecord(map[string]any{"type": "agent_start"})
	emitRecord(map[string]any{"type": "turn_start"})
	emitRecord(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant", "content": []any{}}})
	emitRecord(map[string]any{"type": "message_update", "usage": usage,
		"assistantMessageEvent": map[string]any{"type": "text_start", "contentIndex": 0}})
	emitRecord(map[string]any{"type": "message_update", "usage": usage,
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": firstDelta}})
	if firstDelta != text {
		emitRecord(map[string]any{"type": "message_update", "usage": usage,
			"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": text[len(firstDelta):]}})
	}
	emitRecord(map[string]any{"type": "message_update", "usage": usage,
		"assistantMessageEvent": map[string]any{"type": "text_end", "contentIndex": 0, "content": text}})
	emitRecord(map[string]any{"type": "message_end", "message": message})
	emitRecord(map[string]any{"type": "turn_end", "message": message, "toolResults": []any{}})
	emitRecord(map[string]any{"type": "agent_end", "messages": []any{message}, "willRetry": false})
	emitRecord(map[string]any{"type": "agent_settled"})
}

// emitRecord writes ONE protocol record to stdout (JSON + LF).
func emitRecord(rec map[string]any) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	fmt.Println(string(raw))
}

// logSpawn journals this process start for the scenario's spawn-count and
// session-reuse assertions.
func logSpawn() {
	p := os.Getenv("FAKEPI_SPAWN_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %s %s\n", os.Getpid(), os.Getenv("PF_SESSION_ID"), nowStamp())
}

// ---------------------------------------------------------------------------
// Capsule-side execution state (03 §4.2): the shapes mirror
// internal/capsule/execstate.go. The capsule id is "cap_" + wi id
// (internal/capsule/identity.go ID()).

type pendingQuestion struct {
	QuestionID    string        `json:"question_id"`
	AskedAt       string        `json:"asked_at"`
	Content       string        `json:"content"`
	PendingAnswer *answerRecord `json:"pending_answer"`
}

type answerRecord struct {
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
	ReceivedAt string `json:"received_at"`
}

type archivedAnswer struct {
	QuestionID string `json:"question_id"`
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
}

type stepState struct {
	Status          string           `json:"status"`
	BudgetConsumed  map[string]int64 `json:"budget_consumed"`
	Attempts        []any            `json:"attempts"`
	Answers         []archivedAnswer `json:"answers"`
	PendingQuestion *pendingQuestion `json:"pending_question"`
}

type execution struct {
	CapsuleID      string                `json:"capsule_id"`
	WorkItemID     string                `json:"work_item_id"`
	Generation     int64                 `json:"generation"`
	CurrentStep    string                `json:"current_step"`
	WorkflowStatus string                `json:"workflow_status"`
	Steps          map[string]*stepState `json:"steps"`
	ReviewRounds   map[string]int64      `json:"review_rounds"`
	UpdatedAt      string                `json:"updated_at"`
	Version        int64                 `json:"version"`
}

func wiID() string { return os.Getenv("PF_WORK_ITEM_ID") }
func gen() int64 {
	n, _ := strconv.ParseInt(os.Getenv("PF_GENERATION"), 10, 64)
	return n
}

// writeRunningExecution persists the minimal running state (one in-flight
// code step), optionally parked on an open needs_input question. Atomic:
// tmp + rename, the 03 §4.2 commit contract.
func writeRunningExecution(q *pendingQuestion) {
	st := &stepState{Status: "in_progress", BudgetConsumed: map[string]int64{"requests": 1}, Attempts: []any{}, Answers: []archivedAnswer{}, PendingQuestion: q}
	e := &execution{
		CapsuleID: "cap_" + wiID(), WorkItemID: wiID(), Generation: gen(),
		CurrentStep: "code", WorkflowStatus: "running",
		Steps:        map[string]*stepState{"code": st},
		ReviewRounds: map[string]int64{}, UpdatedAt: nowStamp(), Version: 2,
	}
	saveExecution(e)
}

// consumeAnswer implements the WI Agent's consumption contract (03 §4.2
// D9): archive the answer into steps[].answers and clear pending_question
// in the SAME atomic write.
func consumeAnswer(raw []byte) {
	var msg struct {
		Params struct {
			QuestionID string `json:"question_id"`
			AnswerText string `json:"answer_text"`
			AnsweredAt string `json:"answered_at"`
			AnsweredBy string `json:"answered_by"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	e := loadExecution()
	if e == nil {
		return
	}
	st := e.Steps[e.CurrentStep]
	if st == nil {
		return
	}
	st.Answers = append(st.Answers, archivedAnswer{
		QuestionID: msg.Params.QuestionID, AnswerText: msg.Params.AnswerText,
		AnsweredAt: msg.Params.AnsweredAt, AnsweredBy: msg.Params.AnsweredBy,
	})
	st.PendingQuestion = nil
	e.Version++
	e.UpdatedAt = nowStamp()
	saveExecution(e)
}

// loadExecution reads execution.json (nil on any miss).
func loadExecution() *execution {
	raw, err := os.ReadFile(execPath)
	if err != nil {
		return nil
	}
	var e execution
	if json.Unmarshal(raw, &e) != nil {
		return nil
	}
	return &e
}

// saveExecution writes the state atomically: tmp file, fsync, rename
// (03 §4.2: every write to runtime-state/.execution.json.tmp then rename).
func saveExecution(e *execution) {
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	tmp := execPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, execPath)
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
