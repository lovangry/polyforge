package main

// daemon_test.go: FF-10 — the --workspace-root precedence regression,
// restored under the merged `pf` binary (it was deleted together with
// cmd/pf-daemon instead of being moved with the subcommand; the flags,
// env namespaces and precedence are unchanged by the merge). The
// workspace default must derive from the FINAL --home value
// (flag > env > $HOME fallback), so --home moves the whole local layout
// (data, workflows, skills AND capsules) consistently; POLYFORGE_WORKSPACE
// still overrides the home-derived default, and an explicit
// --workspace-root beats both.

import (
	"path/filepath"
	"testing"
)

func TestResolveWorkspaceRootPrecedence(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv("POLYFORGE_WORKSPACE", "/from-env")
		if got := resolveWorkspaceRoot("/home/x", "/explicit/ws"); got != "/explicit/ws" {
			t.Fatalf("flag = %q", got)
		}
	})
	t.Run("env beats home-derived default", func(t *testing.T) {
		t.Setenv("POLYFORGE_WORKSPACE", "/from-env")
		if got := resolveWorkspaceRoot("/home/x", ""); got != "/from-env" {
			t.Fatalf("env = %q", got)
		}
	})
	t.Run("home-derived default (the --home regression)", func(t *testing.T) {
		t.Setenv("POLYFORGE_WORKSPACE", "")
		// --home /custom/home must derive the workspace under it — the old
		// code defaulted the workspace from $HOME/POLYFORGE_HOME BEFORE
		// flag parsing, leaving capsules under the old workspace while
		// state/workflows/skills moved.
		if got := resolveWorkspaceRoot("/custom/home", ""); got != filepath.Join("/custom/home", "workspace") {
			t.Fatalf("home-derived = %q", got)
		}
	})
}
