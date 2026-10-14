// testfiles.go: the committed-test-files gate (polyforge#156).
//
// A step that authors test files (feature-full's e2e-author, bug-fix's
// repro — any step whose output declares test_files) owes them as a
// commit in their task repo. The prose duty alone was not enough: a
// resume re-clones the capsule and restores each task repo only at its
// carrier ref (refs/pf-task/<name>, which records HEAD), so an authored
// test left uncommitted is silently lost and the next step loops on a
// suite that no longer exists (pi#102). The driver therefore refuses a
// completed envelope whose declared test_files are not committed and
// clean — through the ordinary envelope-rejection retry lane, the same
// one the V4 command-evidence gate uses.
package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// declaredTestFiles reads the top-level output.test_files string list.
// An absent, empty or non-list value declares nothing (the declared
// schema lane stays the authority on its shape).
func declaredTestFiles(output json.RawMessage) []string {
	if len(output) == 0 {
		return nil
	}
	var probe struct {
		TestFiles []any `json:"test_files"`
	}
	if json.Unmarshal(output, &probe) != nil {
		return nil
	}
	var out []string
	for _, v := range probe.TestFiles {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// checkTestFilesCommitted verifies that every path the output declares in
// test_files is committed at HEAD and clean (no staged, unstaged or
// untracked change) in its task repo under reposDir (<capsule>/repos).
//
// Path resolution: test_files are repo-relative paths. A path written as
// repos/<name>/<rel> is judged in repo <name> only. Otherwise the path is
// looked up in every task repo and passes when it is committed and clean
// in at least one; when it exists in none and its first segment names a
// task repo, it is also tried as <name>/<rel> (the multi-repo shorthand).
// The error names every failing path and what is wrong with it.
func checkTestFilesCommitted(reposDir string, output json.RawMessage) error {
	files := declaredTestFiles(output)
	if len(files) == 0 {
		return nil
	}
	repos := taskRepoNames(reposDir)
	var problems []string
	for _, f := range files {
		if p := testFileProblem(reposDir, repos, f); p != "" {
			problems = append(problems, fmt.Sprintf("%s (%s)", f, p))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("uncommitted output.test_files: %s — commit every declared test file in its task repo (one dedicated commit, worktree clean for those paths) before completing; uncommitted files are lost when the work item is resumed",
		strings.Join(problems, "; "))
}

// taskRepoNames lists the task repos under reposDir (the same inventory
// the carrier-ref push walks: non-hidden directories).
func taskRepoNames(reposDir string) []string {
	ents, err := os.ReadDir(reposDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// testFileProblem answers "" when declared resolves to a committed, clean
// path in a task repo, else a short description of what is wrong.
func testFileProblem(reposDir string, repos []string, declared string) string {
	rel := path.Clean(filepath.ToSlash(declared))
	if path.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "not a repo-relative path"
	}
	if len(repos) == 0 {
		return "no task repo in the capsule"
	}
	if rest, ok := strings.CutPrefix(rel, "repos/"); ok {
		name, inner, _ := strings.Cut(rest, "/")
		if !contains(repos, name) || inner == "" {
			return fmt.Sprintf("repos/%s is not a task repo path", name)
		}
		return withRepo(name, fileState(filepath.Join(reposDir, name), inner))
	}
	// Plain repo-relative path: committed and clean in any repo passes;
	// otherwise report the most informative state found.
	var found string
	for _, name := range repos {
		st := fileState(filepath.Join(reposDir, name), rel)
		if st == "" {
			return ""
		}
		if st != stateMissing && found == "" {
			found = withRepo(name, st)
		}
	}
	if found != "" {
		return found
	}
	if name, inner, ok := strings.Cut(rel, "/"); ok && contains(repos, name) {
		return withRepo(name, fileState(filepath.Join(reposDir, name), inner))
	}
	return stateMissing + " in any task repo"
}

const stateMissing = "does not exist"

func withRepo(name, st string) string {
	if st == "" {
		return ""
	}
	return "repos/" + name + ": " + st
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// fileState answers "" when rel is committed at HEAD in repoDir with no
// staged, unstaged or untracked change, else what is wrong.
func fileState(repoDir, rel string) string {
	tree, err := repoGit(repoDir, "ls-tree", "--name-only", "HEAD", "--", rel)
	inHead := err == nil && strings.TrimSpace(tree) != ""
	if !inHead {
		if _, serr := os.Lstat(filepath.Join(repoDir, filepath.FromSlash(rel))); serr != nil {
			return stateMissing
		}
		return "not committed: untracked or only staged"
	}
	status, err := repoGit(repoDir, "status", "--porcelain", "--untracked-files=all", "--", rel)
	if err != nil {
		return "git status failed: " + err.Error()
	}
	if strings.TrimSpace(status) != "" {
		return "has uncommitted changes"
	}
	return ""
}

// repoGit runs one read-only git command inside a task repo with its own
// git-dir: inherited GIT_* variables (a capsule-level GIT_DIR binding
// included) are stripped, pathspecs are literal and no optional lock is
// taken.
func repoGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--no-optional-locks", "--literal-pathspecs"}, args...)...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "LC_ALL=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
