// Package sessionjsonl is THE single copier of the harness session-jsonl
// storage format (the live transcript shape: one JSON record per line,
// appended to concurrently by the harness process): CopyReadonly is the
// one live-transcript copier, consumed by the observe module (the
// commit-boundary sync) and by the runtime's transcript sync face — and
// the package stays a leaf (stdlib plus the atomicfile leaf) so both
// consumers can import it without cycles.
//
// The observe stdout-wire parser (internal/polyforge/observe/stream.go) is
// a DIFFERENT vocabulary (the daemon↔agent RPC record stream) and
// deliberately does not use this package (grill ruling 5): the two formats
// must never merge, and the single-source property is asserted by test.
//
// Deliberate posture: the copy is byte-verbatim — a live transcript may be
// torn mid-write and the copy preserves exactly what the source held at
// read time. Only I/O fails.
package sessionjsonl

import (
	"os"

	"github.com/silgrid/polyforge/internal/common/atomicfile"
)

// CopyReadonly copies the session-jsonl file src onto dst without ever
// opening the source for writing (the source is the harness's own
// session store). The bytes are copied verbatim; the destination is
// written tmp+rename, 0644. It is the one live-transcript copier: the
// observe monitor's commit-boundary sync and the runtime's
// seal/pause/bounded-fail sync both call it.
func CopyReadonly(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return atomicfile.Write(dst, raw, 0o644)
}
