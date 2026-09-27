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
//     deleted before a checkpoint can resurrect. EVERY block the rebuild
//     needs is fetched and sha-verified BEFORE the first mutation, so a
//     missing or corrupt block leaves the old surface intact (03 §6.3
//     rule 3 whole-operation atomicity).
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// Remote is the recovery read face (05 §5.1 rows 16/17/19). With the
// row-19 amendment (polyforge#14) the listing can carry each revision's
// manifest ops inline (include=ops); Recover replays from those and
// falls back to per-seq GetManifest only when a remote's listing is
// metadata-only.
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

// ErrChainInvalid marks a remote revision listing that does not form the
// contiguous, parent-linked chain its HEAD row claims to cap (05 §2.1:
// monotonic seqs, one unique parent per revision, seq 0 carrying the null
// parent sentinel; §2.2: HEAD is the newest COMPLETE revision). A listing
// failing any of these is a data inconsistency — recovery refuses to touch
// the local surface from it, typed, before any mutation.
var ErrChainInvalid = errors.New("recover: revision chain invalid")

// writeJob is one expected entry to materialize on the surface.
type writeJob struct {
	rel string
	fi  revision.FileInfo
}

// Recover rebuilds cap's sync surface from remote. Inventory checkout must
// already have happened (03 §6.1 step 5 orders checkout BEFORE recovery).
func Recover(ctx context.Context, cap *capsule.Capsule, remote Remote) (*Result, error) {
	wi := cap.WorkItemID
	// R3N-F4: an interrupted swap of THIS capsule (a previous life crashed
	// between the two renames) is reconciled before anything reads or
	// builds over the tree, so recovery starts from a consistent capsule.
	if err := ReconcilePendingSwap(cap.Dir); err != nil {
		return nil, err
	}
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
		if m.Seq <= head.Seq {
			chain = append(chain, m)
		}
	}
	sort.Slice(chain, func(i, j int) bool { return chain[i].Seq < chain[j].Seq })
	// Chain validation BEFORE the replay (05 §2.1/§2.2/§7.4): the listing
	// must be the contiguous seq 0..HEAD chain with unique parent links,
	// and HEAD's manifest_sha must be the tip's. A malformed listing is
	// rejected typed without touching the surface.
	if err := validateChain(chain, head); err != nil {
		return nil, err
	}
	// Replay window: newest checkpoint ≤ HEAD resets the set; every
	// incremental after it mutates it (05 §7.4). With the ops-bearing
	// listing (row 19 include=ops, amendment polyforge#14) kind and ops
	// arrive INLINE with the metadata - one round trip for the whole
	// chain; a metadata-only listing falls back to per-seq GetManifest.
	expected := map[string]revision.FileInfo{}
	for _, m := range chain {
		var man revision.Manifest
		if m.Kind != "" {
			man = revision.Manifest{
				WorkItemID: wi, Seq: m.Seq, Parent: m.Parent,
				Generation: m.Generation, Kind: m.Kind, Ops: m.Ops,
			}
		} else {
			fetched, ferr := remote.GetManifest(ctx, wi, m.Seq)
			if ferr != nil {
				return nil, fmt.Errorf("recover: manifest %d: %w", m.Seq, ferr)
			}
			// Row binding (05 §2.1): the fetched manifest must BE the revision
			// its listing row names — same WI, same seq, same generation.
			if fetched.WorkItemID != wi || fetched.Seq != m.Seq || fetched.Generation != m.Generation {
				return nil, fmt.Errorf("%w: manifest %d does not match its listing row (wi/seq/generation binding)", ErrChainInvalid, m.Seq)
			}
			man = fetched
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
				// Same canonical symlink facts the scan records (05 §6.1):
				// sha256 of the target text and its byte length — a bare
				// {IsSymlink, Target} entry would seed the publish state with
				// values no fresh scan ever produces, so every post-recovery
				// flush would report a false "changed" diff.
				expected[op.Path] = revision.SymlinkFileInfo(op.Target)
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

	// Prefetch phase (03 §6.3 rule 3 whole-operation atomicity; 05 §7.4):
	// fetch and sha-verify EVERY block the rebuild will need BEFORE the
	// first mutation of the current surface — the delete-then-write starts
	// only once every byte is in hand. A missing or corrupt block in a LATE
	// entry must leave the old surface intact, never a half-destroyed tree.
	blockDir, blockPaths, err := prefetchBlocks(ctx, cap, remote, wi, jobs)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(blockDir)

	// Current surface (the replacement needs to know what exists).
	current, err := revision.ScanCapsule(cap.Dir)
	if err != nil {
		return nil, err
	}
	// R2N-3: the replacement is built as a COMPLETE tree in a staging
	// directory INSIDE the capsule's parent — same filesystem, so the
	// final swap is a plain rename pair — and only then swapped in
	// atomically (old -> trash, staged -> live, remove trash). Until the
	// swap the LIVE surface is never touched: a failure at ANY point
	// (copy, expected-entry write, swap) leaves the previous surface
	// byte-identical. The old delete-then-write-in-place could die halfway
	// and leave a half-destroyed surface; a system-tmp staging tree could
	// not have been renamed over the capsule at all (cross-filesystem
	// rename fails with EXDEV).
	staging, err := os.MkdirTemp(filepath.Dir(cap.Dir), filepath.Base(cap.Dir)+".recover-staging-")
	if err != nil {
		return nil, err
	}
	// Everything that is NOT being replaced is copied into the staged tree:
	// non-surface content (capsule.json identity, repos/<name>/.git,
	// runtime-state leftovers, materialization targets), the harness/ tree
	// (never part of the replacement, 05 §7.4) and expected entries that
	// already match on disk byte-for-byte (the verify-and-keep optimization
	// for unchanged base checkouts — their blocks were never prefetched).
	if err := copyPreserved(cap.Dir, staging, current.Files, restorable); err != nil {
		os.RemoveAll(staging)
		return nil, fmt.Errorf("recover: staging copy failed (live surface untouched): %w", err)
	}
	// Phase 2: write/verify every expected entry INTO THE STAGED TREE, from
	// the prefetched bytes only — no remote call happens after this point
	// and no mutation of the live surface happens before the swap.
	stagedCap := &capsule.Capsule{Dir: staging, WorkItemID: cap.WorkItemID, Generation: cap.Generation}
	restored := 0
	for _, job := range jobs {
		if err := applyEntryFn(stagedCap, job.rel, job.fi, blockPaths); err != nil {
			os.RemoveAll(staging)
			return nil, fmt.Errorf("recover: staged apply failed for %s (live surface untouched): %w", job.rel, err)
		}
		restored++
	}
	// The swap: old surface to trash, staged tree to live, then trash
	// removal. A failure renaming the staged tree in rolls the old
	// surface back; the window where no tree sits at cap.Dir is the two
	// renames themselves (same directory, same filesystem).
	if err := swapCapsuleTree(cap.Dir, staging); err != nil {
		return nil, err
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

// validateChain checks the revision listing against the chain shape HEAD
// claims to cap (05 §2.1/§2.2/§7.4), BEFORE any replay or mutation:
//   - the chain starts at the seq-0 bootstrap with the null parent sentinel;
//   - seqs are contiguous with each revision's parent pointing at the
//     previous seq (the unique-parent rule);
//   - the chain's tip IS the HEAD row, and when both carry a manifest_sha
//     they agree (HEAD always names one complete published revision).
func validateChain(chain []revision.RevisionMeta, head *revision.HeadInfo) error {
	if len(chain) == 0 {
		return fmt.Errorf("%w: HEAD at %d but no revisions listed", ErrChainInvalid, head.Seq)
	}
	if chain[0].Seq != 0 {
		return fmt.Errorf("%w: chain must start at the seq-0 bootstrap, first listed seq is %d", ErrChainInvalid, chain[0].Seq)
	}
	if chain[0].Parent != nil {
		return fmt.Errorf("%w: seq 0 must carry the null parent sentinel (05 §2.1)", ErrChainInvalid)
	}
	prev := chain[0].Seq
	for _, m := range chain[1:] {
		if m.Seq != prev+1 {
			return fmt.Errorf("%w: gap — seq %d follows %d", ErrChainInvalid, m.Seq, prev)
		}
		if m.Parent == nil || *m.Parent != prev {
			return fmt.Errorf("%w: seq %d must name %d as its unique parent", ErrChainInvalid, m.Seq, prev)
		}
		prev = m.Seq
	}
	if prev != head.Seq {
		return fmt.Errorf("%w: chain tip %d ≠ HEAD %d", ErrChainInvalid, prev, head.Seq)
	}
	last := chain[len(chain)-1]
	if head.ManifestSha != "" && last.ManifestSha != "" && head.ManifestSha != last.ManifestSha {
		return fmt.Errorf("%w: HEAD manifest_sha %s ≠ tip row %s", ErrChainInvalid, head.ManifestSha, last.ManifestSha)
	}
	return nil
}

// prefetchBlocks fetches, decodes and sha-verifies every block the rebuild
// will need into a temporary directory, and returns (dir, sha → path).
// Entries whose on-disk state already matches (the idempotent base-checkout
// case) need no bytes. Called BEFORE the delete phase so a fetch or digest
// failure aborts with the current surface untouched (03 §6.3 rule 3).
func prefetchBlocks(ctx context.Context, cap *capsule.Capsule, remote Remote, wi string, jobs []writeJob) (string, map[string]string, error) {
	need := map[string]bool{}
	for _, job := range jobs {
		if job.fi.IsSymlink {
			continue
		}
		abs, err := cap.Path(job.rel)
		if err != nil {
			return "", nil, err
		}
		if entryAlreadyGood(abs, job.fi) {
			continue
		}
		need[job.fi.Sha256] = true
	}
	if len(need) == 0 {
		return "", map[string]string{}, nil
	}
	dir, err := os.MkdirTemp("", "pf-recover-blocks-")
	if err != nil {
		return "", nil, err
	}
	paths := map[string]string{}
	for sha := range need {
		block, err := remote.GetBlock(ctx, wi, sha)
		if err != nil {
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("recover: block %s: %w", sha, err)
		}
		content, err := revision.DecodeBlock(block)
		if err != nil {
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("recover: block %s undecodable: %w", sha, err)
		}
		if got := revision.DigestHex(content); got != sha {
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("recover: block %s digests to %s (content corruption)", sha, got)
		}
		p := filepath.Join(dir, sha)
		if err := os.WriteFile(p, content, 0o600); err != nil {
			os.RemoveAll(dir)
			return "", nil, err
		}
		paths[sha] = p
	}
	return dir, paths, nil
}

// entryAlreadyGood reports whether the on-disk state at abs already equals
// fi byte-for-byte and mode-for-mode (verify-and-keep, 05 §7.4 idempotent
// recovery for unchanged base checkouts).
func entryAlreadyGood(abs string, fi revision.FileInfo) bool {
	cur, err := os.Lstat(abs)
	if err != nil || !cur.Mode().IsRegular() || cur.Mode().Perm() != (os.FileMode(fi.Mode)&os.ModePerm) {
		return false
	}
	sum, serr := fileSha(abs)
	return serr == nil && sum == fi.Sha256
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

// applyEntryFn is the apply seam used by Recover; a test may replace it to
// inject a mid-apply failure (04 §10: the fault must prove the live
// surface survives byte-identical).
var applyEntryFn = applyEntry

// applyEntry materializes one expected entry from its PREFETCHED bytes
// (never a live remote call — the write phase must be purely local so a
// failure after the delete phase cannot be a fetch failure).
func applyEntry(cap *capsule.Capsule, rel string, fi revision.FileInfo, prefetched map[string]string) error {
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
	if entryAlreadyGood(abs, fi) {
		return nil
	}
	pref, ok := prefetched[fi.Sha256]
	if !ok {
		return fmt.Errorf("recover: block %s for %s was not prefetched", fi.Sha256, rel)
	}
	content, err := os.ReadFile(pref)
	if err != nil {
		return fmt.Errorf("recover: prefetched block %s unreadable: %w", fi.Sha256, err)
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

// copyPreserved copies every part of the LIVE capsule tree that the
// replacement must keep into the staged tree: non-surface content
// (capsule.json identity, repos/<name>/.git, runtime-state leftovers,
// .pi/ / .claude/ materialization targets, workflow.md — SYNC-INV-10),
// the whole harness/ tree (05 §7.4: "harness/<adapter>/ 整体不参与本次
// 替换") and sync-surface members that already match their expected bytes
// (verify-and-keep for unchanged base checkouts). Surface members that
// the expected set replaces or deletes are NOT copied — applyEntry
// materializes the replacements in the staged tree.
func copyPreserved(liveDir, stagingDir string, surface map[string]revision.FileInfo, expected map[string]revision.FileInfo) error {
	return filepath.WalkDir(liveDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(liveDir, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		dst := filepath.Join(stagingDir, rel)
		if d.IsDir() {
			// R3N-F5: when the expected set places a regular file or symlink
			// at this path, the directory must NOT be recreated in the staged
			// tree (nor its subtree copied): the old code recreated every old
			// directory, so a dir->file transition made the staged apply fail
			// on the rename (a file cannot replace a directory) and the whole
			// recovery aborted. Skipping the dir here leaves the path absent
			// for applyEntry to materialize the expected entry; ancestor
			// directories the expected set still needs are created by
			// applyEntry's MkdirAll.
			if _, replaced := expected[relSlash]; replaced {
				return fs.SkipDir
			}
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			return os.MkdirAll(dst, info.Mode().Perm())
		}
		if !strings.HasPrefix(relSlash, "harness/") {
			if _, isSurface := surface[relSlash]; isSurface {
				if exp, keep := expected[relSlash]; !keep || !entryAlreadyGood(p, exp) {
					return nil // replaced or deleted by the expected set
				}
			}
		}
		return copyEntry(p, dst, d)
	})
}

// copyEntry copies one non-directory entry into the staged tree. Symlinks
// are recreated as symlinks (never followed — a surface symlink is itself
// the recorded fact, and following one could copy data from outside the
// capsule).
func copyEntry(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chmod(dst, info.Mode().Perm())
	default:
		// Sockets, fifos, devices: never a legitimate capsule member —
		// skip rather than recreate something the safety rules never
		// anticipated.
		return nil
	}
}

// swapMarker is the crash journal of an in-flight capsule-tree swap
// (R3N-F4): written BEFORE the first rename and removed after the last,
// so the window between Rename(live, trash) and Rename(staged, live) —
// where the canonical capsule path is ABSENT and the old rollback code
// could no longer help — is recoverable deterministically from the three
// recorded paths.
type swapMarker struct {
	Live   string `json:"live"`
	Staged string `json:"staged"`
	Trash  string `json:"trash"`
}

// swapMarkerPath is the fixed marker location for a capsule directory:
// <capsulesRoot>/<wi_id>.recover-swap.json, next to the tree it describes.
func swapMarkerPath(live string) string {
	return filepath.Join(filepath.Dir(live), filepath.Base(live)+".recover-swap.json")
}

// writeSwapMarker durably records the in-flight swap (tmp+fsync+rename,
// the same atomic-write contract as the capsule state files) BEFORE the
// first rename touches anything live.
func writeSwapMarker(m *swapMarker) error {
	blob, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	mp := swapMarkerPath(m.Live) + ".tmp"
	f, err := os.OpenFile(mp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(blob); werr != nil {
		f.Close()
		os.Remove(mp)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		os.Remove(mp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(mp)
		return cerr
	}
	return os.Rename(mp, swapMarkerPath(m.Live))
}

// pathExists reports whether the path is present (any type).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil || !os.IsNotExist(err)
}

// ErrCapsuleSurfaceLost is the typed human-attention signal that a
// pending swap marker found the live, staged AND trash trees ALL absent
// (R4N-3): the canonical capsule tree is gone with no fallback to
// complete from or restore to. The marker is RETAINED and nothing is
// removed — the old code cleared the marker and reported success,
// silently converting a permanently lost capsule into a "reconciled" one.
var ErrCapsuleSurfaceLost = errors.New("recover: capsule surface lost under a pending swap marker")

// ErrSwapMarkerUntrusted is the typed human-attention refusal of a swap
// marker whose recorded paths fail confinement validation (R4N-6): a
// path resolving outside the expected capsules root, or a staged/trash
// name the swap protocol would never have written. The marker is
// retained for forensics and NOTHING is acted on — a corrupted marker
// must never drive renames or removals outside the capsules root.
var ErrSwapMarkerUntrusted = errors.New("recover: swap marker failed confinement validation")

// validateSwapMarkerPaths proves, BEFORE any filesystem action, that the
// marker's three recorded paths are ones this reconciliation may act on
// (R4N-6): every path must resolve inside the expected capsules root,
// and staged/trash must carry the swap protocol's own naming
// (<live-base>.recover-staging-* / <live-base>.recover-trash-* — the
// only names swapCapsuleTree ever writes). A marker failing any check is
// a LOUD typed error and is never acted on; the caller retains it.
func validateSwapMarkerPaths(capsulesRoot string, m *swapMarker) error {
	if m.Live == "" || m.Staged == "" || m.Trash == "" {
		return fmt.Errorf("%w: empty path in marker (live=%q staged=%q trash=%q)", ErrSwapMarkerUntrusted, m.Live, m.Staged, m.Trash)
	}
	for _, p := range []struct{ name, path string }{
		{"live", m.Live}, {"staged", m.Staged}, {"trash", m.Trash},
	} {
		if err := ensureWithinRoot(capsulesRoot, p.path); err != nil {
			return fmt.Errorf("%w: %s path %s: %v", ErrSwapMarkerUntrusted, p.name, p.path, err)
		}
	}
	base := filepath.Base(m.Live)
	if prefix := base + ".recover-staging-"; !strings.HasPrefix(filepath.Base(m.Staged), prefix) {
		return fmt.Errorf("%w: staged path %q does not carry the swap protocol naming %q*", ErrSwapMarkerUntrusted, m.Staged, prefix)
	}
	if prefix := base + ".recover-trash-"; !strings.HasPrefix(filepath.Base(m.Trash), prefix) {
		return fmt.Errorf("%w: trash path %q does not carry the swap protocol naming %q*", ErrSwapMarkerUntrusted, m.Trash, prefix)
	}
	return nil
}

// ensureWithinRoot proves that p resolves inside root: symlinks are
// resolved as far as the path exists and the not-yet-existing tail is
// re-attached verbatim (a missing tail cannot escape by itself); a path
// that cannot be resolved at all is refused (fail closed — an unprovable
// containment is a refusal, never an assumption).
func ensureWithinRoot(root, p string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootResolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return fmt.Errorf("capsules root %s unresolvable: %w", root, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	resolved := abs
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		resolved = r
	} else {
		// The path itself does not exist (mid-swap residue): resolve its
		// deepest existing ancestor and re-attach the missing tail.
		dir, base := filepath.Split(abs)
		rd, derr := filepath.EvalSymlinks(filepath.Clean(dir))
		if derr != nil {
			return fmt.Errorf("unresolvable: %w", derr)
		}
		resolved = filepath.Join(rd, base)
	}
	rel, rerr := filepath.Rel(rootResolved, resolved)
	if rerr != nil {
		return rerr
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("resolves to %s, outside the capsules root %s", resolved, rootResolved)
	}
	return nil
}

// reconcileSwapMarker completes or rolls back an interrupted swap from
// the marker's three recorded paths (R3N-F4). Deterministic rules, by
// what survives the crash:
//   - live absent, staged present: the crash hit BETWEEN the two renames
//     (or the in-process rollback failed) — COMPLETE the swap: the staged
//     tree is fully built and sha-verified, renaming it in is strictly
//     better than restoring the older surface.
//   - live present, staged present: live is either the untouched old
//     surface (crash before the first rename) or the completed new one
//     with a stray staged tree — ROLLBACK to live, drop the staged tree.
//   - live absent, staged absent, trash present: both renames failed AND
//     the in-process rollback failed — restore the old surface from trash.
//   - live absent, staged absent, trash absent (R4N-3): the canonical
//     capsule tree is gone with no fallback — RETAIN the marker and fail
//     typed ErrCapsuleSurfaceLost for human attention; removing the
//     marker here would report a success that silently loses the capsule.
//
// R4N-6: before ANY filesystem action the marker's paths must pass
// confinement validation (validateSwapMarkerPaths) — a corrupted marker
// that would drive renames/removals outside the capsules root is
// refused typed with the marker retained, never acted on.
//
// The trash tree is removed and the marker cleared in every branch that
// reaches a consistent single-tree state; the marker is only removed
// after the capsule path holds exactly one tree again.
func reconcileSwapMarker(capsulesRoot string, m *swapMarker) error {
	if err := validateSwapMarkerPaths(capsulesRoot, m); err != nil {
		return err
	}
	live, staged, trash := pathExists(m.Live), pathExists(m.Staged), pathExists(m.Trash)
	switch {
	case !live && staged:
		if err := os.Rename(m.Staged, m.Live); err != nil {
			return fmt.Errorf("recover: completing interrupted swap for %s failed: %w", m.Live, err)
		}
	case live && staged:
		if err := os.RemoveAll(m.Staged); err != nil {
			return fmt.Errorf("recover: dropping stranded staged tree %s failed: %w", m.Staged, err)
		}
	case !live && !staged && trash:
		if err := os.Rename(m.Trash, m.Live); err != nil {
			return fmt.Errorf("recover: CRITICAL: restoring the old surface from %s failed: %w — the previous capsule surface still sits there", m.Trash, err)
		}
	case !live && !staged && !trash:
		// R4N-3: nothing survives — the capsule's canonical tree is missing
		// forever, and no rule above can restore it. NEVER a silent
		// success: retain the marker (both as the forensic record of what
		// was in flight and so the next boot fails loudly again) and
		// surface the loss typed for human attention.
		return fmt.Errorf("%w: live=%q staged=%q trash=%q are all absent — the canonical capsule tree cannot be restored; marker retained, human attention required", ErrCapsuleSurfaceLost, m.Live, m.Staged, m.Trash)
	}
	if trash {
		// Cleanup of the replaced surface: correctness is already restored,
		// so a failed removal is logged, never fatal.
		if err := os.RemoveAll(m.Trash); err != nil {
			fmt.Fprintf(os.Stderr, "recover: replaced surface at %s could not be removed (left on disk for audit): %v\n", m.Trash, err)
		}
	}
	return os.Remove(swapMarkerPath(m.Live))
}

// ReconcilePendingSwap completes or rolls back an interrupted swap for one
// capsule directory (R3N-F4). A no-op when no marker is pending; the
// startup path (daemon New + RecoverFromState's capsule open) must call it
// BEFORE any capsule.Create/Open could lay a fresh tree over the gap.
func ReconcilePendingSwap(capsuleDir string) error {
	mp := swapMarkerPath(capsuleDir)
	raw, err := os.ReadFile(mp)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var m swapMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("recover: swap marker %s is unreadable: %w", mp, err)
	}
	// The marker sits next to the tree it describes: its parent IS the
	// capsules root the recorded paths must confine to (R4N-6).
	return reconcileSwapMarker(filepath.Dir(capsuleDir), &m)
}

// ReconcileInterruptedSwaps is the startup sweep over a capsules root
// (R3N-F4): every pending *.recover-swap.json marker is completed or
// rolled back, and staging/trash trees stranded OUTSIDE any swap window
// (a crash during the copy phase — before the marker was written — or a
// failed trash removal) are removed. Staging/trash paths still referenced
// by a remaining marker are left to that marker's reconciliation.
func ReconcileInterruptedSwaps(capsulesRoot string) error {
	markers, err := filepath.Glob(filepath.Join(capsulesRoot, "*.recover-swap.json"))
	if err != nil {
		return err
	}
	var firstErr error
	referenced := map[string]bool{}
	for _, mp := range markers {
		raw, rerr := os.ReadFile(mp)
		if rerr != nil {
			firstErr = fmt.Errorf("recover: swap marker %s unreadable: %w", mp, rerr)
			continue
		}
		var m swapMarker
		if uerr := json.Unmarshal(raw, &m); uerr != nil {
			firstErr = fmt.Errorf("recover: swap marker %s unparsable: %w", mp, uerr)
			continue
		}
		referenced[m.Staged] = true
		referenced[m.Trash] = true
		if cerr := reconcileSwapMarker(capsulesRoot, &m); cerr != nil && firstErr == nil {
			firstErr = cerr
		}
	}
	// Stranded staging/trash trees with no pending marker are garbage from
	// an interrupted recovery whose swap either never started or already
	// completed; remove them so they cannot accumulate across crashes.
	for _, pattern := range []string{"*.recover-staging-*", "*.recover-trash-*"} {
		leftovers, lerr := filepath.Glob(filepath.Join(capsulesRoot, pattern))
		if lerr != nil {
			continue
		}
		for _, p := range leftovers {
			if referenced[p] {
				continue
			}
			if rerr := os.RemoveAll(p); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
		}
	}
	return firstErr
}

// swapCapsuleTree atomically replaces the live capsule directory with the
// staged tree (R2N-3): rename live -> trash, rename staged -> live, remove
// trash. Both renames happen inside the capsule's parent (same filesystem)
// so they cannot fail with EXDEV. A failure at ANY point leaves the OLD
// surface in place: before the first rename nothing live was touched; a
// failed second rename rolls the first one back.
//
// R3N-F4: the swap is now a CRASH-RECOVERABLE protocol. The old code's
// rollback only existed in-process: a crash between Rename(live, trash)
// and Rename(staged, live) left the canonical capsule path ABSENT, the
// startup path had no reconciliation for the stranded staging/trash
// trees, and the next capsule.Create would silently build a fresh husk
// over the gap. A swap journal naming the three paths is now written
// BEFORE the first rename and removed after the last; the next boot (or
// a fresh Recover over the same capsule) finds the marker and completes
// or rolls back deterministically (ReconcilePendingSwap).
func swapCapsuleTree(live, staged string) error {
	parent := filepath.Dir(live)
	// Reserve a unique trash name in the parent, then drop the placeholder
	// directory: rename needs a NON-existent target.
	trash, err := os.MkdirTemp(parent, filepath.Base(live)+".recover-trash-")
	if err != nil {
		return err
	}
	if err := os.Remove(trash); err != nil {
		return err
	}
	marker := &swapMarker{Live: live, Staged: staged, Trash: trash}
	if err := writeSwapMarker(marker); err != nil {
		os.RemoveAll(staged)
		return fmt.Errorf("recover: could not journal the capsule swap: %w", err)
	}
	if err := os.Rename(live, trash); err != nil {
		os.Remove(swapMarkerPath(live)) // nothing live was touched: the swap never started
		os.RemoveAll(staged)
		return fmt.Errorf("recover: could not move the old surface aside: %w", err)
	}
	if err := os.Rename(staged, live); err != nil {
		// Rollback: put the old surface back exactly where it was.
		if rb := os.Rename(trash, live); rb != nil {
			// The marker stays: it names the staged and trash trees so the
			// next boot's ReconcilePendingSwap can still COMPLETE the swap.
			return fmt.Errorf("recover: CRITICAL: staged tree swap failed (%v) AND the old surface could not be restored (%v) — the previous surface still sits at %s", err, rb, trash)
		}
		os.Remove(swapMarkerPath(live)) // the old surface is live again: the swap is over
		os.RemoveAll(staged)
		return fmt.Errorf("recover: staged tree swap failed, old surface restored intact: %w", err)
	}
	// The new surface is live and the swap is complete: clear the journal
	// BEFORE the (non-correctness-relevant) trash removal, so a crash here
	// leaves at most a stranded trash tree for the startup sweep.
	if err := os.Remove(swapMarkerPath(live)); err != nil {
		fmt.Fprintf(os.Stderr, "recover: the swap journal at %s could not be removed: %v\n", swapMarkerPath(live), err)
	}
	// Trash removal is cleanup, not correctness.
	if err := os.RemoveAll(trash); err != nil {
		fmt.Fprintf(os.Stderr, "recover: the replaced surface at %s could not be removed (left on disk for audit): %v\n", trash, err)
	}
	return nil
}
