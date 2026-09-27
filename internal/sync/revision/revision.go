// Package revision implements the Daemon-side Revision data model: the
// manifest file format, CAS block encoding, the scan of the sync surface,
// and the aggregate digests. Design authority:
// docs/design/polyforge-vnext/05-sync-revision-recovery.md §4 (manifest
// format, sync roots, CAS), §6.1 (flush-evidence digest inputs), §2.8
// (idempotent manifest identity), SYNC-INV-2/3/6/8.
//
// Wire compatibility: the JSON field set and the manifest_sha
// canonicalization are byte-identical to the Core reader
// (internal/core/capsule) — CAS blocks are zstd-compressed file CONTENT
// while the sha256 in paths and manifests addresses the ORIGINAL
// uncompressed bytes (digest over the original, block body compressed).
// Cross-compatibility is asserted by tests against the real Core package.
package revision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Op is one manifest operation (05 §4.2). Exactly the Core-side wire shape.
type Op struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Mode   *int64 `json:"mode,omitempty"`
	Target string `json:"target,omitempty"`
}

// Op kinds (05 §4.2).
const (
	OpPut     = "put"
	OpDelete  = "delete"
	OpSymlink = "symlink"
	OpChmod   = "chmod"
)

// Manifest is the rev/<seq>/manifest.json object (05 §4.2). Exactly the
// Core-side wire shape, including the `parent` null sentinel for seq 0.
type Manifest struct {
	WorkItemID             string `json:"work_item_id"`
	Seq                    int64  `json:"seq"`
	Parent                 *int64 `json:"parent"`
	Generation             int64  `json:"generation"`
	Kind                   string `json:"kind"`
	Ops                    []Op   `json:"ops"`
	LiveFiles              int64  `json:"live_files"`
	DataBytes              int64  `json:"data_bytes"`
	ExecutionStateIncluded bool   `json:"execution_state_included"`
	BuiltAt                string `json:"built_at"`
}

// Manifest kinds.
const (
	KindIncremental = "incremental"
	KindCheckpoint  = "checkpoint"
)

// Checkpoint cadence (05 §4.2): a checkpoint every 64 revisions or once the
// chain's cumulative data bytes exceed 512 MiB.
const (
	CheckpointInterval = int64(64)
	CheckpointByteCap  = int64(512 << 20)
)

// ShouldCheckpoint decides the manifest kind for the revision at seq.
func ShouldCheckpoint(seq, cumulativeDataBytes int64) bool {
	if seq > 0 && seq%CheckpointInterval == 0 {
		return true
	}
	return cumulativeDataBytes > CheckpointByteCap
}

// HeadInfo mirrors GET /work-items/{id}/head (05 §5.1).
type HeadInfo struct {
	Seq         int64  `json:"seq"`
	Generation  int64  `json:"generation"`
	ManifestSha string `json:"manifest_sha"`
	UpdatedAt   string `json:"updated_at"`
}

// RevisionMeta is one row of the since-listing (05 §5.1).
type RevisionMeta struct {
	Seq         int64  `json:"seq"`
	Parent      *int64 `json:"parent"`
	Generation  int64  `json:"generation"`
	ManifestSha string `json:"manifest_sha"`
	DataBytes   int64  `json:"data_bytes"`
	PublishedAt string `json:"published_at"`
}

// ---------------------------------------------------------------------------
// Block encoding (05 §4.2: blocks are zstd-compressed content; the digest
// addresses the ORIGINAL bytes)

// DigestHex returns sha256(content) as lowercase hex — the CAS address of
// ORIGINAL bytes (never of the compressed wire form).
func DigestHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// manifest_sha: the idempotent identity (05 §4.2 fix B10 / SYNC-INV-8)
//
// The digest input is the manifest's IDEMPOTENT VIEW: the serialized object
// minus every time-varying field (today: built_at). Two scans of identical
// content at different times must produce the SAME manifest_sha. The
// canonical form is 2-space-indent JSON with keys in code-point ascending
// order, no trailing newline — byte-identical to the Core recompute
// (internal/core/capsule.ComputeManifestSha): marshal, decode into a
// generic map with json.Number preservation, strip built_at, MarshalIndent.

// ComputeManifestSha returns "sha256:" + hex(sha256(idempotent view)).
func ComputeManifestSha(m Manifest) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	var tree map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return "", err
	}
	delete(tree, "built_at")
	b, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// StripTimeVaryingFields is the documented extension point for SYNC-INV-8:
// any FUTURE time-varying/environmental manifest field must be added here
// AND to the Core-side recompute; it exists so the contract has one name.
var StripTimeVaryingFields = []string{"built_at"}

// ValidateRelPath mirrors the Core path-safety rule for ops: no absolute,
// no "..", no backslash, no NUL (05 §5.1 validation; SYNC-INV-5 applies the
// same rules on recovery).
func ValidateRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}
