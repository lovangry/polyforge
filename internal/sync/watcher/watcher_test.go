package watcher

// The watcher's ONLY behaviors are observable here: it marks dirty on
// change, and it structurally has no publish path. There is no publish
// callback anywhere in this package's API; this test file asserts the
// observable half of that (the structural half is the package surface).

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func awaitDirty(t *testing.T, capsuleDir string, within time.Duration) (bool, string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		dirty, at, err := ReadDirty(capsuleDir)
		if err != nil {
			t.Fatal(err)
		}
		if dirty {
			return dirty, at
		}
		if time.Now().After(deadline) {
			return false, ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcherMarksDirtyOnSyncSurfaceChange(t *testing.T) {
	capsuleDir := t.TempDir()
	for _, d := range []string{"runtime-state", "artifacts", "repos/r1", "logs", "sync", "harness/pi/session-data"} {
		os.MkdirAll(filepath.Join(capsuleDir, d), 0o755)
	}
	w := Start(capsuleDir, 10*time.Millisecond)
	defer w.Stop()

	// No change yet: not dirty.
	if dirty, _, _ := ReadDirty(capsuleDir); dirty {
		t.Fatal("fresh capsule must not be dirty")
	}
	// A sync-surface write marks dirty.
	os.WriteFile(filepath.Join(capsuleDir, "artifacts", "spec.md"), []byte("v1"), 0o644)
	if dirty, at := awaitDirty(t, capsuleDir, 2*time.Second); !dirty || at == "" {
		t.Fatal("sync-surface change must mark dirty")
	}
}

func TestWatcherIgnoresExcludedPaths(t *testing.T) {
	capsuleDir := t.TempDir()
	for _, d := range []string{"artifacts", "sync", ".pi/agents", "harness/pi/session-data"} {
		os.MkdirAll(filepath.Join(capsuleDir, d), 0o755)
	}
	w := Start(capsuleDir, 10*time.Millisecond)
	defer w.Stop()
	// Write ONLY into excluded areas.
	os.WriteFile(filepath.Join(capsuleDir, ".pi", "agents", "code.md"), []byte("def"), 0o644)
	os.WriteFile(filepath.Join(capsuleDir, "harness", "pi", "session-data", "t.jsonl"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(capsuleDir, "sync", "note"), []byte("x"), 0o644)
	// Watch one full poll cycle beyond the writes.
	time.Sleep(120 * time.Millisecond)
	if dirty, _, _ := ReadDirty(capsuleDir); dirty {
		t.Fatal("excluded-path changes must not mark the capsule dirty")
	}
}

func TestWatcherStopIsIdempotent(t *testing.T) {
	capsuleDir := t.TempDir()
	w := Start(capsuleDir, 5*time.Millisecond)
	w.Stop()
	w.Stop() // must not panic or hang
	// After Stop no further marking happens.
	os.WriteFile(filepath.Join(capsuleDir, "artifacts-x-marker"), []byte("x"), 0o644)
	// Use a sync-root path so a live watcher WOULD mark.
	os.MkdirAll(filepath.Join(capsuleDir, "artifacts"), 0o755)
	os.WriteFile(filepath.Join(capsuleDir, "artifacts", "a"), []byte("x"), 0o644)
	time.Sleep(80 * time.Millisecond)
	if dirty, _, _ := ReadDirty(capsuleDir); dirty {
		t.Fatal("a stopped watcher must not mark")
	}
}
