package runtime

// materialize.go: the F7 lane — the bundle materialization as ONE atomic
// SET with a completion marker, ported from v0's 03 §4.5 semantics
// (v0/internal/capsule/materialize.go, git history):
//
//   - the full bundle tree lands under workflows/<bundle>/ and the
//     step definitions (the bundle's steps/) under agents/;
//   - the completion marker sync/materialized.json records the pinned
//     bundle identity (name/digest), the adapter set and the full
//     targets list, written ATOMICALLY AFTER the last target rename and
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
// sync/ is daemon-private state (03 §5.2 semantics): it never enters the
// capsule's revision chain — the .gitignore carries /sync/ for exactly
// that reason — and is rebuilt by every new generation.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/silgrid/polyforge/internal/common/api"
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
// field set is v0's: the pinned bundle identity, the adapter set the
// snapshot was materialized for, the full targets list the
// trust gate walks, and the completion timestamp.
type materializeMarkerRecord struct {
	BundleName   string   `json:"bundle_name"`
	BundleDigest string   `json:"bundle_digest"`
	Adapters     []string `json:"adapters"`
	// Targets lists every materialized file's capsule-relative path
	// (slash-separated). The trust gate walks this list: a marker whose
	// recorded target is missing on disk is not a complete snapshot.
	Targets     []string `json:"targets"`
	CompletedAt string   `json:"completed_at"`
}

// sameAdapterSet compares two adapter lists as sets (order is an
// artifact of the caller, not of the snapshot).
func sameAdapterSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, y := range b {
		seen[y]--
		if seen[y] < 0 {
			return false
		}
	}
	return true
}

// materializationComplete is the trust gate (v0 03 §4.5): it reports
// whether the materialized snapshot is COMPLETE for THIS job's pinned
// identity and therefore trustworthy. A parseable marker alone proves
// nothing — the gate additionally requires:
//
//   - the marker's recorded identity (bundle name, digest, adapter set)
//     matches the job's pin — a snapshot materialized for a different
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
	if j.bundleName == "" || j.bundleDigest == "" {
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
	if m.BundleName != j.bundleName || m.BundleDigest != j.bundleDigest {
		return false
	}
	if !sameAdapterSet(m.Adapters, []string{r.cfg.Harness}) {
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
// reuses a trusted complete snapshot). Claim-time entry: a digest
// divergence between the checkout and the pin is REFUSED (CAP-405)
// before any live target is touched.
func (r *Runtime) materializeBundle(j *job) error {
	if r.cfg.WorkflowsDir == "" || j.bundleName == "" {
		return nil
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
	// Digest verification (§4.5), moved BEFORE the first live mutation:
	// the STAGED tree is walked and hashed, and the aggregate must equal
	// the pinned digest — a bundle whose checkout diverged from its pin
	// is refused with the live scene untouched and the old marker still
	// standing. The name-derived stand-in (and an absent digest) was
	// never a content pin: verification is skipped with a notice.
	if j.bundleDigest != "" && j.bundleDigest != nameDerivedDigest(j.bundleName) {
		entries, err := bundleEntries(filepath.Join(stage, filepath.FromSlash(wfRel)))
		if err != nil {
			os.RemoveAll(stage)
			return err
		}
		if got := workflow.Digest(entries); got != j.bundleDigest {
			os.RemoveAll(stage)
			return &api.Error{
				Code: api.ErrCapCheckoutDiver,
				Detail: fmt.Sprintf("bundle %s digest mismatch: pinned %s, materialized tree %s",
					j.bundleName, j.bundleDigest, got),
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "runtime: %s: bundle %s carries no pinned content digest; materialized without verification\n", j.wi, j.bundleName)
	}
	// The workflow tree (already staged above) is always a swap piece;
	// the step definitions live under steps/ and are staged to the
	// capsule's agents/ dir (the Go driver reads them directly — no
	// harness discovery copy). A bundle without steps/ simply has no
	// second piece.
	pieces := []string{wfRel}
	if dirExists(filepath.Join(src, "steps")) {
		pieces = append(pieces, capsule.DirAgents) // capsule dir name unchanged (steps/ content)
		if err := copyTree(filepath.Join(src, "steps"), filepath.Join(stage, filepath.FromSlash(capsule.DirAgents))); err != nil {
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
		BundleName:   j.bundleName,
		BundleDigest: j.bundleDigest,
		Adapters:     []string{r.cfg.Harness},
		Targets:      targets,
		CompletedAt:  nowStamp(),
	}, "", "  ")
	if merr != nil {
		return merr
	}
	if err := atomicWrite(markerPath, blob, 0o444); err != nil {
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

// nameDerivedDigest is the name-derived stand-in aihub stores when a work
// item pins a bundle by name only (core/workitem.go's default:
// sha256Hex("bundle-name:"+name)). A pinned digest equal to it is not a
// content pin, so materialization cannot verify against it.
func nameDerivedDigest(name string) string {
	sum := sha256.Sum256([]byte("bundle-name:" + name))
	return hex.EncodeToString(sum[:])
}

// bundleEntries walks the (staged) bundle tree and derives the digest
// input set: one FileEntry per regular file, path slash-separated
// relative to the bundle root, SHA-256 over the LF-normalized content,
// byte length of the normalized content.
func bundleEntries(root string) ([]workflow.FileEntry, error) {
	var out []workflow.FileEntry
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		norm := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		sum := sha256.Sum256(norm)
		out = append(out, workflow.FileEntry{
			Path:   filepath.ToSlash(rel),
			Sha256: hex.EncodeToString(sum[:]),
			Bytes:  uint64(len(norm)),
		})
		return nil
	})
	return out, err
}
