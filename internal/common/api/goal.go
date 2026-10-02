package api

// goal.go: the work-item goal size contract, shared by the Core create
// face, the admin import face and the migration CLI (which must MIRROR
// the import's truncation when it field-compares a re-run).

import "unicode/utf8"

// MaxGoalBytes is the work-item goal size cap in BYTES. The v1.5 "500
// characters" contract is enforced byte-wise (Go len), so CJK-heavy
// goals hit the cap far below 500 runes — the legacy library carries
// many such goals, which is why the import face truncates (seq-fix
// lane, fix 2) instead of refusing.
const MaxGoalBytes = 500

// TruncateUTF8 cuts s to at most maxBytes, ending on a rune boundary
// (never splitting a multi-byte rune). s already within the cap passes
// through untouched.
func TruncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
