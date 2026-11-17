package darwin_vet_gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProcessHygieneDarwinVetGate pins polyforge#28's acceptance: the
// test package — whose process-hygiene helper split by build tag — must
// cross-vet clean for darwin/arm64, darwin/amd64 and linux. The darwin
// arms are the reported regression (SysProcAttr.Pdeathsig is Linux-only
// and one untagged use made the whole package fail to compile on macOS);
// the linux arm keeps the Linux semantics compiling and catches a darwin
// file that lost its build tag (both would define setOwnGroupAttr).
//
// The gate lives outside package test on purpose: package test is the
// tree under vet, and this package must compile on darwin even when that
// one does not — the exec-gate pattern of test/w218_darwin_crossbuild_test.go.
func TestProcessHygieneDarwinVetGate(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go toolchain not on PATH")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ensureEmbedDist(t, root)
	for _, tc := range []struct{ goos, arch string }{
		{"darwin", "arm64"},
		{"darwin", "amd64"},
		{"linux", "arm64"},
	} {
		goos, arch := tc.goos, tc.arch
		t.Run(goos+"_"+arch, func(t *testing.T) {
			cmd := exec.Command(goBin, "vet", "./test")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOOS="+goos, "GOARCH="+arch)
			out, err := cmd.CombinedOutput()
			if err != nil {
				// Every quoted line is indented: a toolchain report line
				// quoted verbatim at a line start must not be mistaken for
				// this gate's own output.
				t.Fatalf("GOWORK=off GOOS=%s GOARCH=%s go vet ./test must exit 0 (the test package must compile; the Linux-only SysProcAttr.Pdeathsig must stay behind a linux build tag); observed failure:\n\t%s",
					goos, arch, strings.ReplaceAll(string(out), "\n", "\n\t"))
			}
		})
	}
}

// ensureEmbedDist guarantees the go:embed all:dist pattern has a file to
// match before go vet compiles the tree: webui/dist is generated and
// gitignored, so a fresh checkout (a replay worktree) has none. An empty
// placeholder is enough for embed discovery — scripts/clean-dist.mjs
// creates the directory the same way when it is missing.
func ensureEmbedDist(t *testing.T, root string) {
	dist := filepath.Join(root, "webui", "dist")
	if entries, err := os.ReadDir(dist); err == nil && len(entries) > 0 {
		return
	}
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatalf("webui/dist (go:embed all:dist) is missing and cannot be created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dist, ".keep"), nil, 0o644); err != nil {
		t.Fatalf("the webui/dist placeholder cannot be written: %v", err)
	}
}
