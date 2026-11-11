package git

// hook_backup_test.go: the preservation half of the carrier rewind
// supersede (polyforge#20, owner ruling 2026-11-11). An authorized
// supersede replaces a carrier tip and preserves the surpassed tip in
// the SAME atomic push, on a fresh timestamped
// refs/pf-task-backup/<repo>/<stamp> ref. These tests pin what the
// shipped pre-receive hook does with that namespace: it accepts a fresh
// backup of task-repo history — the carrier's own gitlink exemption and
// task blob limit, not the chain's — while a backup ref stays write-once
// (no non-fast-forward rewrite) and undeletable.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// TestTaskBackupRefAccepted (polyforge#20): a fresh
// refs/pf-task-backup/<repo>/<stamp> ref accepts the surpassed tip's
// history — a blob above the chain limit but under the carrier limit,
// and a submodule gitlink — exactly like refs/pf-task/<repo> itself.
func TestTaskBackupRefAccepted(t *testing.T) {
	e := newGitEnv(t)
	if e.mgr.MaxBlobBytes != capsule.MaxBlobBytes || e.mgr.MaxTaskBlobBytes != capsule.MaxTaskBlobBytes {
		t.Fatalf("manager limits = %d/%d, want the capsule defaults", e.mgr.MaxBlobBytes, e.mgr.MaxTaskBlobBytes)
	}
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	// Task-repo history: a blob above the chain limit, under the carrier
	// limit (the upstream-asset shape the carrier limit exists for).
	if err := os.WriteFile(filepath.Join(wt, "asset.mp4"), bytes.Repeat([]byte{7}, capsule.MaxBlobBytes+1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "-m", "surpassed session's upstream asset")
	surpassed := strings.TrimSpace(mustGit(t, wt, "rev-parse", "HEAD"))
	// ... and a submodule gitlink: task-repo history, not chain history.
	mustGit(t, wt, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendored-thing")
	mustGit(t, wt, "commit", "-m", "surpassed session's submodule")

	// The carrier itself takes the line...
	if out, err := runGit(t, wt, "push", cred, "HEAD:refs/pf-task/demo-src"); err != nil {
		t.Fatalf("carrier push rejected: %s", out)
	}
	// ...and the supersede's atomic push takes the same history on a
	// fresh timestamped backup ref.
	backup := "refs/pf-task-backup/demo-src/20261111T101112.123456789Z"
	if out, err := runGit(t, wt, "push", cred, surpassed+":"+backup); err != nil {
		t.Fatalf("backup push rejected: %s", out)
	}
	if got := strings.TrimSpace(mustGit(t, wt, "ls-remote", cred, backup)); !strings.Contains(got, surpassed) {
		t.Fatalf("backup ref names %q, want the surpassed tip %s", got, surpassed)
	}
	// The preserved line fetches back out (the manual recovery read).
	wt2 := newWorktree(t)
	mustGit(t, wt2, "remote", "add", "hub", cred)
	mustGit(t, wt2, "fetch", "--quiet", "hub", backup)
	if got := strings.TrimSpace(mustGit(t, wt2, "rev-parse", "FETCH_HEAD")); got != surpassed {
		t.Fatalf("fetched backup head %q, want %q", got, surpassed)
	}
}

// TestTaskBackupRefWriteOnceAndUndeletable (polyforge#20): a backup ref
// is preservation, not a lane — a non-fast-forward rewrite of an existing
// backup name is refused (write-once: the daemon mints a fresh stamp per
// supersede) and deletion is refused like every other ref.
func TestTaskBackupRefWriteOnceAndUndeletable(t *testing.T) {
	e := newGitEnv(t)
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	commitPlain(t, wt, "code.txt", "one\n")
	commitPlain(t, wt, "other.txt", "surpassed session's work\n")
	surpassed := strings.TrimSpace(mustGit(t, wt, "rev-parse", "HEAD"))
	backup := "refs/pf-task-backup/demo-src/20261111T101112.123456789Z"
	if out, err := runGit(t, wt, "push", cred, surpassed+":"+backup); err != nil {
		t.Fatalf("backup push rejected: %s", out)
	}
	// A divergent head (an amend: a sibling of the preserved tip, not a
	// descendant) must not replace the preserved tip under the same
	// name, even forced.
	mustGit(t, wt, "commit", "-q", "--amend", "-m", "not the preserved line")
	divergent := strings.TrimSpace(mustGit(t, wt, "rev-parse", "HEAD"))
	if out, err := runGit(t, wt, "push", cred, "+"+divergent+":"+backup); err == nil {
		t.Fatalf("non-fast-forward backup rewrite accepted: %s", out)
	} else if !strings.Contains(out, "non-fast-forward") {
		t.Fatalf("wrong rejection: %s", out)
	}
	if out, err := runGit(t, wt, "push", cred, ":"+backup); err == nil {
		t.Fatalf("backup ref deletion accepted: %s", out)
	} else if !strings.Contains(out, "deletion") {
		t.Fatalf("wrong rejection: %s", out)
	}
	// The preserved tip stands.
	if got := strings.TrimSpace(mustGit(t, wt, "ls-remote", cred, backup)); !strings.Contains(got, surpassed) {
		t.Fatalf("backup ref names %q, want the preserved tip %s", got, surpassed)
	}
}

// TestTaskBackupBlobOverCarrierLimitRejected: the backup namespace gets
// the carrier's limit, not none — a blob above MaxTaskBlobBytes is
// refused on refs/pf-task-backup/* (shrunk limits keep the test fast).
func TestTaskBackupBlobOverCarrierLimitRejected(t *testing.T) {
	e := newGitEnv(t)
	e.mgr.MaxBlobBytes = 512
	e.mgr.MaxTaskBlobBytes = 4096
	wiID, secret := e.claimWI(t)
	cred := e.credURL(wiID, secret)
	wt := newWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "big.bin"), bytes.Repeat([]byte{1}, 8192), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "-m", "oversize")
	if out, err := runGit(t, wt, "push", cred, "HEAD:refs/pf-task-backup/demo-src/20261111T101112.123456789Z"); err == nil {
		t.Fatalf("backup push of an oversize blob accepted: %s", out)
	} else if !strings.Contains(out, fmt.Sprintf("blob exceeds the %d-byte limit", e.mgr.MaxTaskBlobBytes)) {
		t.Fatalf("wrong rejection: %s", out)
	}
}
