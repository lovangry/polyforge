package test

// v1_s15: the migration lane's committed rehearsal. A synthesized v0
// pilot world (the tools/v0export fixture: v0-schema SQLite, authority
// memory files in the exact v0 frontmatter format, a capsule revision
// chain), exported by the REAL v0-export binary, then seeded into a REAL
// aihub process by the REAL seed runner (tools/seed-v1.sh driving the
// real pf CLI), then verified: every record landed with its terminal
// fields, the reconcile backfilled the memory index (retrieval over the
// real HTTP face), the capsule snapshot tree materialized, and the
// runner's own idempotent re-run asserted zero delta.
//
// The fixture is a copy synthesized per run — CI never touches the real
// pilot directory (/root/polyforge-pilot stays read-only everywhere).

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/tools/v0export"
	_ "modernc.org/sqlite"
)

func TestV1S15MigrationRehearsal(t *testing.T) {
	inst := newV1Install(t) // creates project "e2e", harvests the bootstrap key
	bins := v1Bins(t)

	// 1. the fixture v0 world (a fresh copy per run).
	fix, err := v0export.BuildFixture(filepath.Join(t.TempDir(), "v0pilot"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// 2. the real runner: export → import WIs → import memories →
	// idempotent re-run (its own zero-delta assertion exits non-zero on
	// any drift).
	exportDir := filepath.Join(t.TempDir(), "export")
	script := filepath.Join(v1Root(t), "tools", "seed-v1.sh")
	cmd := exec.Command("sh", script,
		"--pilot-dir", fix.Dir,
		"--server", inst.url,
		"--api-key", inst.adminKey,
		"--data-dir", inst.dataDir,
		"--project", v1Project,
		"--owner", "usr_root",
		"--out", exportDir,
		"--pf", inst.pfPath,
		"--v0-export", filepath.Join(bins, "v0-export"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed runner: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "seed-v1: done") {
		t.Fatalf("runner output incomplete:\n%s", out)
	}
	if !strings.Contains(string(out), "imported 0, skipped 4") {
		t.Errorf("re-run was not a pure skip:\n%s", out)
	}

	// 3. the terminal records landed with their terminal fields.
	wi, err := inst.client.GetWorkItem("wi_wrapped000001")
	if err != nil {
		t.Fatalf("get wrapped: %v", err)
	}
	if wi.Status != "wrapped" || wi.Seq != 1 || wi.Slug != v1Project+"#1" {
		t.Errorf("wrapped record drifted: %+v", wi)
	}
	if wi.ClosedAt == nil || *wi.ClosedAt != "2026-09-02T10:00:00Z" {
		t.Errorf("closed_at not preserved: %v", wi.ClosedAt)
	}
	// the v0 double-l vocabulary normalized to v1's single-l
	canceled, err := inst.client.GetWorkItem("wi_cancelled0001")
	if err != nil || canceled.Status != "canceled" {
		t.Errorf("cancelled normalization: %+v (%v)", canceled, err)
	}
	blocked, err := inst.client.GetWorkItem("wi_blocked000001")
	if err != nil || blocked.Status != "blocked" || blocked.ClosedAt != nil {
		t.Errorf("blocked record drifted: %+v (%v)", blocked, err)
	}

	// 4. the memories: the reconcile backfilled rows + FTS from the
	// placed authority files — retrieval over the real HTTP face proves
	// the searchable index.
	raw, err := inst.client.SearchMemories(v1Project, "ZebraFix", "", nil, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(string(raw), "mem_fix00000001") || !strings.Contains(string(raw), "mem_fix00000002") {
		t.Errorf("reconcile did not make the migrated memories searchable: %s", raw)
	}
	// the redacted stock is soft-deleted: not searchable, but its row
	// and _redacted placement exist.
	if strings.Contains(string(raw), "mem_fix00000003") {
		t.Errorf("redacted stock is searchable: %s", raw)
	}
	redacted := filepath.Join(inst.dataDir, "memory", projID(t, inst), "_redacted", "mem_fix00000003", "memory.md")
	if _, err := os.Stat(redacted); err != nil {
		t.Errorf("redacted placement: %v", err)
	}
	// the resolvable work_item_id was PRESERVED (WIs were imported first)
	placed, err := os.ReadFile(filepath.Join(inst.dataDir, "memory", projID(t, inst), "mem_fix00000001", "memory.md"))
	if err != nil {
		t.Fatalf("placed authority file: %v", err)
	}
	if !strings.Contains(string(placed), "work_item_id: wi_wrapped000001") {
		t.Errorf("resolvable WI ref not preserved:\n%s", placed)
	}
	// and the frontmatter carries the TARGET identity, not the old one
	if !strings.Contains(string(placed), "owner_user_id: usr_root") {
		t.Errorf("owner identity not converted:\n%s", placed)
	}

	// 5. the capsule snapshot tree materialized beside the JSONL exports.
	mainB, err := os.ReadFile(filepath.Join(exportDir, "capsule-snapshots",
		"wi_wrapped000001", "docs", "main.md"))
	if err != nil {
		t.Fatalf("capsule snapshot: %v", err)
	}
	if !strings.Contains(string(mainB), "replacement content v2") {
		t.Errorf("snapshot head wrong: %s", mainB)
	}
	if _, err := os.Stat(filepath.Join(exportDir, "capsule-snapshots",
		"wi_wrapped000001", "docs", "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("the seq-1 delete did not apply to the snapshot")
	}

	// 6. the source fixture was never written: the runner is read-only on
	// the pilot side (spot-check the authority tree is byte-stable).
	srcMem, err := os.ReadFile(filepath.Join(fix.Dir, "memory", fix.Project, "mem_fix00000001", "memory.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(srcMem), "project_id: "+fix.Project) {
		t.Errorf("source authority file was modified:\n%s", srcMem)
	}
}

func projID(t *testing.T, inst *v1Install) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(inst.dataDir, "aihub.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow(`SELECT id FROM projects WHERE name=?`, v1Project).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
