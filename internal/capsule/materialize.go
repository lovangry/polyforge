package capsule

// materialize.go: WI Agent step-0 materialization of the three harness
// targets (03 §4.5): agents/*.md → .pi/agents/ (pi) and .claude/agents/
// (claude, on demand), workflow.md → capsule root. Copied from the Daemon's
// own workflows checkout, verified against the pinned bundle_digest, and
// read-only afterwards. These targets are OUTSIDE the sync surface
// (SYNC-INV-10): never packed, never restored, rebuilt by every new
// generation.
//
// ATOMICITY (SK-5/CR-2): the three targets are ONE set, not independent
// files. The old code copied workflow.md and the agents dirs one by one,
// so a crash mid-copy left a TORN snapshot (new workflow.md + old agents,
// or the reverse) that every later re-derived agent trusted — 04 §7.1
// step 4 reuses "the materialized snapshot" whenever it exists, with no
// completeness check to notice the tear. The mechanism here follows the
// recover package's swap-marker precedent (internal/sync/recover,
// R3N-F4): stage everything FIRST with the live targets untouched,
// REMOVE the completion marker before the first live mutation (from that
// moment nothing is trusted), rename the staged pieces into place, and
// write a fresh marker atomically LAST (AtomicWrite: tmp+fsync+rename).
// A crash at any point leaves either complete-old (crash before the
// marker removal: live targets are the previous complete snapshot, old
// marker intact) or untrusted-torn (crash after: no marker, so the
// consumer re-materializes per 03 §4.5) — never a half-new snapshot read
// as if it were whole. Both marker and staging live under sync/, the
// Daemon/WI-Agent-private staging area 03 §5.2 excludes from subagent
// reach and 05 §4.1 excludes from the sync surface, so they never enter
// a Revision.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// MaterializeWarning reports a bundle digest drift (03 §4.5: warn and
// continue, never hard-reject — v1 trusts the internal checkout; drift is a
// configuration incident, not an attack surface).
type MaterializeWarning struct {
	Pinned  string
	Actual  string
	Message string
}

const (
	// RelMaterializeMarker is the completion marker: sync/materialized.json.
	// Its presence — written atomically AFTER the last target rename, removed
	// BEFORE the first — is the ONLY "snapshot is complete" signal a consumer
	// may trust (MaterializationComplete). It records what landed so a
	// re-derived agent can tell a trusted snapshot from a torn one.
	RelMaterializeMarker = "sync/materialized.json"
	// materializeStagingDir is the staging tree under sync/ where every
	// target is copied BEFORE any live path is touched.
	materializeStagingDir = "sync/materialize-staging"
)

// materializeMarkerRecord is the content of the completion marker.
type materializeMarkerRecord struct {
	BundleName   string   `json:"bundle_name"`
	BundleDigest string   `json:"bundle_digest"`
	Adapters     []string `json:"adapters"`
	// Targets lists every materialized file's capsule-relative path
	// (slash-separated). The trust gate walks this list: a marker whose
	// recorded target is missing on disk (a file deleted after the
	// materialization) is not a complete snapshot. Without it the gate
	// could only check directory existence, which a single deleted agent
	// definition survives — exactly the "missing agent file" hole fix
	// round 2 closes.
	Targets     []string `json:"targets"`
	CompletedAt string   `json:"completed_at"`
}

// MaterializationIdentity is the EXPECTED snapshot identity the trust
// gate compares the recorded marker against: the WI's immutable bundle
// reference (bundle_name/bundle_digest, 02 §4.4 — the claim path knows it
// from the capsule_bootstrap, the crash-re-derivation path from the
// persisted jobs[] record) plus the harness adapter set the daemon
// materializes for. An empty identity never validates a marker — the
// gate is meaningless without the expected values (fail closed).
type MaterializationIdentity struct {
	BundleName   string
	BundleDigest string
	Adapters     []string
}

// sameAdapterSet compares two adapter lists as sets (order is an artifact
// of the caller, not of the snapshot).
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

// MaterializationComplete is the trust gate (fix round 2): it reports
// whether the materialized snapshot (workflow.md + the agents dirs) is
// COMPLETE for the EXPECTED identity and therefore trustworthy. A parseable
// marker alone proves nothing — the gate additionally requires:
//
//   - the marker's recorded identity (bundle_name, bundle_digest, adapter
//     set) matches the expected one — a snapshot materialized for a
//     different bundle/digest is not this WI's pinned snapshot;
//   - every target the marker records actually exists on disk (a missing
//     workflow.md or agent definition file means the snapshot is torn or
//     vandalized, regardless of what the marker claims);
//   - no staging residue: a leftover staging tree proves an interrupted
//     materialization run, and the consumer re-materializes instead of
//     trusting whatever bytes happen to be live.
//
// Torn or partial state reports false, and the caller re-materializes per
// 03 §4.5. The parameterless convenience that trusted ANY parseable marker
// was removed: callers must pass the identity they expect.
func (c *Capsule) MaterializationComplete(id MaterializationIdentity) bool {
	if id.BundleName == "" || id.BundleDigest == "" || len(id.Adapters) == 0 {
		return false // no expectation, no trust (fail closed)
	}
	raw, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(RelMaterializeMarker)))
	if err != nil {
		return false
	}
	var m materializeMarkerRecord
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	if m.BundleName != id.BundleName || m.BundleDigest != id.BundleDigest {
		return false
	}
	if !sameAdapterSet(m.Adapters, id.Adapters) {
		return false
	}
	if len(m.Targets) == 0 {
		return false // a marker that records no target set is not a completion
	}
	for _, rel := range m.Targets {
		info, err := os.Stat(filepath.Join(c.Dir, filepath.FromSlash(rel)))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	// No staging residue: a leftover staging tree (real crash husk) marks
	// the snapshot as interrupted, whatever the marker says.
	if _, err := os.Stat(filepath.Join(c.Dir, filepath.FromSlash(materializeStagingDir))); err == nil {
		return false
	}
	return true
}

// materializeCrashHook is nil in production; tests inject a callback fired
// at the named phases ("copied:<rel>" after one staged file copy,
// "marker-removed" after the marker removal, "swapped:<rel>" after one
// live rename) to simulate a kill between copies. A returned error aborts
// Materialize at exactly that point, leaving the on-disk state a real
// crash would leave.
var materializeCrashHook func(phase string) error

func materializeCrash(phase string) error {
	if materializeCrashHook == nil {
		return nil
	}
	return materializeCrashHook(phase)
}

// Materialize copies workflows/<bundle>/{workflow.md, agents/*.md} from the
// Daemon checkout into the capsule — atomically AS A SET (see the file
// header) — and verifies the source digest against the pin. adapters lists
// which agent dirs to populate (AdapterPi, AdapterClaude).
func (c *Capsule) Materialize(workflowsDir, bundleName, pinnedDigest string, adapters []string) (*MaterializeWarning, error) {
	bundleDir := filepath.Join(workflowsDir, bundleName)
	if _, err := os.Stat(filepath.Join(bundleDir, "workflow.md")); err != nil {
		return nil, errf("CAP-404", "bundle %s: workflow.md missing in checkout", bundleName)
	}
	var warn *MaterializeWarning
	if actual, derr := digest.ComputeBundleDigest(bundleDir); derr == nil && pinnedDigest != "" && actual != pinnedDigest {
		warn = &MaterializeWarning{
			Pinned:  pinnedDigest,
			Actual:  actual,
			Message: "bundle digest drift between checkout and pin; continuing with the pinned snapshot semantics",
		}
	}
	srcAgents, err := filepath.Glob(filepath.Join(bundleDir, "agents", "*.md"))
	if err != nil {
		return warn, err
	}
	sort.Strings(srcAgents)

	// targets collects every file this run lands, in landing order; the
	// completion marker records the list so the trust gate can verify
	// each one still exists.
	targets := []string{"workflow.md"}
	for _, a := range adapters {
		for _, src := range srcAgents {
			targets = append(targets, adapterAgentsRel(a)+"/"+filepath.Base(src))
		}
	}

	// Phase 1 — stage: every target is copied into the staging tree with
	// its final layout; the live targets are untouched (a crash here
	// leaves complete-old + the old marker, still trusted). Leftovers of
	// a previously torn run are removed first: staging is never trusted.
	stage := filepath.Join(c.Dir, filepath.FromSlash(materializeStagingDir))
	if err := os.RemoveAll(stage); err != nil {
		return warn, err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return warn, err
	}
	if err := copyFile(filepath.Join(bundleDir, "workflow.md"), filepath.Join(stage, "workflow.md"), 0o444); err != nil {
		return warn, err
	}
	if err := materializeCrash("copied:workflow.md"); err != nil {
		return warn, err
	}
	for _, a := range adapters {
		dstRel := filepath.FromSlash(adapterAgentsRel(a))
		dst := filepath.Join(stage, dstRel)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return warn, err
		}
		for _, src := range srcAgents {
			if err := copyFile(src, filepath.Join(dst, filepath.Base(src)), 0o444); err != nil {
				return warn, err
			}
			if err := materializeCrash("copied:" + adapterAgentsRel(a) + "/" + filepath.Base(src)); err != nil {
				return warn, err
			}
		}
	}

	// Phase 2 — untrust: the completion marker is removed BEFORE the
	// first live mutation. From this moment a crash anywhere leaves the
	// snapshot untrusted (MaterializationComplete false), never a torn
	// mix read as if it were whole.
	markerPath := filepath.Join(c.Dir, filepath.FromSlash(RelMaterializeMarker))
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return warn, err
	}
	if err := materializeCrash("marker-removed"); err != nil {
		return warn, err
	}

	// Phase 3 — swap: each staged piece is renamed over its live target.
	// A file rename replaces atomically; a directory target is removed
	// first (os.Rename refuses to replace a non-empty dir). The window is
	// untrusted by construction — no marker exists.
	swap := func(rel string) error {
		live := filepath.Join(c.Dir, rel)
		staged := filepath.Join(stage, rel)
		if info, serr := os.Stat(staged); serr == nil && info.IsDir() {
			if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(live); err != nil {
				return err
			}
		}
		return os.Rename(staged, live)
	}
	if err := swap("workflow.md"); err != nil {
		return warn, err
	}
	if err := materializeCrash("swapped:workflow.md"); err != nil {
		return warn, err
	}
	for _, a := range adapters {
		rel := filepath.FromSlash(adapterAgentsRel(a))
		if err := swap(rel); err != nil {
			return warn, err
		}
		if err := materializeCrash("swapped:" + adapterAgentsRel(a)); err != nil {
			return warn, err
		}
	}

	// Phase 4 — trust: the completion marker lands LAST, atomically
	// (tmp+fsync+rename, the same contract as every capsule state file).
	// After this point the snapshot is complete and trusted.
	blob, merr := json.Marshal(materializeMarkerRecord{
		BundleName:   bundleName,
		BundleDigest: pinnedDigest,
		Adapters:     adapters,
		Targets:      targets,
		CompletedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	})
	if merr != nil {
		return warn, merr
	}
	if err := AtomicWrite(markerPath, blob, 0o444); err != nil {
		return warn, err
	}
	// The staging tree is empty now (every piece renamed out); remove the
	// husk so the next run starts clean.
	if err := os.RemoveAll(stage); err != nil {
		return warn, err
	}
	return warn, nil
}

func adapterAgentsRel(adapter string) string {
	switch adapter {
	case AdapterClaude:
		return filepath.Join(".claude", "agents")
	default:
		return filepath.Join(".pi", "agents")
	}
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".mtmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
