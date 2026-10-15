package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCandidate(t *testing.T) {
	ok := []struct{ in, h, m string }{
		{"pi:sub2api-glm/glm-5.3", "pi", "sub2api-glm/glm-5.3"},
		{"claude:claude-opus-5-5", "claude", "claude-opus-5-5"},
		{"pi:anthropic/claude-opus-5", "pi", "anthropic/claude-opus-5"}, // the prefix, not the id shape, decides
		{"claude:sub2api-glm/glm-5.3", "claude", "sub2api-glm/glm-5.3"},
		{"pi:ollama/llama3:8b", "pi", "ollama/llama3:8b"}, // split on the FIRST ':'
		{"  pi : x/y  ", "pi", "x/y"},
	}
	for _, c := range ok {
		got, err := ParseCandidate(c.in)
		if err != nil || got.Harness != c.h || got.Model != c.m {
			t.Errorf("ParseCandidate(%q) = %+v, %v; want %s / %s", c.in, got, err, c.h, c.m)
		}
	}
	if s, _ := Normalize("  pi : x/y "); s != "pi:x/y" {
		t.Errorf("Normalize = %q", s)
	}
	bad := []struct{ in, want string }{
		{"sub2api-glm/glm-5.3", `"sub2api-glm/glm-5.3" needs a harness prefix (pi:… or claude:…)`},
		{"claude-opus-5-5", "needs a harness prefix"},
		{"codex:gpt-6", `unknown harness prefix "codex"`},
		{"PI:x", `unknown harness prefix "PI"`}, // the prefix is case-sensitive
		{"Claude:claude-opus-5-5", `unknown harness prefix "Claude"`},
		{"ollama/llama3:8b", `unknown harness prefix "ollama/llama3"`},
		{"claude-code:claude-opus-5-5", "unknown harness prefix"},
		{":m", "unknown harness prefix"},
		{"pi:", "names no model"},
		{"claude:  ", "names no model"},
		{"", "needs a harness prefix"},
	}
	for _, c := range bad {
		_, err := ParseCandidate(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseCandidate(%q) err = %v, want %q", c.in, err, c.want)
		}
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
	// polyforge#171: the locator is told the claude-config dir explicitly
	// (the caller names the dir its spawner hard-set into the child env);
	// the daemon's process env is never consulted.
	dir := t.TempDir()
	capDir := "/tmp/ws/capsules/e2e-7"
	id, _ := NewSessionID()
	if LocateClaudeSession(id, dir, "") != "" || LocateClaudeSession(id, "", capDir) != "" {
		t.Fatal("a missing claude-config dir or capsule locates nothing")
	}
	if LocateClaudeSession(id, dir, capDir) != "" {
		t.Fatal("located a missing session")
	}
	p := filepath.Join(dir, "projects", "-tmp-ws-capsules-e2e-7", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if LocateClaudeSession(id, dir, capDir) != "" {
		t.Fatal("an empty file is not a session")
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LocateClaudeSession(id, dir, capDir); got != p {
		t.Fatalf("located %q, want %q", got, p)
	}
	// the same uuid under another cwd's project dir is never taken
	if got := LocateClaudeSession(id, dir, "/tmp/ws/capsules/other-1"); got != "" {
		t.Fatalf("located another capsule's session %q", got)
	}
	if LocateClaudeSession("*", dir, capDir) != "" {
		t.Fatal("a glob pattern is not an id")
	}
}
