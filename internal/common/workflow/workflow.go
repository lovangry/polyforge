// Package workflow defines the workflow-bundle digest contract shared by pf
// (materialization into capsules: digest verification of the copied bundle)
// and aihub (serves bundle bytes, validates digests at claim time).
//
// Design authority: docs/design/polyforge-vnext/01-workflow-skills.md,
// frozen digest rules from the v0 implementation (/v0/internal/workflow).
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// FileEntry is one file in the bundle digest input set.
type FileEntry struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"` // LF-normalized content SHA-256, lowercase hex
	Bytes  uint64 `json:"bytes"`
}

// Digest computes the canonical bundle digest: files sorted by path
// (codepoint order), each contributing len(path)+path+len(sha)+sha+8-byte
// LE byte length, then the whole buffer is SHA-256'd (frozen v0 algorithm —
// golden vectors live in /v0/internal/workflow/digest tests; keep byte-for-
// byte compatible so migrated chains stay verifiable).
func Digest(files []FileEntry) string {
	sorted := make([]FileEntry, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var buf strings.Builder
	for _, f := range sorted {
		norm := strings.ReplaceAll(f.Path, "\r\n", "\n")
		buf.WriteString(norm)
		buf.WriteString(f.Sha256)
		var lenBuf [8]byte
		n := uint64(f.Bytes)
		for i := 0; i < 8; i++ {
			lenBuf[i] = byte(n >> (8 * i))
		}
		buf.Write(lenBuf[:])
	}
	sum := sha256.Sum256([]byte(buf.String()))
	return hex.EncodeToString(sum[:])
}
