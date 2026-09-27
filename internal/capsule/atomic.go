package capsule

// atomic.go: the tmp+fsync+rename controlled-update primitive shared by
// capsule.json, execution.json, the daemon-side pending publish records and
// the watcher dirty bit. 03 §4.2 pins this exact sequence as the ONLY
// controlled update interface for capsule state files; every writer here
// uses the same shape so a crash mid-write never produces a torn file.

import (
	"os"
	"path/filepath"
)

// AtomicWrite writes data to path atomically: write to <path>.tmp, fsync,
// then rename over path. dir sync afterwards is best-effort (the rename is
// already atomic within one filesystem; the dir fsync hardens the rename
// itself against power loss).
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// Best-effort directory fsync so the rename itself survives power loss.
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
