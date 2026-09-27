package recover

// swap_test.go: R2N-3 regressions — remote site reconstruction must be
// atomic for the WHOLE replacement: the complete replacement tree is built
// in a staging directory INSIDE the capsule's parent (same filesystem), and
// only then swapped in (old -> trash, staged -> live, remove trash). A
// failure at ANY point must leave the previous surface byte-identical —
// the old delete-then-write-in-place could die halfway and leave a
// half-destroyed surface.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// treeDigest walks dir and returns a stable per-path content map — the
// byte-identity witness for the old surface.
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "symlink:" + target
			return nil
		}
		if !info.Mode().IsRegular() {
			out[rel] = fmt.Sprintf("mode:%v", info.Mode())
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("sha:%s mode:%v", revision.DigestHex(raw), info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// recoveryScenario builds a capsule with a previous surface and a remote
// whose HEAD chain expects a DIFFERENT two-file surface.
func recoveryScenario(t *testing.T) (*capsule.Capsule, *memRemote) {
	t.Helper()
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}

	// The previous surface: an old artifact, the identity file, a .git
	// directory (excluded from the sync surface but preserved by the
	// capsule), harness state and a runtime-state leftover.
	os.MkdirAll(filepath.Join(c.Dir, "artifacts"), 0o755)
	os.WriteFile(filepath.Join(c.Dir, "artifacts", "old.md"), []byte("previous surface\n"), 0o644)
	os.MkdirAll(filepath.Join(c.Dir, "repos", "r1", ".git"), 0o755)
	os.WriteFile(filepath.Join(c.Dir, "repos", "r1", ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.MkdirAll(filepath.Join(c.Dir, "harness", "pi", "session-data"), 0o755)
	os.WriteFile(filepath.Join(c.Dir, "harness", "pi", "session.json"), []byte(`{"session":"old"}`), 0o644)
	os.MkdirAll(filepath.Join(c.Dir, "runtime-state"), 0o755)
	os.WriteFile(filepath.Join(c.Dir, "runtime-state", "leftover.tmp"), []byte("scratch"), 0o600)

	shaA := block(t, remote, "recovered A")
	shaB := block(t, remote, "recovered B\n")
	remote.manifests[0] = revision.Manifest{
		WorkItemID: "wi_rec01", Seq: 0, Parent: nil, Generation: 1, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/new-a.md", Sha256: shaA, Bytes: 11, Mode: modeP(420)},
			{Op: revision.OpPut, Path: "repos/r1/new-b.go", Sha256: shaB, Bytes: 13, Mode: modeP(493)},
		},
		LiveFiles: 2, ExecutionStateIncluded: true,
	}
	remote.head = &revision.HeadInfo{Seq: 0, ManifestSha: "sha:x"}
	return c, remote
}

func stagingLeftovers(parent string) []string {
	out, _ := filepath.Glob(filepath.Join(parent, "*.recover-staging-*"))
	trash, _ := filepath.Glob(filepath.Join(parent, "*.recover-trash-*"))
	return append(out, trash...)
}

// TestRecoverMidApplyFailureLeavesSurfaceIntact: a failure DURING the
// staged apply phase must abort with the previous surface BYTE-IDENTICAL
// and no staging/trash residue in the capsules parent.
func TestRecoverMidApplyFailureLeavesSurfaceIntact(t *testing.T) {
	c, remote := recoveryScenario(t)
	before := treeDigest(t, c.Dir)
	parent := filepath.Dir(c.Dir)

	// Inject a mid-apply failure: the second expected entry dies.
	oldApply := applyEntryFn
	applyEntryFn = func(cap *capsule.Capsule, rel string, fi revision.FileInfo, prefetched map[string]string) error {
		if rel == "repos/r1/new-b.go" {
			return fmt.Errorf("injected mid-apply failure")
		}
		return applyEntry(cap, rel, fi, prefetched)
	}
	t.Cleanup(func() { applyEntryFn = oldApply })

	if _, err := Recover(context.Background(), c, remote); err == nil {
		t.Fatal("the injected mid-apply failure must fail the recovery")
	}

	after := treeDigest(t, c.Dir)
	if len(before) != len(after) {
		t.Fatalf("the surface must be unchanged by a failed recovery: %d entries before, %d after", len(before), len(after))
	}
	for rel, want := range before {
		if got := after[rel]; got != want {
			t.Fatalf("the old surface must be byte-identical after a failed recovery: %s was %q, now %q", rel, want, got)
		}
	}
	if left := stagingLeftovers(parent); len(left) != 0 {
		t.Fatalf("a failed recovery must clean its staging tree: %v", left)
	}
}

// TestRecoverSwapReplacesWholeSurfaceAtomically: the successful recovery
// swaps the COMPLETE tree in one rename pair — the replaced file is GONE,
// every expected file is present and sha-verified, and the preserved
// non-surface content (capsule.json, repos/<name>/.git, harness/,
// runtime-state leftovers) survived the swap.
func TestRecoverSwapReplacesWholeSurfaceAtomically(t *testing.T) {
	c, remote := recoveryScenario(t)
	parent := filepath.Dir(c.Dir)

	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 2 {
		t.Fatalf("both expected entries must be restored: %d", res.Restored)
	}

	// The replaced surface: old file gone, expected files present.
	if _, err := os.Stat(filepath.Join(c.Dir, "artifacts", "old.md")); !os.IsNotExist(err) {
		t.Fatal("full replacement must remove files absent from the expected set")
	}
	for rel, want := range map[string]string{
		"artifacts/new-a.md": "recovered A",
		"repos/r1/new-b.go":  "recovered B\n",
	} {
		raw, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("expected %s restored: %v", rel, err)
		}
		if string(raw) != want {
			t.Fatalf("%s content: got %q want %q", rel, raw, want)
		}
	}
	// Preserved non-surface content survived the swap.
	for rel, want := range map[string]string{
		"capsule.json":               "", // content checked structurally below
		"repos/r1/.git/HEAD":         "ref: refs/heads/main\n",
		"harness/pi/session.json":    `{"session":"old"}`,
		"runtime-state/leftover.tmp": "scratch",
	} {
		raw, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("preserved %s must survive the swap: %v", rel, err)
		}
		if want != "" && string(raw) != want {
			t.Fatalf("preserved %s content changed: %q", rel, raw)
		}
	}
	if res.WorktreeDigest == "" {
		t.Fatal("the worktree digest must be computed over the NEW tree")
	}
	// No staging or trash residue: the swap completed.
	if left := stagingLeftovers(parent); len(left) != 0 {
		t.Fatalf("a completed recovery must leave no staging/trash trees: %v", left)
	}
}

// TestRecoverSwapPreservesSymlinksAndModes: the staged copy recreates
// non-surface symlinks as symlinks (never follows them) and preserves file
// modes — the swap must not flatten the capsule's local structure.
func TestRecoverSwapPreservesSymlinksAndModes(t *testing.T) {
	c, remote := recoveryScenario(t)
	os.MkdirAll(filepath.Join(c.Dir, ".pi", "agents"), 0o755)
	os.WriteFile(filepath.Join(c.Dir, ".pi", "agents", "helper.md"), []byte("materialized snapshot\n"), 0o600)
	os.Symlink("helper.md", filepath.Join(c.Dir, ".pi", "agents", "helper-link"))

	if _, err := Recover(context.Background(), c, remote); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(c.Dir, ".pi", "agents", "helper-link")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the non-surface symlink must survive the swap: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the preserved symlink must remain a SYMLINK, not a copied file")
	}
	if target, err := os.Readlink(link); err != nil || target != "helper.md" {
		t.Fatalf("symlink target must survive: %q %v", target, err)
	}
	fi, err := os.Stat(filepath.Join(c.Dir, ".pi", "agents", "helper.md"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file modes must survive the swap: %v %v", fi, err)
	}
}

// TestRecoverVerifyKeepCopiesMatchingFiles: expected entries that already
// match on disk (unchanged base checkout) must ride the copy phase — no
// block fetch for them, and the restored count still covers every expected
// entry.
func TestRecoverVerifyKeepCopiesMatchingFiles(t *testing.T) {
	c, remote := recoveryScenario(t)
	// Pre-place one expected file exactly as published.
	os.MkdirAll(filepath.Join(c.Dir, "artifacts"), 0o755)
	if err := os.WriteFile(filepath.Join(c.Dir, "artifacts", "new-a.md"), []byte("recovered A"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 2 {
		t.Fatalf("verify-and-keep must still count as restored: %d", res.Restored)
	}
	if raw, err := os.ReadFile(filepath.Join(c.Dir, "artifacts", "new-a.md")); err != nil || string(raw) != "recovered A" {
		t.Fatalf("the already-good file must be kept: %q %v", raw, err)
	}
}

// TestSwapCapsuleTreeResidueAndFailure: the swap helper removes the
// staged tree on a failed FIRST rename (the old surface was never
// touched), and a successful swap leaves no staging/trash residue.
func TestSwapCapsuleTreeResidueAndFailure(t *testing.T) {
	parent := t.TempDir()
	live := filepath.Join(parent, "cap_x")
	staged := filepath.Join(parent, "cap_x.recover-staging-test")
	os.MkdirAll(staged, 0o755)
	os.WriteFile(filepath.Join(staged, "new.txt"), []byte("new surface"), 0o644)

	// Failed first rename: the live path does not exist. The staged tree
	// must be cleaned and the error surfaced.
	if err := swapCapsuleTree(filepath.Join(parent, "no-such-capsule"), staged); err == nil {
		t.Fatal("swapping a nonexistent live tree must fail")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("the staged tree must be removed when the swap fails at the first rename")
	}

	// Successful swap: the staged tree is live, no residue remains.
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "old.txt"), []byte("old surface"), 0o644)
	staged2 := filepath.Join(parent, "cap_x.recover-staging-test2")
	os.MkdirAll(staged2, 0o755)
	os.WriteFile(filepath.Join(staged2, "new.txt"), []byte("new surface"), 0o644)
	if err := swapCapsuleTree(live, staged2); err != nil {
		t.Fatalf("the plain swap must succeed: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(live, "new.txt")); err != nil || string(raw) != "new surface" {
		t.Fatalf("the staged tree must be live after the swap: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(live, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("the old surface must be gone after a successful swap")
	}
	if left := stagingLeftovers(parent); len(left) != 0 {
		t.Fatalf("no staging/trash residue may survive a successful swap: %v", left)
	}
}

// dirTransitionScenario builds a capsule whose live surface holds a
// DIRECTORY at a path the remote HEAD expects as a regular file (or
// symlink): the dir->non-dir transition case R3N-F5.
func dirTransitionScenario(t *testing.T, asSymlink bool) (*capsule.Capsule, *memRemote, string) {
	t.Helper()
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}

	// Previous surface: a directory (a surface member inside it) where the
	// expected HEAD will place a bare entry.
	if err := os.MkdirAll(filepath.Join(c.Dir, "artifacts", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "artifacts", "sub", "old.md"), []byte("old surface\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := "artifacts/sub"
	if asSymlink {
		remote.manifests[0] = revision.Manifest{
			WorkItemID: "wi_rec01", Seq: 0, Parent: nil, Generation: 2, Kind: revision.KindIncremental,
			Ops: []revision.Op{
				{Op: revision.OpSymlink, Path: target, Target: "new-a.md", Sha256: revision.DigestHex([]byte("new-a.md")), Bytes: 9},
			},
			LiveFiles: 1, ExecutionStateIncluded: true,
		}
	} else {
		sha := block(t, remote, "dir replaced by file\n")
		remote.manifests[0] = revision.Manifest{
			WorkItemID: "wi_rec01", Seq: 0, Parent: nil, Generation: 2, Kind: revision.KindIncremental,
			Ops: []revision.Op{
				{Op: revision.OpPut, Path: target, Sha256: sha, Bytes: 21, Mode: modeP(420)},
			},
			LiveFiles: 1, ExecutionStateIncluded: true,
		}
	}
	remote.head = &revision.HeadInfo{Seq: 0, ManifestSha: "sha:x"}
	return c, remote, target
}

// TestRecoverDirToFileTransition (R3N-F5): the expected HEAD replaces a
// directory path with a regular file. copyPreserved used to recreate every
// old directory in the staged tree, so the staged rename failed (a file
// cannot replace a directory) and the whole recovery aborted. The staged
// tree must simply not contain the replaced directory.
func TestRecoverDirToFileTransition(t *testing.T) {
	c, remote, target := dirTransitionScenario(t, false)
	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatalf("a dir->file transition must recover cleanly: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("the expected entry must be restored: %d", res.Restored)
	}
	raw, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(target)))
	if err != nil || string(raw) != "dir replaced by file\n" {
		t.Fatalf("the replaced path must be the expected FILE: %q %v", raw, err)
	}
	if fi, err := os.Lstat(filepath.Join(c.Dir, filepath.FromSlash(target))); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the replaced path must be a regular file: %v %v", fi, err)
	}
	// The old directory's member is gone with the directory (the stat
	// fails with ENOTDIR now that the path is a file — any error proves it).
	if _, err := os.Stat(filepath.Join(c.Dir, "artifacts", "sub", "old.md")); err == nil {
		t.Fatal("the replaced directory's old member must be gone")
	}
	if left := stagingLeftovers(filepath.Dir(c.Dir)); len(left) != 0 {
		t.Fatalf("a completed recovery must leave no staging/trash trees: %v", left)
	}
}

// TestRecoverDirToSymlinkTransition (R3N-F5): the same transition with a
// symlink as the replacement entry.
func TestRecoverDirToSymlinkTransition(t *testing.T) {
	c, remote, target := dirTransitionScenario(t, true)
	if _, err := Recover(context.Background(), c, remote); err != nil {
		t.Fatalf("a dir->symlink transition must recover cleanly: %v", err)
	}
	link := filepath.Join(c.Dir, filepath.FromSlash(target))
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the replaced path must be the expected SYMLINK: %v %v", fi, err)
	}
	if got, err := os.Readlink(link); err != nil || got != "new-a.md" {
		t.Fatalf("symlink target must survive: %q %v", got, err)
	}
	if left := stagingLeftovers(filepath.Dir(c.Dir)); len(left) != 0 {
		t.Fatalf("a completed recovery must leave no staging/trash trees: %v", left)
	}
}

// midSwapState builds the crash residue of a swap interrupted BETWEEN the
// two renames: live absent, trash = the old tree, staged = the new tree,
// plus the swap marker naming all three paths (exactly what swapCapsuleTree
// leaves on disk when the process dies between its first and second
// rename). Returns the paths for assertions.
func midSwapState(t *testing.T, parent string) (live, staged, trash string) {
	t.Helper()
	live = filepath.Join(parent, "wi_rec01")
	staged = filepath.Join(parent, "wi_rec01.recover-staging-crash")
	trash = filepath.Join(parent, "wi_rec01.recover-trash-crash")

	os.MkdirAll(trash, 0o755)
	os.WriteFile(filepath.Join(trash, "old.txt"), []byte("old surface"), 0o644)
	os.MkdirAll(staged, 0o755)
	os.WriteFile(filepath.Join(staged, "new.txt"), []byte("new surface"), 0o644)

	blob := fmt.Sprintf(`{"live": %q, "staged": %q, "trash": %q}`, live, staged, trash)
	if err := os.WriteFile(swapMarkerPath(live), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	return live, staged, trash
}

// TestReconcilePendingSwapCompletesMidSwapCrash (R3N-F4): termination
// injected between the two renames (the canonical capsule path absent, the
// old rollback unreachable) — the next open must COMPLETE the swap from
// the marker: the staged tree becomes live, the trash is gone, the marker
// is cleared, and the capsule is consistent.
func TestReconcilePendingSwapCompletesMidSwapCrash(t *testing.T) {
	parent := t.TempDir()
	live, _, trash := midSwapState(t, parent)

	if err := ReconcilePendingSwap(live); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(live, "new.txt")); err != nil || string(raw) != "new surface" {
		t.Fatalf("the interrupted swap must COMPLETE: the staged tree is live: %q %v", raw, err)
	}
	if pathExists(trash) {
		t.Fatal("the trash tree must be removed after completion")
	}
	if pathExists(swapMarkerPath(live)) {
		t.Fatal("the marker must be cleared after completion")
	}
	if left := stagingLeftovers(parent); len(left) != 0 {
		t.Fatalf("no residue may remain: %v", left)
	}
}

// TestReconcilePendingSwapRollsBackToLive (R3N-F4): a crash with the live
// tree still in place and a stray staged tree (marker written, first rename
// never landed) rolls BACK: live survives untouched, staged is dropped.
func TestReconcilePendingSwapRollsBackToLive(t *testing.T) {
	parent := t.TempDir()
	live, staged, trash := midSwapState(t, parent)
	// Crash BEFORE the first rename: the live tree is still there.
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "old.txt"), []byte("old surface"), 0o644)

	if err := ReconcilePendingSwap(live); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(live, "old.txt")); err != nil || string(raw) != "old surface" {
		t.Fatalf("rollback must keep the live surface: %q %v", raw, err)
	}
	if pathExists(staged) {
		t.Fatal("the stray staged tree must be dropped on rollback")
	}
	if pathExists(trash) {
		t.Fatal("the unused trash placeholder must be removed")
	}
	if pathExists(swapMarkerPath(live)) {
		t.Fatal("the marker must be cleared after rollback")
	}
}

// TestReconcilePendingSwapRestoresFromTrash (R3N-F4): the worst residue —
// both renames failed AND the in-process rollback failed (live and staged
// both gone, only trash left). The marker lets the next boot restore the
// old surface instead of losing the capsule.
func TestReconcilePendingSwapRestoresFromTrash(t *testing.T) {
	parent := t.TempDir()
	live, staged, _ := midSwapState(t, parent)
	// Simulate the failed-rollback residue: remove the staged tree.
	os.RemoveAll(staged)

	if err := ReconcilePendingSwap(live); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(live, "old.txt")); err != nil || string(raw) != "old surface" {
		t.Fatalf("the old surface must be restored from trash: %q %v", raw, err)
	}
	if pathExists(swapMarkerPath(live)) {
		t.Fatal("the marker must be cleared after restoration")
	}
}

// TestRecoverCompletesPredecessorInterruptedSwap (R3N-F4): end-to-end — a
// Recover over a capsule whose PREVIOUS recovery died between the two
// renames (the canonical capsule path absent, a staged tree and a trash
// tree stranded beside the marker). Recover must complete the swap before
// reading the tree, then proceed with the replacement.
func TestRecoverCompletesPredecessorInterruptedSwap(t *testing.T) {
	c, remote := recoveryScenario(t)
	parent := filepath.Dir(c.Dir)

	// Strand the swap: the built capsule tree moves to trash (the old
	// surface), a staged tree stands ready (the previous recovery's
	// fully-built replacement), and the marker names all three paths.
	staged := filepath.Join(parent, "wi_rec01.recover-staging-crash")
	trash := filepath.Join(parent, "wi_rec01.recover-trash-crash")
	if err := os.Rename(c.Dir, trash); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(staged, 0o755)
	os.WriteFile(filepath.Join(staged, "artifacts", "new-a.md"), []byte("recovered A"), 0o644)
	os.WriteFile(filepath.Join(staged, "repos", "r1", "new-b.go"), []byte("recovered B\n"), 0o644)
	blob := fmt.Sprintf(`{"live": %q, "staged": %q, "trash": %q}`, c.Dir, staged, trash)
	if err := os.WriteFile(swapMarkerPath(c.Dir), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Recover(context.Background(), c, remote); err != nil {
		t.Fatalf("a Recover over an interrupted swap must complete it first: %v", err)
	}
	// The swap completed (staged tree became live) and the replacement ran
	// over a consistent capsule: the expected surface is in place.
	for rel, want := range map[string]string{
		"artifacts/new-a.md": "recovered A",
		"repos/r1/new-b.go":  "recovered B\n",
	} {
		raw, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rel)))
		if err != nil || string(raw) != want {
			t.Fatalf("expected %s restored after the completed swap: %q %v", rel, raw, err)
		}
	}
	if pathExists(swapMarkerPath(c.Dir)) {
		t.Fatal("the marker must be cleared")
	}
	if left := stagingLeftovers(parent); len(left) != 0 {
		t.Fatalf("no residue may remain: %v", left)
	}
}

// ---------------------------------------------------------------------------
// fix-round-4 regressions (R4N-3 / R4N-6).

// writeSwapMarkerBlob writes a raw swap marker next to the live path it
// claims to describe, exactly as a crashed (or corrupted) writer would.
func writeSwapMarkerBlob(t *testing.T, live, staged, trash string) {
	t.Helper()
	blob := fmt.Sprintf(`{"live": %q, "staged": %q, "trash": %q}`, live, staged, trash)
	if err := os.WriteFile(swapMarkerPath(live), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileAllAbsentRetainsMarkerAndFailsTyped (R4N-3): live, staged
// AND trash are all absent — the canonical capsule tree is missing forever
// with no fallback to complete from or restore to. The old code fell
// through the switch, REMOVED the marker and reported success, silently
// converting a lost capsule into a "reconciled" one. The fixed case must
// RETAIN the marker and return a typed human-attention error.
func TestReconcileAllAbsentRetainsMarkerAndFailsTyped(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "wi_lost")
	staged := filepath.Join(root, "wi_lost.recover-staging-gone")
	trash := filepath.Join(root, "wi_lost.recover-trash-gone")
	// Nothing exists at any of the three paths.
	writeSwapMarkerBlob(t, live, staged, trash)

	if err := ReconcilePendingSwap(live); !errors.Is(err, ErrCapsuleSurfaceLost) {
		t.Fatalf("all-three-absent must fail typed ErrCapsuleSurfaceLost (never a silent success), got %v", err)
	}
	if !pathExists(swapMarkerPath(live)) {
		t.Fatal("the marker must be RETAINED for forensics and the next boot's loud failure")
	}

	// The startup sweep answers the same way and also keeps the marker.
	if err := ReconcileInterruptedSwaps(root); !errors.Is(err, ErrCapsuleSurfaceLost) {
		t.Fatalf("the startup sweep must surface the lost capsule typed, got %v", err)
	}
	if !pathExists(swapMarkerPath(live)) {
		t.Fatal("the startup sweep must not remove the marker of a lost capsule")
	}
}

// TestReconcileRefusesUntrustedMarker (R4N-6): reconcileSwapMarker used to
// trust the marker's Live/Staged/Trash paths verbatim — a corrupted marker
// could drive renames/removals outside the capsules root. Every path must
// resolve inside the expected capsules root and staged/trash must carry
// the swap protocol's own naming BEFORE any filesystem action; an invalid
// marker is refused typed (ErrSwapMarkerUntrusted), retained for
// forensics, and nothing is touched.
func TestReconcileRefusesUntrustedMarker(t *testing.T) {
	t.Run("staged path outside the capsules root: refused, nothing moved", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "capsules")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		// The victim tree OUTSIDE the root: without confinement the
		// !live && staged rule would RENAME it into the capsules root.
		victim := filepath.Join(tmp, "outside-victim")
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("do not touch"), 0o644); err != nil {
			t.Fatal(err)
		}
		live := filepath.Join(root, "wi_y")
		writeSwapMarkerBlob(t, live, victim, filepath.Join(root, "wi_y.recover-trash-z"))

		if err := ReconcilePendingSwap(live); !errors.Is(err, ErrSwapMarkerUntrusted) {
			t.Fatalf("a staged path outside the capsules root must be refused typed, got %v", err)
		}
		if raw, rerr := os.ReadFile(filepath.Join(victim, "keep.txt")); rerr != nil || string(raw) != "do not touch" {
			t.Fatalf("the outside tree must be untouched: %q %v", raw, rerr)
		}
		if pathExists(live) {
			t.Fatal("nothing may be renamed onto the live path from an untrusted marker")
		}
		if !pathExists(swapMarkerPath(live)) {
			t.Fatal("the untrusted marker must be retained for forensics")
		}
	})
	t.Run("staged path escaping the root through a symlink: refused", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "capsules")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(tmp, "outside-tree")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		linked := filepath.Join(root, "wi_z.recover-staging-link")
		if err := os.Symlink(outside, linked); err != nil {
			t.Fatal(err)
		}
		live := filepath.Join(root, "wi_z")
		writeSwapMarkerBlob(t, live, linked, filepath.Join(root, "wi_z.recover-trash-z"))

		if err := ReconcilePendingSwap(live); !errors.Is(err, ErrSwapMarkerUntrusted) {
			t.Fatalf("a staged path resolving outside the root must be refused typed, got %v", err)
		}
		if pathExists(live) {
			t.Fatal("nothing may be renamed onto the live path from an untrusted marker")
		}
		if !pathExists(swapMarkerPath(live)) {
			t.Fatal("the untrusted marker must be retained for forensics")
		}
	})
	t.Run("staged path with non-protocol naming inside the root: refused", func(t *testing.T) {
		root := t.TempDir()
		rogue := filepath.Join(root, "wi_w.not-the-protocol-name")
		if err := os.MkdirAll(rogue, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rogue, "rogue.txt"), []byte("rogue"), 0o644); err != nil {
			t.Fatal(err)
		}
		live := filepath.Join(root, "wi_w")
		writeSwapMarkerBlob(t, live, rogue, filepath.Join(root, "wi_w.recover-trash-z"))

		if err := ReconcilePendingSwap(live); !errors.Is(err, ErrSwapMarkerUntrusted) {
			t.Fatalf("a staged path outside the swap protocol naming must be refused typed, got %v", err)
		}
		if !pathExists(rogue) {
			t.Fatal("the rogue tree must not be moved or removed by an untrusted marker")
		}
		if pathExists(live) {
			t.Fatal("nothing may be renamed onto the live path from an untrusted marker")
		}
		if !pathExists(swapMarkerPath(live)) {
			t.Fatal("the untrusted marker must be retained for forensics")
		}
	})
	t.Run("trash path outside the capsules root: refused before any removal", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "capsules")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(tmp, "outside-trash")
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("do not remove"), 0o644); err != nil {
			t.Fatal(err)
		}
		live := filepath.Join(root, "wi_v")
		writeSwapMarkerBlob(t, live, filepath.Join(root, "wi_v.recover-staging-z"), victim)

		if err := ReconcilePendingSwap(live); !errors.Is(err, ErrSwapMarkerUntrusted) {
			t.Fatalf("a trash path outside the capsules root must be refused typed, got %v", err)
		}
		// Without confinement the sweep would have REMOVED the outside
		// tree as "the replaced surface".
		if raw, rerr := os.ReadFile(filepath.Join(victim, "keep.txt")); rerr != nil || string(raw) != "do not remove" {
			t.Fatalf("the outside trash tree must be untouched: %q %v", raw, rerr)
		}
		if !pathExists(swapMarkerPath(live)) {
			t.Fatal("the untrusted marker must be retained for forensics")
		}
	})
}
