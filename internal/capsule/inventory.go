package capsule

// inventory.go: repo inventory checkout against the frozen base commit
// (03 §4.3). The declared base is the ONLY baseline: the capsule checks out
// exactly {name, url, subdir?, base} from the claim response's
// capsule_bootstrap, never a default branch head.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// InventorySnapshot is the repos/inventory.json aggregate view (03 §4.3).
type InventorySnapshot struct {
	Digest string          `json:"digest"`
	Repos  []InventoryRepo `json:"repos"`
}

// InventoryRepo is one checked-out repo record.
type InventoryRepo struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Base           string `json:"base"`
	CheckedOutAt   string `json:"checked_out_at"`
	HeadAtCheckout string `json:"head_at_checkout"`
}

// gitRunner is injectable for tests.
type gitRunner func(args ...string) (string, error)

// CheckoutInventory drives 03 §4.3 for every declared repo:
//
//  1. dir = <capsule>/repos/<name>
//  2. dir exists and HEAD == base → verify working tree is not diverged, reuse.
//  3. dir exists and HEAD != base → CAP-405 CHECKOUT_DIVERGED (never auto-reset).
//  4. dir missing → git clone --no-checkout <url> dir && git -C dir checkout <base>.
//  5. write the repos/inventory.json snapshot.
//
// SUBDIR RULING (recorded, v1): repo_entry carries an optional `subdir`
// (03 §4.1 declaration format, storage authority 02 §4.6 "in-repo
// subdirectory anchor"), but the checkout flow deliberately does NOT
// consume it: 03 §3.1 declares <capsule>/repos/<repo_name>/ as the FULL
// working tree of the inventory checkout, 03 §4.3's five-step flow clones
// and checks out the WHOLE repo at the frozen base commit and its
// inventory.json snapshot schema ({name,url,base,checked_out_at,
// head_at_checkout}) has no subdir field, and 03 §7 pins the RunCommand
// cwd to repos/<name> or artifacts. Full-repo checkout at base is therefore
// the intended v1 shape; `subdir` is reserved declaration metadata the
// Capsule layout never reads. If a future revision activates subdir as the
// working anchor, the change must land here (checkout anchor), in the
// snapshot schema (03 §4.3) and in the RunCommand cwd rules (03 §7)
// together — not by local inference.
//
// The .git directory is part of the capsule (subagent read/write per
// 03 §5.2) and is EXCLUDED from the sync surface (05 §4.1: the only
// exclusion inside repos/<name>/).
func (c *Capsule) CheckoutInventory(bootstrap *Bootstrap) error {
	return c.checkoutInventory(bootstrap, nil)
}

func (c *Capsule) checkoutInventory(bootstrap *Bootstrap, run gitRunner) error {
	inDir := func(dir string, args ...string) (string, error) {
		if run != nil {
			return run(args...)
		}
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.Output()
		return string(out), err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snap := InventorySnapshot{Repos: []InventoryRepo{}}
	for _, e := range bootstrap.RepoInventory {
		dir := filepath.Join(c.Dir, "repos", e.Name)
		var head string
		if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
			// Existing checkout: reuse only when HEAD == base (03 §4.3 step 2/3).
			h, herr := inDir(dir, "rev-parse", "HEAD")
			if herr != nil {
				return errf(ErrCheckoutDiverged, "repo %s: unreadable HEAD: %v", e.Name, herr)
			}
			head = firstLine(h)
			if head != e.Base {
				return errf(ErrCheckoutDiverged, "repo %s: HEAD %s ≠ declared base %s", e.Name, head, e.Base)
			}
			// Reuse: working tree keeps whatever it holds; uncommitted changes
			// are legitimate local work, only divergence from the declared
			// base is refused.
		} else {
			if out, cerr := exec.Command("git", "clone", "--no-checkout", e.URL, dir).CombinedOutput(); cerr != nil {
				return fmt.Errorf("clone %s: %v: %s", e.Name, cerr, out)
			}
			if out, cerr := inDir(dir, "checkout", e.Base); cerr != nil {
				return fmt.Errorf("checkout %s@%s: %v: %s", e.Name, e.Base, cerr, out)
			}
			h, herr := inDir(dir, "rev-parse", "HEAD")
			if herr != nil {
				return fmt.Errorf("rev-parse %s: %v", e.Name, herr)
			}
			head = firstLine(h)
		}
		snap.Repos = append(snap.Repos, InventoryRepo{
			Name: e.Name, URL: e.URL, Base: e.Base,
			CheckedOutAt: now, HeadAtCheckout: head,
		})
	}
	// Aggregate digest over the repos list (canonical JSON, sha256). The
	// exact digest input is a Daemon-side convention: 03 §4.3 shows a
	// `digest` field without pinning its input, so we pin "RFC 8785 canonical
	// JSON of the repos array" here and read it back only for equality
	// checks against a fresh write.
	raw, err := digest.MarshalCanonical(snap.Repos)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	snap.Digest = hex.EncodeToString(sum[:])
	blob, _ := json.MarshalIndent(snap, "", "  ")
	return AtomicWrite(filepath.Join(c.Dir, "repos", "inventory.json"), blob, 0o444)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return trimSpace(s[:i])
		}
	}
	return trimSpace(s)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\r' || s[start] == '\n' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\r' || s[end-1] == '\n' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
