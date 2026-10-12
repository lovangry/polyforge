package workflow

// workflow_test: the minimal frontmatter read surface — splitting, the
// defaults + per-step candidate fields, and the typed absent conditions.
// Fixture ids are deliberately provider-qualified placeholders; no real
// model or vendor names appear anywhere in this package.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fmFixture = `---
name: demo
defaults:
  model_candidates: [prov-a/model-a1, prov-a/model-a2, prov-b/model-b1]
steps:
  - id: draft
    kind: author
  - id: gate
    kind: gate
    model_candidates: [prov-b/model-b2]
---

# body narrative

- the frontmatter array is the only order
`

func TestSplitFrontmatter(t *testing.T) {
	front, _, ok := SplitFrontmatter([]byte(fmFixture))
	if !ok {
		t.Fatal("a well-formed fence must split")
	}
	if !strings.Contains(string(front), "model_candidates") {
		t.Fatalf("front block must carry the candidate keys: %q", front)
	}
	if _, _, ok := SplitFrontmatter([]byte("# no frontmatter\n")); ok {
		t.Fatal("a file without a fence must not split")
	}
	if _, _, ok := SplitFrontmatter([]byte("---\nname: torn")); ok {
		t.Fatal("an unterminated fence must not split")
	}
}

func TestReadFrontmatterParsesCandidates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "workflow.md")
	if err := os.WriteFile(p, []byte(fmFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, err := ReadFrontmatter(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if fm == nil {
		t.Fatal("a present frontmatter must parse")
	}
	want := []string{"prov-a/model-a1", "prov-a/model-a2", "prov-b/model-b1"}
	if fm.Defaults == nil || len(fm.Defaults.ModelCandidates) != len(want) {
		t.Fatalf("defaults candidates = %+v, want %v", fm.Defaults, want)
	}
	for i := range want {
		if fm.Defaults.ModelCandidates[i] != want[i] {
			t.Fatalf("defaults candidates = %v, want %v", fm.Defaults.ModelCandidates, want)
		}
	}
	// the per-step override is carried verbatim
	var gate *FrontmatterStep
	for i := range fm.Steps {
		if fm.Steps[i].ID == "gate" {
			gate = &fm.Steps[i]
		}
	}
	if gate == nil || len(gate.ModelCandidates) != 1 || gate.ModelCandidates[0] != "prov-b/model-b2" {
		t.Fatalf("per-step override = %+v, want [prov-b/model-b2]", gate)
	}
}

func TestReadFrontmatterAbsentConditions(t *testing.T) {
	dir := t.TempDir()
	// missing file → typed absent, never an error
	fm, err := ReadFrontmatter(filepath.Join(dir, "workflow.md"))
	if fm != nil || err != nil {
		t.Fatalf("missing file must answer (nil, nil), got (%v, %v)", fm, err)
	}
	// frontmatter-less file → typed absent
	p := filepath.Join(dir, "bare.md")
	if err := os.WriteFile(p, []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, err = ReadFrontmatter(p)
	if fm != nil || err != nil {
		t.Fatalf("bare file must answer (nil, nil), got (%v, %v)", fm, err)
	}
	// torn YAML → an error (the caller decides fatality)
	p2 := filepath.Join(dir, "torn.md")
	if err := os.WriteFile(p2, []byte("---\nsteps: [unclosed\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fm, err = ReadFrontmatter(p2); fm != nil || err == nil {
		t.Fatalf("torn yaml must answer (nil, err), got (%v, %v)", fm, err)
	}
}
