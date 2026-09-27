// Package recover implements remote site reconstruction (05 §7.4): rebuild
// the sync surface of a capsule from the remote revision chain when a
// generation change forces a clean rebuild (takeover / cross-machine /
// new capsule).
//
// Semantics:
//   - HEAD fetch first: 404 → nothing published, the fresh empty capsule
//     stays as created, waiting for its first bootstrap publish.
//   - Replay the newest checkpoint ≤ HEAD plus every incremental after it
//     (no checkpoint → full replay from seq 0) into an expected file set.
//   - FULL REPLACEMENT of the sync surface (delete-then-write): files of the
//     current surface absent from the expected set are removed so nothing
//     deleted before a checkpoint can resurrect.
//   - harness/ is NEVER restored from history (05 §7.4): historical
//     session.json entries are audit metadata only; the new generation's
//     WI Agent writes its own. Materialization targets (.pi/agents/,
//     .claude/agents/, workflow.md) are not in the surface at all
//     (SYNC-INV-10).
//   - Every write passes the 03 §6.3 safety rules (per-level parent lstat,
//     symlink target confinement); a violation aborts the WHOLE recovery
//     with nothing partially applied.
//   - Every restored byte is verified against its expected sha256.
package recover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// Remote is the recovery read face (05 §5.1 rows 16/17/19 + manifest
// content; see the aihub client's package comment for the manifest-content
// endpoint gap).
type Remote interface {
	GetHead(ctx context.Context, wiID string) (*revision.HeadInfo, bool, error)
	ListRevisions(ctx context.Context, wiID string, since int64) ([]revision.RevisionMeta, error)
	GetManifest(ctx context.Context, wiID string, seq int64) (revision.Manifest, error)
	GetBlock(ctx context.Context, wiID, shaHex string) ([]byte, error)
}

// Result reports what a recovery established.
type Result struct {
	Head           *revision.HeadInfo // nil when nothing was published
	LiveFiles      map[string]revision.FileInfo
	BytesDigest    string // of the expected live set (publish-state seed)
	WorktreeDigest string
	Restored       int // files written or verified identical
}

// Recover rebuilds cap's sync surface from remote. Inventory checkout must
// already have happened (03 §6.1 step 5 orders checkout BEFORE recovery).
func Recover(ctx context.Context, cap *capsule.Capsule, remote Remote) (*Result, error) {
	wi := cap.WorkItemID
	head, found, err := remote.GetHead(ctx, wi)
	if err != nil {
		return nil, err
	}
	if !found {
		// No HEAD: nothing published. The empty capsule created by
		// capsule.Create stays untouched; the first publish bootstraps.
		return &Result{LiveFiles: map[string]revision.FileInfo{}}, nil
	}
	metas, err := remote.ListRevisions(ctx, wi, 0)
	if err != nil {
		return nil, err
	}
	var chain []revision.RevisionMeta
	for _, m := range metas {
		if m.Seq > head.Seq {
			break
		}
		chain = append(chain, m)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("recover: HEAD at %d but no revisions listed", head.Seq)
	}
	// Replay window: newest checkpoint ≤ HEAD resets the set; every
	// incremental after it mutates it (05 §7.4). Manifest kind lives in the
	// manifest content, fetched per seq here.
	expected := map[string]revision.FileInfo{}
	for _, m := range chain {
		man, err := remote.GetManifest(ctx, wi, m.Seq)
		if err != nil {
			return nil, fmt.Errorf("recover: manifest %d: %w", m.Seq, err)
		}
		if man.Kind == revision.KindCheckpoint {
			expected = map[string]revision.FileInfo{}
		}
		for _, op := range man.Ops {
			switch op.Op {
			case revision.OpPut:
				mode := int64(0o644)
				if op.Mode != nil {
					mode = *op.Mode
				}
				expected[op.Path] = revision.FileInfo{Sha256: op.Sha256, Bytes: op.Bytes, Mode: mode}
			case revision.OpDelete:
				delete(expected, op.Path)
			case revision.OpSymlink:
				expected[op.Path] = revision.FileInfo{IsSymlink: true, Target: op.Target}
			case revision.OpChmod:
				if fi, ok := expected[op.Path]; ok && op.Mode != nil {
					fi.Mode = *op.Mode
					expected[op.Path] = fi
				}
			}
		}
	}

	// Full replacement (delete-then-write), applied per path with
	// all-or-nothing safety: safety validation of EVERY expected entry runs
	// BEFORE the first mutation.
	type writeJob struct {
		rel string
		fi  revision.FileInfo
	}
	var jobs []writeJob
	restorable := map[string]revision.FileInfo{}
	for rel, fi := range expected {
		if strings.HasPrefix(rel, "harness/") {
			continue // NEVER restored from history (05 §7.4)
		}
		if err := validateExpected(cap, rel, fi); err != nil {
			return nil, err
		}
		restorable[rel] = fi
		jobs = append(jobs, writeJob{rel, fi})
	}

	// Current surface (delete-then-write needs to know what exists).
	current, err := revision.ScanCapsule(cap.Dir)
	if err != nil {
		return nil, err
	}
	// Phase 1 (validation) passed: delete surface members not in the
	// expected set. harness/ members of the CURRENT surface are left alone
	// (the new generation's session dir is local-only state).
	for rel := range current.Files {
		if strings.HasPrefix(rel, "harness/") {
			continue
		}
		if _, keep := restorable[rel]; keep {
			continue
		}
		p, err := cap.Path(rel)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	// Phase 2: write/verify every expected entry.
	restored := 0
	for _, job := range jobs {
		if err := applyEntry(ctx, cap, remote, wi, job.rel, job.fi); err != nil {
			return nil, err
		}
		restored++
	}

	// Seed material for the publish state: the live set as restored.
	digestInput := &revision.ScanResult{Files: restorable}
	bytesDigest, err := digestInput.BytesDigest()
	if err != nil {
		return nil, err
	}
	worktreeDigest, err := revision.WorktreeDigest(cap.Dir)
	if err != nil {
		return nil, err
	}
	return &Result{
		Head: head, LiveFiles: restorable, BytesDigest: bytesDigest,
		WorktreeDigest: worktreeDigest, Restored: restored,
	}, nil
}

// validateExpected pre-checks one expected entry against 03 §6.3 so an
// unsafe manifest aborts BEFORE any mutation (whole-operation atomicity).
func validateExpected(cap *capsule.Capsule, rel string, fi revision.FileInfo) error {
	var target string
	if fi.IsSymlink {
		target = fi.Target
	}
	return cap.ValidateWriteTarget(rel, target)
}

// applyEntry materializes one expected entry with per-file sha verification.
func applyEntry(ctx context.Context, cap *capsule.Capsule, remote Remote, wi, rel string, fi revision.FileInfo) error {
	abs, err := cap.Path(rel)
	if err != nil {
		return err
	}
	if fi.IsSymlink {
		// Replace any existing entry at the path.
		os.Remove(abs)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		return os.Symlink(filepath.FromSlash(fi.Target), abs)
	}
	// Same content already present (e.g. base checkout matches)? Verify and
	// keep — this keeps recovery idempotent and cheap for unchanged repos.
	if cur, err := os.Lstat(abs); err == nil && cur.Mode().IsRegular() && cur.Mode().Perm() == (os.FileMode(fi.Mode)&os.ModePerm) {
		sum, serr := fileSha(abs)
		if serr == nil && sum == fi.Sha256 {
			return nil
		}
	}
	block, err := remote.GetBlock(ctx, wi, fi.Sha256)
	if err != nil {
		return fmt.Errorf("recover: block %s for %s: %w", fi.Sha256, rel, err)
	}
	content, err := revision.DecodeBlock(block)
	if err != nil {
		return fmt.Errorf("recover: block %s undecodable: %w", fi.Sha256, err)
	}
	if got := revision.DigestHex(content); got != fi.Sha256 {
		return fmt.Errorf("recover: sha mismatch for %s: got %s want %s", rel, got, fi.Sha256)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp := abs + ".rtmp"
	if err := os.WriteFile(tmp, content, os.FileMode(fi.Mode)&os.ModePerm); err != nil {
		return err
	}
	// Final verification of what will be renamed into place.
	if got := revision.DigestHex(content); got != fi.Sha256 {
		os.Remove(tmp)
		return fmt.Errorf("recover: corruption writing %s", rel)
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return err
	}
	// Verify the settled file (paranoia per 05 §7.4: 逐文件校验 sha256).
	sum, err := fileSha(abs)
	if err != nil || sum != fi.Sha256 {
		return fmt.Errorf("recover: post-write verification failed for %s", rel)
	}
	return nil
}

func fileSha(p string) (string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return revision.DigestHex(raw), nil
}
