package recover

// Remote-reconstruction tests over an in-memory fake remote (05 §7.4):
// checkpoint+incremental replay, full replacement, harness never restored,
// per-file sha verification, symlink escape refusal aborts whole recovery.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
