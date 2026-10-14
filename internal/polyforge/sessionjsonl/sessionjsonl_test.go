package sessionjsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write file helper: content exactly as given.
func writeT(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// CopyReadonly's byte-verbatim posture: the copy is identical to the
// source bytes — torn last line, malformed lines, empty lines and all
// (the parser faces were deleted with the audit's dead-code sweep; the
// copier preserves exactly what the source held at read time).
func TestCopyReadonlyByteIdentical(t *testing.T) {
	for _, body := range []string{
		"l1\nl2\nl3\n",                       // complete lines
		"l1\nl2\nl3",                         // torn final line
		"{\"type\":\"x\"}\ngarbage\n\n",      // malformed + blank lines
		"",                                   // empty file
		"{\"ts\":\"t1\"}\n{\"ts\":\"t2\"}\n", // timestamped objects ride verbatim
	} {
		src := writeT(t, body)
		dst := filepath.Join(t.TempDir(), "copy.jsonl")
		if err := CopyReadonly(src, dst); err != nil {
			t.Fatalf("CopyReadonly(%q): %v", body, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("copy = %q, want the source bytes %q", got, body)
		}
	}
}

// The source is never opened for writing: its mode is untouched by a copy.
func TestCopyReadonlyLeavesSourceUntouched(t *testing.T) {
	body := "l1\nl2\n"
	src := writeT(t, body)
	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.jsonl")
	if err := CopyReadonly(src, dst); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		t.Fatalf("the source changed: %v -> %v", before, after)
	}
}

func TestCopyReadonlyMissingSourceIsError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "copy.jsonl")
	if err := CopyReadonly(filepath.Join(t.TempDir(), "nope.jsonl"), dst); err == nil {
		t.Fatal("a missing source must be an error, not an empty copy")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("a failed copy must not leave a destination behind")
	}
}

// tmp+rename semantics: no temp residue next to the destination.
func TestCopyReadonlyNoResidue(t *testing.T) {
	src := writeT(t, "l1\nl2\n")
	dir := t.TempDir()
	dst := filepath.Join(dir, "copy.jsonl")
	if err := CopyReadonly(src, dst); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "copy.jsonl" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected residue next to the copy: %s", strings.Join(names, ","))
	}
}
