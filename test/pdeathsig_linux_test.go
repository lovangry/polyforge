//go:build linux

package test

// pdeathsig_linux_test.go: the Linux half of ownGroup's death-signal seam
// — the kernel SIGKILLs the process when the test binary that started it
// dies, catching what a timeout panic's skipped cleanups cannot. Off
// Linux (see pdeathsig_other_test.go) the kernel offers no parent-death
// signal and the group and bin-dir sweeps in proc_hygiene_test.go are
// the backstop.

import "syscall"

func setPdeathsig(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
