package test

import (
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// v1_s02: the needs_input full loop. The agent persists the pending
// question, flush(needs_input)s over the UDS proxy, the CLI observes
// the pending question, the human answers through the real CLI, the
// agent consumes the pf/answer (archiving it in execution.json BEFORE
// acking — the ack carries the question id in "ref", the v1 correlation
// contract), continues its step and wraps. Every leg is asserted.
func TestV1S02NeedsInputLoop(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_v1s02"
	wi := inst.newWI(t, "s02: needs_input round trip",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "ask", "id": qid, "question": "Which design should the code step take: lean or compatible?"},
			{"do": "step", "id": "polish"},
			{"do": "wrap"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)

	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})

	// The CLI observe face shows the pending question and the
	// needs_input status.
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		jv := d.observe(t, wi)
		return jv.Status == protocol.JobNeedsInput && jv.Pending != nil && jv.Pending.ID == qid
	})
	jv := d.observe(t, wi)
	if !strings.Contains(jv.Pending.Question, "lean or compatible") {
		t.Fatalf("pending question text wrong: %+v", jv.Pending)
	}

	// The human answers through the real CLI. The answer's five-field
	// relay must reach the agent, whose ack echoes the question id (ref).
	out := d.pf(t, "answer", wi, "--text", "Take the lean variant.", "--question", qid, "--by", "usr_root")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("answer answer: %q", out)
	}

	// The agent consumed it: the pending projection cleared and the run
	// continued to its terminal state.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// The consumption contract: the answer is archived in execution.json
	// (same atomic write that cleared pending_question), and the sealed
	// chain carries it server-side.
	exec := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	if !strings.Contains(exec, "Take the lean variant.") {
		t.Fatalf("sealed execution.json misses the answer:\n%s", exec)
	}
	if strings.Contains(exec, `"pending_question"`) {
		t.Fatalf("sealed execution.json still carries a pending question:\n%s", exec)
	}
	if !strings.Contains(exec, `"polish"`) || !strings.Contains(exec, `"done"`) {
		t.Fatalf("agent did not continue past the question:\n%s", exec)
	}

	// The fake agent's journal: the pf/answer landed with the right
	// correlation id and the ack it sent echoed that id in "ref".
	capDir := d.capsuleDir(wi)
	var answerControl map[string]any
	for _, rec := range fakeJournal(t, capDir) {
		if rec["type"] == "control" && rec["method"] == "pf/answer" {
			answerControl = rec
		}
	}
	if answerControl == nil {
		t.Fatal("fake journal has no pf/answer control landing")
	}
	if answerControl["ref"] != qid {
		t.Fatalf("pf/answer correlation wrong: %+v", answerControl)
	}

	// The daemon events tell the loop: needs_input then answered.
	kinds := d.eventKinds(t, wi)
	if !contains(kinds, "needs_input") || !contains(kinds, "answered") {
		t.Fatalf("events miss the needs_input loop: %v", kinds)
	}
}
