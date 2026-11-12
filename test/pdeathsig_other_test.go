//go:build !linux

package test

// pdeathsig_other_test.go: the non-Linux half of ownGroup's death-signal
// seam — a no-op where the kernel offers no parent-death signal (darwin):
// the group and bin-dir sweeps in proc_hygiene_test.go catch the leftover
// processes a Linux Pdeathsig would have reaped.

import "syscall"

func setPdeathsig(*syscall.SysProcAttr) {}
