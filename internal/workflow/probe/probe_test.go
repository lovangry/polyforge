package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/workflow/schema"
)

// fakeProber implements the adapter face with a scripted reachability
// map (the "fake adapter" of the test brief).
type fakeProber struct {
	unreachable map[string]error
	calls       []string
}

func (f *fakeProber) Probe(ctx context.Context, candidate string) error {
	f.calls = append(f.calls, candidate)
	if err, ok := f.unreachable[candidate]; ok {
		return err
	}
	return nil
}

// writeBundle writes a fixture bundle with the given frontmatter
// defaults/step candidate declarations.
func writeBundle(t *testing.T, workflowsDir, name, frontmatter string) {
	t.Helper()
	dir := filepath.Join(workflowsDir, name)
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\n" + frontmatter + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "workflow.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const featureCandidates = `defaults:
  model_candidates: [provA/m1, provB/m2]
steps:
  - { id: code, kind: code, model_candidates: [provC/m3] }
  - { id: review, kind: review, gate: true }
`

func TestCandidatesUnionDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "feature-full", featureCandidates)
	fm, err := schema.ValidateWorkflowFrontmatter(filepath.Join(dir, "feature-full"), "feature-full")
	if err != nil {
		t.Fatal(err)
	}
	got := Candidates(fm)
	want := []string{"provA/m1", "provB/m2", "provC/m3"}
	if len(got) != len(want) {
		t.Fatalf("Candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Candidates = %v, want %v", got, want)
		}
	}
}

func TestProbeWorkflowsDirTableAndExitVerdict(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "feature-full", featureCandidates)
	writeBundle(t, dir, "bug-fix", `steps:
  - { id: code, kind: code }
`)
	// A broken bundle must fail closed as a bundle error, not a crash.
	broken := filepath.Join(dir, "broken")
	os.MkdirAll(broken, 0o755)
	os.WriteFile(filepath.Join(broken, "workflow.md"), []byte("not frontmatter"), 0o644)

	fake := &fakeProber{unreachable: map[string]error{
		"provB/m2": errors.New("connection refused (dial tcp 127.0.0.1:1)"),
	}}
	reports, err := ProbeWorkflowsDir(context.Background(), fake, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("want 3 bundle reports, got %+v", reports)
	}
	byName := map[string]*Report{}
	for _, r := range reports {
		byName[r.Bundle] = r
	}
	if byName["broken"].BundleError == "" || byName["broken"].AllReachable {
		t.Fatalf("broken bundle must fail closed: %+v", byName["broken"])
	}
	ff := byName["feature-full"]
	if len(ff.Rows) != 3 {
		t.Fatalf("feature-full rows: %+v", ff.Rows)
	}
	rowByCand := map[string]Row{}
	for _, r := range ff.Rows {
		rowByCand[r.Candidate] = r
	}
	if !rowByCand["provA/m1"].Reachable {
		t.Fatalf("provA/m1 must be reachable: %+v", ff.Rows)
	}
	if rowByCand["provB/m2"].Reachable || !strings.Contains(rowByCand["provB/m2"].Error, "connection refused") {
		t.Fatalf("provB/m2 must be unreachable with its error: %+v", ff.Rows)
	}
	// ONE micro request per candidate, exactly.
	want := map[string]int{"provA/m1": 1, "provB/m2": 1, "provC/m3": 1}
	for c, n := range want {
		if got := count(fake.calls, c); got != n {
			t.Fatalf("candidate %s probed %d times, want %d", c, got, n)
		}
	}
	// bug-fix declares no candidates: no probes, trivially all-reachable.
	bf := byName["bug-fix"]
	if len(bf.Rows) != 0 || !bf.AllReachable {
		t.Fatalf("no-candidate bundle: %+v", bf)
	}
	// Exit verdict: broken bundle + unreachable candidate -> not 0.
	if AllReachable(reports) {
		t.Fatal("verdict must fail with an unreachable candidate and a broken bundle")
	}

	// Table output carries candidate -> reachable/unreachable + error.
	table := FormatTable(reports)
	if !strings.Contains(table, "provB/m2") || !strings.Contains(table, "unreachable") ||
		!strings.Contains(table, "connection refused") || !strings.Contains(table, "reachable") {
		t.Fatalf("table output: %s", table)
	}

	// All-reachable run: exit verdict flips to 0 — over a checkout WITHOUT
	// the broken bundle (a broken bundle fails closed for ANY prober).
	cleanDir := t.TempDir()
	writeBundle(t, cleanDir, "feature-full", featureCandidates)
	fake2 := &fakeProber{}
	reports2, err := ProbeWorkflowsDir(context.Background(), fake2, cleanDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !AllReachable(reports2) {
		t.Fatal("all-reachable run must pass the verdict")
	}
	if !reports2[0].AllReachable || len(reports2[0].Rows) != 3 {
		t.Fatalf("all-reachable report: %+v", reports2[0])
	}
}

func TestProbeWorkflowsDirBundleFilter(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "feature-full", featureCandidates)
	writeBundle(t, dir, "bug-fix", `steps:
  - { id: code, kind: code }
`)
	fake := &fakeProber{}
	reports, err := ProbeWorkflowsDir(context.Background(), fake, dir, "bug-fix")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Bundle != "bug-fix" {
		t.Fatalf("filter: %+v", reports)
	}
	if _, err := ProbeWorkflowsDir(context.Background(), fake, dir, "nope"); err == nil {
		t.Fatal("unknown bundle filter must error")
	}
	if _, err := ProbeWorkflowsDir(context.Background(), fake, filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("unreadable workflows dir must error")
	}
}

func TestPiProberInvokesModelCallPath(t *testing.T) {
	// A fake pi executable: records argv, exits 3 when the model flag is
	// the "down" candidate, exit 0 otherwise.
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "fakepi")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > \""+argvFile+"\"\nfor a in \"$@\"; do\n  [ \"$a\" = \"down/1\" ] && exit 3\ndone\nexit 0\n"), 0o755)
	p := &PiProber{Bin: script}
	ctx := context.Background()
	if err := p.Probe(ctx, "up/1"); err != nil {
		t.Fatalf("up candidate must be reachable: %v", err)
	}
	raw, rerr := os.ReadFile(argvFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	// The micro request rides the adapter's model call path: --model <candidate> -p <prompt>.
	argv := string(raw)
	if !strings.Contains(argv, "--model up/1") || !strings.Contains(argv, "-p") || !strings.Contains(argv, MicroPrompt) {
		t.Fatalf("fake pi argv must carry the model call path: %q", argv)
	}
	err := p.Probe(ctx, "down/1")
	if err == nil || !strings.Contains(err.Error(), "down/1") {
		t.Fatalf("down candidate must be unreachable with the candidate named: %v", err)
	}
}

func TestProbeTimeoutBoundsMicroRequest(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slowpi")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := &PiProber{Bin: script}
	start := time.Now()
	err := p.Probe(ctx, "slow/1")
	if err == nil {
		t.Fatal("a candidate that never answers within the timeout must be unreachable")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the per-candidate timeout must bound the micro request")
	}
}

func count(calls []string, c string) int {
	n := 0
	for _, x := range calls {
		if x == c {
			n++
		}
	}
	return n
}
