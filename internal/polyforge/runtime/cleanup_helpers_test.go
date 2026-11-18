package runtime

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// runMarker scopes a test's PF_WI_ID / PF_DAEMON_ID marker value to this
// test process. The env-marker layer scans the WHOLE host's /proc, so a
// constant marker collides with the same test running concurrently in
// another process (a second checkout, a release gate): one run's sweep
// kills, or its scan counts, the other run's processes.
func runMarker(base string) string { return base + "-" + strconv.Itoa(os.Getpid()) }

// startTree spawns `sh -c "sleep 300 & exec sleep 300"` as its own group
// leader: a parent with one grandchild in the same pgid.
func startTree(t *testing.T, wi string) (pid int, startedAt string) {
	t.Helper()
	cmd := spawnTreeCmd(wi)
	startedAt = nowStamp()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	// the tree never outlives the test, whatever the test's path
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = cmd.Wait() })
	// wait until the tree has both members
	deadline := time.Now().Add(5 * time.Second)
	for groupMemberCount(pid) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("tree never grew a grandchild (members=%d)", groupMemberCount(pid))
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pid, startedAt
}

func spawnTreeCmd(wi string) *exec.Cmd {
	c := exec.Command("sh", "-c", "sleep 300 & exec sleep 300")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if wi != "" {
		c.Env = append(os.Environ(), "PF_WI_ID="+wi, "PF_DAEMON_ID=")
	}
	return c
}

func groupMemberCount(pgid int) int {
	pids, err := procListAll()
	if err != nil {
		return 0
	}
	n := 0
	for _, pid := range pids {
		state, _, pgrp, _, ok := procStat(pid)
		if ok && state != "Z" && int(pgrp) == pgid {
			n++
		}
	}
	return n
}
