package test

// v1_s16: F9 — the needs_input question/answer protocol's recovery
// resilience. Two pilot fractures, both now pinned by e2e:
//
//   - A question id is minted ONCE, persisted with pending_question in
//     the same atomic write and re-presented verbatim across
//     re-derivations; a re-derived instance that re-minted (or presented
//     an empty id) stranded every answer already delivered against the
//     old id.
//   - An answer that was delivered but never acked/consumed stays
//     re-deliverable in the daemon's ledger — and since F9 the monitor
//     re-sends it automatically when the next instance attaches.
//
// Scenarios:
//
//   - A: agent asks → owner answers → the answer is delivered but the
//     agent is killed before consuming it → the resumed instance
//     re-presents the SAME id → the answer is re-delivered
//     automatically → consumed and archived → the run wraps.
//   - B: a needs_input flush with an empty question_id is refused
//     DAEMON-400 at the entry (fail-closed: no unanswerable pending).
//   - C: a repeated needs_input flush with the SAME id is idempotent:
//     one pending, one needs_input event, the loop still finishes.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// udsFlushRaw performs one raw flush round trip on the daemon's UDS face
// (the same transport the agent's proxy uses), returning the HTTP status
// and body. questionID == "" sends the field explicitly empty — the F9
// fail-closed shape under test.
func udsFlushRaw(t *testing.T, d *v1Daemon, wi, attempt, questionID string) (int, string) {
	t.Helper()
	conn, err := net.Dial("unix", d.sock)
	if err != nil {
		t.Fatalf("dial %s: %v", d.sock, err)
	}
	defer conn.Close()
	body := map[string]any{
		"work_item":  wi,
		"attempt_id": attempt,
		"reason":     "needs_input",
		"step":       "code",
	}
	// no question text: a bare re-present must not be able to rewrite the
	// recorded question (the same-id path refreshes asked_at only)
	if questionID == "" {
		body["question_id"] = ""
	} else {
		body["question_id"] = questionID
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", "http://daemon/jobs/"+wi+"/flush", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }},
	}).Do(req)
	if err != nil {
		t.Fatalf("flush round trip: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out)
}

// currentInstance reads the newest derivation's instance id (the flush
// fence value) off the capsule's spawn journal.
func currentInstance(t *testing.T, d *v1Daemon, wi string) string {
	t.Helper()
	entries := spawnJournal(t, d.capsuleDir(wi))
	inst := ""
	for _, rec := range entries {
		if s, ok := rec["instance"].(string); ok && s != "" {
			inst = s
		}
	}
	if inst == "" {
		t.Fatalf("no instance id in the spawn journal of %s (%d entries)", wi, len(entries))
	}
	return inst
}

// writePlanExtra rewrites the fake agent's plan file with extra top-level
// fields beyond actions/resume_actions (the first process start reads
// THIS version; later rewrites steer re-derivations only).
func writePlanExtra(t *testing.T, inst *v1Install, wi string, actions, resume []map[string]any, extra map[string]any) {
	t.Helper()
	body := map[string]any{"actions": actions}
	if resume != nil {
		body["resume_actions"] = resume
	}
	for k, v := range extra {
		body[k] = v
	}
	blob, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.planDir, wi+".json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// answerControls extracts the fake agent's pf/answer control landings
// (in journal order) from one capsule.
func answerControls(t *testing.T, capDir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range fakeJournal(t, capDir) {
		if rec["type"] == "control" && rec["method"] == "pf/answer" {
			out = append(out, rec)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Scenario A: delivered-but-unconsumed answer, kill -9, resume, auto
// redelivery, same-id re-present, consumption, wrap.

func TestV1S16AnswerAutoRedeliveryAfterKill9(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_f9a"
	actions := []map[string]any{
		{"do": "step", "id": "code"},
		{"do": "ask", "id": qid, "question": "Which variant should the recovery take?"},
		{"do": "step", "id": "polish"},
		{"do": "wrap"},
	}
	// The resume plan's ask carries a DIFFERENT plan id on purpose: the
	// persisted pending id must win (mint once, re-present verbatim).
	resume := []map[string]any{
		{"do": "ask", "id": "q_plan_must_be_ignored", "question": "Which variant should the recovery take?"},
		{"do": "step", "id": "polish"},
		{"do": "wrap"},
	}
	wi := inst.newWI(t, "s16 A: answer auto-redelivery across a kill -9", actions, resume)
	// The first derivation lands pf/answer but neither consumes nor acks
	// it — the died-mid-consumption shape that leaves the answer
	// delivered-but-unconsumed in the daemon's ledger.
	writePlanExtra(t, inst, wi, actions, resume, map[string]any{"ack_answer": false})

	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		jv := d.observe(t, wi)
		return jv.Pending != nil && jv.Pending.ID == qid
	})

	// The owner answers. The relay is DELIVERED (the write succeeds) but
	// the agent never acks: the CLI settles after the ack window with
	// the delivered-unconsumed outcome, and the ledger keeps the answer
	// re-deliverable. (Bounded by the daemon's ack wait: ~30s.)
	out := d.pf(t, "answer", wi, "--text", "Take the resilient variant.", "--question", qid, "--by", "usr_root")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("answer outcome: %q", out)
	}

	// The journal proves the unacked delivery landed on the dead-walking
	// instance.
	capDir := d.capsuleDir(wi)
	lands := answerControls(t, capDir)
	if len(lands) != 1 || lands[0]["suppressed"] != true || lands[0]["ref"] != qid {
		t.Fatalf("expected exactly one suppressed pf/answer landing for %s, got %+v", qid, lands)
	}

	// Rewrite the plan WITHOUT the suppression: the resume derivation
	// re-reads the plan file at process start (the first derivation
	// already holds its copy in memory, so this rewrite is invisible to
	// it).
	inst.setPlanFull(t, wi, actions, resume)

	// Kill the agent before it could ever consume the answer.
	pid := d.agentPIDOf(t, wi)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}
	waitUntil(t, 30*time.Second, "no needs_recovery after the kill", func() bool {
		return countOf(d.eventKinds(t, wi), "needs_recovery") == 1
	})

	// The successor re-derived; the daemon automatically re-delivered the
	// stranded answer to it. Wait for the consumption to settle, then the
	// run continues (polish, wrap).
	waitForEvent(t, d, wi, "wi_agent_rederived", 30*time.Second)
	waitForEvent(t, d, wi, "answer_redelivery", 30*time.Second)
	waitForEvent(t, d, wi, "answered", 30*time.Second)
	// Diagnostic dump on a stuck wrap (e2e reds must be diagnosable).
	deadline := time.Now().Add(60 * time.Second)
	for inst.wiStatus(t, wi) != "wrapped" {
		if time.Now().After(deadline) {
			t.Logf("events: %v", d.eventKinds(t, wi))
			for _, rec := range fakeJournal(t, capDir) {
				t.Logf("journal: %+v", rec)
			}
			t.Logf("observe: %+v", d.observe(t, wi))
			t.Logf("daemon log:\n%s", d.logTail())
			t.Fatalf("work item %s never wrapped (now %q)", wi, inst.wiStatus(t, wi))
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The fake journal: the re-derived agent asked with the PERSISTED id
	// (the plan's decoy id was ignored) and consumed the re-delivered
	// answer without re-asking.
	var resumedAsk map[string]any
	for _, rec := range fakeJournal(t, capDir) {
		if rec["type"] == "ask" && rec["question_id"] == qid {
			resumedAsk = rec
		}
	}
	if resumedAsk == nil {
		t.Fatal("the resumed derivation never journaled its ask with the persisted id")
	}
	if resumedAsk["already_answered"] != true {
		t.Fatalf("the resumed ask must find the answer already consumed: %+v", resumedAsk)
	}

	// Exactly two pf/answer landings across the whole run: the suppressed
	// one on the dead instance, the consumed one on the successor.
	lands = answerControls(t, capDir)
	if len(lands) != 2 {
		t.Fatalf("pf/answer landings: got %d, want 2 (one suppressed, one auto-redelivered): %+v", len(lands), lands)
	}
	if lands[1]["suppressed"] == true || lands[1]["ref"] != qid {
		t.Fatalf("the auto-redelivered landing must be a normal consumption of %s: %+v", qid, lands[1])
	}

	// The daemon record: ONE needs_input for the question (the same-id
	// re-present never duplicated it), one recovery, one re-derivation,
	// the redelivery attempt, and the answered event.
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "needs_input"); n != 1 {
		t.Fatalf("needs_input fired %d times, want exactly 1 (same-id re-present is idempotent): %v", n, kinds)
	}
	if n := countOf(kinds, "wi_agent_rederived"); n != 1 {
		t.Fatalf("re-derivations: %d, want 1: %v", n, kinds)
	}
	if n := countOf(kinds, "answer_redelivery"); n < 1 {
		t.Fatalf("no answer_redelivery event: %v", kinds)
	}
	if n := countOf(kinds, "answered"); n < 1 {
		t.Fatalf("no answered event: %v", kinds)
	}

	// The sealed chain: the answer is archived, the pending question is
	// gone, and the run continued past it.
	exec := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	if !strings.Contains(exec, "Take the resilient variant.") {
		t.Fatalf("sealed execution.json misses the re-delivered answer:\n%s", exec)
	}
	if strings.Contains(exec, `"pending_question"`) {
		t.Fatalf("sealed execution.json still carries a pending question:\n%s", exec)
	}
	if !strings.Contains(exec, `"polish"`) {
		t.Fatalf("the resumed agent never continued past the question:\n%s", exec)
	}
}

// ---------------------------------------------------------------------------
// Scenario B: empty question_id flush is refused DAEMON-400 fail-closed.

func TestV1S16EmptyQuestionIDFlushRefused(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s16 B: empty question id fail-closed",
		[]map[string]any{{"do": "step", "id": "code"}, {"do": "park"}}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	attempt := currentInstance(t, d, wi)

	// The flush entry refuses the empty id with DAEMON-400 — never the
	// monitor, never a pending.
	status, body := udsFlushRaw(t, d, wi, attempt, "")
	if status != 400 || !strings.Contains(body, "DAEMON-400") {
		t.Fatalf("empty question_id flush: status %d body %s, want 400 DAEMON-400", status, body)
	}

	// Fail-closed, not fail-deaf: no pending was minted, no needs_input
	// event fired, and the agent is still alive and running.
	if jv := d.observe(t, wi); jv.Pending != nil {
		t.Fatalf("the refused flush minted a pending: %+v", jv.Pending)
	}
	if n := countOf(d.eventKinds(t, wi), "needs_input"); n != 0 {
		t.Fatalf("the refused flush fired needs_input %d times", n)
	}
	if pid := d.agentPIDOf(t, wi); pid <= 0 {
		t.Fatal("the agent must survive the refused flush")
	}

	// And the live attempt's own well-formed flush still works.
	status, body = udsFlushRaw(t, d, wi, attempt, "q_f9b_live")
	if status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("live needs_input flush after the refusal: %d %s", status, body)
	}
	waitUntil(t, 10*time.Second, "the live flush never landed", func() bool {
		jv := d.observe(t, wi)
		return jv.Pending != nil && jv.Pending.ID == "q_f9b_live"
	})
}

// ---------------------------------------------------------------------------
// Scenario C: a repeated needs_input flush with the SAME id is
// idempotent — one pending, one needs_input event, the loop finishes.

func TestV1S16DuplicateFlushSameIDIsIdempotent(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_f9c"
	wi := inst.newWI(t, "s16 C: duplicate flush same id",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "ask", "id": qid, "question": "Which shape: one pending or two?"},
			{"do": "step", "id": "polish"},
			{"do": "wrap"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		jv := d.observe(t, wi)
		return jv.Pending != nil && jv.Pending.ID == qid
	})
	attempt := currentInstance(t, d, wi)

	// A second flush with the SAME id — the recovered re-present shape:
	// acked OK, still exactly one pending, still exactly one
	// needs_input event.
	status, body := udsFlushRaw(t, d, wi, attempt, qid)
	if status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("duplicate same-id flush: %d %s", status, body)
	}
	jv := d.observe(t, wi)
	if jv.Pending == nil || jv.Pending.ID != qid {
		t.Fatalf("the duplicate flush must not change the pending: %+v", jv.Pending)
	}
	if jv.Pending.Question == "" || !strings.Contains(jv.Pending.Question, "one pending or two") {
		t.Fatalf("the duplicate flush must not erase the question text: %+v", jv.Pending)
	}
	if n := countOf(d.eventKinds(t, wi), "needs_input"); n != 1 {
		t.Fatalf("needs_input fired %d times, want 1 (same id is idempotent)", n)
	}

	// The loop still finishes normally on the single pending.
	out := d.pf(t, "answer", wi, "--text", "Exactly one pending.", "--question", qid, "--by", "usr_root")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("answer: %q", out)
	}
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
	exec := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	if strings.Contains(exec, `"pending_question"`) {
		t.Fatalf("sealed execution.json still carries a pending question:\n%s", exec)
	}
}
