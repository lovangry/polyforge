package driver

// testfiles_test.go: the committed-test-files gate (polyforge#156) over
// real temp git repos — a completed result whose output declares
// test_files is accepted only when every path is committed and clean in
// its task repo; anything else is an envelope rejection.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// gitRepo creates <capsule>/repos/<name> as a git repo with one commit
// holding the given files.
func gitRepo(t *testing.T, capsuleDir, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(capsuleDir, capsule.DirRepos, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q")
	for rel, body := range files {
		writeFile(t, dir, rel, body)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "seed")
	return dir
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func testFilesResult(files ...string) *StepResult {
	out, _ := json.Marshal(map[string]any{"scenarios_path": "artifacts/e2e-scenarios.md", "test_files": files})
	return &StepResult{WIID: "wi_tf", StepID: "e2e-author", SpawnN: 1, Outcome: OutcomeCompleted, Output: out}
}

func testFilesSpec(capsuleDir string) ResultSpec {
	return ResultSpec{WIID: "wi_tf", StepID: "e2e-author", SpawnN: 1, ReposDir: filepath.Join(capsuleDir, capsule.DirRepos)}
}

func TestTestFilesAllCommittedAccepted(t *testing.T) {
	caps := t.TempDir()
	gitRepo(t, caps, "app", map[string]string{"e2e/a_test.go": "a", "e2e/b_test.go": "b"})
	gitRepo(t, caps, "lib", map[string]string{"x_test.go": "x"})
	// Plain repo-relative, repos/<name>/ prefixed and <name>/ shorthand.
	res := testFilesResult("e2e/a_test.go", "repos/app/e2e/b_test.go", "x_test.go", "lib/x_test.go")
	if err := ValidateStepResult(res, testFilesSpec(caps)); err != nil {
		t.Fatalf("committed test files must be accepted: %v", err)
	}
}

func TestTestFilesUntrackedRejected(t *testing.T) {
	caps := t.TempDir()
	dir := gitRepo(t, caps, "app", map[string]string{"e2e/a_test.go": "a"})
	writeFile(t, dir, "e2e/new_test.go", "new")
	err := ValidateStepResult(testFilesResult("e2e/a_test.go", "e2e/new_test.go"), testFilesSpec(caps))
	if err == nil {
		t.Fatal("an untracked test file must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{"e2e/new_test.go (repos/app: not committed", "commit every declared test file"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("rejection must contain %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "e2e/a_test.go (") {
		t.Fatalf("the committed path must not be named as a problem: %s", msg)
	}
}

func TestTestFilesStagedOnlyRejected(t *testing.T) {
	caps := t.TempDir()
	dir := gitRepo(t, caps, "app", nil)
	writeFile(t, dir, "e2e/new_test.go", "new")
	runGit(t, dir, "add", "e2e/new_test.go")
	err := ValidateStepResult(testFilesResult("e2e/new_test.go"), testFilesSpec(caps))
	if err == nil || !strings.Contains(err.Error(), "e2e/new_test.go") {
		t.Fatalf("a staged-but-uncommitted test file must be rejected by name: %v", err)
	}
}

func TestTestFilesModifiedRejected(t *testing.T) {
	caps := t.TempDir()
	dir := gitRepo(t, caps, "app", map[string]string{"e2e/a_test.go": "a"})
	writeFile(t, dir, "e2e/a_test.go", "edited after commit")
	err := ValidateStepResult(testFilesResult("e2e/a_test.go"), testFilesSpec(caps))
	if err == nil || !strings.Contains(err.Error(), "e2e/a_test.go (repos/app: has uncommitted changes)") {
		t.Fatalf("a modified-but-uncommitted test file must be rejected by name: %v", err)
	}
}

func TestTestFilesMissingRejected(t *testing.T) {
	caps := t.TempDir()
	gitRepo(t, caps, "app", map[string]string{"e2e/a_test.go": "a"})
	err := ValidateStepResult(testFilesResult("e2e/ghost_test.go"), testFilesSpec(caps))
	if err == nil || !strings.Contains(err.Error(), "e2e/ghost_test.go (does not exist") {
		t.Fatalf("a nonexistent test file must be rejected by name: %v", err)
	}
	for _, p := range []string{"../escape_test.go", "/abs/x_test.go", "repos/nope/x_test.go"} {
		if err := ValidateStepResult(testFilesResult(p), testFilesSpec(caps)); err == nil || !strings.Contains(err.Error(), p) {
			t.Fatalf("%s must be rejected by name: %v", p, err)
		}
	}
}

func TestTestFilesEmptyUnaffected(t *testing.T) {
	caps := t.TempDir() // no repos/ at all
	spec := testFilesSpec(caps)
	for _, out := range []string{`{}`, `{"test_files":[]}`, `{"test_files":null}`} {
		res := testFilesResult()
		res.Output = json.RawMessage(out)
		if err := ValidateStepResult(res, spec); err != nil {
			t.Fatalf("%s: no declared test files must be unaffected: %v", out, err)
		}
	}
	// Non-completed outcomes are never judged by this gate.
	res := testFilesResult("ghost_test.go")
	res.Outcome = OutcomeIncomplete
	if err := ValidateStepResult(res, spec); err != nil {
		t.Fatalf("incomplete must be unaffected: %v", err)
	}
}

// TestRunTestFilesRejectionUsesRetryLane: the gate rides the in-process
// envelope-rejection lane — the step is re-dispatched with the rejection
// (naming the path) as feedback, and a committed retry is accepted.
func TestRunTestFilesRejectionUsesRetryLane(t *testing.T) {
	d, sp, execPath := runEnv(t, StepSpec{ID: "e2e-author"})
	dir := gitRepo(t, d.wi.CapsuleDir, "app", map[string]string{"e2e/ok_test.go": "ok"})
	writeFile(t, dir, "e2e/loose_test.go", "loose")
	sp.script = []fakeEntry{
		done(`{"test_files":["e2e/loose_test.go"]}`),
		done(`{"test_files":["e2e/ok_test.go"]}`),
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(sp.spawns); n != 2 {
		t.Fatalf("uncommitted test files must be re-dispatched once, got %d spawns", n)
	}
	if fb := sp.spawnAt(1).PriorRejection; !strings.Contains(fb, "e2e/loose_test.go") || !strings.Contains(fb, "commit") {
		t.Fatalf("feedback must name the uncommitted path: %q", fb)
	}
	if st := readExec(t, execPath).Steps["e2e-author"]; st.Status != capsule.StepDone {
		t.Fatalf("committed retry must be accepted: %v", st.Status)
	}
}
