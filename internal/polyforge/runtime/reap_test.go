package runtime

// Crash-orphan reaping (v2-converge §6 bug 3): no handle, agent pid 0, the
// step pid only in the capsule spawn journal.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func writeJournalPID(t *testing.T, capsuleDir string, pid int, startedAt string) {
	t.Helper()
	dir := filepath.Join(capsuleDir, "runtime-state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(map[string]any{"pid": pid, "started_at": startedAt, "spawn_n": 1})
	if err := os.WriteFile(filepath.Join(dir, "spawn-journal.jsonl"), append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReapOrphansKillsJournaledTreeWithPidZero(t *testing.T) {
	pid, startedAt := startTree(t, "wi_reap1")
	capsuleDir := t.TempDir()
	writeJournalPID(t, capsuleDir, pid, startedAt)

	r := &Runtime{}
	j := &job{wi: "wi_reap1", capsuleDir: capsuleDir} // agentPID 0, handle nil: the post-crash shape
	r.killAgentTree(j)

	waitDead(t, pid)
	if n := groupMemberCount(pid); n != 0 {
		t.Fatalf("group %d still holds %d members after reap", pid, n)
	}
}

func TestReapOrphansEnvMarkerSweepWithoutJournal(t *testing.T) {
	pid, _ := startTree(t, "wi_reap2")
	// escapee-shaped: not the journaled pid, no journal at all
	(&Runtime{}).reapOrphans("wi_reap2", t.TempDir())
	waitDead(t, pid)
}

func TestReapOrphansSkipsReusedPid(t *testing.T) {
	// an unrelated live process (no PF_WI_ID) whose pid the journal names
	// with a start stamp from long before it was born
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	capsuleDir := t.TempDir()
	writeJournalPID(t, capsuleDir, cmd.Process.Pid, "2001-01-01T00:00:00Z")

	(&Runtime{}).reapOrphans("wi_reap3", capsuleDir)

	if !procExists(cmd.Process.Pid) {
		t.Fatal("unrelated process was killed through a stale journal pid")
	}
	// and without a stamp, an unmarked live pid is not ours either
	writeJournalPID(t, capsuleDir, cmd.Process.Pid, "")
	(&Runtime{}).reapOrphans("wi_reap3", capsuleDir)
	if !procExists(cmd.Process.Pid) {
		t.Fatal("unmarked process killed through a stamp-less journal pid")
	}
}
