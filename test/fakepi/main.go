// Command fakepi is the dogfood stand-in for the real `pi --mode rpc`
// harness binary (04 §6.1a). The REAL pf-daemon spawns it as the persistent
// WI Agent process; it speaks the stdio control-channel contract that
// skills/pf-execute/SKILL.md declares (the same names as
// internal/mainagent's control methods) and does capsule-side work the way
// the real agent's pf-execute loop would: atomic runtime-state/
// execution.json writes, one artifact, needs_input question ask and
// answer consumption (archive + clear pending_question in the SAME atomic
// write, 03 §4.2 D9).
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
//
// On stdin EOF the process stays alive: a kill -9 of the parent pf-daemon
// must leave a LIVE WI Agent behind (04 §7.1 step 3's alive-reattach case),
// so EOF parks instead of exiting.
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
// contract (internal/mainagent Method* constants / SKILL.md).
const (
	methodPrompt     = "user/prompt"
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

	// The stdio control loop: every pf/* method is acked in the shape the
	// supervisor's awaitAck accepts ({"method":..., "result":{"ack":...}}).
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		var msg struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method == "" {
			continue // bulk output / non-control line: ignore
		}
		switch msg.Method {
		case methodPrompt, methodQuiesce, methodResume, methodStopNotice:
			ack(msg.Method)
		case methodAnswer:
			consumeAnswer(sc.Bytes())
			ack(methodAnswer)
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

// ack emits the supervisor-recognized acknowledgment line.
func ack(method string) {
	fmt.Printf("{\"jsonrpc\":\"2.0\",\"method\":%q,\"result\":{\"ack\":%q}}\n", method, method)
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
