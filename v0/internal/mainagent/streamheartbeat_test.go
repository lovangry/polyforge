package mainagent

// streamheartbeat_test.go: the polyforge#48 supervisor-side regressions,
// driven at the same level of realism as the S15/S16 usage scenarios —
// real child processes speaking the REAL pi RPC record shapes, plus the
// PINNED capture fixtures taken from the live harness on the target box.
//
//   - the pinned record-shape regression (the fixtures ARE the contract);
//   - the heartbeat: which records advance it, which open/close a turn,
//     and why `response` must not count;
//   - the infra-incident classification against the REAL 401 capture
//     (there is no type:"error" record — the whole point);
//   - the slow answer consumer: a 3-"minute"-scale delayed ack is
//     DELIVERED, not error-grade, and the late consumption lands;
//   - the confirmation probe: an ack cancels the kill, silence confirms.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// ---------------------------------------------------------------------------
// (7) The pinned real-harness record-shape regression.

// pinnedTurnShape is the record sequence ONE prompt turn produces, in
// order, as captured live from `pi --mode rpc` (pi 0.87.1) on the target
// box and re-verified by a second independent run. The classifier in
// streamheartbeat.go is written against exactly these names; if a
// harness upgrade renames or reorders them, the heartbeat silently stops
// measuring anything and the hang watchdog silently stops firing. That
// failure mode is invisible in production, so it is pinned here.
var pinnedTurnShape = []string{
	"response",      // our command echo (NOT a heartbeat record)
	"agent_start",   // RUN OPEN
	"turn_start",    // TURN OPEN
	"message_start", // system
	"message_end",
	"message_start", // user
	"message_end",
	"message_start",  // assistant
	"message_update", // text_start
	"message_update", // text_delta
	"message_update", // text_end
	"message_end",    // assistant, authoritative
	"turn_end",       // TURN CLOSE
	"agent_end",      // RUN CLOSE
	"agent_settled",
}

// pinnedErrorTurnShape is the SAME turn when the provider refuses (a
// live 401). It is byte-shape identical to the success capture minus the
// message_update deltas: there is no extra record type for the failure.
var pinnedErrorTurnShape = []string{
	"response",
	"agent_start",
	"turn_start",
	"message_start", // system
	"message_end",
	"message_start", // user
	"message_end",
	"message_start", // assistant, stopReason=error
	"message_end",   // assistant, stopReason=error
	"turn_end",      // stopReason=error
	"agent_end",
	"agent_settled",
}

func readFixture(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("pinned capture %s unreadable: %v", name, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func recordTypes(t *testing.T, lines []string) []string {
	t.Helper()
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		var r stdoutRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("every captured line must be a protocol record: %v", err)
		}
		if r.Type == "" {
			t.Fatalf("every captured record must carry a type: %s", truncFor(l))
		}
		out = append(out, r.Type)
	}
	return out
}

func truncFor(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// TestPinnedPiRecordShapes: the captured real streams still have the
// shape the classifier is written against.
func TestPinnedPiRecordShapes(t *testing.T) {
	for _, tc := range []struct {
		file string
		want []string
	}{
		{"pi_rpc_turn_normal.jsonl", pinnedTurnShape},
		{"pi_rpc_turn_provider_error.jsonl", pinnedErrorTurnShape},
	} {
		got := recordTypes(t, readFixture(t, tc.file))
		if len(got) != len(tc.want) {
			t.Fatalf("%s: record count drifted: got %d %v, pinned %d %v", tc.file, len(got), got, len(tc.want), tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: record %d drifted: got %q, pinned %q (full: %v)", tc.file, i, got[i], tc.want[i], got)
			}
		}
	}
	// The lifecycle names the heartbeat branches on must all be present
	// in the real capture — a constant nothing emits is a dead branch.
	normal := recordTypes(t, readFixture(t, "pi_rpc_turn_normal.jsonl"))
	for _, must := range []string{recResponse, recAgentStart, recTurnStart, recTurnEnd, recAgentEnd, recAgentSettled} {
		found := false
		for _, g := range normal {
			if g == must {
				found = true
			}
		}
		if !found {
			t.Errorf("lifecycle record %q is not in the real capture: the heartbeat branches on a name the harness never emits", must)
		}
	}
}

// TestPinnedProviderErrorCarrier: the ACCURACY finding, pinned. pi emits
// no type:"error" record and no pi-side result record; the failure rides
// message.stopReason=="error" + message.errorMessage. A classifier
// written against an assumed record type would match nothing here, which
// is exactly the silent-dead-signal failure this test exists to prevent.
func TestPinnedProviderErrorCarrier(t *testing.T) {
	lines := readFixture(t, "pi_rpc_turn_provider_error.jsonl")
	var sawErrorType, sawResultRecord bool
	carriers := map[string]string{}
	for _, l := range lines {
		var r stdoutRecord
		if json.Unmarshal([]byte(l), &r) != nil {
			continue
		}
		if r.Type == "error" {
			sawErrorType = true
		}
		if r.Type == "result" {
			sawResultRecord = true
		}
		if len(r.Message) == 0 {
			continue
		}
		var ec errorCarrier
		if json.Unmarshal(r.Message, &ec) != nil || ec.StopReason != "error" {
			continue
		}
		carriers[r.Type] = ec.ErrorMessage
	}
	if sawErrorType || sawResultRecord {
		t.Fatalf("the capture grew a type:error / result record: re-derive the classifier against the new shape (error=%v result=%v)", sawErrorType, sawResultRecord)
	}
	for _, want := range []string{"message_start", "message_end", "turn_end"} {
		if carriers[want] == "" {
			t.Errorf("the provider failure must ride %s as stopReason=error + errorMessage; got carriers %v", want, carriers)
		}
	}
	if !strings.Contains(carriers["turn_end"], "401") {
		t.Errorf("the captured errorMessage must carry the provider's own text, got %q", carriers["turn_end"])
	}
}

// ---------------------------------------------------------------------------
// (heartbeat unit level, fed by the REAL captures)

// feedFixture runs one captured stream through the real classifier of a
// bare Proc and returns it.
func feedFixture(t *testing.T, name string) *Proc {
	t.Helper()
	p := &Proc{ctrl: newLineQueue(), stream: &streamState{}}
	for _, line := range readFixture(t, name) {
		p.classifyStdoutLine(line)
	}
	return p
}

// TestHeartbeatOverRealNormalTurn: the captured healthy turn advances
// the heartbeat and leaves NO turn in flight (agent_end/agent_settled
// closed what agent_start/turn_start opened). A leaked in-flight flag
// would make every idle agent look hung.
func TestHeartbeatOverRealNormalTurn(t *testing.T) {
	p := feedFixture(t, "pi_rpc_turn_normal.jsonl")
	hb := p.StreamHeartbeat()
	if !hb.Observable {
		t.Fatal("a supervised Proc must expose an observable stream")
	}
	if hb.LastRecordAt.IsZero() {
		t.Fatal("the captured turn must advance the heartbeat")
	}
	if hb.InFlight {
		t.Fatal("a COMPLETED turn must leave no turn in flight (agent_end/turn_end close what agent_start/turn_start opened)")
	}
	// 15 captured records, one of which is the `response` echo.
	if hb.Records != 14 {
		t.Fatalf("the response echo must NOT count as a heartbeat record: want 14 of 15, got %d", hb.Records)
	}
}

// TestHeartbeatResponseEchoIsNotProgress: the daemon's OWN command echo
// must not reset the window it is measuring. This is the property that
// keeps the confirmation probe honest — a harness wedged in an upstream
// call still answers its RPC layer instantly.
func TestHeartbeatResponseEchoIsNotProgress(t *testing.T) {
	p := &Proc{ctrl: newLineQueue(), stream: &streamState{}}
	p.classifyStdoutLine(`{"type":"turn_start"}`)
	hb1 := p.StreamHeartbeat()
	if !hb1.InFlight {
		t.Fatal("turn_start must open a turn")
	}
	time.Sleep(20 * time.Millisecond)
	p.classifyStdoutLine(`{"id":"pf-9","type":"response","command":"prompt","success":true}`)
	hb2 := p.StreamHeartbeat()
	if !hb2.LastRecordAt.Equal(hb1.LastRecordAt) {
		t.Fatal("a response echo must NOT advance the heartbeat: a harness whose model call is wedged still answers its RPC layer, which is the false negative polyforge#48 is about")
	}
	if !hb2.LastAnyRecordAt.After(hb1.LastAnyRecordAt) {
		t.Fatal("the response echo must still be recorded as auxiliary evidence")
	}
	// Silence while in flight is the hang criterion.
	if !hb2.Silent(time.Now().Add(time.Hour), time.Minute) {
		t.Fatal("an in-flight stream silent past the window must be Silent()")
	}
	// Closing the turn makes the same silence legal.
	p.classifyStdoutLine(`{"type":"turn_end","message":{"role":"assistant","content":[]}}`)
	if p.StreamHeartbeat().Silent(time.Now().Add(time.Hour), time.Minute) {
		t.Fatal("silence with NO turn in flight is an idle agent, never a hang")
	}
}

// TestInFlightNestedRunAndTurn: agent_start wraps turn_start in the real
// stream, so the depth must survive the inner close and only clear when
// the run ends. A bool would have cleared on turn_end and declared every
// between-turns moment idle.
func TestInFlightNestedRunAndTurn(t *testing.T) {
	p := &Proc{ctrl: newLineQueue(), stream: &streamState{}}
	p.classifyStdoutLine(`{"type":"agent_start"}`)
	p.classifyStdoutLine(`{"type":"turn_start"}`)
	p.classifyStdoutLine(`{"type":"turn_end","message":{"role":"assistant","content":[]}}`)
	if !p.StreamHeartbeat().InFlight {
		t.Fatal("the run is still open after its inner turn closed")
	}
	p.classifyStdoutLine(`{"type":"agent_end","messages":[]}`)
	if p.StreamHeartbeat().InFlight {
		t.Fatal("agent_end closes the run")
	}
	// A stray close must not drive the depth negative and wedge the flag.
	p.classifyStdoutLine(`{"type":"agent_settled"}`)
	p.classifyStdoutLine(`{"type":"turn_start"}`)
	if !p.StreamHeartbeat().InFlight {
		t.Fatal("a stray close must saturate at zero, not wedge the flag below it")
	}
}

// ---------------------------------------------------------------------------
// (4) Error record ⇒ instant infra incident.

// TestProviderErrorBecomesInfraIncident: feeding the REAL 401 capture
// through the classifier must produce exactly ONE incident carrying the
// provider's own text, marked fatal because the turn ended on it. Before
// polyforge#48 this whole capture was bulk.
func TestProviderErrorBecomesInfraIncident(t *testing.T) {
	p := feedFixture(t, "pi_rpc_turn_provider_error.jsonl")
	got := p.DrainStreamIncidents()
	if len(got) != 1 {
		t.Fatalf("one provider failure must surface as exactly ONE incident (the same errorMessage rides three records): got %d %+v", len(got), got)
	}
	in := got[0]
	if in.Kind != daemon.IncidentProviderError {
		t.Errorf("a 401 is a provider error, got kind %q", in.Kind)
	}
	if !strings.Contains(in.Detail, "401") {
		t.Errorf("the incident must carry the provider's own text, got %q", in.Detail)
	}
	if !in.Fatal {
		t.Error("the turn ENDED on this error (turn_end carried stopReason=error): the incident must be fatal")
	}
	if rest := p.DrainStreamIncidents(); len(rest) != 0 {
		t.Errorf("drain must empty the queue, got %d more", len(rest))
	}
	// A healthy capture produces none.
	if n := len(feedFixture(t, "pi_rpc_turn_normal.jsonl").DrainStreamIncidents()); n != 0 {
		t.Errorf("a successful turn must produce no incident, got %d", n)
	}
}

// TestRetryIncidentKind: a retry report is separated from a terminal
// provider error so an operator can tell "still trying" from "dead".
func TestRetryIncidentKind(t *testing.T) {
	p := &Proc{ctrl: newLineQueue(), stream: &streamState{}}
	p.classifyStdoutLine(`{"type":"turn_start"}`)
	p.classifyStdoutLine(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"upstream rate limit; retrying in 5s"}}`)
	got := p.DrainStreamIncidents()
	if len(got) != 1 || got[0].Kind != daemon.IncidentRetry {
		t.Fatalf("a retry report must classify as %q, got %+v", daemon.IncidentRetry, got)
	}
	if got[0].Fatal {
		t.Error("a mid-turn retry report is not fatal: the turn has not ended")
	}
}

// TestIncidentQueueBounded: a provider failing on every record cannot
// grow the queue without bound, and the drop is reported rather than
// hidden.
func TestIncidentQueueBounded(t *testing.T) {
	p := &Proc{ctrl: newLineQueue(), stream: &streamState{}}
	for i := 0; i < maxStreamIncidents*3; i++ {
		// A NEW turn each time, so the per-turn dedup does not collapse them.
		p.classifyStdoutLine(`{"type":"turn_start"}`)
		p.classifyStdoutLine(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"boom ` + string(rune('a'+i%26)) + `"}}`)
	}
	got := p.DrainStreamIncidents()
	if len(got) > maxStreamIncidents+1 {
		t.Fatalf("the incident queue must stay bounded at %d (+1 drop report), got %d", maxStreamIncidents, len(got))
	}
	last := got[len(got)-1]
	if !strings.Contains(last.Detail, "dropped") {
		t.Errorf("the drop must be REPORTED, never silent; last incident: %q", last.Detail)
	}
}

// ---------------------------------------------------------------------------
// (5) The slow answer consumer.

// TestSlowAnswerConsumerIsDeliveredNotFailed reproduces the pilot of
// 2026-09-29 01:10 at fake-harness scale: the agent takes a whole turn
// to consume the answer and acks well after the OLD 30-second window.
// The delivery must be reported as DELIVERED with consumption pending —
// never as an error — and the late consumption must actually land.
//
// The real delay was 45-90 seconds against a 30s window; the test keeps
// the RATIO and shrinks the clock (ack after ~3x the window under test)
// so it runs in under a second without weakening what is asserted.
func TestSlowAnswerConsumerIsDeliveredNotFailed(t *testing.T) {
	consumed := filepath.Join(t.TempDir(), "consumed.log")
	// A harness that records the relay immediately (the pending_answer
	// persist) but only acks a long turn later.
	slowBin := writeHarness(t, "slowconsumer", `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
  case "$line" in
    *pf/answer*)
      printf '%s\n' '{"type":"turn_start"}'
      printf 'answer-received\n' >> `+consumed+`
      sleep 0.6
      printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"pf/answer\",\"result\":{\"ack\":\"pf/answer\"}}"}]}}'
      printf 'answer-consumed\n' >> `+consumed+`
      printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","content":[]}}'
      ;;
  esac
done
`)
	s := NewSupervisor(slowBin, slowBin, "")
	p := newSpawnParams(t, capsule.AdapterPi, "sess_slowans")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Terminate(2 * time.Second) })
	proc := h.(*Proc)

	relay := daemon.AnswerRelay{
		QuestionID: "q_slow01", AnswerID: "a_slow01", AnswerText: "take your time",
		AnsweredAt: "2026-09-29T01:10:00Z", AnsweredBy: "u_ops",
	}

	// Phase A — the window is SHORTER than the agent's turn: the pilot's
	// exact shape. It must report ack-pending, NOT an error.
	saved := answerAckBound
	answerAckBound = 150 * time.Millisecond
	err = proc.DeliverAnswer(relay)
	answerAckBound = saved
	if err == nil {
		t.Fatal("precondition: the ack must not have arrived yet at this window")
	}
	if !isAckPending(err) {
		t.Fatalf("an ack-window expiry must report DELIVERED/consumption-pending, never a delivery failure: got %#v", err)
	}
	// Delivery really happened: the agent has the relay in hand.
	if !waitFileContains(consumed, "answer-received", 2*time.Second) {
		t.Fatal("phase one (delivery) must be real: the agent received the relay")
	}
	// Phase B — the late consumption lands anyway (the pilot's 45-90s
	// tail): the agent finishes its turn and acks.
	if !waitFileContains(consumed, "answer-consumed", 3*time.Second) {
		t.Fatal("the late consumption must still land: a pending ack is not a lost answer")
	}

	// And with a turn-scale window the SAME agent acks inside it — the
	// #49 fix proper, not merely a relabelled timeout.
	relay2 := relay
	relay2.AnswerID, relay2.QuestionID = "a_slow02", "q_slow02"
	if err := proc.DeliverAnswer(relay2); err != nil {
		t.Fatalf("at the turn-scale default window the ack must arrive: %v", err)
	}
}

// isAckPending reports the two-phase pending sentinel.
func isAckPending(err error) bool {
	return err == daemon.ErrAnswerAckPending
}

// waitFileContains polls path for want until the bound expires.
func waitFileContains(path, want string, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestDeliverAnswerDeadAgentStaysAFailure: the polyforge#48 relabelling
// must NOT swallow a real delivery failure. An agent that dies without
// acking is still an error (answerrelay_test.go's SK-4/CR-3 invariant),
// distinguishable from the pending case.
func TestDeliverAnswerDeadAgentStaysAFailure(t *testing.T) {
	dieBin := writeHarness(t, "dieonanswer48", `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *pf/answer*) exit 0 ;;
  esac
done
`)
	s := NewSupervisor(dieBin, dieBin, "")
	p := newSpawnParams(t, capsule.AdapterPi, "sess_deadans")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	err = h.DeliverAnswer(daemon.AnswerRelay{
		QuestionID: "q_d1", AnswerID: "a_d1", AnswerText: "x",
		AnsweredAt: "2026-09-29T01:00:00Z", AnsweredBy: "u_x",
	})
	if err == nil {
		t.Fatal("a dead agent is a DELIVERY FAILURE, not a pending consumption")
	}
	if isAckPending(err) {
		t.Fatalf("a dead agent must never be reported as consumption-pending: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The confirmation probe (supervisor half).

// TestProbeLivenessAckAndSilence: the probe rides the ordinary control
// channel and its ack is the agent's. A harness that acks answers nil
// (the kill is cancelled); one that echoes the RPC response but never
// acks fails — which is precisely the wedged-upstream shape.
func TestProbeLivenessAckAndSilence(t *testing.T) {
	ackBin := writeHarness(t, "probeack", `#!/bin/sh
`+rpcHarnessBodyWithProbe)
	s := NewSupervisor(ackBin, ackBin, "")
	h, err := s.Spawn(context.Background(), newSpawnParams(t, capsule.AdapterPi, "sess_probe1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Terminate(2 * time.Second) })
	lp, ok := h.(daemon.AgentLivenessProbe)
	if !ok {
		t.Fatal("a supervised Proc must expose the confirmation-probe face")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lp.ProbeLiveness(ctx); err != nil {
		t.Fatalf("an agent that acks the probe must cancel the kill: %v", err)
	}

	// The wedged shape: the RPC layer answers (response record) but no
	// agent turn ever completes, so no ack. Accepting the transport echo
	// would rebuild the false negative.
	wedgedBin := writeHarness(t, "probewedged", `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
done
`)
	s2 := NewSupervisor(wedgedBin, wedgedBin, "")
	h2, err := s2.Spawn(context.Background(), newSpawnParams(t, capsule.AdapterPi, "sess_probe2"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h2.Terminate(2 * time.Second) })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel2()
	if err := h2.(daemon.AgentLivenessProbe).ProbeLiveness(ctx2); err == nil {
		t.Fatal("a harness that answers its RPC layer but never completes a turn must NOT satisfy the confirmation probe")
	}
}

// rpcHarnessBodyWithProbe is rpcHarnessBody plus the pf/probe ack — the
// two-sided contract's fifth method.
const rpcHarnessBodyWithProbe = `emit_resp() {
  printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$1"
}
ack_turn() {
  printf '%s\n' '{"type":"turn_start"}'
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"method\":\"'$1'\",\"result\":{\"ack\":\"'$1'\"}}"}]}}'
  printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","content":[]}}'
}
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/probe*)   emit_resp "$id"; ack_turn pf/probe ;;
    *pf/answer*)  emit_resp "$id"; ack_turn pf/answer ;;
    *)            emit_resp "$id" ;;
  esac
done
`
