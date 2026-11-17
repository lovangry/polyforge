//go:build darwin

package test

import "syscall"

// Darwin has no SysProcAttr.Pdeathsig. Setpgid still isolates each command;
// killGroup reaps stragglers during normal teardown, while fakepi's pid-watch
// exits agents orphaned when the test binary is killed. These fallbacks do not
// provide the kernel-level immediate parent-death action Linux provides.
func setOwnGroupAttr(attr *syscall.SysProcAttr) {
	attr.Setpgid = true
}
