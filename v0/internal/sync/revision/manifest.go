package revision

// manifest.go: manifest construction from a scan against the parent's live
// set (05 §4.2 / §6 step 3). SYNC-INV-6: data/artifact ops are fixed FIRST,
// the runtime-state/execution.json put is determined LAST — both in the op
// order and in the fixed read order the scan handed the publisher.
//
// SYNC-INV-3: every manifest carries a put for the COMPLETE current
// execution.json (asserted; a manifest without it is refused by Core with
// SYNC-400 EXECUTION_STATE_MISSING).

import (
	"fmt"
	"sort"
	"time"
)

// BuildOptions configures one manifest build.
type BuildOptions struct {
	WorkItemID string
	Seq        int64
	Parent     *int64 // nil ONLY for the seq-0 bootstrap (05 §2.1 sentinel)
	Generation int64
	Kind       string // incremental | checkpoint
	BuiltAt    time.Time
}

// ExecutionJSONPath is the capsule-relative path of the execution authority.
const ExecutionJSONPath = "runtime-state/execution.json"

// BuildManifest computes the ops of a new revision:
//
//   - kind=incremental: diff of current vs parent live set — put (new or
//     sha-changed), delete (gone), symlink (new or target-changed), chmod
//     (same sha, mode differs).
//   - kind=checkpoint: full put inventory of the live set.
//
// Ordering (SYNC-INV-6): every data op first, path-sorted; the
// execution.json put is ALWAYS last. The execution.json op is included in
// every manifest regardless of change (SYNC-INV-3); on a checkpoint it is a
// plain put like the rest but still positioned last.
func BuildManifest(parent *ScanResult, current *ScanResult, opts BuildOptions) (*Manifest, error) {
	if opts.Kind != KindIncremental && opts.Kind != KindCheckpoint {
		return nil, fmt.Errorf("revision: unknown kind %q", opts.Kind)
	}
	if current.Files[ExecutionJSONPath] == (FileInfo{}) {
		return nil, fmt.Errorf("revision: SYNC-INV-3 violated: %s not present in scan", ExecutionJSONPath)
	}
	ops := []Op{}
	if opts.Kind == KindCheckpoint {
		for path, fi := range current.Files {
			if path == ExecutionJSONPath {
				continue
			}
			ops = append(ops, fileOp(path, fi))
		}
	} else {
		parentFiles := map[string]FileInfo{}
		if parent != nil {
			parentFiles = parent.Files
		}
		for path, fi := range current.Files {
			if path == ExecutionJSONPath {
				continue
			}
			pf, existed := parentFiles[path]
			switch {
			case !existed:
				ops = append(ops, fileOp(path, fi))
			case fi.IsSymlink && pf.IsSymlink && fi.Target == pf.Target:
				// unchanged symlink
			case fi.IsSymlink != pf.IsSymlink:
				ops = append(ops, fileOp(path, fi))
			case fi.Sha256 == pf.Sha256 && fi.Mode != pf.Mode && !fi.IsSymlink:
				// Same content, different permission bits → chmod only.
				mode := fi.Mode
				ops = append(ops, Op{Op: OpChmod, Path: path, Mode: &mode})
			case fi.Sha256 != pf.Sha256:
				ops = append(ops, fileOp(path, fi))
			}
		}
		for path := range parentFiles {
			if path == ExecutionJSONPath {
				continue
			}
			if _, still := current.Files[path]; !still {
				ops = append(ops, Op{Op: OpDelete, Path: path})
			}
		}
	}
	// Deterministic order: data ops by path; execution.json appended LAST.
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].Path < ops[j].Path })
	exec := current.Files[ExecutionJSONPath]
	mode := exec.Mode
	ops = append(ops, Op{Op: OpPut, Path: ExecutionJSONPath, Sha256: exec.Sha256, Bytes: exec.Bytes, Mode: &mode})

	m := &Manifest{
		WorkItemID:             opts.WorkItemID,
		Seq:                    opts.Seq,
		Parent:                 opts.Parent,
		Generation:             opts.Generation,
		Kind:                   opts.Kind,
		Ops:                    ops,
		LiveFiles:              int64(len(current.Files)),
		DataBytes:              totalBytes(current),
		ExecutionStateIncluded: true,
		BuiltAt:                opts.BuiltAt.UTC().Format(time.RFC3339Nano),
	}
	return m, nil
}

func fileOp(path string, fi FileInfo) Op {
	if fi.IsSymlink {
		return Op{Op: OpSymlink, Path: path, Target: fi.Target}
	}
	mode := fi.Mode
	return Op{Op: OpPut, Path: path, Sha256: fi.Sha256, Bytes: fi.Bytes, Mode: &mode}
}

func totalBytes(s *ScanResult) int64 {
	var n int64
	for _, fi := range s.Files {
		n += fi.Bytes
	}
	return n
}

// ValidateOps applies the same wire-level checks the Core accept chain runs
// (unknown op, path escape, symlink target escape), so a Daemon-side
// construction bug surfaces locally before any network round trip.
func ValidateOps(ops []Op) error {
	for _, op := range ops {
		switch op.Op {
		case OpPut, OpDelete, OpSymlink, OpChmod:
		default:
			return fmt.Errorf("revision: unknown op %q", op.Op)
		}
		if !ValidateRelPath(op.Path) {
			return fmt.Errorf("revision: path escape in %q", op.Path)
		}
		if op.Op == OpPut {
			if op.Mode == nil {
				return fmt.Errorf("revision: MODE_MISSING for put %s", op.Path)
			}
		}
		if op.Op == OpSymlink && !ValidateRelPath(op.Target) {
			return fmt.Errorf("revision: symlink target escape in %q", op.Target)
		}
	}
	return nil
}
