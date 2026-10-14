package daemon

// answer_step_origin_test.go: polyforge#122 — a grill-review step agent
// answered its own human question with `pf answer` and the workflow moved
// on. The daemon now refuses an answer whose UDS peer belongs to a step
// process tree of this daemon (the PF_WI_ID/PF_DAEMON_ID markers every
// step process is spawned with, on the peer or an ancestor). The
// scenarios dial the real UDS face from a CHILD process, so the kernel's
// SO_PEERCRED names that child — exactly what an agent's bash tool is.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// TestPF122AnswerHelperProcess is not a test: re-executed as a child, it
// posts one answer over the socket named in its environment and prints
// the outcome.
func TestPF122AnswerHelperProcess(t *testing.T) {
	sock := os.Getenv("PF122_SOCK")
	if sock == "" {
		t.Skip("helper process only")
	}
	_, err := cli.NewDaemonClient(sock).Answer(os.Getenv("PF122_WI"),
		cli.AnswerPayload{QuestionID: os.Getenv("PF122_Q"), AnswerText: "self-answer", AnsweredBy: "agent"})
	if err != nil {
		fmt.Printf("PF122-REFUSED %v\n", err)
		return
	}
	fmt.Println("PF122-ACCEPTED")
}

func pf122Daemon(t *testing.T) (*testDaemon, *answerAgent) {
	t.Helper()
	aa := &answerAgent{answered: make(chan string, 4)}
	td := newTestDaemon(t, func(c *Config) {
		inner := fakeEngineStarter()
		c.AgentStarter = func(ctx context.Context, in runtime.AgentStartInput) (runtime.AgentHandle, error) {
			h, err := inner(ctx, in)
			if err != nil {
				return nil, err
			}
			aa.AgentHandle = h
			return aa, nil
		}
	})
	td.submitClaim(t, "wi_122")
	td.waitMonitored(t, "wi_122")
	td.seedPendingQuestion(t, "wi_122", "q_122", "grill-review", "Is this spec what you want?")
	return td, aa
}

// runAnswerChild runs argv (the helper, or a shell around it) with the
// given extra environment and answers its printed outcome.
func runAnswerChild(t *testing.T, td *testDaemon, argv []string, env ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), append([]string{"PF122_SOCK=" + td.sock, "PF122_WI=wi_122", "PF122_Q=q_122"}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("answer child: %v\n%s", err, out)
	}
	return string(out)
}

func helperArgv() []string {
	return []string{os.Args[0], "-test.run=^TestPF122AnswerHelperProcess$", "-test.count=1"}
}

func assertNotDelivered(t *testing.T, aa *answerAgent) {
	t.Helper()
	select {
	case q := <-aa.answered:
		t.Fatalf("REPRODUCED #122: the step process's answer reached question %q", q)
	default:
	}
}

// The #122 scenario: the step agent (carrying the step markers) runs
// `pf answer` against its own question. Refused DAEMON-403, never
// delivered.
func TestAnswerFromStepProcessIsRefused(t *testing.T) {
	td, aa := pf122Daemon(t)
	out := runAnswerChild(t, td, helperArgv(), "PF_WI_ID=wi_122", "PF_DAEMON_ID="+td.cfg.MachineID)
	if !strings.Contains(out, "PF122-REFUSED") || !strings.Contains(out, "DAEMON-403") || !strings.Contains(out, "step process tree") {
		t.Fatalf("REPRODUCED #122: a step process answered a human question: %s", out)
	}
	assertNotDelivered(t, aa)
}

// Clearing the environment does not launder the caller: an `env -u`
// child of a marked step shell is still in the step's tree.
func TestAnswerFromScrubbedChildOfStepIsRefused(t *testing.T) {
	td, aa := pf122Daemon(t)
	h := helperArgv()
	script := `env -u PF_WI_ID -u PF_DAEMON_ID "$0" "$1" "$2"; exit $?`
	out := runAnswerChild(t, td, []string{"sh", "-c", script, h[0], h[1], h[2]},
		"PF_WI_ID=wi_122", "PF_DAEMON_ID="+td.cfg.MachineID)
	if !strings.Contains(out, "PF122-REFUSED") || !strings.Contains(out, "DAEMON-403") {
		t.Fatalf("a scrubbed child of a step shell answered: %s", out)
	}
	assertNotDelivered(t, aa)
}

// The control: the same answer from a human session (no step markers on
// the caller or its ancestors) is delivered. Markers of ANOTHER daemon
// (e.g. this test itself running inside a real step) do not count.
func TestAnswerFromHumanSessionIsDelivered(t *testing.T) {
	td, aa := pf122Daemon(t)
	out := runAnswerChild(t, td, helperArgv(), "PF_WI_ID=wi_other", "PF_DAEMON_ID=another-daemon")
	if !strings.Contains(out, "PF122-ACCEPTED") {
		t.Fatalf("a human session's answer must be accepted: %s", out)
	}
	select {
	case q := <-aa.answered:
		if q != "q_122" {
			t.Fatalf("answer delivered to %q, want q_122", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the human answer was never delivered")
	}
}
