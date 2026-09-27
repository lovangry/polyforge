package capsule

// materialize.go: WI Agent step-0 materialization of the three harness
// targets (03 §4.5): agents/*.md → .pi/agents/ (pi) and .claude/agents/
// (claude, on demand), workflow.md → capsule root. Copied from the Daemon's
// own workflows checkout, verified against the pinned bundle_digest, and
// read-only afterwards. These targets are OUTSIDE the sync surface
// (SYNC-INV-10): never packed, never restored, rebuilt by every new
// generation.

import (
	"io"
	"os"
	"path/filepath"
	"sort"

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

// Materialize copies workflows/<bundle>/{workflow.md, agents/*.md} from the
// Daemon checkout into the capsule and verifies the source digest against
// the pin. adapters lists which agent dirs to populate (AdapterPi,
// AdapterClaude).
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
	// workflow.md → capsule root.
	if err := copyFile(filepath.Join(bundleDir, "workflow.md"), filepath.Join(c.Dir, "workflow.md"), 0o444); err != nil {
		return warn, err
	}
	// agents/*.md → .pi/agents/ (always; capsule creation guarantees the
	// empty dir exists) and .claude/agents/ (only for the claude adapter).
	srcAgents, err := filepath.Glob(filepath.Join(bundleDir, "agents", "*.md"))
	if err != nil {
		return warn, err
	}
	sort.Strings(srcAgents)
	for _, a := range adapters {
		dst := filepath.Join(c.Dir, adapterAgentsRel(a))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return warn, err
		}
		for _, src := range srcAgents {
			if err := copyFile(src, filepath.Join(dst, filepath.Base(src)), 0o444); err != nil {
				return warn, err
			}
		}
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
