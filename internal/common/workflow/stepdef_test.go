package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// StepDefinitionPath: the bundle's own steps/<id>.md wins; without it the
// shared workflows/_shared/steps/<id>.md is used; with neither the
// bundle's own path is answered so the read error names it.
func TestStepDefinitionPathPrecedence(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "b")
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	own := filepath.Join(bundle, "steps", "deliver.md")
	shared := filepath.Join(root, SharedDir, "steps", "deliver.md")

	if got := StepDefinitionPath(bundle, "deliver"); got != own {
		t.Fatalf("neither present: %s, want %s", got, own)
	}
	write(shared)
	if got := StepDefinitionPath(bundle, "deliver"); got != shared {
		t.Fatalf("shared only: %s, want %s", got, shared)
	}
	write(own)
	if got := StepDefinitionPath(bundle, "deliver"); got != own {
		t.Fatalf("own must win: %s, want %s", got, own)
	}
}
