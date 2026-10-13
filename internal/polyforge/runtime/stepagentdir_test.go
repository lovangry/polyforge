package runtime

// The dedicated step agent directory (audit-subtraction pass B, addendum
// 2 / owner ruling A): the minimal-set invariant, the copy (not link)
// discipline, the re-sync on a changed source, the placement under the
// daemon data dir, and the loud refusal when an explicitly configured
// source carries no credentials.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// operatorDirFixture lays out a realistic operator agent directory: the
// minimal set AND the isolation set (settings.json declaring a
// user-level package — the reinstall-poison shape — plus the other
// instruction surfaces that must never be snapshotted).
func operatorDirFixture(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("auth.json", `{"token":"operator-secret"}`)
	write("models.json", `{"providers":{"x":{}}}`)
	write("agents/reviewer.md", "# reviewer subagent\n")
	write("agents/nested/deep.md", "# nested subagent\n")
	// the isolation set — none of this may ever be copied
	write("settings.json", `{"packages":["npm:pi-background-tasks"]}`)
	write("keybindings.json", `{}`)
	write("mcp.json", `{}`)
	write("AGENTS.md", "operator context\n")
	write("extensions/ext.js", "// operator extension\n")
	write("skills/op/SKILL.md", "operator skill\n")
	return src
}

func listDirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestStepAgentDirMinimalSetAndPlacement(t *testing.T) {
	src := operatorDirFixture(t)
	cfg := Config{DataDir: t.TempDir(), OperatorAgentDir: src, OperatorAgentDirExplicit: true}
	if err := cfg.SyncStepAgentDir(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	dir := cfg.StepAgentDir()
	// A1: under the daemon data dir, never inside a capsule.
	if rel, err := filepath.Rel(cfg.DataDir, dir); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("the dedicated dir %s is not under the data dir %s", dir, cfg.DataDir)
	}
	// A2: EXACTLY the enumerated set.
	got := listDirNames(t, dir)
	want := map[string]bool{"auth.json": true, "models.json": true, "agents": true}
	if len(got) != len(want) {
		t.Fatalf("the dedicated dir must hold exactly auth.json/models.json/agents, got %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected entry %q in the dedicated dir (%v)", n, got)
		}
	}
	// the subagent definitions travel, recursively
	for _, rel := range []string{"agents/reviewer.md", "agents/nested/deep.md"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s was not copied: %v", rel, err)
		}
	}
	// credentials land 0600 and are a COPY (no symlink into the
	// operator's directory, which would re-open the whole dir)
	st, err := os.Lstat(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("auth.json must be a copy, never a symlink into the operator agent dir")
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth.json must be 0600, got %v", perm)
	}
}

func TestStepAgentDirNeverCopiesSettings(t *testing.T) {
	src := operatorDirFixture(t)
	cfg := Config{DataDir: t.TempDir(), OperatorAgentDir: src}
	if err := cfg.SyncStepAgentDir(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// the whole tree, not just the top level: no settings.json and no
	// package declaration anywhere under the dedicated directory
	err := filepath.WalkDir(cfg.StepAgentDir(), func(p string, de os.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() {
			return err
		}
		if de.Name() == "settings.json" {
			t.Fatalf("settings.json (the user-level package declaration) was copied to %s", p)
		}
		raw, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(raw), "pi-background-tasks") {
			t.Fatalf("a background-task package declaration reached %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStepAgentDirResyncFollowsSourceAndPrunesExtras(t *testing.T) {
	src := operatorDirFixture(t)
	cfg := Config{DataDir: t.TempDir(), OperatorAgentDir: src}
	if err := cfg.SyncStepAgentDir(); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	dir := cfg.StepAgentDir()
	// an operator credential rotation (size+mtime change) is picked up
	// by the next sync — the per-spawn re-check
	rotated := `{"token":"rotated-secret-value"}`
	authSrc := filepath.Join(src, "auth.json")
	if err := os.WriteFile(authSrc, []byte(rotated), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(authSrc, future, future); err != nil {
		t.Fatal(err)
	}
	// something (a stray write, an older layout) put extras in the
	// dedicated dir: the sync restores the minimal-set invariant
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"packages":["npm:x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a source file that disappeared must disappear here too
	if err := os.Remove(filepath.Join(src, "models.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "agents", "reviewer.md")); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SyncStepAgentDir(); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil || string(raw) != rotated {
		t.Fatalf("the rotated credentials were not re-copied: %q (%v)", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "models.json")); !os.IsNotExist(err) {
		t.Fatalf("a models.json the source no longer has must be dropped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "reviewer.md")); !os.IsNotExist(err) {
		t.Fatalf("a deleted subagent definition must be dropped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "nested", "deep.md")); err != nil {
		t.Fatalf("a surviving subagent definition must stay: %v", err)
	}
	for _, extra := range []string{"settings.json", "extensions"} {
		if _, err := os.Stat(filepath.Join(dir, extra)); !os.IsNotExist(err) {
			t.Fatalf("the sync must prune %q from the dedicated dir: %v", extra, err)
		}
	}
}

func TestStepAgentDirExplicitSourceWithoutCredentialsRefuses(t *testing.T) {
	src := t.TempDir() // configured, but no auth.json
	cfg := Config{DataDir: t.TempDir(), OperatorAgentDir: src, OperatorAgentDirExplicit: true}
	err := cfg.SyncStepAgentDir()
	if err == nil {
		t.Fatal("an explicitly configured source without credentials must refuse (a step agent without credentials is a dead end)")
	}
	var sae *StepAgentDirError
	if !errors.As(err, &sae) {
		t.Fatalf("the refusal must be the typed StepAgentDirError, got %T: %v", err, err)
	}
	// the implicit default (no flag/env) carries on instead: the harness
	// adapter may hold its credentials elsewhere
	cfg2 := Config{DataDir: t.TempDir(), OperatorAgentDir: src}
	if err := cfg2.SyncStepAgentDir(); err != nil {
		t.Fatalf("the implicit default without file credentials must carry on, got: %v", err)
	}
	if _, err := os.Stat(cfg2.StepAgentDir()); err != nil {
		t.Fatalf("the dedicated dir must exist even without credentials: %v", err)
	}
}

func TestStepAgentDirDefaultsAndSessionsAreSeparate(t *testing.T) {
	cfg := Config{DataDir: t.TempDir()}
	cfg.FillDefaults()
	if cfg.OperatorAgentDir == "" {
		t.Fatal("the source defaults to the operator agent dir")
	}
	if cfg.OperatorAgentDirExplicit {
		t.Fatal("a defaulted source is never explicit (its missing credentials are a warning, not a refusal)")
	}
	if cfg.HarnessSessionsRoot != cfg.StepAgentSessionsDir() {
		t.Fatalf("the live-transcript locator root must follow the step processes' session store, got %q", cfg.HarnessSessionsRoot)
	}
	// the sessions live OUTSIDE the dedicated agent dir, so the minimal
	// set stays exactly the enumerated one
	if rel, err := filepath.Rel(cfg.StepAgentDir(), cfg.StepAgentSessionsDir()); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the session store %s must not sit inside the dedicated agent dir %s", cfg.StepAgentSessionsDir(), cfg.StepAgentDir())
	}
}
