// Package harness names the step harnesses a workflow step can run on
// (polyforge#159) and the one model ↔ harness compatibility rule. There
// is no model registry: compatibility is read off the model id's shape.
//
//   - claude (Claude Code, `claude -p --bare`) serves the official
//     Anthropic model ids: a `claude-` prefix and no `/`
//     (claude-opus-5-5). Aliases (opus, sonnet) are not accepted — they
//     name "the latest" and are not reproducible.
//   - pi serves every other id: the provider-qualified `provider/model`
//     form its --model takes (sub2api-glm/glm-5.3, anthropic/claude-…),
//     and — so every pre-#159 configuration keeps its meaning — any
//     other id that is not a claude id.
//
// The two sets are disjoint, so one candidate list may mix both: a pi
// step uses its pi ids, a claude step its claude ids.
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// The closed harness set. An absent harness ("") is Pi everywhere.
const (
	Pi     = "pi"
	Claude = "claude"
)

// Names is the closed set in display order.
var Names = []string{Pi, Claude}

// Valid reports whether name is a harness of the closed set.
func Valid(name string) bool { return name == Pi || name == Claude }

// Of normalizes an absent harness to Pi.
func Of(name string) string {
	if name == "" {
		return Pi
	}
	return name
}

// IsClaudeModel reports whether id has the official Claude model-id
// shape: `claude-` prefix, no `/`.
func IsClaudeModel(id string) bool {
	return strings.HasPrefix(id, "claude-") && !strings.Contains(id, "/")
}

// Serves reports whether harness h ("" = pi) can run model id.
func Serves(h, id string) bool {
	if id == "" {
		return false
	}
	switch Of(h) {
	case Claude:
		return IsClaudeModel(id)
	case Pi:
		return !IsClaudeModel(id)
	}
	return false
}

// Filter answers the ids of list harness h can run, order kept. A nil
// list stays nil (no list in effect); a non-empty list none of whose
// ids h serves answers an empty, non-nil list.
func Filter(h string, list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, id := range list {
		if Serves(h, id) {
			out = append(out, id)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Claude Code session identity (polyforge#159). The daemon mints the
// session uuid of every claude dispatch itself (--session-id), so the
// native transcript's file name is known before the process starts:
// <claude config dir>/projects/<dashed cwd>/<uuid>.jsonl. The locator
// globs by the uuid instead of re-deriving Claude Code's directory
// naming rule, and accepts only the canonical uuid shape — the id may
// be read back from the agent-writable spawn journal.

// NewSessionID mints a random (v4) uuid in canonical lowercase form.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// ValidSessionID reports whether s is a canonical lowercase uuid
// (8-4-4-4-12 hex digits) — the only shape a session id is trusted in.
func ValidSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// ClaudeConfigDir answers Claude Code's config dir as the step process
// sees it: $CLAUDE_CONFIG_DIR when the daemon's environment sets it (the
// child inherits it), else $HOME/.claude. The daemon never sets it.
func ClaudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// LocateClaudeSession answers the native session file of uuid
// (<config dir>/projects/*/<uuid>.jsonl, a non-empty regular file), or
// "" when id is not a canonical uuid or no such file exists.
func LocateClaudeSession(id string) string {
	if !ValidSessionID(id) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(ClaudeConfigDir(), "projects", "*", id+".jsonl"))
	for _, m := range matches {
		if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return m
		}
	}
	return ""
}
