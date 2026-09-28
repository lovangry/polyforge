package mainagent

// Supervisor + adapter tests against a scripted fake harness binary: the
// full stdio protocol (initial injection, quiesce ack, stop notice, answer
// relay), process-group termination, session id reuse rules. The fake
// harness speaks the REAL pi RPC wire protocol — prompt commands in,
// response records + assistant-message event bursts (with the embedded ack
// line) out — the same shapes test/fakepi reproduces for the dogfood
// scenarios.

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// rpcHarnessBody is the shared body of the REAL-protocol fake harnesses:
// a prompt-command loop that answers every command with a response record
// (echoing its id) and acks each pf/* control method with a REAL event
// burst whose multi-line assistant message embeds the ack JSON line — the
// exact shapes the supervisor's record-stream classifier consumes.
// Non-control prompts (the initial pf-execute instruction) are answered
// with the response record only.
const rpcHarnessBody = `emit_resp() {
  printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$1"
}
ack_turn() {
  printf '%s\n' '{"type":"agent_start"}'
  printf '%s\n' '{"type":"message_start","message":{"role":"assistant","content":[]}}'
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"at a safe boundary now\n{\"method\":\"'$1'\",\"result\":{\"ack\":\"'$1'\"}}\ncontinuing the step"}]}}'
  printf '%s\n' '{"type":"agent_settled"}'
}
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *pf/quiesce*)     emit_resp "$id"; ack_turn pf/quiesce ;;
    *pf/resume*)      emit_resp "$id"; ack_turn pf/resume ;;
    *pf/stop_notice*) emit_resp "$id"; ack_turn pf/stop_notice ;;
    *pf/answer*)      emit_resp "$id"; ack_turn pf/answer ;;
    *)                emit_resp "$id" ;;
  esac
done
`

// writeFakeHarness writes a REAL-protocol shell harness: answers every
// prompt command with a response record and acks every pf/* control method
// via an assistant-message event burst. leaveGrandchild starts a sleeping
// child that ignores nothing (the group-kill drill needs a second member).
func writeFakeHarness(t *testing.T, leaveGrandchild bool) string {
	t.Helper()
	script := "#!/bin/sh\n"
	if leaveGrandchild {
		script += "sh -c 'sleep 300' &\n"
	}
	script += rpcHarnessBody
	return writeHarness(t, "fakeharness", script)
}

func newSpawnParams(t *testing.T, adapter, sessionID string) daemon.SpawnParams {
	t.Helper()
	c, _, err := capsule.Create(t.TempDir(), "wi_ag01", 3, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureHarnessDir(adapter); err != nil {
		t.Fatal(err)
	}
	return daemon.SpawnParams{
		WorkItemID: "wi_ag01", CapsuleDir: c.Dir, Generation: 3,
		SessionID: sessionID, Adapter: adapter, InitialPrompt: "# pf-execute\nrun the workflow",
	}
}

func TestSupervisorSpawnInjectQuiesceAnswer(t *testing.T) {
	bin := writeFakeHarness(t, false)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_wi_ag01_3")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Terminate(2 * time.Second) })
	if h.PID() <= 0 {
		t.Fatal("process must have a pid")
	}
	if !h.Alive() {
		t.Fatal("agent must be alive after spawn")
	}
	// Freeze face: quiesce acks within the bounded window.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	// Stop-advancing notice (graceful shutdown step 3).
	if err := h.NotifyStop(ctx); err != nil {
		t.Fatalf("notify stop: %v", err)
	}
	// Answer relay passes the five-field payload through.
	relay := daemon.AnswerRelay{
		QuestionID: "q_abc123", AnswerID: "a_7k9m2p", AnswerText: "yes",
		AnsweredAt: "2026-09-27T00:00:00Z", AnsweredBy: "u_x",
	}
	if err := h.DeliverAnswer(relay); err != nil {
		t.Fatal(err)
	}
	// Session identity persisted once in the capsule.
	idn, err := (&capsule.Capsule{Dir: p.CapsuleDir, WorkItemID: p.WorkItemID}).ReadSessionIdentity(capsule.AdapterPi)
	if err != nil || idn.SessionID != "sess_wi_ag01_3" {
		t.Fatalf("session.json: %+v %v", idn, err)
	}
}

func TestSupervisorSessionIDReuseGuard(t *testing.T) {
	bin := writeFakeHarness(t, false)
	s := NewSupervisor(bin, bin)
	// First derivation writes sess_A.
	p := newSpawnParams(t, capsule.AdapterPi, "sess_A")
	h1, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h1.Terminate(2 * time.Second)
	// A "new generation" arriving with a DIFFERENT session id while the old
	// one is on disk: refused (a new generation must start clean, 03 §4.4).
	p2 := newSpawnParams(t, capsule.AdapterPi, "sess_B")
	// Same capsule dir to hit the on-disk identity guard.
	p2.CapsuleDir = p.CapsuleDir
	if _, err := s.Spawn(context.Background(), p2); err == nil {
		t.Fatal("conflicting session id on the same capsule must be refused")
	}
	// Crash continuation with the SAME id: accepted (only reuse case).
	p3 := newSpawnParams(t, capsule.AdapterPi, "sess_A")
	p3.CapsuleDir = p.CapsuleDir
	h2, err := s.Spawn(context.Background(), p3)
	if err != nil {
		t.Fatalf("same-session re-derivation must be allowed: %v", err)
	}
	h2.Terminate(2 * time.Second)
}

func TestSupervisorProcessGroupKill(t *testing.T) {
	bin := writeFakeHarness(t, true) // leaves a grandchild behind
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_g")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	pgid := h.PID()
	if err := h.Terminate(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	// The grandchild (sleep 300) must ALSO be gone: group kill, not just
	// the leader (04 §7.1 residue collection).
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("process group must be fully collected")
	}
}

func TestPiAdapterArgv(t *testing.T) {
	a := &PiAdapter{Bin: "/custom/pi"}
	cmd, err := a.BuildCmd(daemon.SpawnParams{
		CapsuleDir: "/caps", SessionID: "sess_1", WorkItemID: "wi_1", Generation: 2,
	}, "/caps/harness/pi/session-data")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Args) != 3 || cmd.Args[0] != "/custom/pi" || cmd.Args[1] != "--mode" || cmd.Args[2] != "rpc" {
		t.Fatalf("pi argv: %v", cmd.Args)
	}
	if cmd.Dir != "/caps" {
		t.Fatal("pi session dir must be Capsule-local (cwd inside the capsule)")
	}
	m, err := a.InitialMessage(daemon.SpawnParams{InitialPrompt: "X"})
	if err != nil || m.Type != "prompt" || m.Message != "X" || m.ID == "" {
		t.Fatalf("initial message must be a real prompt command: %+v %v", m, err)
	}
}

func TestClaudeAdapterArgv(t *testing.T) {
	a := &ClaudeAdapter{Bin: "/custom/claude"}
	cmd, err := a.BuildCmd(daemon.SpawnParams{
		CapsuleDir: "/caps", SessionID: "sess_c", InitialPrompt: "SYS",
	}, "/caps/harness/claude/session-data")
	if err != nil {
		t.Fatal(err)
	}
	joined := fmt.Sprint(cmd.Args)
	if !contains(joined, "-p") || !contains(joined, "--session-id") || !contains(joined, "--append-system-prompt") {
		t.Fatalf("claude argv: %v", cmd.Args)
	}
	// Crash continuation: --resume <same session id>.
	a.Resume = true
	cmd2, _ := a.BuildCmd(daemon.SpawnParams{SessionID: "sess_c"}, "d")
	if !contains(fmt.Sprint(cmd2.Args), "--resume") || !contains(fmt.Sprint(cmd2.Args), "sess_c") {
		t.Fatalf("claude resume argv: %v", cmd2.Args)
	}
	// No first-prompt message: the system prompt carries pf-execute.
	if m, _ := a.InitialMessage(daemon.SpawnParams{}); m != nil {
		t.Fatal("claude injection is the system prompt, not a first prompt")
	}
	// The claude control frame is the raw control line (UNVERIFIED legacy
	// path, unchanged by the pi-transport rework).
	if got := a.FrameControl("pf-1", `{"jsonrpc":"2.0","method":"pf/quiesce"}`); got != `{"jsonrpc":"2.0","method":"pf/quiesce"}` {
		t.Fatalf("claude FrameControl must return the raw line, got %v", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// Guard: the supervisor satisfies the daemon's spawner face at compile time.
var _ daemon.AgentSpawner = (*Supervisor)(nil)
var _ daemon.AgentHandle = (*Proc)(nil)
