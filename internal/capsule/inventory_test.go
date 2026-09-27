package capsule

// Inventory checkout tests (03 §4.3) against real local git repositories:
// fresh checkout at base, reuse when HEAD==base, CAP-405 when diverged.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// makeRepo creates a local git repo with two commits; returns url and the
// first commit sha (the "base").
func makeRepo(t *testing.T, dir, name string) (url, base string) {
	t.Helper()
	r := filepath.Join(dir, name)
	if err := os.MkdirAll(r, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", r}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(r, "a.txt"), []byte("one\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "one")
	base = firstLine(run("rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(r, "a.txt"), []byte("two\n"), 0o644)
	run("commit", "-q", "-am", "two")
	return r, base
}

func TestCheckoutInventoryFreshAndReuse(t *testing.T) {
	src := t.TempDir()
	url, base := makeRepo(t, src, "ieops")
	c := newCapsule(t, 3)
	boot := &Bootstrap{
		WorkItemID:    c.WorkItemID,
		RepoInventory: []RepoEntry{{Position: 1, Name: "ieops", URL: url, Base: base}},
	}
	if err := c.CheckoutInventory(boot); err != nil {
		t.Fatal(err)
	}
	// Working tree at base: a.txt says "one".
	raw, err := os.ReadFile(filepath.Join(c.Dir, "repos", "ieops", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "one\n" {
		t.Fatalf("checkout not at base: %q", raw)
	}
	// inventory.json snapshot written.
	snapRaw, err := os.ReadFile(filepath.Join(c.Dir, "repos", "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapRaw) == 0 {
		t.Fatal("inventory.json empty")
	}
	// Idempotent re-checkout with same base: reuse, no error.
	if err := c.CheckoutInventory(boot); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutInventoryDiverged(t *testing.T) {
	src := t.TempDir()
	url, base := makeRepo(t, src, "ieops")
	c := newCapsule(t, 3)
	// Pre-existing working tree at a DIFFERENT head.
	if err := exec.Command("git", "clone", "-q", url, filepath.Join(c.Dir, "repos", "ieops")).Run(); err != nil {
		t.Fatal(err)
	}
	boot := &Bootstrap{
		WorkItemID:    c.WorkItemID,
		RepoInventory: []RepoEntry{{Position: 1, Name: "ieops", URL: url, Base: base}},
	}
	err := c.CheckoutInventory(boot)
	if err == nil {
		t.Fatal("diverged checkout must be refused")
	}
	if err.(*Error).Code != ErrCheckoutDiverged {
		t.Fatalf("expected CAP-405, got %v", err)
	}
}

func TestBootstrapParse(t *testing.T) {
	if _, err := ParseBootstrap([]byte(`{"work_item_id":"wi_x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBootstrap([]byte(`{}`)); err == nil {
		t.Fatal("missing work_item_id must be refused")
	}
	// Nine entries: over the 03 §4.1 bound of 8.
	tooMany := `{"work_item_id":"wi_x","repo_inventory":[` +
		`{"name":"r1","url":"u","base":"b"},{"name":"r2","url":"u","base":"b"},{"name":"r3","url":"u","base":"b"},` +
		`{"name":"r4","url":"u","base":"b"},{"name":"r5","url":"u","base":"b"},{"name":"r6","url":"u","base":"b"},` +
		`{"name":"r7","url":"u","base":"b"},{"name":"r8","url":"u","base":"b"},{"name":"r9","url":"u","base":"b"}]}`
	if _, err := ParseBootstrap([]byte(tooMany)); err == nil {
		t.Fatal("9 inventory entries must be refused")
	}
}

// TestBootstrapRejectsUnsafeRepoNames is the FE-3 regression: an inventory
// name becomes the path segment <capsule>/repos/<name> via filepath.Join —
// a name carrying separators or ".." escapes the capsule, and a duplicate
// aliases one checkout under two declarations. Both the parse and the
// checkout refuse them (03 §4.1/§4.3; uniqueness mirrors 02 §4.6's
// UNIQUE(work_item_id, name)).
func TestBootstrapRejectsUnsafeRepoNames(t *testing.T) {
	for _, name := range []string{"../evil", "../../evil", "a/b", `a\b`, "..", ".", "a\x00b"} {
		// json.Marshal gives the NUL a valid \u0000 escape; %q would not.
		entry, merr := json.Marshal(map[string]string{"name": name, "url": "u", "base": "b"})
		if merr != nil {
			t.Fatal(merr)
		}
		raw := `{"work_item_id":"wi_x","repo_inventory":[` + string(entry) + `]}`
		if _, err := ParseBootstrap([]byte(raw)); err == nil {
			t.Errorf("unsafe inventory name %q must be refused at parse time", name)
		}
	}
	// A duplicate name aliases one checkout: refused.
	dup := `{"work_item_id":"wi_x","repo_inventory":[` +
		`{"name":"r1","url":"u","base":"b"},{"name":"r1","url":"u2","base":"b2"}]}`
	if _, err := ParseBootstrap([]byte(dup)); err == nil {
		t.Error("a duplicate inventory name must be refused at parse time")
	}
	// Defense in depth: a DIRECTLY assembled Bootstrap (not via ParseBootstrap)
	// is re-validated at checkout, where the join would actually escape.
	c := newCapsule(t, 3)
	for _, boot := range []*Bootstrap{
		{WorkItemID: c.WorkItemID, RepoInventory: []RepoEntry{{Name: "../evil", URL: "u", Base: "b"}}},
		{WorkItemID: c.WorkItemID, RepoInventory: []RepoEntry{{Name: "../../evil", URL: "u", Base: "b"}}},
		{WorkItemID: c.WorkItemID, RepoInventory: []RepoEntry{
			{Name: "r1", URL: "u", Base: "b"}, {Name: "r1", URL: "u2", Base: "b2"}}},
	} {
		if err := c.CheckoutInventory(boot); err == nil {
			t.Errorf("checkout must refuse the unsafe inventory: %+v", boot.RepoInventory)
		}
	}
	// The escapes must not have materialized anything outside the capsule
	// ("../evil" would land at <capsule>/evil, "../../evil" outside the
	// capsules root) nor inside it.
	for _, p := range []string{
		filepath.Join(c.Dir, "evil"),
		filepath.Join(filepath.Dir(c.Dir), "evil"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("an unsafe name must never materialize a checkout at %s", p)
		}
	}
}

func TestMaterialize(t *testing.T) {
	ws := t.TempDir()
	wfd := filepath.Join(ws, "workflows")
	if err := os.MkdirAll(filepath.Join(wfd, "demo", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wfd, "demo", "workflow.md"), []byte("---\nname: demo\n---\nbody\n"), 0o644)
	os.WriteFile(filepath.Join(wfd, "demo", "agents", "code.md"), []byte("---\nname: code\n---\n"), 0o644)
	// Compute the real digest via the shared helper so the pin matches.
	sum := sha256OfFile(t, filepath.Join(wfd, "demo"))

	c := newCapsule(t, 3)
	if _, err := c.Materialize(wfd, "demo", sum, []string{AdapterPi}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "workflow.md")); err != nil {
		t.Fatal("workflow.md not materialized")
	}
	if _, err := os.Stat(filepath.Join(c.Dir, ".pi", "agents", "code.md")); err != nil {
		t.Fatal(".pi/agents/code.md not materialized")
	}
	// Wrong pin → warning, not an error (03 §4.5: warn and continue).
	warn, err := c.Materialize(wfd, "demo", "sha256:dead", []string{AdapterPi})
	if err != nil {
		t.Fatal(err)
	}
	if warn == nil {
		t.Fatal("digest drift must produce a warning")
	}
	// Claude adapter populates .claude/agents/ on demand.
	if _, err := c.Materialize(wfd, "demo", sum, []string{AdapterClaude}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, ".claude", "agents", "code.md")); err != nil {
		t.Fatal(".claude/agents/code.md not materialized")
	}
}

func sha256OfFile(t *testing.T, bundleDir string) string {
	t.Helper()
	d, err := digest.ComputeBundleDigest(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
