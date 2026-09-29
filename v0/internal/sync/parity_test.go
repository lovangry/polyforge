package sync

// parity_test.go: the WRITE side of the zstd/manifest_sha parity lock
// (the pinned ruling of 05 §4.2 / §7.4, recorded in 02 §6): manifest_sha is
// sha256 over the manifest's idempotent view canonicalized identically on
// both sides, the sha256 in CAS paths and manifests addresses the RAW
// (decompressed) content, and the block BODY on the wire is the
// zstd-compressed form of that content. The Daemon write side
// (internal/sync/revision) and the Core receive side
// (internal/core/capsule) must compute byte-identical manifest_sha for the
// same fixture ops; the receive-side half of this lock lives in
// internal/core/capsule/parity_receive_test.go and additionally proves
// acceptance end-to-end plus the flipped-block-bit typed error.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// fixtureManifest builds one manifest exercising every op kind (put with
// CJK + code content, delete, symlink, chmod) plus the mandatory
// runtime-state/execution.json put. The SAME construction feeds the
// write-side and receive-side parity tests; the receive side rebuilds its
// Core-side manifest from this object's WIRE FORM (JSON round-trip), so
// the fixture is literally shared, not re-typed.
func fixtureManifest(builtAt string) revision.Manifest {
	mode := int64(0o644)
	execMode := int64(0o755)
	return revision.Manifest{
		WorkItemID: "wi_parity00000",
		Seq:        0,
		Parent:     nil,
		Generation: 3,
		Kind:       revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/notes.md", Sha256: "", Bytes: int64(len(fixtureNotes)), Mode: &mode},
			{Op: revision.OpPut, Path: "src/main.go", Sha256: "", Bytes: int64(len(fixtureCode)), Mode: &execMode},
			{Op: revision.OpDelete, Path: "old/obsolete.txt"},
			{Op: revision.OpSymlink, Path: "reports/latest", Target: "reports/v1"},
			{Op: revision.OpChmod, Path: "scripts/run.sh"},
			{Op: revision.OpPut, Path: "runtime-state/execution.json", Sha256: "", Bytes: int64(len(fixtureExec)), Mode: &mode},
		},
		LiveFiles:              4,
		DataBytes:              int64(len(fixtureNotes) + len(fixtureCode) + len(fixtureExec)),
		ExecutionStateIncluded: true,
		BuiltAt:                builtAt,
	}
}

// fillPutDigests stamps each put op's sha256 with the RAW content digest —
// the CAS address per the pinned ruling.
func fillPutDigests(m *revision.Manifest) {
	for i := range m.Ops {
		switch m.Ops[i].Path {
		case "artifacts/notes.md":
			m.Ops[i].Sha256 = revision.DigestHex([]byte(fixtureNotes))
		case "src/main.go":
			m.Ops[i].Sha256 = revision.DigestHex([]byte(fixtureCode))
		case "runtime-state/execution.json":
			m.Ops[i].Sha256 = revision.DigestHex([]byte(fixtureExec))
		}
	}
}

const (
	fixtureNotes = "# 状态报告\n\n你好世界 polyforge parity probe — 三元组检索样例。\n"
	fixtureCode  = "package main\n\nfunc main() { println(\"parity\") }\n"
	fixtureExec  = `{"version":1,"current_step":"code"}`
)

// TestManifestShaParityWriteSide: for the same fixture ops the Daemon
// write side and the Core receive side must produce the IDENTICAL
// manifest_sha — including across different built_at values (the
// idempotent view strips time-varying fields on both sides) — and the two
// wire types must serialize byte-identically (the wire shape is the
// contract, not the Go structs).
func TestManifestShaParityWriteSide(t *testing.T) {
	m1 := fixtureManifest("2026-09-27T00:00:00Z")
	fillPutDigests(&m1)
	m2 := fixtureManifest("2026-09-28T12:34:56Z") // different build moment
	fillPutDigests(&m2)

	daemonSha1, err := revision.ComputeManifestSha(m1)
	if err != nil {
		t.Fatal(err)
	}
	daemonSha2, err := revision.ComputeManifestSha(m2)
	if err != nil {
		t.Fatal(err)
	}

	// Same fixture through the CORE recompute: rebuild the Core-side
	// manifest from the write side's WIRE FORM (the only thing that
	// actually crosses the boundary).
	blob, err := json.Marshal(m1)
	if err != nil {
		t.Fatal(err)
	}
	var coreM capsule.Manifest
	if err := json.Unmarshal(blob, &coreM); err != nil {
		t.Fatalf("the Daemon wire form must decode as the Core manifest type: %v", err)
	}
	coreSha := capsule.ComputeManifestSha(coreM)

	if daemonSha1 != coreSha {
		t.Fatalf("manifest_sha parity broken: Daemon %s != Core %s", daemonSha1, coreSha)
	}
	// The idempotent view: a different built_at must not move the digest
	// on either side.
	daemonSha2Core := func() string {
		b, _ := json.Marshal(m2)
		var cm capsule.Manifest
		_ = json.Unmarshal(b, &cm)
		return capsule.ComputeManifestSha(cm)
	}()
	if daemonSha1 != daemonSha2 {
		t.Fatalf("built_at leaked into the Daemon manifest_sha: %s vs %s", daemonSha1, daemonSha2)
	}
	if daemonSha2 != daemonSha2Core {
		t.Fatalf("built_at leaked into the Core manifest_sha: %s vs %s", daemonSha2, daemonSha2Core)
	}

	// Wire-shape parity: the two types serialize byte-identically for the
	// same values (no struct drift the wire could ever observe).
	coreBlob, _ := json.MarshalIndent(coreM, "", "  ")
	daemonBlob, _ := json.MarshalIndent(m1, "", "  ")
	if !bytes.Equal(coreBlob, daemonBlob) {
		t.Fatalf("wire shapes diverged:\nDaemon: %s\nCore:   %s", daemonBlob, coreBlob)
	}
}

// TestBlockEncodingRulingWriteSide pins the zstd ruling on the write side:
// the CAS digest addresses the ORIGINAL content, the wire body is the
// compressed form (a zstd frame, magic-prefixed, never equal to the raw
// bytes for incompressible content), and a single flipped bit in the
// compressed body must fail digest verification — never pass.
func TestBlockEncodingRulingWriteSide(t *testing.T) {
	// Incompressible content so raw != compressed (deterministic here).
	content := []byte(strings.Repeat("parity-zstd-ruling-\x00\x01\x02", 64))

	sha := revision.DigestHex(content)
	if len(sha) != 64 {
		t.Fatalf("digest = %q, want 64-hex", sha)
	}

	encoded, err := revision.EncodeBlock(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) < 4 || !bytes.Equal(encoded[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		t.Fatalf("encoded block is not a zstd frame: %x", encoded[:4])
	}
	if bytes.Equal(encoded, content) {
		t.Fatal("incompressible content must not equal its compressed form")
	}
	// The digest addresses the RAW content, never the wire bytes.
	if revision.DigestHex(encoded) == sha {
		t.Fatal("the CAS digest must not be over the compressed wire form")
	}

	// Round-trip.
	back, err := revision.DecodeBlock(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, content) {
		t.Fatal("decode(encode(content)) != content")
	}

	// Verification accepts the correct pairing...
	if err := revision.VerifyBlockDigest(encoded, sha); err != nil {
		t.Fatalf("correct zstd block must verify: %v", err)
	}
	// ...and rejects the compressed form paired with a wrong raw digest.
	wrong := revision.DigestHex([]byte("something else"))
	if err := revision.VerifyBlockDigest(encoded, wrong); !errors.Is(err, revision.ErrDigestMismatch) {
		t.Fatalf("wrong digest: want ErrDigestMismatch, got %v", err)
	}

	// Flipped block bit: one bit in the middle of the compressed stream.
	// Verification must FAIL — either as ErrDigestMismatch (the frame
	// still decoded, to different bytes) or as a zstd decode error; both
	// are rejections, and on the CORE receive side both surface as the
	// typed SYNC-400 DIGEST_MISMATCH (see parity_receive_test.go).
	flipped := append([]byte{}, encoded...)
	flipped[len(flipped)/2] ^= 0x01
	if err := revision.VerifyBlockDigest(flipped, sha); err == nil {
		t.Fatal("a flipped block bit must never pass digest verification")
	}
}
