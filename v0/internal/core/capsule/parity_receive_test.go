package capsule_test

// parity_receive_test.go: the CORE RECEIVE side of the zstd/manifest_sha
// parity lock (pinned ruling of 05 §4.2, recorded in 02 §6 and mirrored by
// internal/sync/revision's package doc): the Daemon uploads blocks
// zstd-COMPRESSED while the sha256 in the CAS path and the manifest
// addresses the ORIGINAL (decompressed) content, and manifest_sha is
// computed identically on both sides over the manifest's idempotent view.
//
// The write-side half lives in internal/sync/parity_test.go; this file
// proves the receive side end-to-end against the REAL Store:
//
//   - a manifest built by the Daemon write side (internal/sync/revision)
//     with its Daemon-computed manifest_sha is ACCEPTED by the Core
//     PostManifest recompute (identical manifest_sha for the same fixture
//     ops — the strongest form of the parity lock);
//   - blocks in the Daemon's zstd wire form are accepted with the RAW
//     content digest as their address, and read back decodable;
//   - a single flipped bit in a block body (compressed or raw form) is
//     rejected with the typed SYNC-400 DIGEST_MISMATCH, and nothing is
//     written to the CAS for the failed digest.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// parityFixture builds the same fixture ops as the write-side test
// (internal/sync/parity_test.go fixtureManifest) as a Daemon-side
// revision.Manifest.
func parityFixture() revision.Manifest {
	mode := int64(0o644)
	execMode := int64(0o755)
	m := revision.Manifest{
		WorkItemID: "", // stamped per test below
		Seq:        0,
		Parent:     nil,
		Generation: 0,
		Kind:       revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/notes.md", Sha256: revision.DigestHex([]byte(parityNotes)), Bytes: int64(len(parityNotes)), Mode: &mode},
			{Op: revision.OpPut, Path: "src/main.go", Sha256: revision.DigestHex([]byte(parityCode)), Bytes: int64(len(parityCode)), Mode: &execMode},
			{Op: revision.OpDelete, Path: "old/obsolete.txt"},
			{Op: revision.OpSymlink, Path: "reports/latest", Target: "reports/v1"},
			{Op: revision.OpChmod, Path: "scripts/run.sh"},
			{Op: revision.OpPut, Path: "runtime-state/execution.json", Sha256: revision.DigestHex([]byte(parityExec)), Bytes: int64(len(parityExec)), Mode: &mode},
		},
		LiveFiles:              4,
		DataBytes:              int64(len(parityNotes) + len(parityCode) + len(parityExec)),
		ExecutionStateIncluded: true,
		BuiltAt:                "2026-09-27T00:00:00Z",
	}
	return m
}

const (
	parityNotes = "# 状态报告\n\n你好世界 polyforge parity probe — 三元组检索样例。\n"
	parityCode  = "package main\n\nfunc main() { println(\"parity\") }\n"
	parityExec  = `{"version":1,"current_step":"code"}`
)

// TestManifestShaParityReceiveSide: the Core recompute accepts the
// Daemon-computed manifest_sha for the same fixture ops, over blocks
// uploaded in the Daemon's zstd wire form addressed by RAW-content
// digests. This is the cross-side lock end-to-end: if either side
// changed its canonicalization or its digest ruling, PostManifest would
// answer SYNC-400 MANIFEST_HASH_MISMATCH and this test would fail.
func TestManifestShaParityReceiveSide(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)

	// Build the fixture on the DAEMON side and stamp the receive context.
	m := parityFixture()
	m.WorkItemID = wi
	m.Generation = r.Generation

	// Upload every put block in the Daemon wire form: zstd-compressed
	// body, RAW-content digest as the CAS address (the pinned ruling).
	for _, content := range []string{parityNotes, parityCode, parityExec} {
		encoded, err := revision.EncodeBlock([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if err := hh.cap.PutBlock(wi, revision.DigestHex([]byte(content)), encoded, r.GenerationToken); err != nil {
			t.Fatalf("Core must accept the Daemon zstd wire form for %q: %v", content, err)
		}
	}

	// The DAEMON-computed manifest_sha must satisfy the CORE recompute.
	daemonSha, err := revision.ComputeManifestSha(m)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit parity of the two pure computations over the wire form.
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var coreM capsule.Manifest
	if err := json.Unmarshal(blob, &coreM); err != nil {
		t.Fatalf("the Daemon wire form must decode as the Core manifest type: %v", err)
	}
	if got := capsule.ComputeManifestSha(coreM); got != daemonSha {
		t.Fatalf("manifest_sha parity broken: Daemon %s != Core %s", daemonSha, got)
	}

	// End-to-end: PostManifest recomputes with capsule.ComputeManifestSha
	// and must accept the Daemon-submitted digest.
	head, err := hh.cap.PostManifest(wi, coreM, daemonSha, r.GenerationToken)
	if err != nil {
		t.Fatalf("Core refused the Daemon-computed manifest_sha (%s): %v", daemonSha, err)
	}
	if head != 0 {
		t.Fatalf("head_seq = %d, want 0 (bootstrap)", head)
	}

	// The published HEAD carries the shared digest verbatim.
	h, err := hh.cap.Head(wi)
	if err != nil {
		t.Fatal(err)
	}
	if h.ManifestSha != daemonSha {
		t.Fatalf("HEAD manifest_sha = %s, want the Daemon-computed %s", h.ManifestSha, daemonSha)
	}

	// Read-through reconstructs execution.json from the zstd-stored block:
	// the receive side decompresses transparently (digest addressed the
	// original, so reconstruction must yield the original bytes).
	_, _, _, body, err := hh.cap.Progress(wi)
	if err != nil {
		t.Fatal(err)
	}
	var exec map[string]any
	if err := json.Unmarshal(body, &exec); err != nil {
		t.Fatalf("progress not JSON: %v (%s)", err, body)
	}
	if exec["current_step"] != "code" {
		t.Fatalf("progress content: %s", body)
	}
}

// TestFlippedBlockBitFailsTypedOnReceiveSide: a single flipped bit in the
// block body — in the Daemon's zstd wire form OR in a raw (non-zstd)
// payload — is rejected by the Core receive side with the typed
// SYNC-400 DIGEST_MISMATCH error (a decode failure and a genuine content
// mismatch both surface as the same typed error), and no CAS file is
// written for the rejected digest.
func TestFlippedBlockBitFailsTypedOnReceiveSide(t *testing.T) {
	hh := setup(t)
	wi, r := newClaimedWI(t, hh)

	casPath := func(sha string) string {
		return filepath.Join(hh.dir, "capsules", wi, "rev", "_cas", sha[:2], sha)
	}

	// --- zstd wire form with one flipped bit in the compressed stream ---
	content := []byte(parityNotes + parityCode)
	goodSha := revision.DigestHex(content)
	encoded, err := revision.EncodeBlock(content)
	if err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte{}, encoded...)
	flipped[len(flipped)/2] ^= 0x01 // one bit, mid-stream

	err = hh.cap.PutBlock(wi, goodSha, flipped, r.GenerationToken)
	e := ae(t, err)
	// Both failure shapes (undecodable frame / decodes to different bytes)
	// are the SAME typed error on the receive side.
	if e.Code != "SYNC-400" || e.Reason != "DIGEST_MISMATCH" {
		t.Fatalf("flipped zstd bit: want SYNC-400 DIGEST_MISMATCH, got %v", e)
	}
	if _, serr := os.Stat(casPath(goodSha)); serr == nil {
		t.Fatal("a rejected digest must not write a CAS block")
	}

	// The intact form of the same content is still accepted afterwards.
	if err := hh.cap.PutBlock(wi, goodSha, encoded, r.GenerationToken); err != nil {
		t.Fatalf("intact zstd block rejected: %v", err)
	}

	// --- raw (non-zstd) payload with one flipped bit ---
	raw := []byte(parityExec)
	rawSha := revision.DigestHex(raw)
	corrupted := append([]byte{}, raw...)
	corrupted[0] ^= 0x01

	err = hh.cap.PutBlock(wi, rawSha, corrupted, r.GenerationToken)
	e = ae(t, err)
	if e.Code != "SYNC-400" || e.Reason != "DIGEST_MISMATCH" {
		t.Fatalf("flipped raw bit: want SYNC-400 DIGEST_MISMATCH, got %v", e)
	}
	if _, serr := os.Stat(casPath(rawSha)); serr == nil {
		t.Fatal("a rejected digest must not write a CAS block")
	}

	// Round-trip sanity of the accepted zstd block: stored bytes are the
	// compressed form (as stored, 05 §4.2) and decode back to the content.
	stored, err := hh.cap.GetBlock(wi, goodSha)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stored, content) {
		t.Fatal("the CAS must store the zstd-compressed form, not the raw content")
	}
	back, err := revision.DecodeBlock(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, content) {
		t.Fatal("stored block does not decode back to the original content")
	}
}
