package capsule_test

// Tests for the manifest accept chain (05 §2.6/§2.8/§5.1/§6), read-through
// reconstruction (02 §5.2) and deletion protection (05 §2.10).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
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
		wi: workitem.NewManager(db, filepath.Join(dir, "workflows")),
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
	metas, _ := hh.cap.RevisionsSince(wi, 0)
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

func TestManifestValidationRejections(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)
	good := putOp("runtime-state/execution.json", "{}")
	if err := hh.cap.PutBlock(wi, good.Sha256, []byte("{}"), r.GenerationToken); err != nil {
		t.Fatal(err)
	}

	// manifest_sha mismatch → SYNC-400 MANIFEST_HASH_MISMATCH.
	m, _ := buildManifest(wi, 0, nil, r.Generation, []capsule.Op{good})
	_, err := hh.cap.PostManifest(wi, m, "sha256:" + strings.Repeat("0", 64), r.GenerationToken)
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
			BytesDigest:  "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
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
