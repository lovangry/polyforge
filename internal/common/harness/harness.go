// Package harness names the step harnesses a workflow step can run on
// (polyforge#159) and the one model-candidate format (polyforge#175).
//
// A model candidate is `<harness>:<model>`: the harness of the closed set
// (pi | claude) and the model id that harness's --model takes, split on
// the FIRST `:` (a model id may contain `/` and further `:`). The prefix
// is mandatory and is the whole truth of where a candidate runs — there
// is no inference from the model id's shape and no per-step harness
// setting. One candidate chain may therefore mix harnesses
// (`[claude:claude-opus-5-5, pi:sub2api-glm/glm-5.3]`).
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The closed harness set. An absent harness ("") is Pi where a record
// predates the harness field (spawn-journal lines before #159).
const (
	Pi     = "pi"
	Claude = "claude"
)

// Valid reports whether name is a harness of the closed set.
func Valid(name string) bool { return name == Pi || name == Claude }

// Of normalizes an absent harness to Pi.
func Of(name string) string {
	if name == "" {
		return Pi
	}
	return name
}

// Candidate is one parsed model candidate: the harness it runs on and
// the model id handed to that harness.
type Candidate struct {
	Harness string
	Model   string
}

// String answers the canonical `<harness>:<model>` form.
func (c Candidate) String() string { return c.Harness + ":" + c.Model }

// PrefixHint is the shape every refusal of an unprefixed candidate names.
const PrefixHint = "pi:… or claude:…"

// ParseCandidate parses one `<harness>:<model>` candidate (surrounding
// whitespace of the whole string and of each half is ignored). A
// candidate without a `:`, with a harness outside the closed set, or
// with an empty model is refused; the error text quotes the candidate
// and is meant to be prefixed with its location by the caller.
func ParseCandidate(s string) (Candidate, error) {
	raw := strings.TrimSpace(s)
	h, m, ok := strings.Cut(raw, ":")
	if !ok {
		return Candidate{}, fmt.Errorf("%q needs a harness prefix (%s)", raw, PrefixHint)
	}
	h, m = strings.TrimSpace(h), strings.TrimSpace(m)
	if !Valid(h) {
		return Candidate{}, fmt.Errorf("%q has an unknown harness prefix %q (want %s)", raw, h, PrefixHint)
	}
	if m == "" {
		return Candidate{}, fmt.Errorf("%q names no model after the harness prefix (want %s)", raw, PrefixHint)
	}
	return Candidate{Harness: h, Model: m}, nil
}

// Normalize answers the canonical form of one candidate, or the
// ParseCandidate refusal.
func Normalize(s string) (string, error) {
	c, err := ParseCandidate(s)
	if err != nil {
		return "", err
	}
	return c.String(), nil
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

// ClaudeProjectDirName is Claude Code's per-cwd project directory name:
// every character outside [A-Za-z0-9] becomes '-'.
func ClaudeProjectDirName(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, cwd)
}

// LocateClaudeSession answers the native session file of uuid for a step
// that ran in capsuleDir: <config dir>/projects/<dir of capsuleDir>/<uuid>.jsonl,
// a non-empty regular file — or "" when id is not a canonical uuid or no
// such file exists. configDir is EXPLICIT (polyforge#171): the caller
// names the claude-config dir it hard-set into the child env (the
// runtime config's ClaudeConfigDir); the locator never falls back to the
// daemon process env (the child env is the only place CLAUDE_CONFIG_DIR
// is ever set in production). Path stays direct concatenation (no mtime
// locator, ruling 7). Only the project directory of the capsule (as
// given, or with symlinks resolved) is consulted: a same-named file of
// another cwd is never copied or resumed.
func LocateClaudeSession(id, configDir, capsuleDir string) string {
	if !ValidSessionID(id) || configDir == "" || capsuleDir == "" {
		return ""
	}
	dirs := []string{ClaudeProjectDirName(capsuleDir)}
	if real, err := filepath.EvalSymlinks(capsuleDir); err == nil && real != capsuleDir {
		dirs = append(dirs, ClaudeProjectDirName(real))
	}
	for _, d := range dirs {
		p := filepath.Join(configDir, "projects", d, id+".jsonl")
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return p
		}
	}
	return ""
}
