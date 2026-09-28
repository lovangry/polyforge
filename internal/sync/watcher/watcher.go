// Package watcher implements the Capsule file watcher whose ONLY duty is
// dirty-bit marking (05 §2.3 / 00 INV-11):
//
//   - it observes the sync surface (05 §4.1 roots, exclusions applied),
//   - on any detected change it writes {dirty, marked_at} into the capsule's
//     local sync staging area (<capsule>/sync/dirty.json),
//   - it NEVER scans for manifests, NEVER builds revisions, NEVER calls any
//     publish path and has no timer-driven publish: there is no code path
//     from this package to publishing at all.
//
// The dirty bit is a PERFORMANCE HINT for the next triggered full scan
// ("worth scanning"), never a correctness basis: a lost dirty bit costs one
// redundant scan, nothing more.
package watcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// DirtyRecord is the <capsule>/sync/dirty.json shape.
type DirtyRecord struct {
	Dirty    bool   `json:"dirty"`
	MarkedAt string `json:"marked_at"`
}

// Watcher polls one capsule's sync surface.
type Watcher struct {
	capsuleDir string
	interval   time.Duration

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// Start launches the poll loop for one capsule. interval <= 0 defaults to
// 2s. The watcher only ever WRITES the dirty record; the Daemon (or a test)
// reads it via ReadDirty.
func Start(capsuleDir string, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	w := &Watcher{
		capsuleDir: capsuleDir,
		interval:   interval,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	// The baseline is captured SYNCHRONOUSLY in Start: a change made
	// immediately after Start returns is guaranteed to be detected on the
	// first tick (a goroutine-local baseline would race with writes that
	// happen between Start returning and the goroutine first running).
	last := signature(capsuleDir)
	go func() {
		defer close(w.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				cur := signature(capsuleDir)
				if !sameSignature(cur, last) {
					markDirty(capsuleDir)
					last = cur
				}
			}
		}
	}()
	return w
}

// Stop halts the poll loop.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

// DirtyPath returns the dirty-record location for a capsule dir.
func DirtyPath(capsuleDir string) string {
	return filepath.Join(capsuleDir, "sync", "dirty.json")
}

// ReadDirty reads the current dirty state (absent file = not dirty).
func ReadDirty(capsuleDir string) (bool, string, error) {
	raw, err := os.ReadFile(DirtyPath(capsuleDir))
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	var rec DirtyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return false, "", err
	}
	return rec.Dirty, rec.MarkedAt, nil
}

func markDirty(capsuleDir string) {
	rec := DirtyRecord{Dirty: true, MarkedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	blob, _ := json.Marshal(rec)
	p := DirtyPath(capsuleDir)
	os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return
	}
	os.Rename(tmp, p)
}

// signature is a cheap stat-level fingerprint of the sync surface: path →
// (size, mtimeUnixNano, mode). Content digests are the scan's job.
func signature(capsuleDir string) map[string]string {
	out := map[string]string{}
	roots := []string{"runtime-state", "artifacts", "logs", "repos"}
	for _, root := range roots {
		base := filepath.Join(capsuleDir, root)
		filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if p == base {
				return nil
			}
			rel := filepath.ToSlash(strings.TrimPrefix(p, capsuleDir+string(os.PathSeparator)))
			if capsule.IsExcludedFromSync(rel) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			out[rel] = joinSig(info)
			return nil
		})
	}
	// Optional-sync harness session identities.
	for _, adapter := range []string{"pi", "claude"} {
		rel := "harness/" + adapter + "/session.json"
		if !capsule.IsExcludedFromSync(rel) {
			if fi, err := os.Lstat(filepath.Join(capsuleDir, filepath.FromSlash(rel))); err == nil {
				out[rel] = joinSig(fi)
			}
		}
	}
	return out
}

func joinSig(fi os.FileInfo) string {
	return itoa(fi.Size()) + ":" + itoa(fi.ModTime().UnixNano()) + ":" + itoa(int64(fi.Mode()))
}

func sameSignature(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
