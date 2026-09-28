package cli

// verbs_probe_test.go: `pf probe-candidates` at the command dispatch
// level — reachability table output and the exit-code contract
// (07 §6.1 step 6): 0 only when every candidate answered, 1 on any
// unreachable candidate, 2 on usage errors.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProbePi answers per candidate: any candidate containing "down"
// exits 3, everything else exits 0.
const fakeProbePi = "#!/bin/sh\nfor a in \"$@\"; do\n  case \"$a\" in *down*) exit 3;; esac\ndone\nexit 0\n"

func runProbeCandidates(t *testing.T, workflowsDir, extra string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	args := []string{"probe-candidates", "--workflows-dir", workflowsDir, "--pi-bin", writeFakeProbePi(t)}
	if extra != "" {
		args = append(args, strings.Fields(extra)...)
	}
	code := Run(args, strings.NewReader(""), &out, &errOut)
	return code, out.String() + errOut.String()
}

func writeFakeProbePi(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakepi")
	if err := os.WriteFile(p, []byte(fakeProbePi), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func probeBundleDir(t *testing.T, candidates string) string {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "demo")
	os.MkdirAll(filepath.Join(bundle, "agents"), 0o755)
	body := "---\nname: demo\ndefaults:\n  model_candidates: " + candidates + "\nsteps:\n  - { id: code, kind: code }\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProbeCandidatesAllReachableExitsZero(t *testing.T) {
	dir := probeBundleDir(t, "[prov/up-1, prov/up-2]")
	code, out := runProbeCandidates(t, dir, "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, out)
	}
	if !strings.Contains(out, "prov/up-1") || !strings.Contains(out, "reachable") {
		t.Fatalf("table output: %s", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Fatalf("no candidate may be unreachable: %s", out)
	}
}

func TestProbeCandidatesUnreachableExitsOne(t *testing.T) {
	dir := probeBundleDir(t, "[prov/up-1, prov/down-1]")
	code, out := runProbeCandidates(t, dir, "")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (unreachable candidate present)", code)
	}
	if !strings.Contains(out, "prov/down-1") || !strings.Contains(out, "unreachable") {
		t.Fatalf("the table must show the unreachable candidate: %s", out)
	}
	// --json mode (a GLOBAL flag, ahead of the verb): the machine-readable
	// report rides the same exit verdict.
	var out2 strings.Builder
	code = Run([]string{"--json", "probe-candidates", "--workflows-dir", dir, "--pi-bin", writeFakeProbePi(t)},
		strings.NewReader(""), &out2, &out2)
	if code != 1 {
		t.Fatalf("json exit code = %d, want 1", code)
	}
	out = out2.String()
	if !strings.Contains(out, `"all_reachable": false`) || !strings.Contains(out, `"candidate": "prov/down-1"`) {
		t.Fatalf("json report: %s", out)
	}
}

func TestProbeCandidatesErrorExitCodes(t *testing.T) {
	dir := probeBundleDir(t, "[prov/up-1]")
	// unknown adapter flag value -> typed runtime error (exit 1)
	code, _ := runProbeCandidates(t, dir, "--adapter nope")
	if code != 1 {
		t.Fatalf("unknown adapter exit code = %d, want 1", code)
	}
	// a stray positional -> runtime error (exit 1, the CLI's convention
	// for argument-count mistakes inside a verb; 2 is reserved for flag
	// parse errors, which the flag package itself reports).
	var out strings.Builder
	if code := Run([]string{"probe-candidates", "stray-positional"}, strings.NewReader(""), &out, &out); code != 1 {
		t.Fatalf("positional argument exit code = %d, want 1", code)
	}
	// unknown bundle filter -> runtime error (1)
	code, outStr := runProbeCandidates(t, dir, "--bundle missing")
	if code != 1 || !strings.Contains(outStr, "missing") {
		t.Fatalf("unknown bundle: code=%d out=%s", code, outStr)
	}
}
