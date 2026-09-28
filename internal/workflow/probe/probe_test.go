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

// TestProbeWorkflowsDirDedupesCandidatesAcrossBundles: the reference
// checkout case (bug-fix, chore, feature-full, quick-fix all declaring
// the same three candidates) must issue ONE micro request per unique
// candidate — 4 × 3 declarations → exactly 3 Probe calls — while every
// bundle's report still carries its full row set, each shared candidate
// showing the SAME shared outcome in every bundle that declares it.
func TestProbeWorkflowsDirDedupesCandidatesAcrossBundles(t *testing.T) {
	dir := t.TempDir()
	sameThree := `defaults:
  model_candidates: [provA/m1, provB/m2, provC/m3]
steps:
  - { id: code, kind: code }
`
	for _, name := range []string{"bug-fix", "chore", "feature-full", "quick-fix"} {
		writeBundle(t, dir, name, sameThree)
	}
	fake := &fakeProber{unreachable: map[string]error{
		"provB/m2": errors.New("connection refused (dial tcp 127.0.0.1:1)"),
	}}
	reports, err := ProbeWorkflowsDir(context.Background(), fake, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 4 {
		t.Fatalf("want 4 bundle reports, got %d", len(reports))
	}
	// Exactly 3 Probe calls: one per unique candidate, none repeated per
	// bundle.
	if len(fake.calls) != 3 {
		t.Fatalf("Probe called %d times (%v), want exactly 3 (one per unique candidate)", len(fake.calls), fake.calls)
	}
	want := map[string]int{"provA/m1": 1, "provB/m2": 1, "provC/m3": 1}
	for c, n := range want {
		if got := count(fake.calls, c); got != n {
			t.Fatalf("candidate %s probed %d times, want %d", c, got, n)
		}
	}
	// Report completeness: every bundle carries all three candidates in
	// its declared order, the shared unreachable candidate reading the
	// SAME shared outcome (unreachable + error) in each bundle.
	for _, rep := range reports {
		if len(rep.Rows) != 3 {
			t.Fatalf("bundle %s rows = %+v, want all 3 candidates", rep.Bundle, rep.Rows)
		}
		for i, cand := range []string{"provA/m1", "provB/m2", "provC/m3"} {
			if rep.Rows[i].Candidate != cand {
				t.Fatalf("bundle %s row %d = %q, want %q (declared order)", rep.Bundle, i, rep.Rows[i].Candidate, cand)
			}
		}
		m2 := rep.Rows[1]
		if m2.Reachable || m2.Error != "connection refused (dial tcp 127.0.0.1:1)" {
			t.Fatalf("bundle %s must show provB/m2 unreachable with the shared error: %+v", rep.Bundle, m2)
		}
		if !rep.Rows[0].Reachable || !rep.Rows[2].Reachable {
			t.Fatalf("bundle %s: provA/m1 and provC/m3 must be reachable: %+v", rep.Bundle, rep.Rows)
		}
		if rep.AllReachable {
			t.Fatalf("bundle %s with an unreachable candidate must not be all-reachable", rep.Bundle)
		}
	}
	// The verdict fails on the unreachable candidate, and the table still
	// renders every bundle's full row set with the shared outcome.
	if AllReachable(reports) {
		t.Fatal("verdict must fail with an unreachable candidate")
	}
	table := FormatTable(reports)
	for _, s := range []string{"bug-fix", "chore", "feature-full", "quick-fix", "provA/m1", "provB/m2", "provC/m3", "connection refused"} {
		if !strings.Contains(table, s) {
			t.Fatalf("table must contain %q:\n%s", s, table)
		}
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
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > \""+argvFile+"\"\nfor a in \"$@\"; do\n  [ \"$a\" = \"down\" ] && exit 3\ndone\nexit 0\n"), 0o755)
	p := &PiProber{Bin: script}
	ctx := context.Background()
	if err := p.Probe(ctx, "up/1"); err != nil {
		t.Fatalf("up candidate must be reachable: %v", err)
	}
	raw, rerr := os.ReadFile(argvFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	// The micro request rides the adapter's model call path: a
	// provider-qualified candidate must arrive SPLIT as --provider up
	// --model 1 (the whole qualified string in --model makes pi fall
	// back to its default provider — measured on the first real
	// install), then -p <prompt>.
	argv := string(raw)
	if !strings.Contains(argv, "--provider up") || !strings.Contains(argv, "--model 1") || strings.Contains(argv, "--model up/1") || !strings.Contains(argv, "-p") || !strings.Contains(argv, MicroPrompt) {
		t.Fatalf("fake pi argv must carry the split provider/model call path: %q", argv)
	}
	// A bare model id keeps the single-flag form: provider left to
	// pi's own default.
	if err := p.Probe(ctx, "bare-model"); err != nil {
		t.Fatalf("bare candidate must be reachable: %v", err)
	}
	if raw, _ := os.ReadFile(argvFile); !strings.Contains(string(raw), "--model bare-model") {
		t.Fatalf("bare candidate must pass --model bare-model: %q", string(raw))
	}
	err := p.Probe(ctx, "down/1")
	if err == nil || !strings.Contains(err.Error(), "down/1") {
		t.Fatalf("down candidate must be unreachable with the candidate named: %v", err)
	}
}

func TestClaudeProberStripsProviderPrefix(t *testing.T) {
	// A fake claude executable: records argv, exit 0.
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "fakeclaude")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > \""+argvFile+"\"\nexit 0\n"), 0o755)
	c := &ClaudeProber{Bin: script}
	if err := c.Probe(context.Background(), "sub2api-claude/pi-claude-opus-5"); err != nil {
		t.Fatalf("probe must succeed through the fake: %v", err)
	}
	raw, _ := os.ReadFile(argvFile)
	// claude --model takes the LOCAL model id only; the provider prefix
	// must be stripped, never passed as part of the model string.
	if !strings.Contains(string(raw), "--model pi-claude-opus-5") || strings.Contains(string(raw), "--model sub2api-claude/pi-claude-opus-5") {
		t.Fatalf("fake claude argv must carry the stripped model: %q", string(raw))
	}
	// A bare id passes through unchanged.
	if err := c.Probe(context.Background(), "opus-x"); err != nil {
		t.Fatalf("bare candidate must be reachable: %v", err)
	}
	if raw, _ := os.ReadFile(argvFile); !strings.Contains(string(raw), "--model opus-x") {
		t.Fatalf("bare candidate must pass --model opus-x: %q", string(raw))
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
