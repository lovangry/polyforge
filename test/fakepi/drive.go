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
	d.plan = pl
	return d
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
func (d *driveEnv) askAndWait(qid, question string) {
	if qid == "" {
		qid = "q_anon"
	}
	if question == "" {
		question = "Which shape should the work take: A or B?"
	}
	stepID := ""
	d.mu.Lock()
	if d.exec != nil && d.exec.CurrentStep != "" {
		stepID = d.exec.CurrentStep
	} else {
		stepID = "interactive"
	}
	d.mu.Unlock()
	d.commit(func(x *execState) {
		x.CurrentStep = stepID
		st := x.step(stepID)
		if st.Status != "in_progress" {
			st.Status = "in_progress"
			st.StepStartedAt = nowStamp()
		}
		st.PendingQuestion = &pendingQuestion{
			ID: qid, Step: stepID, Question: question, AskedAt: nowStamp(),
		}
	})
	d.mu.Lock()
	ch := make(chan struct{})
	d.askWait[qid] = ch
	d.mu.Unlock()
	d.journal("ask", map[string]any{"question_id": qid, "question": question, "step": stepID})
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
// spawn-window races (the daemon's listener or its RECOVERING→RUNNING
// transition may lag the freshly spawned agent by a moment) answer
// connection-refused / DAEMON-409 transiently, and a real agent retries
// its lifeline calls the same way (the v0 baseline documented this
// discipline for its own proxy calls).
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
		// a close/recovery race is transient; a stale attempt is forever
		return false, bytes.Contains(out, []byte("already in progress")) ||
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
	_ = os.WriteFile(filepath.Join(dir, "work.txt"), []byte("fake agent repo work for "+d.wi+"\n"), 0o644)
	_ = run("add", "-A")
	_ = run("-c", "user.name=fake-agent", "-c", "user.email=fake-agent@invalid",
		"commit", "-q", "-m", "repo work "+d.wi)
	if origin != "" {
		_ = run("remote", "add", "origin", origin)
	}
	d.journal("repo", map[string]any{"name": name, "origin": origin})
}
