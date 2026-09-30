// drive.go: the v1 fake agent's scripted executor. The plan file is a
// JSON document describing, in order, what this derivation does on the
// capsule side — write execution.json (Version monotonically increasing,
// atomic tmp+rename), call flush(reason) over the real UDS proxy, emit
// record bursts, park or die. Every action and control-method landing is
// journaled to logs/fakepi.jsonl inside the capsule, which the scenarios
// assert on.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// planAction is one scripted step of a derivation.
type planAction struct {
	Do       string `json:"do"`
	ID       string `json:"id"`                 // step id / question id
	Question string `json:"question"`           // ask: the question text
	Count    int    `json:"count"`              // provider_error: number of error turns
	MS       int    `json:"ms"`                 // sleep: milliseconds
	Name     string `json:"name"`               // repo: the repos/ subdirectory
	Origin   string `json:"origin"`             // repo: the origin remote URL
}

// plan is the whole scripted behavior of one derivation.
type plan struct {
	Actions []planAction `json:"actions"`
	// ResumeActions, when set, replace Actions for a RESUME derivation
	// (the recovery engine re-derives with --session): a scenario scripts
	// the crash and the continuation in one file, deterministically —
	// no rewrite race against the recovery spawn.
	ResumeActions []planAction `json:"resume_actions"`
	// AckProbe false makes the agent ignore liveness probes (the hang
	// scenario: silence must extend to the probe channel).
	AckProbe *bool `json:"ack_probe"`
	// AckAnswer false makes the agent land pf/answer on its journal
	// WITHOUT consuming or acking it (the F9 recovery scenario: the relay
	// reached stdin, then the instance died mid-consumption — the
	// daemon's ledger keeps the answer re-deliverable). A RESUME
	// derivation re-reads the plan file, so a scenario clears the flag
	// to let the successor consume normally.
	AckAnswer *bool `json:"ack_answer"`
	// FailSpawnResume true makes every RESUME derivation die AT BIRTH:
	// the process exits before it reads one byte of stdin, so the
	// daemon's initial-prompt injection finds no reader, the pipe write
	// fails and Spawn itself returns an error. That is the ONLY shape the
	// spawn-failure ladder counts as a failed resume
	// (runtime.Runtime.cascade increments resumeFails on a spawnAgent
	// error and nothing else). A derivation that accepts the prompt and
	// dies AFTERWARDS is a materially different lane — a new agent_death
	// recovery, which RESETS the budgets — so this mode must never
	// degrade into that one; see the pipe-capacity contract below.
	FailSpawnResume bool `json:"fail_spawn_resume"`
	// FailSpawnFresh true makes every fresh RE-derivation die the same
	// way. "Re-derivation" is discriminated on the lineage suffix the
	// runtime mints into PF_INSTANCE_ID (`<attempt>#N`): the ORIGINAL
	// derivation carries the bare attempt id and is NEVER affected by
	// this flag. One plan file therefore scripts a healthy first run AND
	// a must-fail recovery ladder with no rewrite race against the
	// recovery spawn.
	//
	// PIPE-CAPACITY CONTRACT (both flags). "Dies before the prompt
	// lands" is not something this process can win by racing: the daemon
	// writes the prompt a few hundred microseconds after fork/exec,
	// while a Go binary spends about as long in runtime start-up
	// (measured on the ladder's own shape: a child that exits
	// immediately loses the race ~93% of the time against a ~27 KiB
	// prompt). The mode is deterministic only when the daemon's prompt
	// write CANNOT complete into the stdin pipe buffer — i.e. the
	// initial prompt exceeds the pipe capacity (64 KiB on Linux). Then
	// both orderings converge on the same answer: the parent blocks
	// mid-write and this process's exit breaks the pipe (EPIPE), or this
	// process is gone first and the very first write breaks it. A
	// scenario using these flags MUST size the daemon's executor prompt
	// accordingly (test/v1_s17_recovery_budget_test.go does, and asserts
	// it).
	FailSpawnFresh bool `json:"fail_spawn_fresh"`
}

// execState mirrors the execution.json contract (03 §4.2 wire shape; the
// observe lane unmarshals the real capsule.Execution, so the fields here
// are a subset that keeps every unmarshal honest).
type execState struct {
	CapsuleID      string                `json:"capsule_id"`
	WorkItemID     string                `json:"work_item_id"`
	Generation     int64                 `json:"generation"`
	CurrentStep    string                `json:"current_step"`
	WorkflowStatus string                `json:"workflow_status"`
	Steps          map[string]*execStep  `json:"steps"`
	UpdatedAt      string                `json:"updated_at"`
	Version        int64                 `json:"version"`
}

type execStep struct {
	Status          string           `json:"status"`
	Model           string           `json:"model,omitempty"` // the pinned candidate this derivation was spawned with
	StepStartedAt   string           `json:"step_started_at,omitempty"`
	Attempts        []any            `json:"attempts"`
	Answers         []archivedAnswer `json:"answers"`
	PendingQuestion *pendingQuestion `json:"pending_question,omitempty"`
}

type pendingQuestion struct {
	ID       string `json:"id"`
	Step     string `json:"step"`
	Question string `json:"question"`
	AskedAt  string `json:"asked_at"`
}

type archivedAnswer struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
	AnsweredBy string `json:"answered_by"`
	AnsweredAt string `json:"answered_at"`
}

// driveEnv is the shared state of one fake agent process.
type driveEnv struct {
	wi        string
	gen       int64
	instance  string // PF_INSTANCE_ID: the flush attempt-id fence value
	socket    string // PF_DAEMON_SOCKET
	sessionArg string
	resumed   bool

	mu        sync.Mutex // guards exec, journal, transcript, askWaiters
	exec      *execState
	plan      *plan
	transcript string
	askWait   map[string]chan struct{}
	hanging   bool
	ackProbe  bool
	// ackAnswer false: land pf/answer on the journal but neither consume
	// nor ack it (the died-mid-consumption shape; see plan.AckAnswer).
	ackAnswer bool
	// mustFail: this derivation is scripted to die at birth, before it
	// reads the daemon's initial prompt (see plan.FailSpawnResume).
	mustFail bool

	shutOnce sync.Once
}

// newDriveEnv assembles the executor from the spawn env + the plan file.
// A missing plan parks the agent loudly (the scenario fails on its own
// bounded wait — never a silent no-op).
func newDriveEnv() *driveEnv {
	wi := os.Getenv("PF_WI_ID")
	inst := os.Getenv("PF_INSTANCE_ID")
	if wi == "" || inst == "" {
		fmt.Fprintln(os.Stderr, "fakepi: PF_WI_ID/PF_INSTANCE_ID not set; parking")
		return nil
	}
	gen := int64(0)
	if g := os.Getenv("PF_GENERATION"); g != "" {
		if n, err := strconv.ParseInt(g, 10, 64); err == nil {
			gen = n
		}
	}
	pl, path := loadPlan(wi)
	if pl == nil {
		fmt.Fprintf(os.Stderr, "fakepi: no plan file for %s (looked at %s); parking\n", wi, path)
		return nil
	}
	d := &driveEnv{
		wi:         wi,
		gen:        gen,
		instance:   inst,
		socket:     os.Getenv("PF_DAEMON_SOCKET"),
		sessionArg: transcriptPathOf(),
		resumed:    hasSessionArg(),
		askWait:    map[string]chan struct{}{},
		ackProbe:   pl.AckProbe == nil || *pl.AckProbe,
		ackAnswer:  pl.AckAnswer == nil || *pl.AckAnswer,
		transcript: transcriptPathOf(),
	}
	d.exec = d.loadExec()
	if d.exec == nil {
		capID := "cap_" + wi
		d.exec = &execState{
			CapsuleID:      capID,
			WorkItemID:     wi,
			Generation:     gen,
			WorkflowStatus: "running",
			Steps:          map[string]*execStep{},
			Version:        0,
		}
	}
	if d.resumed && len(pl.ResumeActions) > 0 {
		pl.Actions = pl.ResumeActions
	}
	d.mustFail = mustFailSpawn(pl, d.resumed, inst)
	d.plan = pl
	return d
}

// isRederivation reports whether a lineage id carries the recovery
// suffix the runtime mints for every derivation after the first
// (`<attempt>#N`). The ORIGINAL derivation carries the bare attempt id.
func isRederivation(instance string) bool { return strings.Contains(instance, "#") }

// mustFailSpawn decides whether THIS derivation is the scripted
// must-fail-at-spawn kind: a resume derivation under FailSpawnResume, or
// a fresh RE-derivation under FailSpawnFresh. The original derivation is
// never selected by either flag.
func mustFailSpawn(pl *plan, resumed bool, instance string) bool {
	if resumed {
		return pl.FailSpawnResume
	}
	return pl.FailSpawnFresh && isRederivation(instance)
}
func hasSessionArg() bool {
	for i, a := range os.Args {
		if a == "--session" && i+1 < len(os.Args) {
			return true
		}
	}
	return false
}

// loadPlan reads the plan file: FAKEPI_PLANS/<wi>.json, falling back to
// FAKEPI_PLANS/default.json. The file is re-read at every process start,
// so a scenario rewrites it to steer a re-derived agent.
func loadPlan(wi string) (*plan, string) {
	dir := os.Getenv("FAKEPI_PLANS")
	if dir == "" {
		return nil, ""
	}
	path := filepath.Join(dir, wi+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		path = filepath.Join(dir, "default.json")
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, path
		}
	}
	var pl plan
	if json.Unmarshal(raw, &pl) != nil {
		return nil, path
	}
	return &pl, path
}

// ---------------------------------------------------------------------------
// runDriveLoop executes the plan actions in order. The plan is re-read at
// every process start, so a re-derived agent follows whatever the scenario
// wrote most recently.
func runDriveLoop(d *driveEnv) {
	// Boot settle: a real harness spends its first moments loading
	// context before the first action; the pause also keeps an
	// immediately-flushing plan clear of the spawn-window races (the
	// monitor adopts the new handle on the RTRecovered attach, and the
	// runtime finishes its RECOVERING→RUNNING transition, both of which
	// lag a just-spawned process by microseconds).
	sleepMS(250)
	for _, act := range d.plan.Actions {
		switch act.Do {
		case "step":
			d.runStep(act.ID)
		case "ask":
			d.askAndWait(act.ID, act.Question)
		case "wrap":
			d.flushWrap("wrapped")
			parkForever()
		case "fail":
			d.flushWrap("failed")
			parkForever()
		case "hang":
			d.beginHang()
			parkForever()
		case "provider_error":
			n := act.Count
			if n <= 0 {
				n = 2
			}
			for i := 0; i < n; i++ {
				emitAssistantTurn(d, "",
					fmt.Sprintf("upstream connection reset after %d retries (fake harness injected)", i+1))
				sleepMS(60)
			}
		case "park":
			parkForever()
		case "exit":
			d.journal("exit", map[string]any{"code": 0})
			os.Exit(0)
		case "sleep":
			sleepMS(act.MS)
		case "repo":
			d.makeRepo(act.Name, act.Origin)
		default:
			fmt.Fprintf(os.Stderr, "fakepi: unknown plan action %q; parking\n", act.Do)
			parkForever()
		}
	}
	// plan exhausted: park (the scenario closes the WI itself)
	parkForever()
}

func sleepMS(n int) {
	if n <= 0 {
		n = 50
	}
	time.Sleep(time.Duration(n) * time.Millisecond)
}

// pinnedModelOf reads the --model argv the runtime pinned this derivation
// with ("" when the spawn carried no pin — the no-chain shape).
func pinnedModelOf() string {
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--model" {
			return args[i+1]
		}
	}
	return ""
}

// runStep writes one step through execution.json: in_progress, then done
// — two atomic writes, Version increasing each time (the async push
// surface the observe lane polls).
func (d *driveEnv) runStep(stepID string) {
	if stepID == "" {
		stepID = "step-anon"
	}
	d.commit(func(x *execState) {
		st := x.step(stepID)
		st.Status = "in_progress"
		st.Model = pinnedModelOf() // mirror the executor contract: record the effective model
		st.StepStartedAt = nowStamp()
		st.PendingQuestion = nil
		x.CurrentStep = stepID
		x.WorkflowStatus = "running"
	})
	d.journal("step", map[string]any{"step": stepID, "phase": "in_progress"})
	sleepMS(80)
	d.commit(func(x *execState) {
		x.step(stepID).Status = "done"
	})
	// a step artifact: proves the capsule chain carries agent-written bytes
	if err := os.MkdirAll("artifacts", 0o755); err == nil {
		_ = os.WriteFile(filepath.Join("artifacts", stepID+".md"),
			[]byte("# "+stepID+"\n\nfake agent output of step "+stepID+"\n"), 0o644)
	}
	d.journal("step", map[string]any{"step": stepID, "phase": "done"})
	sleepMS(60)
}

func (x *execState) step(id string) *execStep {
	st := x.Steps[id]
	if st == nil {
		st = &execStep{Status: "pending", Attempts: []any{}, Answers: []archivedAnswer{}}
		x.Steps[id] = st
	}
	return st
}

// askAndWait persists the open pending_question (BEFORE the flush, the
// idempotent double-source contract), fires the real flush(needs_input)
// over the UDS proxy, then parks until the control loop's answer
// consumption signals this question id. The SAME step continues in
// place once the answer lands (the interactive contract: the step is
// started, parks on its open question, never re-dispatches).
//
// F9 contract behavior (mirroring the executor skill): the question id
// is MINTED ONCE and lives in execution.json — whatever the plan says,
// a pending question already persisted for this step IS the question's
// identity, and a re-derived instance re-presents that id verbatim,
// never a fresh one. An answer that already landed for that id (the
// daemon re-delivers delivered-but-unconsumed answers at attachment,
// and the re-delivery can outrun the plan) is consumed silently, with
// no re-ask.
func (d *driveEnv) askAndWait(qid, question string) {
	if qid == "" {
		qid = "q_anon"
	}
	if question == "" {
		question = "Which shape should the work take: A or B?"
	}
	d.mu.Lock()
	stepID := "interactive"
	if d.exec != nil && d.exec.CurrentStep != "" {
		stepID = d.exec.CurrentStep
	}
	answered := false
	// The once-minted id: a persisted pending question for this step wins
	// over the plan's id (re-present verbatim, never re-mint). A step
	// whose interactive question was already CONSUMED (the answer is
	// archived — possibly by this derivation's own control loop moments
	// after the attach-time re-delivery) is a finished ask: consume
	// without re-asking, whatever id the plan carries.
	if d.exec != nil {
		st := d.exec.Steps[stepID]
		if st != nil && st.PendingQuestion != nil && st.PendingQuestion.ID != "" {
			qid = st.PendingQuestion.ID
			if st.PendingQuestion.Question != "" {
				question = st.PendingQuestion.Question
			}
			for _, a := range st.Answers {
				if a.QuestionID == qid {
					answered = true
				}
			}
		} else if st != nil && len(st.Answers) > 0 {
			qid = st.Answers[len(st.Answers)-1].QuestionID
			answered = true
		}
	}
	var ch chan struct{}
	if !answered {
		// Register the waiter under the SAME lock as the decision, so a
		// concurrent consumeAnswer either lands before the check (seen
		// above) or closes exactly the channel registered here.
		ch = make(chan struct{})
		d.askWait[qid] = ch
	}
	d.mu.Unlock()
	d.journal("ask", map[string]any{
		"question_id": qid, "question": question, "step": stepID,
		"already_answered": answered,
	})
	if answered {
		return
	}
	var stale bool
	d.commit(func(x *execState) {
		// Re-check atomically with the persist: the answer may have been
		// consumed in the window since the check above — then there is
		// nothing to re-present and nothing to park on.
		st := x.step(stepID)
		for _, a := range st.Answers {
			if a.QuestionID == qid {
				stale = true
				return
			}
		}
		x.CurrentStep = stepID
		if st.Status != "in_progress" {
			st.Status = "in_progress"
			st.StepStartedAt = nowStamp()
		}
		st.PendingQuestion = &pendingQuestion{
			ID: qid, Step: stepID, Question: question, AskedAt: nowStamp(),
		}
	})
	if stale {
		d.journal("ask_skip", map[string]any{
			"question_id": qid, "reason": "answer landed before the re-present",
		})
		return
	}
	ack := d.flush("needs_input", map[string]any{
		"question_id": qid, "question": question, "step": stepID,
	})
	d.journal("flush", map[string]any{"reason": "needs_input", "ok": ack})
	<-ch
}

// consumeAnswer is the control-goroutine half: archive the answer and
// clear pending_question in the SAME atomic write, signal the parked
// ask, and only then does the caller ack (the ack is the durability
// signal).
func (d *driveEnv) consumeAnswer(qid, answer string) {
	who := "usr_root"
	now := nowStamp()
	d.commit(func(x *execState) {
		if x.CurrentStep == "" {
			return
		}
		st := x.step(x.CurrentStep)
		if st.PendingQuestion == nil || st.PendingQuestion.ID != qid {
			return // not the waiting question: nothing to consume
		}
		st.Answers = append(st.Answers, archivedAnswer{
			QuestionID: qid, Answer: answer, AnsweredBy: who, AnsweredAt: now,
		})
		st.PendingQuestion = nil
	})
	d.mu.Lock()
	if ch, ok := d.askWait[qid]; ok {
		close(ch)
		delete(d.askWait, qid)
	}
	d.mu.Unlock()
}

// beginHang opens a turn and goes silent: the in-flight silence the hang
// watchdog probes.
func (d *driveEnv) beginHang() {
	d.mu.Lock()
	d.hanging = true
	d.mu.Unlock()
	d.journal("hang", map[string]any{"at": nowStamp()})
	emitOpenTurn()
}

// suppressProbeAck reports whether the plan wants probes ignored.
func (d *driveEnv) suppressProbeAck() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hanging || !d.ackProbe
}

// beginShutdownFlush performs the agent's flush(shutdown) once: commit
// what exists, ack the result, never wait — the graceful-shutdown
// contract (§五). The intent is journaled BEFORE the call: the daemon
// may tear the process down the instant its own push event lands, so
// a post-call journal line would race the teardown.
func (d *driveEnv) beginShutdownFlush() {
	d.shutOnce.Do(func() {
		d.journal("flush_begin", map[string]any{"reason": "shutdown"})
		go func() {
			ack := d.flush("shutdown", nil)
			d.journal("flush", map[string]any{"reason": "shutdown", "ok": ack})
		}()
	})
}

// ---------------------------------------------------------------------------
// flush(reason): the agent→daemon UDS proxy call (ruling C2).

func (d *driveEnv) flushWrap(finalStatus string) {
	ack := d.flush("wrap", map[string]any{"final_status": finalStatus})
	d.journal("flush", map[string]any{"reason": "wrap", "final_status": finalStatus, "ok": ack})
}

// flush delivers the agent→daemon proxy call with a bounded retry: the
// spawn-window races (the daemon's listener, its RECOVERING→RUNNING
// transition, or the monitor's adoption of the recovered instance may
// lag the freshly spawned agent by a moment) answer connection-refused
// or one of the DAEMON-409 transients (a close genuinely in progress, or
// the recovery window's typed "instance just spawned; retry shortly"),
// and a real agent retries its lifeline calls the same way (the v0
// baseline documented this discipline for its own proxy calls).
func (d *driveEnv) flush(reason string, extra map[string]any) bool {
	if d.socket == "" {
		fmt.Fprintln(os.Stderr, "fakepi: PF_DAEMON_SOCKET not set; flush unavailable")
		return false
	}
	body := map[string]any{
		"work_item":  d.wi,
		"attempt_id": d.instance,
		"reason":     reason,
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ok, retry := d.flushOnce(reason, raw)
		if ok {
			return true
		}
		if !retry || time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// flushOnce is one flush round trip. retry reports whether the failure
// is the transient spawn-window shape worth retrying.
func (d *driveEnv) flushOnce(reason string, raw []byte) (ok, retry bool) {
	conn, err := net.Dial("unix", d.socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi: flush(%s): dial: %v\n", reason, err)
		return false, true // the listener may not be up yet
	}
	defer conn.Close()
	req, err := http.NewRequest("POST", "http://daemon/jobs/"+d.wi+"/flush", bytes.NewReader(raw))
	if err != nil {
		return false, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil },
	}}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepi: flush(%s): %v\n", reason, err)
		return false, true
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "fakepi: flush(%s): HTTP %d: %s\n", reason, resp.StatusCode, out)
		// a close/recovery race is transient; a stale attempt is forever.
		// "instance just spawned" is the flushGate's typed RECOVERING
		// transient (the recovery window); "already in progress" is a
		// close genuinely holding the seal guard.
		return false, bytes.Contains(out, []byte("already in progress")) ||
			bytes.Contains(out, []byte("instance just spawned")) ||
			resp.StatusCode >= 500
	}
	var fa struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(out, &fa)
	return fa.OK, false
}

// ---------------------------------------------------------------------------
// execution.json: atomic writes, monotonic Version.

func (d *driveEnv) loadExec() *execState {
	raw, err := os.ReadFile(execPath)
	if err != nil {
		return nil
	}
	var x execState
	if json.Unmarshal(raw, &x) != nil {
		return nil
	}
	if x.Steps == nil {
		x.Steps = map[string]*execStep{}
	}
	return &x
}

// commit applies mut under the lock and lands the state atomically with
// the version bump (one write per mutation — Version strictly increases).
func (d *driveEnv) commit(mut func(*execState)) {
	d.mu.Lock()
	d.exec.Version++
	mut(d.exec)
	d.exec.UpdatedAt = nowStamp()
	raw, err := json.Marshal(d.exec)
	path := execPath
	d.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// appendTranscript lands one JSONL line on the harness transcript — the
// recovery engine's resume discriminator reads this file (exists,
// non-empty, first line parses as a JSON record).
func (d *driveEnv) appendTranscript(rec map[string]any) {
	d.mu.Lock()
	path := d.transcript
	d.mu.Unlock()
	if path == "" {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// journal appends one record to the capsule's assertion ledger.
func (d *driveEnv) journal(kind string, body map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	body = withKind(body, kind)
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func withKind(body map[string]any, kind string) map[string]any {
	if body == nil {
		body = map[string]any{}
	}
	out := make(map[string]any, len(body)+2)
	for k, v := range body {
		out[k] = v
	}
	out["type"] = kind
	out["at"] = nowStamp()
	return out
}

// makeRepo lays a real git worktree under repos/<name> with an origin
// remote — the ship scenario's inventory (the daemon's ship bridge pushes
// this worktree's HEAD to its own origin as the fixed-name branch).
// When an origin carries a main baseline, the worktree descends from it
// (the real-agent shape: clone the repo, work on top) so the origin's
// history and the shipped branch share a root — github's PR API refuses
// to open a PR for a head with no history in common with the base. An
// origin without a main (s04's fresh empty bare repo) keeps today's
// fresh-init worktree.
func (d *driveEnv) makeRepo(name, origin string) {
	if name == "" {
		name = "demo"
	}
	dir := filepath.Join("repos", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakepi: repo %s: git %v: %v: %s\n", name, args, err, out)
			return err
		}
		return nil
	}
	if err := run("init", "-q", "-b", "main"); err != nil {
		return
	}
	if origin != "" {
		_ = run("remote", "add", "origin", origin)
		if run("fetch", "--quiet", "origin", "main") == nil {
			_ = run("checkout", "--quiet", "-B", "main", "FETCH_HEAD")
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "work.txt"), []byte("fake agent repo work for "+d.wi+"\n"), 0o644)
	_ = run("add", "-A")
	_ = run("-c", "user.name=fake-agent", "-c", "user.email=fake-agent@invalid",
		"commit", "-q", "-m", "repo work "+d.wi)
	d.journal("repo", map[string]any{"name": name, "origin": origin})
}
