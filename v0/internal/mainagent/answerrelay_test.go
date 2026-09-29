package mainagent

// answerrelay_test.go: SK-4/CR-3 and SK-6/CR-4 regressions.
//
// SK-4/CR-3: DeliverAnswer must not report success on a bare pipe write.
// The agent's pf/answer ack is the durability signal (pf-execute §12: the
// agent acks only "after the relay is durably recorded"), so an agent that
// dies between the write and its atomic pending_answer persist must
// surface as a DELIVERY FAILURE — never success — or the answer_id
// idempotency (00 §3.4a) turns that crash window into a permanent loss of
// the answer. The failed delivery keeps the answer re-deliverable: the
// same relay, sent to a re-derived agent on the same capsule, succeeds.
//
// SK-6/CR-4: bulk/tool JSON lines that merely carry a "method" field must
// be classified BULK; only the closed pf/* vocabulary (the four methods +
// the exact ack shape of pf-execute §12) may enter the bounded control
// queue. A bulk flood shaped {"method":"status",...} must leave the
// control queue untouched and the agent healthy, with a real pf/quiesce
// ack still arriving.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// TestDeliverAnswerLossWhenAgentDiesBeforeAck: the agent consumes the
// relay off its stdin and dies WITHOUT acking — the supervisor must
// report failure (not success), and the same relay must then succeed
// against a re-derived agent (same capsule, same session id: the crash
// continuation case), proving the answer stayed re-deliverable.
func TestDeliverAnswerLossWhenAgentDiesBeforeAck(t *testing.T) {
	dieBin := writeHarness(t, "dieonanswer", `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *pf/answer*) exit 0 ;;
  esac
done
`)
	s := NewSupervisor(dieBin, dieBin, "")
	p := newSpawnParams(t, capsule.AdapterPi, "sess_ans")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	relay := daemon.AnswerRelay{
		QuestionID: "q_abc123", AnswerID: "a_7k9m2p", AnswerText: "yes",
		AnsweredAt: "2026-09-27T00:00:00Z", AnsweredBy: "u_x",
	}
	if err := h.DeliverAnswer(relay); err == nil {
		t.Fatal("DeliverAnswer must FAIL when the agent dies after the write but before its pf/answer ack (the answer would be lost forever behind the answer_id idempotency)")
	}
	// The failure must not consume the relay: a re-derived agent (crash
	// continuation — same capsule, same session id) receives the SAME
	// answer and acks it.
	ackBin := writeFakeHarness(t, false)
	s2 := NewSupervisor(ackBin, ackBin, "")
	p2 := newSpawnParams(t, capsule.AdapterPi, "sess_ans")
	p2.CapsuleDir = p.CapsuleDir
	h2, err := s2.Spawn(context.Background(), p2)
	if err != nil {
		t.Fatalf("re-derivation for redelivery must be allowed: %v", err)
	}
	defer h2.Terminate(2 * time.Second)
	if err := h2.DeliverAnswer(relay); err != nil {
		t.Fatalf("the answer must be re-deliverable to the re-derived agent: %v", err)
	}
}

// TestBulkMethodLinesAreBulk: N bulk records whose assistant text carries
// {"method":"status",...} (a plausible tool/progress vocabulary) must
// never enter the control queue — the agent stays healthy and a real
// pf/quiesce ack still arrives behind the flood.
func TestBulkMethodLinesAreBulk(t *testing.T) {
	bin := writeHarness(t, "bulkmethodharness", `#!/bin/sh
i=0
while [ $i -lt 3000 ]; do
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"status\",\"progress\":\"bulk\"}"}]}}'
  i=$((i+1))
done
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/quiesce\",\"result\":{\"ack\":\"pf/quiesce\"}}"}]}}'
      ;;
  esac
done
`)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterPi, "sess_bulkmethod")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	// Give the reader a moment to classify the flood, then freeze: the
	// ack must arrive — the control queue was never touched by the bulk
	// lines, so the agent is healthy.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("a bulk {\"method\":...} flood must not break the control path: %v", err)
	}
	if proc.Unhealthy() {
		t.Fatal("bulk lines carrying a method field must never mark the agent unhealthy")
	}
	if d := atomic.LoadInt64(&proc.bulkDropped); d < 3000 {
		t.Fatalf("the bulk lines must be counted as dropped bulk output, got %d", d)
	}
}

// TestIsControlLineClosedSet: the classifier admits ONLY the exact ack
// shape — {"method":X,"result":{"ack":X}}, X one of the four pf/*
// methods — and nothing else (fix round 2: the previous classifier also
// admitted any line whose method field named one of the four, letting bulk
// {"method":"pf/quiesce","progress":"bulk"} lines flood the bounded
// control queue and spoof acks).
func TestIsControlLineClosedSet(t *testing.T) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"pf/answer","result":{"ack":"pf/answer"}}`,
		`{"method":"pf/quiesce","result":{"ack":"pf/quiesce"}}`,
		`{"method":"pf/resume","result":{"ack":"pf/resume"}}`,
		`{"method":"pf/stop_notice","result":{"ack":"pf/stop_notice"},"id":7}`,
	} {
		if !isControlLine(line) {
			t.Errorf("exact-ack-shape control line must classify as control: %s", line)
		}
	}
	for _, line := range []string{
		// Bulk lines carrying EACH of the four method names — the spoof
		// shapes that previously flooded the control queue.
		`{"method":"pf/quiesce","progress":"bulk"}`,
		`{"method":"pf/resume","progress":"bulk"}`,
		`{"method":"pf/stop_notice","progress":"bulk"}`,
		`{"method":"pf/answer","progress":"bulk"}`,
		// Method-only lines (no result.ack): the agent never initiates
		// control traffic, these are bulk or spoof, never acks.
		`{"method":"pf/quiesce"}`,
		`{"method":"pf/resume"}`,
		`{"method":"pf/stop_notice","id":7}`,
		`{"method":"pf/answer"}`,
		// Ack result without the method echo, or echoing a different
		// method: not the two-sided ack shape.
		`{"result":{"ack":"pf/quiesce"}}`,
		`{"method":"pf/quiesce","result":{"ack":"pf/answer"}}`,
		`{"method":"status","progress":1}`,
		`{"method":"tool_call","params":{}}`,
		`{"method":"noise"}`,
		`{"method":"user/prompt","result":{"ack":"user/prompt"}}`,
		`{"result":{"ack":"nope"}}`,
		`{"result":{"progress":0.5}}`,
		`{"id":1,"result":"ok"}`,
		`plain text`,
		`{not json`,
	} {
		if isControlLine(line) {
			t.Errorf("non-ack-shape line must classify as BULK: %s", line)
		}
	}
}

// TestBulkMethodSpoofLinesNeverSatisfyAckNorFloodQueue (F2-2): a noisy
// harness emitting assistant messages whose text carries EACH of the four
// control method names (without the exact ack shape) must not spoof
// Quiesce/Resume/NotifyStop/DeliverAnswer success — the pending ack is
// satisfied only by the exact ack shape — and must not flood the control
// queue: the extracted lines are classified BULK, the agent stays healthy,
// and a genuine ack still arrives behind the flood.
func TestBulkMethodSpoofLinesNeverSatisfyAckNorFloodQueue(t *testing.T) {
	bin := writeHarness(t, "methodspoofharness", `#!/bin/sh
i=0
while [ $i -lt 2000 ]; do
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/quiesce\",\"progress\":\"bulk\"}"}]}}'
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/resume\",\"progress\":\"bulk\"}"}]}}'
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/stop_notice\",\"progress\":\"bulk\"}"}]}}'
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/answer\",\"progress\":\"bulk\"}"}]}}'
  i=$((i+1))
done
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)
      printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
      printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/quiesce\",\"result\":{\"ack\":\"pf/quiesce\"}}"}]}}'
      ;;
  esac
done
`)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterPi, "sess_spoof")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	// Give the reader a moment to classify the flood, then freeze. The
	// spoof lines must not have satisfied the pending quiesce ack NOR
	// flooded the control queue (no overflow, no unhealthy marker): the
	// genuine ack — emitted only after the harness sees the pf/quiesce
	// request — still arrives and satisfies it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("a bulk {\"method\":\"pf/*\"} flood must neither spoof the ack nor break the control path: %v", err)
	}
	if proc.Unhealthy() {
		t.Fatal("bulk method-spoof lines must never mark the agent unhealthy (they never enter the control queue)")
	}
	if d := atomic.LoadInt64(&proc.bulkDropped); d < 8000 {
		t.Fatalf("every spoof line must be counted as dropped bulk output, got %d", d)
	}
}

// TestBulkMethodSpoofNeverSatisfiesPendingAck (F2-2, negative half): the
// same flood WITHOUT any genuine ack — under the old classifier the
// spoofed {"method":"pf/quiesce"} line satisfied awaitAck and Quiesce
// returned "success" although the agent never acked. Now the wait must
// run to its deadline and fail; the agent stays healthy.
func TestBulkMethodSpoofNeverSatisfiesPendingAck(t *testing.T) {
	bin := writeHarness(t, "spoofnoackharness", `#!/bin/sh
i=0
while [ $i -lt 2000 ]; do
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/quiesce\",\"progress\":\"bulk\"}"}]}}'
  i=$((i+1))
done
sleep 300
`)
	s := NewSupervisor(bin, bin, "")
	p := newSpawnParams(t, capsule.AdapterPi, "sess_spoofno")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := h.Quiesce(ctx); err == nil {
		t.Fatal("a bulk {\"method\":\"pf/quiesce\"} line must NOT satisfy the pending ack — only the exact {method,result.ack} shape may")
	}
	if proc.Unhealthy() {
		t.Fatal("the unsatisfied ack wait is a timeout, not an unhealthy agent — the flood stayed in the bulk path")
	}
}
