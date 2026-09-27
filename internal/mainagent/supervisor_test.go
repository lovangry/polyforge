package mainagent

// Supervisor + adapter tests against a scripted fake harness binary: the
// full stdio protocol (initial injection, quiesce ack, stop notice, answer
// relay), process-group termination, session id reuse rules.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// fakeHarness is a shell "harness": reads JSON lines, acks every pf/* and
// user/prompt method, and leaves a grandchild alive on SIGTERM only if
// asked (to exercise group kill).
func writeFakeHarness(t *testing.T, leaveGrandchild bool) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fakeharness")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *pf/stop_notice*) echo '{"method":"pf/stop_notice","result":{"ack":"pf/stop_notice"}}' ;;
    *pf/quiesce*)     echo '{"method":"pf/quiesce","result":{"ack":"pf/quiesce"}}' ;;
    *pf/answer*)      echo '{"method":"pf/answer","result":{"ack":"pf/answer"}}' ;;
    *user/prompt*)    echo '{"method":"user/prompt","result":{"ack":"user/prompt"}}' ;;
  esac
done
`
	if leaveGrandchild {
		script = `#!/bin/sh
sh -c 'sleep 300' &
` + script
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
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
	if err != nil || m.Method != "user/prompt" || m.Params["text"] != "X" {
		t.Fatalf("initial message: %+v %v", m, err)
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

// Silence unused warnings in constrained builds.
var _ = exec.Command
