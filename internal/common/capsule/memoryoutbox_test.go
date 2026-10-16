package capsule

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestOutboxMemoryRoundTrip(t *testing.T) {
	m := OutboxMemory{Type: "experience.debug", Title: "a: b — 中文", Tags: []string{"policy-proposal"}, Body: "line 1\n---\nline 3\n"}
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseOutboxMemory(raw)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip = %+v, %v\n%s", got, err, raw)
	}
	for raw, want := range map[string]string{
		"no fence\n":                     "no frontmatter",
		"---\ntype: x\n":                 "unterminated",
		"---\ntype: x\nbogus: 1\n---\nb": "frontmatter",
	} {
		if _, err := ParseOutboxMemory([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err = %v, want %q", raw, err, want)
		}
	}
}

func TestReadOutboxFileRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "zero.md")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOutboxFile(link); err == nil {
		t.Fatal("a symlink to /dev/zero was read")
	}
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, make([]byte, OutboxFileMax+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOutboxFile(big); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized: %v", err)
	}
	fifo := filepath.Join(dir, "fifo.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOutboxFile(fifo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("fifo: %v", err)
	}
	ok := filepath.Join(dir, "ok.md")
	if err := os.WriteFile(ok, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if raw, err := ReadOutboxFile(ok); err != nil || string(raw) != "x" {
		t.Fatalf("regular: %q %v", raw, err)
	}
}
