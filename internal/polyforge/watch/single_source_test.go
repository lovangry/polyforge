package watch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The single-source property (spec: "exactly ONE package parses the
// session-jsonl storage format"), asserted from inside the tree: the
// watch module's internal imports and the observe sync face's internal
// imports (every observe file except the stdout-wire parser stream.go)
// intersect in exactly ONE package — this package's sessionjsonl parser —
// with the legitimately shared infra excluded (runtime for the
// LocateLiveTranscript reuse, common/*). stream.go itself must import
// neither the parser nor the watch module: the stdout-wire vocabulary
// stays separate and unchanged.
func TestSingleSourceSessionJSONLParser(t *testing.T) {
	// The test binary's cwd is this package's source directory.
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	obsDir := filepath.Join(pkgDir, "..", "observe")

	watchImports := dirInternalImports(t, pkgDir)
	if len(watchImports) == 0 || dirGoFileCount(pkgDir) == 0 {
		t.Fatalf("the watch module must exist as a Go package: %s", pkgDir)
	}
	streamPath := filepath.Join(obsDir, "stream.go")
	if _, err := os.Stat(streamPath); err != nil {
		t.Fatalf("internal/polyforge/observe/stream.go must stay where it is: %v", err)
	}
	syncImports := dirInternalImportsExcept(t, obsDir, "stream.go")

	shared := map[string]bool{}
	for imp := range watchImports {
		if !syncImports[imp] {
			continue
		}
		if strings.Contains(imp, "/internal/common/") ||
			strings.HasSuffix(imp, "/internal/polyforge/runtime") {
			continue // infra both legitimately share, not the parser
		}
		shared[imp] = true
	}
	delete(shared, "github.com/silgrid/polyforge/internal/polyforge/observe")
	delete(shared, "github.com/silgrid/polyforge/internal/polyforge/watch")
	if len(shared) != 1 {
		t.Fatalf("exactly ONE session-jsonl parser package must be shared between watch and the observe sync face; got %v", shared)
	}
	parserPkg := ""
	for p := range shared {
		parserPkg = p
	}
	if parserPkg != "github.com/silgrid/polyforge/internal/polyforge/sessionjsonl" {
		t.Fatalf("the shared parser package must be sessionjsonl, got %q", parserPkg)
	}
	streamImports := fileInternalImports(t, streamPath)
	if streamImports[parserPkg] || streamImports["github.com/silgrid/polyforge/internal/polyforge/watch"] {
		t.Fatalf("observe/stream.go must stay a separate, unchanged vocabulary: %v", streamImports)
	}
	// And the consumers really consume it: watch's non-test sources
	// import the parser, and so does the observe sync face.
	if !watchImports[parserPkg] {
		t.Fatalf("the watch module must import the shared parser (%s): %v", parserPkg, watchImports)
	}
	if !syncImports[parserPkg] {
		t.Fatalf("the observe sync face must import the shared parser (%s): %v", parserPkg, syncImports)
	}
}

func fileInternalImports(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if f == nil {
		return out
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(p, "/internal/") {
			out[p] = true
		}
	}
	return out
}

func dirInternalImports(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for imp := range dirInternalImportsExcept(t, dir, "") {
		out[imp] = true
	}
	return out
}

func dirInternalImportsExcept(t *testing.T, dir, skip string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") || e.Name() == skip {
			continue
		}
		for imp := range fileInternalImports(t, filepath.Join(dir, e.Name())) {
			out[imp] = true
		}
	}
	return out
}

func dirGoFileCount(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			n++
		}
	}
	return n
}
