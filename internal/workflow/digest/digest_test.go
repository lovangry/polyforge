package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenVector is the mechanically reproducible golden case from
// 01-workflow-skills.md §4.5: bundle with only workflow.md ("---\nname: demo\n---\n")
// and agents/spec.md ("abc\n") must digest to the documented value.
func TestGoldenVector(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "workflow.md"), "---\nname: demo\n---\n")
	mustWrite(t, filepath.Join(dir, "agents", "spec.md"), "abc\n")

	got, err := ComputeBundleDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	const want = "a652ff86f53331152994bb0b93b5ce6b8ae1a96fdca4ecded6c835b0f639e515"
	if got != want {
		t.Fatalf("bundle digest = %s, want %s", got, want)
	}
}

// TestEmptySyncBytesDigest verifies the empty-chain constant equals
// sha256("[]") (02 §6.4 / 05 §6.1).
func TestEmptySyncBytesDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("[]"))
	if got := hex.EncodeToString(sum[:]); got != EmptySyncBytesDigest {
		t.Fatalf("sha256(\"[]\") = %s, want %s", got, EmptySyncBytesDigest)
	}
}

// TestLFNormalizationAndBOM: CRLF and lone CR normalize to LF and change the
// digest; a BOM is rejected outright (01 §4.5 step 2).
func TestLFNormalizationAndBOM(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "workflow.md"), "---\nname: demo\n---\n")
	baseDigest, err := ComputeBundleDigest(base)
	if err != nil {
		t.Fatal(err)
	}

	crlf := t.TempDir()
	mustWrite(t, filepath.Join(crlf, "workflow.md"), "---\r\nname: demo\r\n---\r\n")
	crlfDigest, err := ComputeBundleDigest(crlf)
	if err != nil {
		t.Fatal(err)
	}
	if crlfDigest != baseDigest {
		t.Fatalf("CRLF normalization failed: %s != %s", crlfDigest, baseDigest)
	}

	bom := t.TempDir()
	mustWrite(t, filepath.Join(bom, "workflow.md"), "\xEF\xBB\xBF---\nname: demo\n---\n")
	if _, err := ComputeBundleDigest(bom); err == nil {
		t.Fatal("expected BOM rejection")
	}
}

// TestAgentsOnlyMarkdown: non-.md file under agents/ is rejected (V7).
func TestAgentsOnlyMarkdown(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "workflow.md"), "---\nname: demo\n---\n")
	mustWrite(t, filepath.Join(dir, "agents", "run.sh"), "#!/bin/sh\n")
	if _, err := ComputeBundleDigest(dir); err == nil {
		t.Fatal("expected rejection of non-.md file under agents/")
	}
}

// TestCanonicalKeyOrderAndEscaping exercises MarshalCanonical: key sort is by
// UTF-16 code unit, control characters use lowercase \u escapes, integral
// floats collapse to shortest integer form, non-integral floats are refused.
func TestCanonicalKeyOrderAndEscaping(t *testing.T) {
	in := map[string]any{
		"zeta":  1,
		"alpha": "a\"b\\c\nd\x01",
		"ütf":   2,
	}
	b, err := MarshalCanonical(in)
	if err != nil {
		t.Fatal(err)
	}
	// Key sort by UTF-16 code unit: 'a'(97) < 'z'(122) < 'ü'(252).
	want := "{\"alpha\":\"a\\\"b\\\\c\\nd\\u0001\",\"zeta\":1,\"ütf\":2}"
	if string(b) != want {
		t.Fatalf("canonical = %q, want %q", b, want)
	}
	if _, err := MarshalCanonical(map[string]any{"x": 1.5}); err == nil {
		t.Fatal("expected non-integral number rejection")
	}
	f, err := MarshalCanonical(map[string]any{"x": 3.0})
	if err != nil || string(f) != `{"x":3}` {
		t.Fatalf("integral float: %q, %v", f, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
