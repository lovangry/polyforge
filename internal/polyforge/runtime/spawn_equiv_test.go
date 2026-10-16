package runtime

// spawn_equiv_test.go: the started exec.Cmd of both step harnesses — the
// argv, cwd, env names, process-group attribute and pipe wiring a Spawn
// actually starts — pinned per harness, so the shared start path behind
// piSpawner.Spawn and claudeSpawner.Spawn cannot drift from what each
// harness's builder produces. No real pi/claude runs: the binary is a
// shell stand-in that drains stdin.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/harness"
)

func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		out = append(out, name)
	}
	return out
}

func TestSpawnStartedCmdMatchesBuilder(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "stand-in")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{PiBin: bin, ClaudeBin: bin, MachineID: "m-eq"}
	base := SpawnParams{
		WorkItemID: "wi-eq", CapsuleDir: dir, InitialPrompt: "p",
		Model: "x/y", HarnessSession: testSession,
		OnRecord: func(string) {},
	}
	piP := base
	piP.Harness = harness.Pi
	clP := base
	clP.Harness = harness.Claude
	clWant, err := (&claudeSpawner{cfg: cfg}).buildCmd(clP)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		p         SpawnParams
		want      []string // argv after argv[0]
		wantEnv   []string
		procStdin bool // the handle keeps stdin (pi RPC) or closed it (claude)
	}{
		{"pi", piP, []string{"--mode", "rpc", "--no-skills", "--no-context-files",
			"--no-prompt-templates", "--model", "x/y"},
			newPiSpawner(cfg).buildCmd(piP).Env, true},
		{"claude", clP, clWant.Args[1:], clWant.Env, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := DefaultSpawner(cfg).Spawn(context.Background(), tc.p)
			if err != nil {
				t.Fatal(err)
			}
			pr := h.(*proc)
			defer func() {
				_ = pr.Terminate(time.Second)
				<-pr.exited
			}()
			c := pr.cmd
			if c.Path != bin || c.Args[0] != bin || !slices.Equal(c.Args[1:], tc.want) {
				t.Fatalf("argv %q", c.Args)
			}
			if c.Dir != dir {
				t.Fatalf("dir %q", c.Dir)
			}
			if !slices.Equal(envNames(c.Env), envNames(tc.wantEnv)) {
				t.Fatalf("env names drifted:\n got %q\nwant %q", envNames(c.Env), envNames(tc.wantEnv))
			}
			if !slices.Equal(c.Env, tc.wantEnv) {
				t.Fatal("env values drifted from the builder")
			}
			if c.SysProcAttr == nil || !c.SysProcAttr.Setpgid {
				t.Fatal("not its own process group")
			}
			if _, ok := c.Stdout.(*os.File); !ok {
				t.Fatalf("stdout is %T, want an own *os.File pipe", c.Stdout)
			}
			if _, ok := c.Stderr.(*os.File); !ok {
				t.Fatalf("stderr is %T, want an own *os.File pipe", c.Stderr)
			}
			if c.Stdin == nil {
				t.Fatal("no stdin pipe")
			}
			if (pr.stdin != nil) != tc.procStdin {
				t.Fatalf("handle stdin kept = %v, want %v", pr.stdin != nil, tc.procStdin)
			}
			if len(pr.taps) != 1 {
				t.Fatalf("taps = %d, want the OnRecord tap", len(pr.taps))
			}
		})
	}
}
