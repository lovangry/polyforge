package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestServes(t *testing.T) {
	cases := []struct {
		h, id string
		want  bool
	}{
		{"pi", "sub2api-glm/glm-5.3", true},
		{"", "sub2api-glm/glm-5.3", true},
		{"pi", "anthropic/claude-opus-5", true}, // provider-qualified: pi's
		{"pi", "claude-opus-5-5", false},
		{"pi", "s12-m1", true}, // neither shape: pi (pre-#159 meaning)
		{"claude", "claude-opus-5-5", true},
		{"claude", "claude-sonnet-5", true},
		{"claude", "anthropic/claude-opus-5", false},
		{"claude", "sub2api-glm/glm-5.3", false},
		{"claude", "opus", false}, // aliases are not accepted
		{"claude", "sonnet", false},
		{"claude", "", false},
		{"pi", "", false},
		{"codex", "gpt-6", false},
	}
	for _, c := range cases {
		if got := Serves(c.h, c.id); got != c.want {
			t.Errorf("Serves(%q, %q) = %v, want %v", c.h, c.id, got, c.want)
		}
	}
}

func TestFilter(t *testing.T) {
	mixed := []string{"sub2api-glm/glm-5.3", "claude-opus-5-5", "x/y", "claude-sonnet-5"}
	if got := Filter("claude", mixed); !reflect.DeepEqual(got, []string{"claude-opus-5-5", "claude-sonnet-5"}) {
		t.Fatalf("claude filter = %v", got)
	}
	if got := Filter("pi", mixed); !reflect.DeepEqual(got, []string{"sub2api-glm/glm-5.3", "x/y"}) {
		t.Fatalf("pi filter = %v", got)
	}
	if got := Filter("claude", nil); got != nil {
		t.Fatalf("nil list must stay nil, got %v", got)
	}
	if got := Filter("claude", []string{"a/b"}); got == nil || len(got) != 0 {
		t.Fatalf("no served id: want empty non-nil, got %#v", got)
	}
	if !Valid("pi") || !Valid("claude") || Valid("") || Valid("claude-code") {
		t.Fatal("Valid closed set")
	}
}

func TestSessionIDs(t *testing.T) {
	id, err := NewSessionID()
	if err != nil || !ValidSessionID(id) {
		t.Fatalf("minted %q (%v) is not valid", id, err)
	}
	for _, bad := range []string{"", "x", "../../etc/passwd", "ABCDEF01-2345-6789-abcd-ef0123456789", "0123456789abcdef0123456789abcdef0123", "01234567-89ab-cdef-0123-456789abcdeg"} {
		if ValidSessionID(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestLocateClaudeSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	capDir := "/tmp/ws/capsules/e2e-7"
	id, _ := NewSessionID()
	if LocateClaudeSession(id, capDir) != "" {
		t.Fatal("located a missing session")
	}
	p := filepath.Join(dir, "projects", "-tmp-ws-capsules-e2e-7", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if LocateClaudeSession(id, capDir) != "" {
		t.Fatal("an empty file is not a session")
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LocateClaudeSession(id, capDir); got != p {
		t.Fatalf("located %q, want %q", got, p)
	}
	// the same uuid under another cwd's project dir is never taken
	if got := LocateClaudeSession(id, "/tmp/ws/capsules/other-1"); got != "" {
		t.Fatalf("located another capsule's session %q", got)
	}
	if LocateClaudeSession("*", capDir) != "" {
		t.Fatal("a glob pattern is not an id")
	}
}
