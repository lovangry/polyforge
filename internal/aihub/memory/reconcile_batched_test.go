package memory

// reconcile_batched_test.go: the batched FTS rebuild pass.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// placeAuthority writes an authority file directly under the tree (direct placement) with valid frontmatter.
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
	n, err := e.store.RebuildIndex()
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
