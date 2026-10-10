package observe

// release_intent_test.go: the seal-time release-intent gate's pins
// (polyforge#123, P1a + P1c). The fragment (artifacts/release-intent.json)
// is self-reported and trusted as to content; the gate validates SHAPE
// only (repo named, closed bump vocabulary, no duplicates), and the wrap
// predicate demands one entry per repo whose base→worktree diff touches
// anything outside the pure docs/tests exemption (only *.md and test/**
// are exempt). A pure docs/tests WI seals with no fragment at all.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

func gitInit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@invalid"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// intentRepoFixture lays one capsule directory with a single task-repo
// worktree (one commit on record, recorded as the base) and answers the
// capsule dir, repo name and base sha.
func intentRepoFixture(t *testing.T) (capDir, repo, base string) {
	t.Helper()
	capDir = t.TempDir()
	repo = "demo-src"
	dir := filepath.Join(capDir, capsule.DirRepos, repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-q", "-m", "seed")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	base = strings.TrimSpace(string(out))

	idn := capsule.Identity{
		WorkItemID: "wi_intent",
		Repos:      map[string]string{repo: base},
	}
	raw, err := json.Marshal(idn)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, capsule.CapsuleJSON), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return capDir, repo, base
}

// writeIntent writes (or removes, with content "") the capsule's fragment.
func writeIntent(t *testing.T, capDir, content string) {
	t.Helper()
	p := filepath.Join(capDir, filepath.FromSlash(ReleaseIntentFile))
	if content == "" {
		_ = os.Remove(p)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// changeRepo writes one file into the repo worktree and (when commit it)
// commits it — the committed and untracked shapes must gate identically.
func changeRepo(t *testing.T, capDir, repo, rel, content string, commit bool) {
	t.Helper()
	dir := filepath.Join(capDir, capsule.DirRepos, repo)
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if commit {
		gitInit(t, dir, "add", "-A")
		gitInit(t, dir, "commit", "-q", "-m", "change "+rel)
	}
}

func TestParseReleaseIntentSchema(t *testing.T) {
	t.Run("valid fragment with note", func(t *testing.T) {
		frag, err := parseReleaseIntent([]byte(`{"repos": [{"repo": "demo-src", "bump": "minor", "note": "one line"}]}`))
		if err != nil {
			t.Fatalf("valid fragment refused: %v", err)
		}
		if !frag.covers("demo-src") || frag.covers("other") {
			t.Fatalf("covers misbehaves: %+v", frag)
		}
	})
	t.Run("name alias accepted", func(t *testing.T) {
		frag, err := parseReleaseIntent([]byte(`{"repos": [{"name": "demo-src", "bump": "patch"}]}`))
		if err != nil {
			t.Fatalf("alias fragment refused: %v", err)
		}
		if !frag.covers("demo-src") {
			t.Fatalf("alias entry lost: %+v", frag)
		}
	})
	t.Run("empty repos array is valid shape", func(t *testing.T) {
		if _, err := parseReleaseIntent([]byte(`{"repos": []}`)); err != nil {
			t.Fatalf("empty repos refused: %v", err)
		}
	})
	t.Run("missing repo name refused", func(t *testing.T) {
		_, err := parseReleaseIntent([]byte(`{"repos": [{"bump": "patch"}]}`))
		if err == nil || !strings.Contains(err.Error(), ReleaseIntentFile) {
			t.Fatalf("want a typed refusal naming the file, got %v", err)
		}
	})
	t.Run("bump outside the vocabulary refused", func(t *testing.T) {
		_, err := parseReleaseIntent([]byte(`{"repos": [{"repo": "r", "bump": "huge"}]}`))
		if err == nil || !strings.Contains(err.Error(), "huge") {
			t.Fatalf("want the bump vocabulary refusal, got %v", err)
		}
	})
	t.Run("duplicate repo refused", func(t *testing.T) {
		_, err := parseReleaseIntent([]byte(`{"repos": [{"repo": "r", "bump": "patch"}, {"repo": "r", "bump": "minor"}]}`))
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("want the duplicate refusal, got %v", err)
		}
	})
	t.Run("invalid json refused", func(t *testing.T) {
		_, err := parseReleaseIntent([]byte(`{`))
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("want the JSON refusal, got %v", err)
		}
	})
	t.Run("absent file answers nil nil", func(t *testing.T) {
		frag, err := loadReleaseIntent(t.TempDir())
		if err != nil || frag != nil {
			t.Fatalf("absent fragment: %v %+v, want nil nil", err, frag)
		}
	})
}

func TestIntentExemptPath(t *testing.T) {
	exempt := []string{"notes.md", "docs/guide.md", "test/w123fixture.md", "internal/pkg/test/deep_test.md", "test"}
	releaseable := []string{"src/feature.go", "main.go", "cmd/pf/main.go", "testdata/fixture.bin", "internal/thing.md.go.txt"}
	for _, p := range exempt {
		if !intentExemptPath(p) {
			t.Fatalf("%q must be exempt", p)
		}
	}
	for _, p := range releaseable {
		if intentExemptPath(p) {
			t.Fatalf("%q must be releaseable", p)
		}
	}
}

func TestReleaseIntentMissingFor(t *testing.T) {
	t.Run("non-docs change with no fragment is missing", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "src/feature.go", "package demo\n", false)
		missing := releaseIntentMissingFor(capDir, "wi_intent")
		if len(missing) != 1 || missing[0] != repo {
			t.Fatalf("missing = %v, want [%s]", missing, repo)
		}
		if ref := releaseIntentRefusal("wi_intent", missing); !strings.Contains(ref, ReleaseIntentFile) || !strings.Contains(ref, repo) {
			t.Fatalf("refusal must name the file and the repo: %q", ref)
		}
	})
	t.Run("committed non-docs change is missing too", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "src/feature.go", "package demo\n", true)
		if missing := releaseIntentMissingFor(capDir, "wi_intent"); len(missing) != 1 {
			t.Fatalf("missing = %v, want the repo", missing)
		}
	})
	t.Run("a covering fragment seals", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "src/feature.go", "package demo\n", false)
		writeIntent(t, capDir, `{"repos": [{"repo": "demo-src", "bump": "minor", "note": "fixture"}]}`)
		if missing := releaseIntentMissingFor(capDir, "wi_intent"); len(missing) != 0 {
			t.Fatalf("missing = %v, want empty", missing)
		}
	})
	t.Run("a fragment without the changed repo's entry is missing", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "src/feature.go", "package demo\n", false)
		writeIntent(t, capDir, `{"repos": [{"repo": "some-other-repo", "bump": "patch", "note": "wrong repo"}]}`)
		if missing := releaseIntentMissingFor(capDir, "wi_intent"); len(missing) != 1 || missing[0] != repo {
			t.Fatalf("missing = %v, want [%s]", missing, repo)
		}
	})
	t.Run("a schema-invalid fragment is refused", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "src/feature.go", "package demo\n", false)
		writeIntent(t, capDir, `{"repos": [{"repo": "demo-src", "bump": "enormous"}]}`)
		missing := releaseIntentMissingFor(capDir, "wi_intent")
		if len(missing) != 1 || !strings.Contains(missing[0], ReleaseIntentFile) {
			t.Fatalf("missing = %v, want the typed fragment error", missing)
		}
	})
	t.Run("pure docs and tests seals with no fragment", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "notes.md", "# notes\n", false)
		changeRepo(t, capDir, repo, "test/fixture.md", "test fixture\n", false)
		if missing := releaseIntentMissingFor(capDir, "wi_intent"); len(missing) != 0 {
			t.Fatalf("missing = %v, want empty (docs/tests exempt)", missing)
		}
	})
	t.Run("docs change plus code change still needs the entry", func(t *testing.T) {
		capDir, repo, _ := intentRepoFixture(t)
		changeRepo(t, capDir, repo, "notes.md", "# notes\n", false)
		changeRepo(t, capDir, repo, "src/feature.go", "package demo\n", false)
		if missing := releaseIntentMissingFor(capDir, "wi_intent"); len(missing) != 1 {
			t.Fatalf("missing = %v, want the repo", missing)
		}
	})
	t.Run("a capsule with no repos owes no fragment", func(t *testing.T) {
		capDir := t.TempDir()
		raw, err := json.Marshal(capsule.Identity{WorkItemID: "wi_x", Repos: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(capDir, capsule.CapsuleJSON), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if missing := releaseIntentMissingFor(capDir, "wi_x"); len(missing) != 0 {
			t.Fatalf("missing = %v, want empty", missing)
		}
	})
}
