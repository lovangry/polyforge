// testfiles.go: the committed-test-files gate (polyforge#156).
//
// A step that authors test files (feature-full's e2e-author, bug-fix's
// repro — any step whose output declares test_files) owes them as a
// commit in their task repo. The prose duty alone was not enough: a
// resume re-clones the capsule and restores each task repo only at its
// carrier ref (refs/pf-task/<name>, which records HEAD), so an authored
// test left uncommitted is silently lost and the next step loops on a
// suite that no longer exists (pi#102). The driver therefore refuses a
// completed envelope whose declared test_files are not committed at
// HEAD — through the ordinary envelope-rejection retry lane, the same
// one the V4 command-evidence gate uses.
//
// The task repos are agent-controlled and agents are untrusted, so the
// gate only ever runs `git ls-tree` (no worktree scan: no fsmonitor, no
// clean/smudge filter, no hook), with repo-local fsmonitor and hooks
// disabled on the command line, system config ignored, inherited GIT_*
// stripped and a hard timeout.
package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// testFilesGitTimeout bounds one git invocation of the gate.
const testFilesGitTimeout = 10 * time.Second

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

// checkTestFilesCommitted verifies the paths the output declares in
// test_files against the task repos under reposDir (<capsule>/repos).
//
// The rule (the step schemas' shape): every path is repo-relative
// (cleaned, not absolute, no ".."); it must exist on disk in at least
// one task repo; and in EVERY task repo where it exists on disk it must
// be committed at HEAD — the only state a resume restores. The error
// names each failing path and the repo(s) where it is not committed.
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
	return fmt.Errorf("uncommitted output.test_files: %s — commit every declared test file in its task repo (one dedicated commit; `git add -f` a path under a .gitignore'd directory) BEFORE writing the result envelope (the step process is torn down once the envelope appears); uncommitted files are lost when the work item is resumed",
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

// testFileProblem answers "" when declared passes the rule, else a short
// description of what is wrong.
func testFileProblem(reposDir string, repos []string, declared string) string {
	rel := path.Clean(filepath.ToSlash(declared))
	if path.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "not a repo-relative path"
	}
	var present, uncommitted []string
	for _, name := range repos {
		dir := filepath.Join(reposDir, name)
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			continue
		}
		present = append(present, name)
		if !committedAtHead(dir, rel) {
			uncommitted = append(uncommitted, "repos/"+name)
		}
	}
	if len(present) == 0 {
		return "does not exist in any task repo"
	}
	if len(uncommitted) > 0 {
		return "not committed at HEAD in " + strings.Join(uncommitted, ", ")
	}
	return ""
}

// committedAtHead reports whether rel is part of HEAD's tree in repoDir.
// An unborn HEAD, a non-repo or a git failure all answer false.
func committedAtHead(repoDir, rel string) bool {
	out, err := repoGit(repoDir, "ls-tree", "--name-only", "HEAD", "--", rel)
	return err == nil && strings.TrimSpace(out) != ""
}

// repoGit runs one read-only git command inside an (untrusted) task repo:
// its own git-dir (inherited GIT_* stripped), system config ignored,
// fsmonitor and hooks disabled, literal pathspecs, no optional locks and
// a hard timeout.
func repoGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), testFilesGitTimeout)
	defer cancel()
	argv := append([]string{
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"--no-optional-locks", "--literal-pathspecs",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "LC_ALL=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
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
