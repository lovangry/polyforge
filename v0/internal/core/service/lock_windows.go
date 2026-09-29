//go:build windows

package service

import "os"

// lockFile: v1 targets Linux/macOS only (00 §1.2). On Windows the lock is a
// stub so the package still builds.
func lockFile(f *os.File) error { return nil }

func unlockFile(f *os.File) {}
