package memory

// reconcile_scoped_test.go: the project-scoped rebuild (seq-fix lane 3a)
// — RebuildIndexProject walks only the target project's authority tree
// and orphan-scans only its rows; other projects' stock is untouched.
// This is the migrate-memories shape: the rebuild's work and its
// reindexed answer cover exactly the placed tree the count
// reconciliation compares against.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
)

// placeAuthority writes an authority file directly under the tree (the
// CLI直写 migration path) with valid frontmatter.
func placeAuthority(t *testing.T, e *env, projectID, memID, content string) {
	t.Helper()
	dir := filepath.Join(e.store.dataDir, "memory", projectID, memID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("---\nid: %s\ntype: experience.debug\nowner_user_id: %s\nvisibility: project\nproject_id: %s\ncreated_at: \"2026-10-01T00:00:00Z\"\nupdated_at: \"2026-10-01T00:00:00Z\"\n---\n%s\n", memID, e.user, projectID, content)
	if err := os.WriteFile(filepath.Join(dir, "memory.md"), []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildIndexScopedToProject(t *testing.T) {
	e := newEnv(t)
	var alphaID, betaID string
	if err := e.db.QueryRow(`SELECT id FROM projects WHERE name='alpha'`).Scan(&alphaID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.CreateProject(core.CreateProjectRequest{Name: "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(`SELECT id FROM projects WHERE name='beta'`).Scan(&betaID); err != nil {
		t.Fatal(err)
	}
	// Two projects' authority files, both with NO rows (the migrate
	// placement state before the reconcile backfills them).
	placeAuthority(t, e, alphaID, "mem_alpha_scoped", "alpha scoped body token AlphaScopedOne")
	placeAuthority(t, e, betaID, "mem_beta_scoped", "beta scoped body token BetaScopedOne")

	// The scoped rebuild backfills ONLY the target project's tree and
	// answers its count.
	n, err := e.store.RebuildIndexProject(alphaID)
	if err != nil {
		t.Fatalf("scoped rebuild: %v", err)
	}
	if n != 1 {
		t.Fatalf("scoped rebuild reindexed %d, want 1 (alpha's tree only)", n)
	}
	var rows int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project_id=?1`, alphaID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("alpha rows = %d (%v), want 1", rows, err)
	}
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project_id=?1`, betaID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("beta rows = %d (%v), want 0 — the scoped rebuild must not touch other projects", rows, err)
	}
	// The backfilled entry is retrievable (FTS row rebuilt in batch).
	hits, err := e.store.Search("AlphaScopedOne", "", nil, 10, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("scoped rebuild FTS miss: %v %+v", err, hits)
	}

	// The full rebuild now picks up beta too.
	if n, err := e.store.RebuildIndex(); err != nil || n != 2 {
		t.Fatalf("full rebuild: %v %d", err, n)
	}
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project_id=?1`, betaID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("beta rows after full rebuild = %d (%v), want 1", rows, err)
	}

	// The orphan scan is scoped too: an active ROW of ANOTHER project
	// whose file went missing survives a scoped rebuild (it is the full
	// rebuild's — or the INV-7f gate's — business), while the target
	// project's orphan dies. beta's backfilled entry becomes the orphan.
	if err := os.Remove(filepath.Join(e.store.dataDir, "memory", betaID, "mem_beta_scoped", "memory.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RebuildIndexProject(alphaID); err != nil {
		t.Fatalf("scoped rebuild 2: %v", err)
	}
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id='mem_beta_scoped'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("foreign orphan must survive a SCOPED rebuild: rows=%d err=%v", rows, err)
	}
	if _, err := e.store.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id='mem_beta_scoped'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("foreign orphan must die in a FULL rebuild: rows=%d err=%v", rows, err)
	}

	// An empty/absent scope is refused; an unknown project answers 404.
	if _, err := e.store.RebuildIndexProject(""); err == nil {
		t.Fatal("empty scope accepted")
	}
	if _, err := e.store.RebuildIndexProject("pj_nonexistent00"); err == nil {
		t.Fatal("unknown project accepted")
	}
}

// TestRebuildFTSBatched: the batched FTS pass (ftsRebuildBatch rows per
// transaction) answers with the full count and leaves every entry
// retrievable — the batching must be invisible above the store.
func TestRebuildFTSBatched(t *testing.T) {
	e := newEnv(t)
	var alphaID string
	if err := e.db.QueryRow(`SELECT id FROM projects WHERE name='alpha'`).Scan(&alphaID); err != nil {
		t.Fatal(err)
	}
	const total = ftsRebuildBatch + 37 // forces at least one batch rotation
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("mem_batch%06d", i)
		placeAuthority(t, e, alphaID, id, fmt.Sprintf("batch body token BatchToken%06d", i))
		ids = append(ids, id)
	}
	n, err := e.store.RebuildIndexProject(alphaID)
	if err != nil {
		t.Fatalf("batched rebuild: %v", err)
	}
	if n != total {
		t.Fatalf("reindexed %d, want %d", n, total)
	}
	// Spot-check entries on both sides of a batch boundary.
	for _, i := range []int{0, ftsRebuildBatch - 1, ftsRebuildBatch, total - 1} {
		hits, err := e.store.Search(fmt.Sprintf("BatchToken%06d", i), "", nil, 10, nil)
		if err != nil || len(hits) != 1 || hits[0].ID != ids[i] {
			t.Fatalf("entry %d not retrievable after batched rebuild: %v %+v", i, err, hits)
		}
	}
}
