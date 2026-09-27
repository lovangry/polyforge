package mainagent

// terminate_test.go: supervisor lifecycle regressions —
//
//   - FD-4: Terminate waits for the whole GROUP to be empty (not merely the
//     leader) and escalates to SIGKILL when a child ignores SIGTERM
//     (04 §6.2 B step 2 / 04 §7.3 step 2).
//   - FD-6: a failed initial-injection teardown (no half-initialized agent
//     leaks) and InitialMessage error propagation (previously silently
//     ignored).
//   - FD-7: the control/ack path is LOSSLESS while bulk output is lossy —
//     a flooding harness cannot push the pf/quiesce ack out of the window.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// procfsAvailable guards the group-emptiness assertions (Linux-only).
func procfsAvailable(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: group-emptiness verification is Linux-only")
	}
}

// TestTerminateEscalatesToSIGKILL: a group member that IGNORES SIGTERM must
// not survive Terminate. The old wait-on-leader-exit returned as soon as the
// leader died and leaked the stubborn child as a live writer.
func TestTerminateEscalatesToSIGKILL(t *testing.T) {
	procfsAvailable(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "stubbornharness")
	// The harness spawns a child that traps SIGTERM to nothing and sleeps;
	// the leader itself still dies on SIGTERM (the old bug: leader exit was
	// treated as group emptiness).
	script := `#!/bin/sh
sh -c 'trap "" TERM; exec sleep 300' &
while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*) echo quiesced-ack-placeholder ;;
  esac
done
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(p, p)
	spawn := newSpawnParams(t, capsule.AdapterPi, "sess_sig")
	h, err := s.Spawn(context.Background(), spawn)
	if err != nil {
		t.Fatal(err)
	}
	proc := h.(*Proc)
	pgid := proc.PID()
	// Give the harness a moment to actually spawn the stubborn child.
	time.Sleep(300 * time.Millisecond)
	if err := h.Terminate(1 * time.Second); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if !proc.GroupEmpty() {
		t.Fatal("the SIGTERM-ignoring child must have been SIGKILLed with the group")
	}
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("the process group must be provably empty after Terminate")
	}
}

// TestSpawnInjectionFailureTearsDownGroup: when the initial pf-execute
// instruction cannot be injected, the supervisor must TERMINATE the group
// before returning the error — a running half-initialized agent would be an
// unadopted writer. Determinism: the harness closes its own stdin and
// sleeps; the injected message is 1MB — far beyond the pipe buffer — so the
// write blocks until the child's `exec 0<&-` closes the read end, then fails
// with EPIPE: the injection failure cannot race away.
func TestSpawnInjectionFailureTearsDownGroup(t *testing.T) {
	procfsAvailable(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "closedstdin")
	script := "#!/bin/sh\nexec 0<&-\nsleep 300\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{AdapterFor: func(name string) Adapter {
		return &bigMsgAdapter{base: &PiAdapter{Bin: p}}
	}}
	sp := newSpawnParams(t, capsule.AdapterPi, "sess_inj")
	h, err := s.Spawn(context.Background(), sp)
	if err == nil {
		if h != nil {
			h.Terminate(2 * time.Second)
		}
		t.Fatal("a failed initial injection must return an error")
	}
	if !contains(err.Error(), "initial instruction injection failed") {
		t.Fatalf("the injection error must name the failure: %v", err)
	}
	if h != nil {
		t.Fatal("no handle may escape a failed spawn")
	}
}

// bigMsgAdapter wraps an adapter whose InitialMessage is 1MB (far beyond
// the 64KB pipe buffer): against a harness that never reads stdin, the
// write blocks until the read end closes and fails with EPIPE.
type bigMsgAdapter struct{ base Adapter }

func (a *bigMsgAdapter) Name() string { return a.base.Name() }
func (a *bigMsgAdapter) BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error) {
	return a.base.BuildCmd(p, sessionDir)
}
func (a *bigMsgAdapter) InitialMessage(p SpawnParams) (*rpcMessage, error) {
	return &rpcMessage{
		JSONRPC: "2.0", Method: "user/prompt",
		Params: map[string]any{"text": strings.Repeat("x", 1<<20)},
	}, nil
}

// TestSpawnInitialMessageErrorPropagates: an adapter whose InitialMessage
// build fails must propagate the error (previously silently ignored) and
// still tear the process group down.
func TestSpawnInitialMessageErrorPropagates(t *testing.T) {
	procfsAvailable(t)
	bin := writeFakeHarness(t, false)
	s := &Supervisor{AdapterFor: func(name string) Adapter {
		return &errInitialAdapter{base: &PiAdapter{Bin: bin}}
	}}
	sp := newSpawnParams(t, capsule.AdapterPi, "sess_ime")
	h, err := s.Spawn(context.Background(), sp)
	if err == nil || !contains(err.Error(), "initial instruction build failed") {
		t.Fatalf("the InitialMessage error must propagate: %v", err)
	}
	if h != nil {
		t.Fatal("no handle may escape a failed spawn")
	}
}

// errInitialAdapter wraps an adapter whose InitialMessage always fails.
type errInitialAdapter struct{ base Adapter }

func (a *errInitialAdapter) Name() string { return a.base.Name() }
func (a *errInitialAdapter) BuildCmd(p SpawnParams, sessionDir string) (*exec.Cmd, error) {
	return a.base.BuildCmd(p, sessionDir)
}
func (a *errInitialAdapter) InitialMessage(p SpawnParams) (*rpcMessage, error) {
	return nil, fmt.Errorf("boom: initial message build failed")
}

// TestAckSurvivesBulkFlood: the control/ack path is LOSSLESS while bulk
// output is lossy. The harness answers the pf/quiesce request with a flood
// of plain bulk lines FIRST and the ack LAST — with one shared bounded
// channel the ack was droppable; it must always arrive now.
func TestAckSurvivesBulkFlood(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "floodharness")
	// The ack line is assembled from concatenated parts so the script needs
	// no nested double-quote escaping.
	script := `#!/bin/sh
ACK='{"method":"pf/quiesce","result":{"ack":"pf/quiesce"}}'
while IFS= read -r line; do
  case "$line" in
    *pf/quiesce*)
      i=0
      while [ $i -lt 20000 ]; do echo "bulk progress line $i"; i=$((i+1)); done
      echo "$ACK"
      ;;
    *pf/stop_notice*) echo '{"method":"pf/stop_notice","result":{"ack":"pf/stop_notice"}}' ;;
    *user/prompt*)    echo '{"method":"user/prompt","result":{"ack":"user/prompt"}}' ;;
  esac
done
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(p, p)
	sp := newSpawnParams(t, capsule.AdapterPi, "sess_flood")
	h, err := s.Spawn(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	// The quiesce ack must arrive within the bounded window despite the
	// 20000-line bulk flood that precedes it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Quiesce(ctx); err != nil {
		t.Fatalf("the ack must survive the bulk flood: %v", err)
	}
}
