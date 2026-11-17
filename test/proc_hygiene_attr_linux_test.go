//go:build linux

package test

import (
	"syscall"
	"testing"
)

// setOwnGroupAttr keeps Linux's kernel-level parent-death guarantee while
// retaining process-group isolation for teardown.
func setOwnGroupAttr(attr *syscall.SysProcAttr) {
	attr.Setpgid = true
	attr.Pdeathsig = syscall.SIGKILL
}

func TestSetOwnGroupAttrPreservesLinuxProcessDeathGuarantee(t *testing.T) {
	attr := &syscall.SysProcAttr{}
	setOwnGroupAttr(attr)
	if !attr.Setpgid {
		t.Fatal("Linux process hygiene must create a dedicated process group")
	}
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Linux process hygiene Pdeathsig = %v, want SIGKILL", attr.Pdeathsig)
	}
}
