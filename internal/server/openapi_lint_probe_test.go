package server_test

// R3C-F5 regression probes for the wire-contract lint
// (api/openapi-lint.go, run via `go run` because it carries the `ignore`
// build tag). The lint's generationTokenParam resolved the token parameter
// but never checked `in`, so a contract edit moving the X-PF-Generation-Token
// credential to in:query — URLs and access logs — passed the presence and
// required-flag checks. The probes below build the REAL linter and run it
// over MUTATED copies of api/openapi.yaml: moving either token parameter
// (components/parameters/GenerationToken or GenerationTokenTiered) to
// in:query must fail with the in:header message, and the pristine contract
// must stay green as the positive control.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository root, relative to this package's test
// working directory (internal/server).
const lintRepoRoot = "../.."

// buildLint compiles the linter (file mode bypasses the `ignore` tag, the
// same way the Makefile's `go run ./api/openapi-lint.go` does) and returns
// the binary path.
func buildLint(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "openapi-lint")
	cmd := exec.Command("go", "build", "-o", bin, "./api/openapi-lint.go")
	cmd.Dir = lintRepoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the linter: %v\n%s", err, out)
	}
	return bin
}

// runLint runs the linter binary over the given contract copy and reports
// whether it passed, plus its combined output.
func runLint(t *testing.T, bin, contract string) (pass bool, output string) {
	t.Helper()
	cmd := exec.Command(bin, contract)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// mutatedContract copies api/openapi.yaml with exactly one token
// parameter's `in: header` flipped to `in: query`, and returns the copy's
// path.
func mutatedContract(t *testing.T, param string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lintRepoRoot, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The components block shape is
	// `    <Param>:\n      name: X-PF-Generation-Token\n      in: header`.
	old := param + ":\n      name: X-PF-Generation-Token\n      in: header"
	new := param + ":\n      name: X-PF-Generation-Token\n      in: query"
	if !strings.Contains(string(raw), old) {
		t.Fatalf("fixture drift: the %s parameter block was not found in api/openapi.yaml", param)
	}
	p := filepath.Join(t.TempDir(), "openapi-mutated.yaml")
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), old, new, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLintPristineContractPasses: positive control — the real linter over
// the untouched contract must exit zero (the lint is otherwise wired only
// into the Makefile's check target, so the test suite now guards it too).
func TestLintPristineContractPasses(t *testing.T) {
	bin := buildLint(t)
	pass, out := runLint(t, bin, filepath.Join(lintRepoRoot, "api", "openapi.yaml"))
	if !pass {
		t.Fatalf("pristine contract must lint clean:\n%s", out)
	}
}

// TestLintRejectsTokenParamMovedToQuery: BOTH token parameters must carry
// in:header. Moving either one to in:query must fail the lint with the
// R3C-F5 in:header message — never pass silently as it did pre-fix.
func TestLintRejectsTokenParamMovedToQuery(t *testing.T) {
	bin := buildLint(t)
	for _, param := range []string{"GenerationToken", "GenerationTokenTiered"} {
		pass, out := runLint(t, bin, mutatedContract(t, param))
		if pass {
			t.Fatalf("%s moved to in:query passed the lint", param)
		}
		if !strings.Contains(out, "in:header") {
			t.Fatalf("%s: expected the in:header failure, got:\n%s", param, out)
		}
	}
}
