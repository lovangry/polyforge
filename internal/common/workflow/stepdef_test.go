package workflow

import (
	"os"
	"path/filepath"
	"strings"
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

// RenderStepVars fills {{name}} from vars (a block scalar's trailing
// newline dropped, values never re-scanned) and is fail-closed both ways.
func TestRenderStepVars(t *testing.T) {
	got, err := RenderStepVars([]byte("a {{x}} b {{y_2}}"), map[string]string{"x": "one\n", "y_2": "{{x}}"})
	if err != nil || string(got) != "a one b {{x}}" {
		t.Fatalf("render = %q, %v", got, err)
	}
	if _, err := RenderStepVars([]byte("a {{x}} {{x}}"), nil); err == nil || !strings.Contains(err.Error(), "undeclared vars [x] ") {
		t.Fatalf("missing var: %v", err)
	}
	if _, err := RenderStepVars([]byte("a"), map[string]string{"x": "1"}); err == nil || !strings.Contains(err.Error(), "never uses") {
		t.Fatalf("unused var: %v", err)
	}
	if got, err := RenderStepVars([]byte("{{ x }} {{X}}"), nil); err != nil || string(got) != "{{ x }} {{X}}" {
		t.Fatalf("non-placeholder braces must pass through: %q, %v", got, err)
	}
}
