package runtime

// materialize.go: the F7 lane — the bundle materialization as ONE atomic
// SET with a completion marker, ported from the v0 materialization semantics (reference.md §7.1)
// (v0/internal/capsule/materialize.go, git history):
//
//   - the full bundle tree lands under workflows/<bundle>/ (the driver
//     reads step definitions from workflows/<bundle>/steps/ directly);
//     the shared step definitions (workflows/_shared/, when the checkout
//     has them) land beside it under workflows/_shared/;
//   - the completion marker sync/materialized.json records the pinned
//     bundle identity (name) and the full targets list, written ATOMICALLY AFTER the last target rename and
//     removed BEFORE the first live mutation — the ONLY "snapshot is
//     complete" signal any consumer may trust.
//
// Atomicity follows the v0 staging precedent: stage every target FIRST
// with the live ones untouched, drop the marker (untrust), rename the
// staged pieces over the live targets (a rename replaces a file
// atomically; a reader holding the old inode keeps reading it — a
// recovery-time re-materialization never breaks the scene a live agent
// already read), and write the fresh marker LAST. A crash at any point
// leaves either complete-old (marker still standing) or untrusted-torn
// (no marker, so the next pass re-materializes) — never a half-new
// snapshot read as if it were whole.
//
// sync/ is daemon-private state (reference.md §7.1 semantics): it never enters the
// capsule's revision chain — the .gitignore carries /sync/ for exactly
// that reason — and is rebuilt by every new generation.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

const (
	// relMaterializeMarker is the completion marker: sync/materialized.json.
	RelMaterializeMarker = "sync/materialized.json"
	// relMaterializeStaging is the staging tree under sync/ where every
	// target is copied before any live path is touched.
	relMaterializeStaging = "sync/materialize-staging"
	// gitignoreSyncLine is appended to the capsule .gitignore: sync/ is
	// daemon-private staging/marker state and never enters the chain.
	gitignoreSyncLine = "/sync/\n"
)

// materializeMarkerRecord is the content of the completion marker. The
// field set: the pinned bundle identity, the full targets list the
// trust gate walks, and the completion timestamp. Markers written by
// older daemons also carry an "adapters" key; it is ignored on parse.
type materializeMarkerRecord struct {
	BundleName string `json:"bundle_name"`
	// Targets lists every materialized file's capsule-relative path
	// (slash-separated). The trust gate walks this list: a marker whose
	// recorded target is missing on disk is not a complete snapshot.
	Targets     []string `json:"targets"`
	CompletedAt string   `json:"completed_at"`
}

// materializationComplete is the trust gate (reference.md §7.1): it reports
// whether the materialized snapshot is COMPLETE for THIS job's pinned
// identity and therefore trustworthy. A parseable marker alone proves
// nothing — the gate additionally requires:
//
//   - the marker's recorded identity (bundle name) matches the job's pin — a snapshot materialized for a different
//     bundle is not this WI's snapshot;
//   - every recorded target actually exists on disk (a deleted file
//     means the snapshot is torn or vandalized, whatever the marker
//     claims);
//   - no staging residue (a leftover staging tree proves an interrupted
//     run).
//
// Torn or partial state answers false and the caller re-materializes.
// The pilot's manual compensation (a marker written by hand next to
// hand-copied targets) either carries the same identity — then it
// validates and is taken over untouched — or it does not, and the
// idempotent re-materialization rebuilds the identical set and rewrites
// the marker: smooth either way.
func (r *Runtime) materializationComplete(j *job) bool {
	if j.bundleName == "" {
		return false // no expectation, no trust (fail closed)
	}
	raw, err := os.ReadFile(filepath.Join(j.capsuleDir, filepath.FromSlash(RelMaterializeMarker)))
	if err != nil {
		return false
	}
	var m materializeMarkerRecord
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	if m.BundleName != j.bundleName {
		return false
	}
	if len(m.Targets) == 0 {
		return false // a marker that records no target set is not a completion
	}
	for _, rel := range m.Targets {
		info, serr := os.Stat(filepath.Join(j.capsuleDir, filepath.FromSlash(rel)))
		if serr != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	// No staging residue: a leftover staging tree (a real crash husk)
	// marks the snapshot as interrupted, whatever the marker says.
	if _, serr := os.Stat(filepath.Join(j.capsuleDir, filepath.FromSlash(relMaterializeStaging))); serr == nil {
		return false
	}
	return true
}

// materializeBundle lays out the bundle targets as one atomic set (or
// reuses a trusted complete snapshot). Claim-time entry.
func (r *Runtime) materializeBundle(j *job) error {
	if r.cfg.WorkflowsDir == "" || j.bundleName == "" {
		return nil
	}
	if j.bundleName == workflow.SharedDir {
		return fmt.Errorf("runtime: %s: %q holds the shared step definitions and is not a workflow bundle", j.wi, workflow.SharedDir)
	}
	src := filepath.Join(r.cfg.WorkflowsDir, j.bundleName)
	if !dirExists(src) {
		fmt.Fprintf(os.Stderr, "runtime: %s: bundle %s not in checkout; capsule materialized without it\n", j.wi, j.bundleName)
		return nil
	}
	// The trust gate first: a complete snapshot for this identity is
	// reused as-is (the "marker present and targets whole → do not
	// touch" rule — this is also what makes a same-generation husk
	// reuse and the pilot's compensated scene churn-free).
	if r.materializationComplete(j) {
		return nil
	}
	return r.materializeBundleSet(j, src)
}

// materializeBundleSet is the staged swap itself (see the file header).
// It never touches a live target before the marker is removed, and it
// writes the fresh marker atomically LAST.
func (r *Runtime) materializeBundleSet(j *job, src string) error {
	stage := filepath.Join(j.capsuleDir, filepath.FromSlash(relMaterializeStaging))
	// Phase 1 — stage: every target is copied into the staging tree with
	// its final layout; the live targets are untouched (a crash here
	// leaves complete-old + the old marker, still trusted). Leftovers
	// of a previously torn run are removed first: staging is never
	// trusted.
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	wfRel := capsule.DirWorkflows + "/" + j.bundleName
	if err := copyTree(src, filepath.Join(stage, filepath.FromSlash(wfRel))); err != nil {
		os.RemoveAll(stage)
		return err
	}
	// The workflow tree is the one swap piece: the driver reads the step
	// definitions from workflows/<bundle>/steps/ (an agents/ dir left by
	// an older daemon in a recovered capsule is inert and untouched).
	pieces := []string{wfRel}
	// The shared step definitions (workflows/_shared/) ride along beside
	// the bundle: the driver falls back to them for any step the bundle
	// carries no own steps/<id>.md for.
	if shared := filepath.Join(filepath.Dir(src), workflow.SharedDir); dirExists(shared) {
		sharedRel := capsule.DirWorkflows + "/" + workflow.SharedDir
		pieces = append(pieces, sharedRel)
		if err := copyTree(shared, filepath.Join(stage, filepath.FromSlash(sharedRel))); err != nil {
			os.RemoveAll(stage)
			return err
		}
	}
	// targets records every staged file, in sorted order for a stable
	// marker; the gate walks this list on every later consumer pass.
	targets, err := stagedTargets(stage)
	if err != nil {
		os.RemoveAll(stage)
		return err
	}
	// Phase 2 — untrust: the completion marker is removed BEFORE the
	// first live mutation. From this moment a crash anywhere leaves the
	// snapshot untrusted, never a torn mix read as if it were whole.
	markerPath := filepath.Join(j.capsuleDir, filepath.FromSlash(RelMaterializeMarker))
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		os.RemoveAll(stage)
		return err
	}
	// Phase 3 — swap: each staged piece is renamed over its live target.
	// A directory target is removed first (os.Rename refuses to replace
	// a non-empty dir); a file rename replaces atomically, so a reader
	// holding the old inode keeps reading the old bytes — the
	// recovery-time re-materialization does not break the scene the
	// live agents already read.
	for _, rel := range pieces {
		live := filepath.Join(j.capsuleDir, filepath.FromSlash(rel))
		staged := filepath.Join(stage, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
			return err
		}
		if err := os.RemoveAll(live); err != nil {
			return err
		}
		if err := os.Rename(staged, live); err != nil {
			return err
		}
	}
	// Phase 4 — trust: the fresh marker lands LAST, atomically
	// (tmp+fsync+rename, the same contract as every capsule state file).
	blob, merr := json.MarshalIndent(materializeMarkerRecord{
		BundleName:  j.bundleName,
		Targets:     targets,
		CompletedAt: nowStamp(),
	}, "", "  ")
	if merr != nil {
		return merr
	}
	if err := atomicfile.Write(markerPath, blob, 0o444); err != nil {
		return err
	}
	// The staging tree is empty now (every piece was renamed out);
	// remove the husk so the next run starts clean.
	return os.RemoveAll(stage)
}

// stagedTargets walks the staging tree and answers every staged file's
// capsule-relative path, slash-separated and sorted.
func stagedTargets(stage string) ([]string, error) {
	var out []string
	err := filepath.Walk(stage, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(stage, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}
