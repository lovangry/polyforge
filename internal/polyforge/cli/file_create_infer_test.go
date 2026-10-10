package cli

// file_create_infer_test.go: the polyforge#79 file-create default — the
// cwd → workspace containment through the expanded [workspace] mapping,
// the shared strict-schema yaml loader, the single-project default, the
// ambiguity guards, and the byte-for-byte bypass of an explicit
// --project.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withWorkspaceMappingAt writes a scratch cli.toml whose [workspace]
// section maps the GIVEN workspace root to the given projects, and
// answers the XDG_CONFIG_HOME env entry pointing a runPf call at it.
func withWorkspaceMappingAt(t *testing.T, ws string, projects ...string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "server = \"http://infer-config.invalid:1\"\n\n[workspace.\"" + ws + "\"]\nprojects = [\"" + strings.Join(projects, "\", \"") + "\"]\n"
	if err := os.WriteFile(filepath.Join(xdg, "polyforge", "cli.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"XDG_CONFIG_HOME": xdg}
}

// runPfInCwd drives runPf with the inference cwd seam pinned to cwd (the
// production seam reads the process working directory; the test pins it
// — the CLI itself never chdirs).
func runPfInCwd(t *testing.T, cwd string, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	orig := osGetwd
	osGetwd = func() (string, error) { return cwd, nil }
	defer func() { osGetwd = orig }()
	return runPf(t, env, args...)
}

// inferFixture builds the shared shape: a fake aihub, a scratch
// workspace mapped to project "e2e", and (optionally) a workspace yaml.
func inferFixture(t *testing.T, yaml string) (srvHost string, env map[string]string, ws string) {
	t.Helper()
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	ws = t.TempDir()
	env = withWorkspaceMappingAt(t, ws, "e2e")
	env["PF_SERVER"] = srv.URL
	env["PF_API_KEY"] = "k_cli"
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(ws, ".polyforge.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return srv.URL, env, ws
}

// A cwd inside a mapped workspace whose yaml lists exactly one project
// infers it: the create proceeds as if --project had been passed.
func TestFileCreateInferenceSingleProject(t *testing.T) {
	_, env, ws := inferFixture(t, "projects:\n  e2e:\n    repos: []\n")
	code, _, stderr := runPfInCwd(t, ws, env, "file", "create", "--goal", "inferred goal")
	if code != 0 {
		t.Fatalf("inferred create: code=%d stderr=%s", code, stderr)
	}
}

// A multi-project yaml errors asking for --project — no silent pick.
func TestFileCreateInferenceMultiProjectAsksForFlag(t *testing.T) {
	_, env, ws := inferFixture(t, "projects:\n  e2e:\n    repos: []\n  second:\n    repos: []\n")
	code, _, stderr := runPfInCwd(t, ws, env, "file", "create", "--goal", "no pick")
	if code != 1 {
		t.Fatalf("multi-project inference: code=%d, want 1", code)
	}
	if !strings.Contains(stderr, "--project") {
		t.Fatalf("multi-project stderr %q does not ask for --project", stderr)
	}
}

// No yaml (and a cwd matching no workspace) keeps the #113 required-flag
// error byte-for-byte.
func TestFileCreateInferenceNoYamlKeepsRequiredFlagError(t *testing.T) {
	_, env, ws := inferFixture(t, "")
	code, _, stderr := runPfInCwd(t, ws, env, "file", "create", "--goal", "no yaml")
	if code != 1 {
		t.Fatalf("no-yaml inference: code=%d, want 1", code)
	}
	for _, tok := range []string{"--project is required", "polyforge#113", "[workspace", "doctor"} {
		if !strings.Contains(stderr, tok) {
			t.Fatalf("no-yaml stderr %q misses %q (the #113 error must stand verbatim)", stderr, tok)
		}
	}
	// A cwd matching NO mapped workspace: same error.
	code, _, _ = runPfInCwd(t, t.TempDir(), env, "file", "create", "--goal", "elsewhere")
	if code != 1 {
		t.Fatalf("cwd-outside inference: code=%d, want 1", code)
	}
}

// A valid workflow-only yaml (no projects section) also defers to the
// required-flag error — the yaml carries no project inventory to infer
// from.
func TestFileCreateInferenceWorkflowOnlyYamlDefers(t *testing.T) {
	_, env, ws := inferFixture(t, "workflow_defaults:\n  model_candidates: [prov/a]\n")
	code, _, stderr := runPfInCwd(t, ws, env, "file", "create", "--goal", "workflow only")
	if code != 1 {
		t.Fatalf("workflow-only inference: code=%d, want 1", code)
	}
	if !strings.Contains(stderr, "--project is required") {
		t.Fatalf("workflow-only stderr %q misses the required-flag guidance", stderr)
	}
}

// An invalid yaml under inference surfaces the typed error naming the
// file and the reason — never a silent skip to the required-flag error.
func TestFileCreateInferenceInvalidYamlSurfacesTypedError(t *testing.T) {
	_, env, ws := inferFixture(t, "projects:\n  e2e:\n    repos: [\n")
	code, _, stderr := runPfInCwd(t, ws, env, "file", "create", "--goal", "torn yaml")
	if code != 1 {
		t.Fatalf("invalid-yaml inference: code=%d, want 1", code)
	}
	if !strings.Contains(stderr, ".polyforge.yaml") {
		t.Fatalf("invalid-yaml stderr %q does not name the file", stderr)
	}
	if strings.Contains(stderr, "--project is required") {
		t.Fatalf("invalid-yaml stderr %q silently degraded to the required-flag error", stderr)
	}
}

// An explicit --project bypasses inference byte-for-byte: it wins over a
// yaml whose single listed project differs, and an invalid yaml never
// breaks an explicit create.
func TestFileCreateInferenceExplicitProjectBypasses(t *testing.T) {
	// A valid single-project yaml naming a DIFFERENT project.
	_, env, ws := inferFixture(t, "projects:\n  second:\n    repos: []\n")
	code, _, stderr := runPfInCwd(t, ws, env, "file", "create", "--project", "e2e", "--goal", "explicit wins")
	if code != 0 {
		t.Fatalf("explicit create over inference: code=%d stderr=%s", code, stderr)
	}
	// An invalid yaml never breaks an explicit create (no inference read).
	if err := os.WriteFile(filepath.Join(ws, ".polyforge.yaml"), []byte("projects:\n  e2e:\n    repos: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runPfInCwd(t, ws, env, "file", "create", "--project", "e2e", "--goal", "explicit over torn yaml")
	if code != 0 {
		t.Fatalf("explicit create over invalid yaml: code=%d stderr=%s", code, stderr)
	}
}

// Nested (degenerate) mapping config: the cwd inside two mapped
// workspaces errors asking for --project, naming the ambiguity.
func TestFileCreateInferenceNestedWorkspacesAskForFlag(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "server = \"http://infer-nested.invalid:1\"\n\n" +
		"[workspace.\"" + outer + "\"]\nprojects = [\"outer\"]\n\n" +
		"[workspace.\"" + inner + "\"]\nprojects = [\"inner\"]\n"
	if err := os.WriteFile(filepath.Join(xdg, "polyforge", "cli.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	env := map[string]string{"XDG_CONFIG_HOME": xdg, "PF_SERVER": srv.URL, "PF_API_KEY": "k_cli"}
	code, _, stderr := runPfInCwd(t, inner, env, "file", "create", "--goal", "nested")
	if code != 1 {
		t.Fatalf("nested-workspace inference: code=%d, want 1", code)
	}
	if !strings.Contains(stderr, "--project") || !strings.Contains(stderr, outer) || !strings.Contains(stderr, inner) {
		t.Fatalf("nested stderr %q must ask for --project and name the ambiguity", stderr)
	}
}
