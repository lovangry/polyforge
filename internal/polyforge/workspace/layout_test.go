package workspace

// layout_test.go: the runtime-artifact convention (polyforge#177) —
// Init's idempotent CLAUDE.md upsert, the stray-entry classification
// and the scratch staleness/prune rules. Temp dirs only.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInitFreshWorkspace(t *testing.T) {
	ws := t.TempDir()
	res, err := Init(ws)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scratch != "created" || res.NotesFile != "created" {
		t.Fatalf("fresh init = %+v", res)
	}
	if st, err := os.Stat(filepath.Join(ws, ScratchDir)); err != nil || !st.IsDir() {
		t.Fatalf("scratch/ not created: %v", err)
	}
	text := readFile(t, filepath.Join(ws, NotesFile))
	for _, want := range []string{managedOpen, managedClose, layoutOpen, layoutClose, "doc.design", "doc.review", "doc.decision", "doc.audit", "experience.*", "fact.*", "scratch/", ".repo/", "capsules/", ".polyforge/", "14 days", "--prune-scratch"} {
		if !strings.Contains(text, want) {
			t.Fatalf("CLAUDE.md lacks %q:\n%s", want, text)
		}
	}
	// a second run changes nothing
	res, err = Init(ws)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scratch != "exists" || res.NotesFile != "unchanged" {
		t.Fatalf("second init = %+v", res)
	}
	if again := readFile(t, filepath.Join(ws, NotesFile)); again != text {
		t.Fatalf("second init rewrote CLAUDE.md:\n%s", again)
	}
}

func TestInitKeepsHumanPartAndManagedListing(t *testing.T) {
	ws := t.TempDir()
	orig := managedOpen + "\n## Workspace\n\n### polyforge\nrepo listing\n" + managedClose + "\n\n## My notes\n\nhuman text\n"
	writeFile(t, filepath.Join(ws, NotesFile), orig)
	res, err := Init(ws)
	if err != nil {
		t.Fatal(err)
	}
	if res.NotesFile != "inserted" {
		t.Fatalf("init = %+v, want inserted", res)
	}
	text := readFile(t, filepath.Join(ws, NotesFile))
	want := managedOpen + "\n## Workspace\n\n### polyforge\nrepo listing\n\n" + LayoutBlock() + managedClose + "\n\n## My notes\n\nhuman text\n"
	if text != want {
		t.Fatalf("CLAUDE.md =\n%s\nwant\n%s", text, want)
	}
	// idempotent
	if res, err := Init(ws); err != nil || res.NotesFile != "unchanged" {
		t.Fatalf("second init = %+v, %v", res, err)
	}
	// an outdated layout section is replaced in place, nothing else moves
	old := strings.Replace(text, LayoutBlock(), layoutOpen+"\nold convention\n"+layoutClose+"\n", 1)
	writeFile(t, filepath.Join(ws, NotesFile), old)
	if res, err := Init(ws); err != nil || res.NotesFile != "updated" {
		t.Fatalf("refresh init = %+v, %v", res, err)
	}
	if got := readFile(t, filepath.Join(ws, NotesFile)); got != want {
		t.Fatalf("refreshed CLAUDE.md =\n%s\nwant\n%s", got, want)
	}
}

func TestInitWithoutManagedBlockPrependsOne(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, NotesFile), "# Human notes\n")
	if res, err := Init(ws); err != nil || res.NotesFile != "inserted" {
		t.Fatalf("init = %+v, %v", res, err)
	}
	text := readFile(t, filepath.Join(ws, NotesFile))
	if !strings.HasPrefix(text, managedOpen+"\n"+LayoutBlock()+managedClose+"\n\n") || !strings.HasSuffix(text, "# Human notes\n") {
		t.Fatalf("CLAUDE.md =\n%s", text)
	}
}

func TestInitRefusesBrokenMarkersAndScratchFile(t *testing.T) {
	ws := t.TempDir()
	broken := "x\n" + layoutOpen + "\nno end\n"
	writeFile(t, filepath.Join(ws, NotesFile), broken)
	if _, err := Init(ws); err == nil {
		t.Fatal("init accepted an unclosed layout section")
	}
	if got := readFile(t, filepath.Join(ws, NotesFile)); got != broken {
		t.Fatalf("a refused init wrote CLAUDE.md: %q", got)
	}
	ws2 := t.TempDir()
	writeFile(t, filepath.Join(ws2, ScratchDir), "a file")
	if _, err := Init(ws2); err == nil {
		t.Fatal("init accepted a scratch file in place of scratch/")
	}
}

func TestStrayEntries(t *testing.T) {
	ws := t.TempDir()
	for _, d := range []string{".repo/a", "capsules/x", "scratch", ".polyforge/repo-map", ".polyforge/watchers", ".pi", ".claude", "docs", "clone/.git"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".polyforge.yaml", "CLAUDE.md", "AGENTS.md", ".gitignore", ".mcp.json", ".env.example",
		".polyforge.yaml.bak-v270", "notes.md", "run.log", ".polyforge/design.md", ".polyforge/state.json", ".polyforge/repo-map/x.md"} {
		writeFile(t, filepath.Join(ws, f), "x")
	}
	got, err := StrayEntries(ws)
	if err != nil {
		t.Fatal(err)
	}
	want := []Stray{
		{".polyforge.yaml.bak-v270", DestScratch},
		{".polyforge/design.md", DestAihub},
		{".polyforge/state.json", DestScratch},
		{"clone", DestRepo},
		{"docs", DestDir},
		{"notes.md", DestAihub},
		{"run.log", DestScratch},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("strays =\n%+v\nwant\n%+v", got, want)
	}
	clean := t.TempDir()
	if _, err := Init(clean); err != nil {
		t.Fatal(err)
	}
	if got, err := StrayEntries(clean); err != nil || len(got) != 0 {
		t.Fatalf("an initialized workspace has strays %+v (%v)", got, err)
	}
}

func TestStaleScratchAndPrune(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	old := now.Add(-15 * 24 * time.Hour)
	set := func(rel string, at time.Time) {
		t.Helper()
		if err := os.Chtimes(filepath.Join(ws, ScratchDir, rel), at, at); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(ws, ScratchDir, "old.log"), "x")
	writeFile(t, filepath.Join(ws, ScratchDir, "fresh.log"), "x")
	writeFile(t, filepath.Join(ws, ScratchDir, "olddir", "a.txt"), "x")
	writeFile(t, filepath.Join(ws, ScratchDir, "mixed", "old.txt"), "x")
	writeFile(t, filepath.Join(ws, ScratchDir, "mixed", "new.txt"), "x")
	set("old.log", old)
	set("olddir/a.txt", old)
	set("olddir", old)
	set("mixed/old.txt", old)
	set("mixed", old) // the dir is old, but new.txt under it is fresh
	stale, err := StaleScratch(ws, now, ScratchMaxAge)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"old.log", "olddir"}; !reflect.DeepEqual(stale, want) {
		t.Fatalf("stale = %v, want %v", stale, want)
	}
	removed, err := PruneScratch(ws, stale)
	if err != nil || !reflect.DeepEqual(removed, stale) {
		t.Fatalf("prune = %v, %v", removed, err)
	}
	left, _ := os.ReadDir(filepath.Join(ws, ScratchDir))
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if want := []string{"fresh.log", "mixed"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("scratch after prune = %v, want %v", names, want)
	}
	if _, err := PruneScratch(ws, []string{"../x"}); err == nil {
		t.Fatal("prune accepted a path outside scratch/")
	}
	if stale, err := StaleScratch(t.TempDir(), now, ScratchMaxAge); err != nil || stale != nil {
		t.Fatalf("no scratch/: %v %v", stale, err)
	}
}
