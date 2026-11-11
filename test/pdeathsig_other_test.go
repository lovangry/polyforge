//go:build !linux

package test

// The no-op twin of pdeathsig_linux_test.go: SysProcAttr has no
// Pdeathsig field off Linux, so nothing can be armed here — an agent
// orphaned by a killed test binary is reaped by its FAKEPI_TEST_PID
// watch instead (proc_hygiene_test.go's header).
import "syscall"

func setPdeathsig(*syscall.SysProcAttr) {}
