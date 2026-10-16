package runtime

// review_wave3_test.go — runtime regressions for the 2026-10-16 repo
// review, wave 3 (runtime lane).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// B-7: the terminal close of a completed workflow must seal-push the
// capsule like every other terminal close. The driver does no git; the
// terminal execution.json (and the transcript tail) reach the hub only
// through the observe flush — which may have failed or timed out — so
// skipping the seal push let SessionEnd(wrapped) land on a chain missing
// the final state.
func TestTerminalWrapSealPushesTheFinalState(t *testing.T) {
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const wi = "wi_w3_sealpush"
	j := seedJob(r, wi)
	bare := initGitCapsule(t, j)
	hub.hold(wi, j.sessionID, api.StatusRunning)
	execDir := filepath.Join(j.capsuleDir, "runtime-state")
	if err := os.MkdirAll(execDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(map[string]string{"workflow_status": capsule.WorkflowStatusCompleted})
	if err := os.WriteFile(filepath.Join(execDir, "execution.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.agent = &recordingHandle{}
	j.agentPID = 0
	j.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	if msg := recvRT(t, toOB); msg.Kind != protocol.RTFinished || msg.Reason != api.StatusWrapped {
		t.Fatalf("want RTFinished(wrapped), got %+v", msg)
	}
	out, err := exec.Command("git", "--git-dir", bare, "show", "refs/heads/main:runtime-state/execution.json").CombinedOutput()
	if err != nil || !strings.Contains(string(out), capsule.WorkflowStatusCompleted) {
		t.Fatalf("hub chain lacks the terminal execution.json: %v: %s", err, out)
	}
}
