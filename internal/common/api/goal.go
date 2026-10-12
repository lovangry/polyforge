package api

// goal.go: the work-item goal size contract of the Core create face.

// MaxGoalBytes is the work-item goal size cap in BYTES. The v1.5 "500
// characters" contract is enforced byte-wise (Go len), so CJK-heavy
// goals hit the cap far below 500 runes.
const MaxGoalBytes = 500
