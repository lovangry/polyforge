package capsule_test

// Tests for the manifest accept chain (05 §2.6/§2.8/§5.1/§6), read-through
// reconstruction (02 §5.2) and deletion protection (05 §2.10).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/core/owner"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/core/workitem"
)

type h struct {
	db  *store.DB
	svc *service.Service
	own *owner.Manager
	wi  *workitem.Manager
	cap *capsule.Store
	dir string
}

func setup(t *testing.T) *h {
	t.Helper()
	db, dir := testsupport.NewDB(t)
	svc, err := service.Register(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.MakeDemoBundle(dir); err != nil {
		t.Fatal(err)
	}
	own := owner.NewManager(db, svc)
	hh := &h{db: db, svc: svc, own: own, dir: dir,
		wi:  workitem.NewManager(db, filepath.Join(dir, "workflows")),
		cap: capsule.NewStore(db, dir, own)}
	return hh
}

func newClaimedWI(t *testing.T, hh *h) (string, *owner.ClaimResponse) {
	t.Helper()
	d, err := hh.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "g", Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := hh.own.Claim(d.ID, owner.ClaimRequest{IdempotencyKey: "k-" + d.ID, MachineID: "mac_a"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d.ID, r
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// buildManifest assembles a manifest with the given ops and returns the
// manifest object plus its computed idempotent-view digest.
func buildManifest(wi string, seq int64, parent *int64, gen int64, ops []capsule.Op) (capsule.Manifest, string) {
	m := capsule.Manifest{
		WorkItemID: wi, Seq: seq, Parent: parent, Generation: gen,
		Kind: "incremental", Ops: ops, LiveFiles: int64(len(ops)),
		DataBytes: 0, ExecutionStateIncluded: true,
		BuiltAt: "2026-09-27T00:00:00Z",
	}
	return m, capsule.ComputeManifestSha(m)
}

func ae(t *testing.T, err error) *capsule.APIError {
	t.Helper()
	e, ok := err.(*capsule.APIError)
	if !ok {
		t.Fatalf("expected *capsule.APIError, got %T %v", err, err)
	}
	return e
}

func putOp(path, content string) capsule.Op {
	mode := int64(0o644)
	return capsule.Op{Op: "put", Path: path, Sha256: sha([]byte(content)), Bytes: int64(len(content)), Mode: &mode}
}

func TestBootstrapChainAndIdempotency(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)

	exec := `{"version":1,"current_step":"code"}`
	// Block upload BEFORE manifest (先数据后状态).
	if err := hh.cap.PutBlock(wi, sha([]byte(exec)), []byte(exec), r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	// Re-put the same block: idempotent no-op.
	if err := hh.cap.PutBlock(wi, sha([]byte(exec)), []byte(exec), r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	m0, s0 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{putOp("runtime-state/execution.json", exec)})
	if _, err := hh.cap.PostManifest(wi, m0, s0, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	hd, err := hh.cap.Head(wi)
	if err != nil || hd.Seq != 0 {
		t.Fatalf("head: %+v %v", hd, err)
	}

	// Idempotent replay of the SAME (seq, manifest_sha): success, no second row.
	if _, err := hh.cap.PostManifest(wi, m0, s0, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	metas, _ := hh.cap.RevisionsSince(wi, 0, false)
	if len(metas) != 1 {
		t.Fatalf("replay must not insert a second row: %+v", metas)
	}

	// Next revision: parent must equal HEAD.
	m1ops := []capsule.Op{putOp("artifacts/out.txt", "hello")}
	if err := hh.cap.PutBlock(wi, sha([]byte("hello")), []byte("hello"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	p0 := int64(0)
	m1, s1 := buildManifest(wi, 1, &p0, r.Generation, append(m1ops, putOp("runtime-state/execution.json", exec)))
	if _, err := hh.cap.PostManifest(wi, m1, s1, r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// Wrong parent → SYNC-409, HEAD does not move.
	p5 := int64(5)
	mBad, sBad := buildManifest(wi, 2, &p5, r.Generation, m1.Ops)
	_, err = hh.cap.PostManifest(wi, mBad, sBad, r.GenerationToken)
	e := ae(t, err)
	if e.Code != "SYNC-409" {
		t.Fatalf("got %v", e)
	}
	hd, _ = hh.cap.Head(wi)
	if hd.Seq != 1 {
		t.Fatalf("HEAD moved on failed publish: %+v", hd)
	}

	// Read-through reconstructs execution.json at HEAD.
	hs, hg, _, body, err := hh.cap.Progress(wi)
	if err != nil {
		t.Fatal(err)
	}
	if *hs != 1 || *hg != r.Generation {
		t.Fatalf("head meta: %d %d", *hs, *hg)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("progress not JSON: %v (%q)", err, body)
	}
	if decoded["current_step"] != "code" {
		t.Fatalf("progress content: %s", body)
	}
}

// TestRevisionsSinceIncludeOps: the default listing stays metadata-only;
// includeOps reads kind+ops from the rev/<seq>/manifest.json audit copies
// (row 19 amendment, polyforge#14) while the metadata fields are unchanged.
func TestRevisionsSinceIncludeOps(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	exec := `{"version":1}`
	if err := hh.cap.PutBlock(wi, sha([]byte(exec)), []byte(exec), r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	wantOps := []capsule.Op{putOp("runtime-state/execution.json", exec)}
	m0, s0 := buildManifest(wi, 0, nil, r.Generation, wantOps)
	if _, err := hh.cap.PostManifest(wi, m0, s0, r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// Default: metadata only, no ops-bearing extension.
	plain, err := hh.cap.RevisionsSince(wi, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 {
		t.Fatalf("rows: %+v", plain)
	}
	if plain[0].Kind != "" || plain[0].Ops != nil {
		t.Fatalf("default listing must be metadata-only: %+v", plain[0])
	}

	// includeOps: kind + ops inline, metadata fields identical.
	withOps, err := hh.cap.RevisionsSince(wi, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(withOps) != 1 {
		t.Fatalf("rows: %+v", withOps)
	}
	row := withOps[0]
	if row.Kind != "incremental" {
		t.Fatalf("kind = %q", row.Kind)
	}
	if len(row.Ops) != len(wantOps) {
		t.Fatalf("ops = %+v, want %+v", row.Ops, wantOps)
	}
	for i, got := range row.Ops {
		want := wantOps[i]
		if got.Op != want.Op || got.Path != want.Path || got.Sha256 != want.Sha256 ||
			got.Bytes != want.Bytes || got.Target != want.Target ||
			got.Mode == nil || *got.Mode != *want.Mode {
			t.Fatalf("ops[%d] = %+v, want %+v", i, got, want)
		}
	}
	if row.Seq != plain[0].Seq || row.Generation != plain[0].Generation ||
		row.ManifestSha != plain[0].ManifestSha || row.DataBytes != plain[0].DataBytes ||
		row.PublishedAt != plain[0].PublishedAt {
		t.Fatalf("metadata fields changed with includeOps: %+v vs %+v", row, plain[0])
	}
}

func TestManifestValidationRejections(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	good := putOp("runtime-state/execution.json", "{}")
	if err := hh.cap.PutBlock(wi, good.Sha256, []byte("{}"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// manifest_sha mismatch → SYNC-400 MANIFEST_HASH_MISMATCH.
	m, _ := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good})
	_, err := hh.cap.PostManifest(wi, m, "sha256:"+strings.Repeat("0", 64), r.GenerationToken)
	e := ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "MANIFEST_HASH_MISMATCH" {
		t.Fatalf("got %v", e)
	}

	// SYNC-INV-3 violation.
	m2, s2 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good})
	m2.ExecutionStateIncluded = false
	s2 = capsule.ComputeManifestSha(m2)
	_, err = hh.cap.PostManifest(wi, m2, s2, r.GenerationToken)
	e = ae(t, err)
	if e.Code != "SYNC-400" {
		t.Fatalf("got %v", e)
	}

	// Missing block → SYNC-400 BLOCK_MISSING.
	missing := putOp("repos/x/a.go", "not-uploaded")
	m3, s3 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good, missing})
	_, err = hh.cap.PostManifest(wi, m3, s3, r.GenerationToken)
	e = ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "BLOCK_MISSING" {
		t.Fatalf("got %v", e)
	}

	// MODE_MISSING.
	nomode := capsule.Op{Op: "put", Path: "a", Sha256: good.Sha256, Bytes: 2}
	m4, s4 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good, nomode})
	_, err = hh.cap.PostManifest(wi, m4, s4, r.GenerationToken)
	e = ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "MODE_MISSING" {
		t.Fatalf("got %v", e)
	}

	// Path escape.
	esc := capsule.Op{Op: "put", Path: "../evil", Sha256: good.Sha256, Bytes: 2, Mode: &[]int64{0o644}[0]}
	m5, s5 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good, esc})
	_, err = hh.cap.PostManifest(wi, m5, s5, r.GenerationToken)
	e = ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "PATH_ESCAPE" {
		t.Fatalf("got %v", e)
	}

	// Fenced token (fabricated) → SYNC-403 before anything else.
	m6, s6 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good})
	_, err = hh.cap.PostManifest(wi, m6, s6, hex.EncodeToString(make([]byte, 32)))
	e = ae(t, err)
	if e.Code != "SYNC-403" {
		t.Fatalf("got %v", e)
	}

	// Fencing fires at BLOCK upload too (no orphan blocks from old owners).
	err = hh.cap.PutBlock(wi, sha([]byte("orphan")), []byte("orphan"), hex.EncodeToString(make([]byte, 32)))
	e = ae(t, err)
	if e.Code != "SYNC-403" {
		t.Fatalf("block-level fencing missing: %v", e)
	}
}

func TestReadThroughCheckpointReplay(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)

	execV1 := `{"current_step":"spec"}`
	execV2 := `{"current_step":"code"}`
	for _, c := range []string{execV1, execV2} {
		if err := hh.cap.PutBlock(wi, sha([]byte(c)), []byte(c), r.GenerationToken); err != nil {
			t.Fatal(err)
		}
	}
	if err := hh.cap.PutBlock(wi, sha([]byte("artifact")), []byte("artifact"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// seq0: incremental with exec v1.
	m0, s0 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{putOp("runtime-state/execution.json", execV1)})
	if _, err := hh.cap.PostManifest(wi, m0, s0, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	// seq1: incremental replacing exec with v2.
	p0 := int64(0)
	m1, s1 := buildManifest(wi, 1, &p0, r.Generation, []capsule.Op{
		putOp("artifacts/x.md", "artifact"),
		putOp("runtime-state/execution.json", execV2),
	})
	if _, err := hh.cap.PostManifest(wi, m1, s1, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	// seq2: checkpoint full list (exec v2 + artifact).
	p1 := int64(1)
	ck := capsule.Manifest{
		WorkItemID: wi, Seq: 2, Parent: &p1, Generation: r.Generation,
		Kind: "checkpoint", ExecutionStateIncluded: true, BuiltAt: "2026-09-27T00:00:00Z",
		Ops: []capsule.Op{
			putOp("artifacts/x.md", "artifact"),
			putOp("runtime-state/execution.json", execV2),
		},
	}
	if _, err := hh.cap.PostManifest(wi, ck, capsule.ComputeManifestSha(ck), r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	// seq3: delete the artifact (incremental after checkpoint).
	p2 := int64(2)
	m3, s3 := buildManifest(wi, 3, &p2, r.Generation, []capsule.Op{
		{Op: "delete", Path: "artifacts/x.md"},
		putOp("runtime-state/execution.json", execV2),
	})
	if _, err := hh.cap.PostManifest(wi, m3, s3, r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	_, _, _, body, err := hh.cap.Progress(wi)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"code"`) {
		t.Fatalf("progress should replay to the latest state, got %s", body)
	}
}

func TestDeletionProtection(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)

	// Non-terminal WI cannot be tombstoned.
	err := hh.cap.Tombstone(wi, wi)
	e := ae(t, err)
	if e.Code != "SYNC-409" {
		t.Fatalf("got %v", e)
	}
	// Confirm mismatch.
	err = hh.cap.Tombstone(wi, "wrong")
	e = ae(t, err)
	if e.Code != "SYNC-400" {
		t.Fatalf("got %v", e)
	}

	// Wrap the WI (empty-chain evidence), then tombstone + revert.
	fe := owner.FlushEvidence{
		PublishedGeneration: r.Generation, PublishedSeq: 0, ManifestSha: "",
		FrozenCheckpoint: &owner.FrozenCheckpoint{
			ScannedAt: "2026-09-27T00:00:00Z",
			// Stale-by-window is fine here: freshness not enforced for this
			// path in the test because EndGeneration runs below only after
			// we patch scanned_at to now via the emptyFlush semantics.
			BytesDigest:   "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
			WorktreeState: "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
		},
	}
	fe.FrozenCheckpoint.ScannedAt = nowRFC3339()
	if err := hh.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "wrap", FlushEvidence: &fe}, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.Tombstone(wi, wi); err != nil {
		t.Fatal(err)
	}
	// Revert inside the window.
	if err := hh.cap.RevertTombstone(wi); err != nil {
		t.Fatal(err)
	}
	// Nothing left to revert.
	err = hh.cap.RevertTombstone(wi)
	e = ae(t, err)
	if e.Code != "SYNC-410" {
		t.Fatalf("got %v", e)
	}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// --- fix #1: path traversal guards ---

func TestPathTraversalGuards(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	goodSha := sha([]byte("x"))
	if err := hh.cap.PutBlock(wi, goodSha, []byte("x"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	badWiIDs := []string{
		"../etc/passwd",
		"..",
		"a/../../b",
		"foo/bar",
		"foo%2Fbar", // a %2F-smuggled separator: literal '%' is outside the allowed charset
		"",
		strings.Repeat("a", 200),
	}
	for _, bad := range badWiIDs {
		if err := hh.cap.PutBlock(bad, goodSha, []byte("x"), r.GenerationToken); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("PutBlock(%q): expected SYNC-400, got %v", bad, err)
		}
		if _, err := hh.cap.GetBlock(bad, goodSha); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("GetBlock(%q): expected SYNC-400, got %v", bad, err)
		}
		if _, err := hh.cap.Head(bad); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("Head(%q): expected SYNC-400, got %v", bad, err)
		}
		if _, err := hh.cap.RevisionsSince(bad, 0, false); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("RevisionsSince(%q): expected SYNC-400, got %v", bad, err)
		}
		if err := hh.cap.Tombstone(bad, bad); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("Tombstone(%q): expected SYNC-400, got %v", bad, err)
		}
		if err := hh.cap.RevertTombstone(bad); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("RevertTombstone(%q): expected SYNC-400, got %v", bad, err)
		}
	}

	// Short sha must not panic on sha[:2] and must be rejected cleanly.
	for _, shortSha := range []string{"", "a", "ab", "deadbeef"} {
		if err := hh.cap.PutBlock(wi, shortSha, []byte("x"), r.GenerationToken); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("PutBlock short sha %q: expected SYNC-400, got %v", shortSha, err)
		}
		if _, err := hh.cap.GetBlock(wi, shortSha); ae(t, err).Code != "SYNC-400" {
			t.Fatalf("GetBlock short sha %q: expected SYNC-400, got %v", shortSha, err)
		}
	}
}

// --- fix #6: PutBlock digest verification ---

func TestPutBlockDigestVerification(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)

	// Wrong digest (plain content) rejected.
	content := []byte("hello world")
	wrongSha := sha([]byte("something else"))
	err := hh.cap.PutBlock(wi, wrongSha, content, r.GenerationToken)
	e := ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "DIGEST_MISMATCH" {
		t.Fatalf("got %v", e)
	}

	// zstd-compressed block whose DECOMPRESSED content does not match the
	// presented digest is rejected (digest addresses the original, not the
	// compressed wire bytes).
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := []byte("the real content")
	compressed := enc.EncodeAll(inner, nil)
	enc.Close()
	wrongInnerSha := sha([]byte("not the real content"))
	err = hh.cap.PutBlock(wi, wrongInnerSha, compressed, r.GenerationToken)
	e = ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "DIGEST_MISMATCH" {
		t.Fatalf("zstd wrong-inner-content: got %v", e)
	}

	// Correct zstd block accepted, and idempotent re-put of identical bytes OK.
	correctSha := sha(inner)
	if err := hh.cap.PutBlock(wi, correctSha, compressed, r.GenerationToken); err != nil {
		t.Fatalf("correct zstd block rejected: %v", err)
	}
	if err := hh.cap.PutBlock(wi, correctSha, compressed, r.GenerationToken); err != nil {
		t.Fatalf("idempotent re-put of identical bytes failed: %v", err)
	}

	// Plain (non-zstd) idempotent re-put of identical, correctly-digested bytes.
	plainSha := sha(content)
	if err := hh.cap.PutBlock(wi, plainSha, content, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.PutBlock(wi, plainSha, content, r.GenerationToken); err != nil {
		t.Fatalf("idempotent re-put failed: %v", err)
	}
}

// --- fix #8: manifest.json durability ---

func TestManifestWriteFailureAbortsPublish(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	good := putOp("runtime-state/execution.json", "{}")
	if err := hh.cap.PutBlock(wi, good.Sha256, []byte("{}"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// Force the manifest file write to fail: pre-create the seq directory
	// PATH as a plain FILE, so os.MkdirAll(.../rev/0/manifest.json's dir)
	// fails with "not a directory" regardless of process privilege (a
	// permission-only failure would be bypassed when tests run as root).
	seqDir := filepath.Join(hh.dir, "capsules", wi, "rev", "0")
	if err := os.MkdirAll(filepath.Dir(seqDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seqDir, []byte("not a directory"), 0o640); err != nil {
		t.Fatal(err)
	}

	m, s := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good})
	_, err := hh.cap.PostManifest(wi, m, s, r.GenerationToken)
	if err == nil {
		t.Fatal("expected the publish to fail when the manifest file cannot be written")
	}

	// No capsule_revisions row and HEAD unmoved (still bootstrap / no HEAD).
	metas, merr := hh.cap.RevisionsSince(wi, 0, false)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(metas) != 0 {
		t.Fatalf("expected no committed revision, got %+v", metas)
	}
	if _, herr := hh.cap.Head(wi); ae(t, herr).Code != "404" {
		t.Fatalf("HEAD must not have moved: %v", herr)
	}
}

// --- fix #10: PostManifest validation order ---

func TestPostManifestValidationOrder(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	good := putOp("runtime-state/execution.json", "{}")
	if err := hh.cap.PutBlock(wi, good.Sha256, []byte("{}"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	// Bootstrap HEAD at seq 0 first.
	m0, s0 := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good})
	if _, err := hh.cap.PostManifest(wi, m0, s0, r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// A manifest with BOTH a stale parent (HEAD is 0, not 5) AND a bad
	// manifest_sha must surface SYNC-409 (SEQ_CONFLICT): HEAD validation runs
	// before the manifest_sha recompute in the pinned chain (fix #10).
	staleParent := int64(5)
	m1, _ := buildManifest(wi, 6, &staleParent, r.Generation, []capsule.Op{good})
	_, err := hh.cap.PostManifest(wi, m1, "sha256:"+strings.Repeat("0", 64), r.GenerationToken)
	e := ae(t, err)
	if e.Code != "SYNC-409" || e.Reason != "SEQ_CONFLICT" {
		t.Fatalf("expected SYNC-409 SEQ_CONFLICT (HEAD checked before hash), got %v", e)
	}

	// HEAD did not move.
	hd, herr := hh.cap.Head(wi)
	if herr != nil || hd.Seq != 0 {
		t.Fatalf("HEAD moved: %+v %v", hd, herr)
	}
}

// --- fix #14: tombstone semantics ---

func TestTombstoneRevertedCanBeRetombstonedFresh(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	fe := emptyFlushCapsule(r.Generation)
	if err := hh.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "wrap", FlushEvidence: fe}, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.Tombstone(wi, wi); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.RevertTombstone(wi); err != nil {
		t.Fatal(err)
	}
	// Re-tombstoning a reverted row must succeed with a FRESH window, not
	// silently no-op (the old ON CONFLICT DO NOTHING behavior).
	if err := hh.cap.Tombstone(wi, wi); err != nil {
		t.Fatalf("re-tombstoning a reverted row should succeed: %v", err)
	}
	// It is pending again and revertible.
	if err := hh.cap.RevertTombstone(wi); err != nil {
		t.Fatalf("expected the fresh tombstone to be revertible: %v", err)
	}
}

func TestRevertTombstoneWindowElapsed(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	fe := emptyFlushCapsule(r.Generation)
	if err := hh.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "wrap", FlushEvidence: fe}, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.Tombstone(wi, wi); err != nil {
		t.Fatal(err)
	}
	// Backdate hard_delete_at into the past directly (simulating window
	// elapse without waiting 24h).
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := hh.db.Exec(`UPDATE capsule_tombstones SET hard_delete_at=?1 WHERE work_item_id=?2`, past, wi); err != nil {
		t.Fatal(err)
	}
	err := hh.cap.RevertTombstone(wi)
	e := ae(t, err)
	if e.Code != "SYNC-409" || e.Reason != "WINDOW_ELAPSED" {
		t.Fatalf("got %v", e)
	}
}

func emptyFlushCapsule(gen int64) *owner.FlushEvidence {
	return &owner.FlushEvidence{
		PublishedGeneration: gen,
		PublishedSeq:        0,
		ManifestSha:         "",
		FrozenCheckpoint: &owner.FrozenCheckpoint{
			ScannedAt:     nowRFC3339(),
			BytesDigest:   "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
			WorktreeState: "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
			ManifestSha:   "",
			MatchesHead:   false,
		},
	}
}

// --- fix #16: Progress CORE-504 ---

func TestProgressCore504WhenClaimedButNoHead(t *testing.T) {
	hh := setup(t)
	wi, _ := newClaimedWI(t, hh)
	// Claimed (owner_generations row exists) but nothing published yet.
	_, _, _, _, err := hh.cap.Progress(wi)
	e := ae(t, err)
	if e.Code != "CORE-504" {
		t.Fatalf("got %v", e)
	}

	// A never-claimed registration ticket still gets head_seq: null (nil
	// error, nil seq), not CORE-504.
	d, err := hh.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "never claimed", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	seq, _, _, _, err := hh.cap.Progress(d.ID)
	if err != nil {
		t.Fatalf("never-claimed WI must not error: %v", err)
	}
	if seq != nil {
		t.Fatalf("never-claimed WI must report head_seq nil, got %v", *seq)
	}
}
