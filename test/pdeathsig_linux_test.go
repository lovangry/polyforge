//go:build linux

package test

// pdeathsig_linux_test.go / pdeathsig_other_test.go: the Linux-only half
// of ownGroup. SysProcAttr.Pdeathsig exists only on Linux (the kernel's
// parent-death signal), so the assignment lives here and every other
// platform compiles with the no-op twin — the fakepi agents' own
// FAKEPI_TEST_PID watch (proc_hygiene_test.go's header) is the
// platform-independent backstop that already covers an orphaned agent.
import "syscall"

// setPdeathsig arms the kernel to SIGKILL this process the moment its
// parent (the test binary) dies.
func setPdeathsig(sa *syscall.SysProcAttr) {
	sa.Pdeathsig = syscall.SIGKILL
}
