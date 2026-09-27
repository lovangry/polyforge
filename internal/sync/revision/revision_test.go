package revision

// Manifest construction and digest tests. The manifest_sha cross-check
// against the REAL Core reader (internal/core/capsule.ComputeManifestSha) is
// the wire-compatibility contract: both sides must derive identical values
// for identical manifests.

import (
	"encoding/json"
	"testing"
	"time"

	corecapsule "github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/workflow/digest"
)

func modePtr(m int64) *int64 { return &m }

func TestBuildManifestIncrementalOrdering(t *testing.T) {
	parent := &ScanResult{Files: map[string]FileInfo{
		"runtime-state/execution.json": {Sha256: "aaa", Bytes: 10, Mode: 420},
		"artifacts/old.md":             {Sha256: "bbb", Bytes: 3, Mode: 420},
		"repos/r/a.go":                 {Sha256: "ccc", Bytes: 5, Mode: 420},
	}}
	current := &ScanResult{Files: map[string]FileInfo{
		"runtime-state/execution.json": {Sha256: "aaa2", Bytes: 12, Mode: 420},
		"repos/r/a.go":                 {Sha256: "ccc", Bytes: 5, Mode: 493}, // chmod only
		"repos/r/new.go":               {Sha256: "ddd", Bytes: 7, Mode: 420}, // new file
		"logs/l.txt":                   {Sha256: "eee", Bytes: 2, Mode: 420},
	}}
	parentOf := int64(-1)
	m, err := BuildManifest(parent, current, BuildOptions{
		WorkItemID: "wi_x", Seq: 1, Parent: &parentOf, Generation: 1,
		Kind: KindIncremental, BuiltAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Ops) != 5 {
		t.Fatalf("ops: %+v", m.Ops)
	}
	// SYNC-INV-6: execution.json put LAST.
	last := m.Ops[len(m.Ops)-1]
	if last.Path != "runtime-state/execution.json" || last.Op != OpPut {
		t.Fatalf("execution.json must be the last op: %+v", last)
	}
	// Data ops path-sorted before it.
	for i := 0; i < len(m.Ops)-1; i++ {
		if m.Ops[i].Path > m.Ops[i+1].Path && i+1 < len(m.Ops)-1 {
			t.Fatalf("ops not path-sorted: %+v", m.Ops)
		}
	}
	// artifacts/old.md deleted.
	var sawDelete bool
	for _, op := range m.Ops {
		if op.Op == OpDelete && op.Path == "artifacts/old.md" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatalf("missing delete: %+v", m.Ops)
	}
	// chmod for same-sha mode change.
	for _, op := range m.Ops {
		if op.Op == OpChmod && op.Path == "repos/r/a.go" && *op.Mode == 493 {
			goto sawChmod
		}
	}
	t.Fatalf("missing chmod: %+v", m.Ops)
sawChmod:
	if !m.ExecutionStateIncluded {
		t.Fatal("SYNC-INV-3 assertion field must be true")
	}
}

func TestBuildManifestRequiresExecutionJSON(t *testing.T) {
	current := &ScanResult{Files: map[string]FileInfo{"artifacts/a.md": {Sha256: "x", Mode: 420}}}
	if _, err := BuildManifest(nil, current, BuildOptions{WorkItemID: "wi_x", Kind: KindIncremental}); err == nil {
		t.Fatal("manifest without execution.json must be refused")
	}
}

func TestBuildManifestBootstrap(t *testing.T) {
	current := &ScanResult{Files: map[string]FileInfo{
		"runtime-state/execution.json": {Sha256: "aaa", Bytes: 10, Mode: 420},
	}}
	m, err := BuildManifest(nil, current, BuildOptions{WorkItemID: "wi_x", Seq: 0, Parent: nil, Generation: 1, Kind: KindIncremental})
	if err != nil {
		t.Fatal(err)
	}
	if m.Parent != nil {
		t.Fatal("seq-0 bootstrap parent must be the null sentinel")
	}
}

func TestBuildManifestCheckpointFullInventory(t *testing.T) {
	current := &ScanResult{Files: map[string]FileInfo{
		"runtime-state/execution.json": {Sha256: "aaa", Bytes: 10, Mode: 420},
		"repos/r/a.go":                 {Sha256: "ccc", Bytes: 5, Mode: 420},
		"artifacts/x.md":               {Sha256: "ddd", Bytes: 7, Mode: 420},
	}}
	m, err := BuildManifest(nil, current, BuildOptions{WorkItemID: "wi_x", Seq: 64, Parent: modePtr(63), Generation: 1, Kind: KindCheckpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Ops) != 3 {
		t.Fatalf("checkpoint must carry the full inventory: %+v", m.Ops)
	}
	if m.Ops[len(m.Ops)-1].Path != "runtime-state/execution.json" {
		t.Fatal("execution.json still last on checkpoints")
	}
	if m.LiveFiles != 3 {
		t.Fatalf("live_files = %d", m.LiveFiles)
	}
}

func TestManifestShaCrossChecksWithCore(t *testing.T) {
	p := modePtr(63)
	m := Manifest{
		WorkItemID: "wi_abc123", Seq: 7, Parent: p, Generation: 3,
		Kind: KindIncremental,
		Ops: []Op{
			{Op: OpPut, Path: "repos/ieops/internal/x.go", Sha256: "aa11", Bytes: 1840, Mode: modePtr(420)},
			{Op: OpDelete, Path: "artifacts/scratch.tmp"},
			{Op: OpSymlink, Path: "repos/ieops/vendor/self", Target: "."},
			{Op: OpChmod, Path: "scripts/run.sh", Mode: modePtr(493)},
			{Op: OpPut, Path: "runtime-state/execution.json", Sha256: "bb22", Bytes: 412, Mode: modePtr(420)},
		},
		LiveFiles: 118, DataBytes: 241000, ExecutionStateIncluded: true,
		BuiltAt: "2026-09-26T08:00:00Z",
	}
	ours, err := ComputeManifestSha(m)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the same manifest as a Core-side value and compare digests.
	var coreM corecapsule.Manifest
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &coreM); err != nil {
		t.Fatal(err)
	}
	theirs := corecapsule.ComputeManifestSha(coreM)
	if ours != theirs {
		t.Fatalf("manifest_sha divergence: daemon=%s core=%s", ours, theirs)
	}
	// Idempotent identity: built_at must NOT affect the digest (SYNC-INV-8).
	m.BuiltAt = "2030-01-01T00:00:00Z"
	again, err := ComputeManifestSha(m)
	if err != nil {
		t.Fatal(err)
	}
	if again != theirs {
		t.Fatal("built_at must not participate in manifest_sha")
	}
}

func TestBlockEncodeDecodeDigest(t *testing.T) {
	content := []byte("the quick brown fox\n" + string(make([]byte, 4096)))
	sha := DigestHex(content)
	block, err := EncodeBlock(content)
	if err != nil {
		t.Fatal(err)
	}
	if DigestHex(block) == sha && len(block) != len(content) {
		t.Fatal("compressed block unexpectedly addressed by original digest")
	}
	if err := VerifyBlockDigest(block, sha); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBlockDigest(block, DigestHex([]byte("other"))); err == nil {
		t.Fatal("digest mismatch must be detected")
	}
	back, err := DecodeBlock(block)
	if err != nil || string(back) != string(content) {
		t.Fatalf("round trip failed: %v", err)
	}
	// Uncompressed payload passes through like the Core decoder.
	if err := VerifyBlockDigest(content, sha); err != nil {
		t.Fatal(err)
	}
}

func TestBytesDigestEmptyIsConstant(t *testing.T) {
	empty := &ScanResult{Files: map[string]FileInfo{}}
	d, err := empty.BytesDigest()
	if err != nil {
		t.Fatal(err)
	}
	if d != digest.EmptySyncBytesDigest {
		t.Fatalf("empty sync surface digest = %s, want the EMPTY constant %s", d, digest.EmptySyncBytesDigest)
	}
}

func TestBytesDigestSortedAndStable(t *testing.T) {
	s := &ScanResult{Files: map[string]FileInfo{
		"runtime-state/execution.json": {Sha256: "aaa", Mode: 420},
		"artifacts/z.md":               {Sha256: "zzz", Mode: 420},
		"artifacts/a.md":               {Sha256: "aaa", Mode: 493},
	}}
	d1, err := s.BytesDigest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.BytesDigest()
	if err != nil || d1 != d2 {
		t.Fatal("bytes digest must be deterministic")
	}
}

func TestShouldCheckpoint(t *testing.T) {
	if !ShouldCheckpoint(64, 0) {
		t.Fatal("seq 64 must checkpoint")
	}
	if ShouldCheckpoint(63, 0) {
		t.Fatal("seq 63 must not checkpoint on cadence")
	}
	if !ShouldCheckpoint(10, CheckpointByteCap+1) {
		t.Fatal("cumulative > 512MiB must checkpoint")
	}
}

func TestValidateOps(t *testing.T) {
	good := []Op{
		{Op: OpPut, Path: "a/b", Sha256: "x", Mode: modePtr(420)},
		{Op: OpSymlink, Path: "a/l", Target: "b"},
	}
	if err := ValidateOps(good); err != nil {
		t.Fatal(err)
	}
	bad := [][]Op{
		{{Op: "nuke", Path: "a"}},
		{{Op: OpPut, Path: "../escape", Mode: modePtr(420)}},
		{{Op: OpPut, Path: "a/b", Sha256: "x"}}, // MODE_MISSING
		{{Op: OpSymlink, Path: "a/l", Target: "../out"}},
	}
	for _, ops := range bad {
		if err := ValidateOps(ops); err == nil {
			t.Errorf("ops %+v must be refused", ops)
		}
	}
}
