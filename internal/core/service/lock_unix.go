//go:build !windows

package service

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive non-blocking lock via flock(2) — OS-level, so it
// is released automatically when the process dies (crash-safe single-active).
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
