package recover

// Remote-reconstruction tests over an in-memory fake remote (05 §7.4):
// checkpoint+incremental replay, full replacement, harness never restored,
// per-file sha verification, symlink escape refusal aborts whole recovery.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

func modeP(m int64) *int64 { return &m }

// memRemote stores manifests and blocks directly: its listing rows carry
// NO ops (metadata-only), so Recover exercises the per-seq GetManifest
// fallback path of the Remote face.
type memRemote struct {
	manifests map[int64]revision.Manifest
	blocks    map[string][]byte // sha → ORIGINAL content
	head      *revision.HeadInfo
}

func (m *memRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	if m.head == nil {
		return nil, false, nil
	}
	cp := *m.head
	return &cp, true, nil
}

func (m *memRemote) ListRevisions(ctx context.Context, wi string, since int64) ([]revision.RevisionMeta, error) {
	out := []revision.RevisionMeta{}
	for seq := int64(0); seq <= m.head.Seq; seq++ {
		man, ok := m.manifests[seq]
		if !ok {
			continue
		}
		out = append(out, revision.RevisionMeta{Seq: seq, Parent: man.Parent, Generation: man.Generation, ManifestSha: "sha:x", DataBytes: man.DataBytes})
	}
	return out, nil
}

func (m *memRemote) GetManifest(ctx context.Context, wi string, seq int64) (revision.Manifest, error) {
	man, ok := m.manifests[seq]
	if !ok {
		return revision.Manifest{}, fmt.Errorf("no manifest %d", seq)
	}
	return man, nil
}

func (m *memRemote) GetBlock(ctx context.Context, wi, sha string) ([]byte, error) {
	content, ok := m.blocks[sha]
	if !ok {
		return nil, fmt.Errorf("no block %s", sha)
	}
	blk, err := revision.EncodeBlock(content)
	if err != nil {
		return nil, err
	}
	return blk, nil
}

func block(t *testing.T, m *memRemote, content string) (sha string) {
	t.Helper()
	m.blocks[revision.DigestHex([]byte(content))] = []byte(content)
	return revision.DigestHex([]byte(content))
}

func newCapsuleWithLayout(t *testing.T) *capsule.Capsule {
	t.Helper()
	c, _, err := capsule.Create(t.TempDir(), "wi_rec01", 2, "m-01", "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRecoverNoHeadLeavesCapsuleUntouched(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.Head != nil || len(res.LiveFiles) != 0 {
		t.Fatalf("no HEAD must be a no-op: %+v", res)
	}
}

func TestRecoverFullReplacement(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}

	// Local stale state the replacement must clear.
	os.MkdirAll(filepath.Join(c.Dir, "artifacts"), 0o755)
	os.WriteFile(filepath.Join(c.Dir, "artifacts", "stale.md"), []byte("stale"), 0o644)

	// seq 0: bootstrap with two files; seq 1: incremental deleting one,
	// adding a symlink + a chmod'd script.
	shaA := block(t, remote, "content A")
	shaB := block(t, remote, "content B\nsecond line")
	shaX := block(t, remote, "#!/bin/sh\nexit 0\n")
	remote.manifests[0] = revision.Manifest{
		WorkItemID: "wi_rec01", Seq: 0, Parent: nil, Generation: 1, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: shaA, Bytes: 9, Mode: modeP(420)},
			{Op: revision.OpPut, Path: "repos/r1/x.go", Sha256: shaB, Bytes: 20, Mode: modeP(493)},
		},
		LiveFiles: 2, ExecutionStateIncluded: true,
	}
	p0v := int64(0)
	remote.manifests[1] = revision.Manifest{
		WorkItemID: "wi_rec01", Seq: 1, Parent: &p0v, Generation: 1, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpDelete, Path: "artifacts/a.md"},
			{Op: revision.OpPut, Path: "artifacts/keep.md", Sha256: shaA, Bytes: 9, Mode: modeP(420)},
			{Op: revision.OpSymlink, Path: "repos/r1/link", Target: "x.go"},
			{Op: revision.OpPut, Path: "repos/r1/run.sh", Sha256: shaX, Bytes: 20, Mode: modeP(493)},
			{Op: revision.OpPut, Path: "runtime-state/execution.json", Sha256: block(t, remote, `{"version":4}`), Bytes: 12, Mode: modeP(420)},
		},
		LiveFiles: 4, ExecutionStateIncluded: true,
	}
	remote.head = &revision.HeadInfo{Seq: 1, Generation: 1, ManifestSha: "sha:x"}

	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	// Stale file gone (full replacement, no resurrection).
	if _, err := os.Stat(filepath.Join(c.Dir, "artifacts", "stale.md")); !os.IsNotExist(err) {
		t.Fatal("stale surface file must be removed by full replacement")
	}
	// Deleted-before-checkpoint file not resurrected.
	if _, err := os.Stat(filepath.Join(c.Dir, "artifacts", "a.md")); !os.IsNotExist(err) {
		t.Fatal("deleted file must not resurrect")
	}
	// Kept files with content + mode.
	raw, _ := os.ReadFile(filepath.Join(c.Dir, "artifacts", "keep.md"))
	if string(raw) != "content A" {
		t.Fatalf("keep.md: %q", raw)
	}
	fi, _ := os.Stat(filepath.Join(c.Dir, "repos", "r1", "run.sh"))
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode = %o", fi.Mode().Perm())
	}
	// Symlink restored.
	tgt, err := os.Readlink(filepath.Join(c.Dir, "repos", "r1", "link"))
	if err != nil || tgt != "x.go" {
		t.Fatalf("symlink: %v %q", err, tgt)
	}
	// Result seeds the publish state correctly (harness excluded, exec included).
	if _, ok := res.LiveFiles["runtime-state/execution.json"]; !ok {
		t.Fatal("execution.json must be in the recovered live set")
	}
	if len(res.LiveFiles) != 5 {
		t.Fatalf("live set = %d entries", len(res.LiveFiles))
	}
}

func TestRecoverNeverRestoresHarness(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	shaS := block(t, remote, `{"session_id":"old"}`)
	remote.manifests[0] = revision.Manifest{
		WorkItemID: "wi_rec01", Seq: 0, Parent: nil, Generation: 1, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "harness/pi/session.json", Sha256: shaS, Bytes: 20, Mode: modeP(420)},
		},
		LiveFiles: 1, ExecutionStateIncluded: false, // tolerated at recovery (accept chain is the publisher's concern)
	}
	remote.head = &revision.HeadInfo{Seq: 0, Generation: 1, ManifestSha: "sha:x"}
	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "harness", "pi", "session.json")); !os.IsNotExist(err) {
		t.Fatal("harness/ must NEVER be restored from history (05 §7.4)")
	}
	if len(res.LiveFiles) != 0 {
		t.Fatal("harness entries must not seed the publish state")
	}
}

func TestRecoverCheckpointReplay(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	shaA := block(t, remote, "A")
	shaB := block(t, remote, "B")
	// seq 0 puts a.md; seq 1 (checkpoint) full inventory of only b.md;
	// seq 2 incremental adds c.md. Replay from the checkpoint must NOT
	// resurrect a.md even though seq 0 contained it.
	remote.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
		Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: shaA, Mode: modeP(420)}}}
	p0 := int64(0)
	remote.manifests[1] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 1, Parent: &p0, Kind: revision.KindCheckpoint,
		Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/b.md", Sha256: shaB, Mode: modeP(420)}}}
	p1 := int64(1)
	remote.manifests[2] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 2, Parent: &p1, Kind: revision.KindIncremental,
		Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/c.md", Sha256: shaA, Mode: modeP(420)}}}
	remote.head = &revision.HeadInfo{Seq: 2, Generation: 1}
	if _, err := Recover(context.Background(), c, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "artifacts", "a.md")); !os.IsNotExist(err) {
		t.Fatal("pre-checkpoint file must not resurrect through checkpoint replay")
	}
	for _, f := range []string{"b.md", "c.md"} {
		if _, err := os.Stat(filepath.Join(c.Dir, "artifacts", f)); err != nil {
			t.Fatalf("%s missing after replay", f)
		}
	}
}

func TestRecoverSymlinkEscapeAbortsWhole(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	shaA := block(t, remote, "A")
	// One legal file AND one escaping symlink: the whole recovery must abort
	// with NOTHING applied (03 §6.3 rule 3: whole-operation atomicity).
	remote.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/ok.md", Sha256: shaA, Mode: modeP(420)},
			{Op: revision.OpSymlink, Path: "repos/r1/evil", Target: "../../runtime-state"},
		}}
	remote.head = &revision.HeadInfo{Seq: 0, Generation: 1}
	_, err := Recover(context.Background(), c, remote)
	if err == nil {
		t.Fatal("escaping symlink must abort recovery")
	}
	if _, serr := os.Stat(filepath.Join(c.Dir, "artifacts", "ok.md")); !os.IsNotExist(serr) {
		t.Fatal("no partial application: the legal entry must not be written when another entry aborts")
	}
}

func TestRecoverVerifiesSha(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	// Manifest claims sha X but the block content digests differently.
	remote.blocks["deadbeef"] = []byte("tampered content")
	remote.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
		Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/bad.md", Sha256: "deadbeef", Mode: modeP(420)}}}
	remote.head = &revision.HeadInfo{Seq: 0, Generation: 1}
	if _, err := Recover(context.Background(), c, remote); err == nil {
		t.Fatal("sha mismatch must fail the recovery")
	}
	if _, serr := os.Stat(filepath.Join(c.Dir, "artifacts", "bad.md")); !os.IsNotExist(serr) {
		t.Fatal("corrupt file must not be left on disk")
	}
}

// TestRecoverMissingLateBlockLeavesSurfaceIntact is the FE-2 regression:
// recovery used to delete the current surface BEFORE fetching the blocks,
// so a missing or corrupt LATE block left a half-destroyed tree. Every
// block must be fetched and verified BEFORE the first mutation — a failed
// prefetch aborts with the old surface intact (03 §6.3 rule 3;
// 05 §7.4 delete-then-write runs only once all bytes are in hand).
func TestRecoverMissingLateBlockLeavesSurfaceIntact(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	// The current surface holds local state the failed rebuild must not touch.
	if err := os.MkdirAll(filepath.Join(c.Dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "artifacts", "old.md"), []byte("old surface"), 0o644); err != nil {
		t.Fatal(err)
	}
	shaA := block(t, remote, "A")
	// The SECOND entry's block is missing from the remote CAS.
	remote.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/first.md", Sha256: shaA, Mode: modeP(420)},
			{Op: revision.OpPut, Path: "artifacts/second.md", Sha256: revision.DigestHex([]byte("never uploaded")), Mode: modeP(420)},
		}}
	remote.head = &revision.HeadInfo{Seq: 0, Generation: 1, ManifestSha: "sha:x"}
	_, err := Recover(context.Background(), c, remote)
	if err == nil {
		t.Fatal("a missing block must abort the recovery")
	}
	// The old surface is intact: the pre-existing file still holds its bytes.
	raw, rerr := os.ReadFile(filepath.Join(c.Dir, "artifacts", "old.md"))
	if rerr != nil || string(raw) != "old surface" {
		t.Fatalf("the old surface must survive a failed rebuild untouched: %q %v", raw, rerr)
	}
	// And nothing was partially applied: not even the fetchable first entry.
	if _, serr := os.Stat(filepath.Join(c.Dir, "artifacts", "first.md")); !os.IsNotExist(serr) {
		t.Fatal("no expected entry may be written before every block is in hand")
	}
}

// TestRecoverRejectsMalformedListing is the FE-4 regression: the replay
// must validate the revision chain BEFORE touching the surface — contiguous
// seqs from the seq-0 bootstrap, unique parent links, and a HEAD row that
// matches the tip. A malformed listing is rejected typed (ErrChainInvalid)
// with the local surface untouched (05 §2.1/§2.2/§7.4).
func TestRecoverRejectsMalformedListing(t *testing.T) {
	// Each case builds a remote whose listing is malformed in one way.
	cases := []struct {
		name    string
		build   func(*memRemote)
		headSha string
	}{
		{
			name: "gap in seqs",
			build: func(m *memRemote) {
				m.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: revision.DigestHex([]byte("A")), Mode: modeP(420)}}}
				// seq 1 missing; seq 2 exists → listing shows 0, 2.
				p1 := int64(1)
				m.manifests[2] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 2, Parent: &p1, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/c.md", Sha256: revision.DigestHex([]byte("C")), Mode: modeP(420)}}}
				m.head = &revision.HeadInfo{Seq: 2, Generation: 1, ManifestSha: "sha:x"}
			},
		},
		{
			name: "broken parent link",
			build: func(m *memRemote) {
				m.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: revision.DigestHex([]byte("A")), Mode: modeP(420)}}}
				p0 := int64(0)
				// seq 2 claims parent 0 — not the previous seq 1.
				m.manifests[1] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 1, Parent: &p0, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/b.md", Sha256: revision.DigestHex([]byte("B")), Mode: modeP(420)}}}
				m.manifests[2] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 2, Parent: &p0, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/c.md", Sha256: revision.DigestHex([]byte("C")), Mode: modeP(420)}}}
				m.head = &revision.HeadInfo{Seq: 2, Generation: 1, ManifestSha: "sha:x"}
			},
		},
		{
			name: "bootstrap with a non-null parent",
			build: func(m *memRemote) {
				px := int64(5)
				m.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Parent: &px, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: revision.DigestHex([]byte("A")), Mode: modeP(420)}}}
				m.head = &revision.HeadInfo{Seq: 0, Generation: 1, ManifestSha: "sha:x"}
			},
		},
		{
			name: "HEAD names a different manifest than the tip",
			build: func(m *memRemote) {
				m.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
					Ops: []revision.Op{{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: revision.DigestHex([]byte("A")), Mode: modeP(420)}}}
				m.head = &revision.HeadInfo{Seq: 0, Generation: 1, ManifestSha: "sha256:someone-else"}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapsuleWithLayout(t)
			remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
			tc.build(remote)
			// Local state that proves the surface was never touched.
			if err := os.MkdirAll(filepath.Join(c.Dir, "artifacts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.Dir, "artifacts", "sentinel.md"), []byte("local"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Recover(context.Background(), c, remote)
			if !errors.Is(err, ErrChainInvalid) {
				t.Fatalf("malformed listing must be rejected typed, got %v", err)
			}
			raw, rerr := os.ReadFile(filepath.Join(c.Dir, "artifacts", "sentinel.md"))
			if rerr != nil || string(raw) != "local" {
				t.Fatal("a rejected listing must leave the surface untouched")
			}
			for _, f := range []string{"a.md", "b.md", "c.md"} {
				if _, serr := os.Stat(filepath.Join(c.Dir, "artifacts", f)); !os.IsNotExist(serr) {
					t.Fatalf("%s must not be written from a rejected listing", f)
				}
			}
		})
	}
}

// TestRecoverSeedsSymlinkFactsLikeTheScan is the FE-7 regression: the
// replay used to seed symlink LiveFiles with an empty sha and zero bytes,
// though the scan defines a symlink from its TARGET TEXT — every
// post-recovery flush would then report a false "changed" diff. The seeded
// live set must equal what a fresh full scan of the recovered tree produces.
func TestRecoverSeedsSymlinkFactsLikeTheScan(t *testing.T) {
	c := newCapsuleWithLayout(t)
	remote := &memRemote{manifests: map[int64]revision.Manifest{}, blocks: map[string][]byte{}}
	shaA := block(t, remote, "A")
	shaExec := block(t, remote, `{"version":4}`)
	remote.manifests[0] = revision.Manifest{WorkItemID: "wi_rec01", Seq: 0, Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/a.md", Sha256: shaA, Bytes: 1, Mode: modeP(420)},
			{Op: revision.OpPut, Path: "repos/r1/x.go", Sha256: shaA, Bytes: 1, Mode: modeP(420)},
			{Op: revision.OpSymlink, Path: "repos/r1/link", Target: "x.go"},
			{Op: revision.OpPut, Path: "runtime-state/execution.json", Sha256: shaExec, Bytes: 13, Mode: modeP(420)},
		}}
	remote.head = &revision.HeadInfo{Seq: 0, Generation: 1, ManifestSha: "sha:x"}
	res, err := Recover(context.Background(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	// The recovered symlink entry must carry the scan's facts, not a husk.
	link, ok := res.LiveFiles["repos/r1/link"]
	if !ok || !link.IsSymlink {
		t.Fatalf("symlink missing from the live set: %+v", res.LiveFiles)
	}
	want := revision.SymlinkFileInfo("x.go")
	if link.Sha256 != want.Sha256 || link.Bytes != want.Bytes || link.Target != want.Target || link.Mode != want.Mode {
		t.Fatalf("replayed symlink facts must match the scan's: got %+v want %+v", link, want)
	}
	// Whole-set equality: the seeded publish state IS a fresh scan of the
	// recovered tree — the no-change flush precondition (05 §6.1 branch 2).
	scan, err := revision.ScanCapsule(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.LiveFiles, scan.Files) {
		t.Fatalf("recovered live set must equal a fresh scan (else every flush reports a false change):\nrecovered: %+v\nscan:      %+v", res.LiveFiles, scan.Files)
	}
}
