// Package capsule implements the Daemon-side Execution Capsule: directory
// layout, identity (capsule.json), create/reuse/quarantine lifecycle, repo
// inventory checkout, runtime-state/execution.json atomic commit, harness
// session directories, subagent-definition materialization, and the path /
// symlink safety rules used by Revision recovery writes.
//
// Design authority: docs/design/polyforge-vnext/03-execution-capsule.md
// (layout §3.1, capsule.json §2.2, lifecycle §6.1, inventory §4.1/§4.3,
// execution.json §4.2, harness session dirs §4.4, materialization §4.5,
// subagent write restriction mechanisms §6.2, symlink safety §6.3, crash
// resume rules §7) cross-referenced with 05-sync-revision-recovery.md §4.1
// (sync roots and exclusions).
//
// The Core counterpart lives in internal/core/capsule (Revision storage,
// CAS accept chain). This package never imports it: the Daemon talks to
// AIHub over HTTP only.
package capsule

import "fmt"

// Error is a typed Capsule-side error (03 §5.3 CAP-* codes plus the local
// lifecycle conditions). Code is machine-readable; Detail is human context.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Detail
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// CAP error codes (03 §5.3).
const (
	ErrCapsuleFenced    = "CAP-409" // generation mismatch vs local capsule.json
	ErrCheckoutDiverged = "CAP-405" // checked-out HEAD != declared base
	ErrSymlinkEscape    = "CAP-406" // recovery write hit a symlinked parent / escaping target
	ErrRunnerCrashed    = "CAP-500"
	ErrInventoryMissing = "CAP-404"
)
