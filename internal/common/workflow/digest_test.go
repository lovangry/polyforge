package workflow

// digest_test: golden coverage for Digest, carried over from the retired v0
// tree (its TestGoldenVector bundle) plus the sort/mutation/sensitivity
// properties v0 asserted.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// digestDemoEntries is the golden bundle used by v0's TestGoldenVector:
// workflow.md = "---\nname: demo\n---\n", agents/spec.md = "abc\n".
func digestDemoEntries() []FileEntry {
	wf := sha256.Sum256([]byte("---\nname: demo\n---\n"))
	spec := sha256.Sum256([]byte("abc\n"))
	return []FileEntry{
		{Path: "workflow.md", Sha256: hex.EncodeToString(wf[:]), Bytes: 19},
		{Path: "agents/spec.md", Sha256: hex.EncodeToString(spec[:]), Bytes: 4},
	}
}

// TestDigestGoldenVector pins the live Digest value for the demo bundle.
// NOTE: this differs from v0's value for the same bundle (a652ff86...,
// RFC 8785 canonical JSON of the files object, doc 01 section 4.5): the live
// Digest concatenates path+sha+8-byte-LE-length per file. Pinned digests in
// the field come from this live algorithm, so this is a regression pin of
// live behaviour, not a v0-compat check.
func TestDigestGoldenVector(t *testing.T) {
	const want = "c63583fe719ab50cdfbffeb60dc1a1218eef920a2c5b3f900f5bc3124dcbc631"
	if got := Digest(digestDemoEntries()); got != want {
		t.Fatalf("Digest(demo) = %s, want %s", got, want)
	}
}

// TestDigestByteLayout recomputes the digest from the documented layout
// independently of the implementation.
func TestDigestByteLayout(t *testing.T) {
	es := digestDemoEntries() // sorted order: agents/spec.md, workflow.md
	var buf []byte
	for _, e := range []FileEntry{es[1], es[0]} {
		buf = append(buf, e.Path...)
		buf = append(buf, e.Sha256...)
		var l [8]byte
		binary.LittleEndian.PutUint64(l[:], e.Bytes)
		buf = append(buf, l[:]...)
	}
	sum := sha256.Sum256(buf)
	if got, want := Digest(es), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("Digest = %s, layout recompute = %s", got, want)
	}
}

// TestDigestOrderIndependentAndNonMutating mirrors v0's code-point sort
// rule: input order must not matter, and the caller's slice is untouched.
func TestDigestOrderIndependentAndNonMutating(t *testing.T) {
	es := digestDemoEntries()
	rev := []FileEntry{es[1], es[0]}
	if Digest(es) != Digest(rev) {
		t.Fatal("digest must not depend on input order")
	}
	if es[0].Path != "workflow.md" {
		t.Fatalf("Digest mutated its input: %v", es)
	}
}

// TestDigestSensitivity: any change to path, sha or byte length changes the
// digest; an empty set is the SHA-256 of the empty string.
func TestDigestSensitivity(t *testing.T) {
	base := Digest(digestDemoEntries())
	for name, mut := range map[string]func(e []FileEntry){
		"path":  func(e []FileEntry) { e[0].Path = "workflow2.md" },
		"sha":   func(e []FileEntry) { e[0].Sha256 = strings.Repeat("0", 64) },
		"bytes": func(e []FileEntry) { e[0].Bytes++ },
	} {
		es := digestDemoEntries()
		mut(es)
		if Digest(es) == base {
			t.Fatalf("changing %s must change the digest", name)
		}
	}
	empty := sha256.Sum256(nil)
	if got := Digest(nil); got != hex.EncodeToString(empty[:]) {
		t.Fatalf("Digest(nil) = %s", got)
	}
}
