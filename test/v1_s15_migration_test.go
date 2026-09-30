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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// The cutover window: records changed and added in v0 AFTER the first
// successful run. --delta tops them up; a changed record hard-fails the
// run without --accept-changed (per-field diff, no baseline promotion)
// and is applied in place (new version wins) with it.
func TestV1S15DeltaTopUpRehearsal(t *testing.T) {
	inst := newV1Install(t)
	bins := v1Bins(t)

	fix, err := v0export.BuildFixture(filepath.Join(t.TempDir(), "v0pilot"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	script := filepath.Join(v1Root(t), "tools", "seed-v1.sh")
	v0exportBin := filepath.Join(bins, "v0-export")
	runSeed := func(extra ...string) (string, error) {
		args := []string{"sh", script,
			"--pilot-dir", fix.Dir,
			"--server", inst.url,
			"--api-key", inst.adminKey,
			"--data-dir", inst.dataDir,
			"--project", v1Project,
			"--owner", "usr_root",
			"--pf", inst.pfPath,
			"--v0-export", v0exportBin,
		}
		args = append(args, extra...)
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		return string(out), err
	}

	// Full seed first (establishes the baseline state file).
	exportFull := filepath.Join(t.TempDir(), "export-full")
	out, err := runSeed("--out", exportFull)
	if err != nil {
		t.Fatalf("full seed: %v\n%s", err, out)
	}
	stateFile := filepath.Join(inst.dataDir, "seed-v1.state.json")
	baseRaw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("the full run did not establish the state file: %v", err)
	}

	// --delta with no baseline at the state path is a hard error.
	out, err = runSeed("--delta", "--state-file", filepath.Join(t.TempDir(), "missing.state.json"),
		"--out", filepath.Join(t.TempDir(), "export-missing"))
	if err == nil || !strings.Contains(out, "no baseline") {
		t.Fatalf("--delta without a baseline must hard-error: err=%v\n%s", err, out)
	}

	// The v0 world moves on: one changed WI, one new WI, one changed
	// memory (its row's content_sha updated with the file, as the live
	// writer would).
	memPath := filepath.Join(fix.Dir, "memory", fix.Project, "mem_fix00000002", "memory.md")
	memRaw, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatal(err)
	}
	memNew := strings.Replace(string(memRaw), "fixture body", "fixture body EDITED", 1)
	if err := os.WriteFile(memPath, []byte(memNew), 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(memNew))
	db, err := sql.Open("sqlite", "file:"+fix.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE work_items SET goal='fixture goal EDITED after the first seed'
		WHERE id='wi_wrapped000001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO work_items(id, seq, project_id, goal, content, status, priority,
		labels, attrs, reporter, waiting_on, bundle_name, bundle_digest, workflow_budget_baseline,
		created_at, updated_at, closed_at)
		VALUES ('wi_newafter0001', 5, ?1, 'a record created after the first seed', NULL, 'wrapped',
		'normal','[]','{}','usr_root',NULL,NULL,NULL,NULL,
		'2026-09-03T08:00:00Z','2026-09-03T09:00:00Z','2026-09-03T09:00:00Z')`, fix.Project); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE memories SET content_sha=?1, content_len=?2, updated_at='2026-09-02T09:00:00Z'
		WHERE id='mem_fix00000002'`, hex.EncodeToString(sum[:]), len(memNew)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Delta WITHOUT --accept-changed: the changed record hard-fails with
	// the per-field diff, and the failed run must NOT promote the
	// baseline (its changes stay visible to the next delta run).
	exportDelta := filepath.Join(t.TempDir(), "export-delta")
	out, err = runSeed("--delta", "--out", exportDelta)
	if err == nil || !strings.Contains(out, "diverge") {
		t.Fatalf("the changed record must fail-closed without --accept-changed: err=%v\n%s", err, out)
	}
	nowRaw, rerr := os.ReadFile(stateFile)
	if rerr != nil || string(nowRaw) != string(baseRaw) {
		t.Errorf("the failed delta run promoted the baseline")
	}

	// Delta WITH --accept-changed: the top-up lands.
	exportAccept := filepath.Join(t.TempDir(), "export-accept")
	out, err = runSeed("--delta", "--accept-changed", "--out", exportAccept)
	if err != nil {
		t.Fatalf("delta + --accept-changed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "delta work items:    1 new, 1 changed") {
		t.Errorf("work-item delta counts wrong:\n%s", out)
	}
	if !strings.Contains(out, "delta memories:     0 new, 1 changed") {
		t.Errorf("memory delta counts wrong:\n%s", out)
	}
	if !strings.Contains(out, "capsule snapshots:  skipped (delta mode)") {
		t.Errorf("capsule skip not reported:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(exportAccept, "capsule-snapshots")); !os.IsNotExist(serr) {
		t.Errorf("delta export replayed capsule snapshots")
	}
	// the changed record's new version won; the new record landed
	wi, err := inst.client.GetWorkItem("wi_wrapped000001")
	if err != nil || wi.Goal != "fixture goal EDITED after the first seed" {
		t.Errorf("the changed record was not applied: %+v (%v)", wi, err)
	}
	newWI, err := inst.client.GetWorkItem("wi_newafter0001")
	if err != nil || newWI.Status != "wrapped" || newWI.Seq != 5 {
		t.Errorf("the new record did not land: %+v (%v)", newWI, err)
	}
	// the changed memory re-placed and the reconcile serves it
	raw, err := inst.client.SearchMemories(v1Project, "EDITED", "", nil, 10)
	if err != nil || !strings.Contains(string(raw), "mem_fix00000002") {
		t.Errorf("changed memory not retrievable after the top-up: %s (%v)", raw, err)
	}
	// the baseline was refreshed by the successful run
	refreshed, rerr := os.ReadFile(stateFile)
	if rerr != nil || string(refreshed) == string(baseRaw) {
		t.Errorf("the successful delta run did not promote the new baseline")
	}

	// A second delta with nothing further is a zero-delta top-up.
	exportZero := filepath.Join(t.TempDir(), "export-zero")
	out, err = runSeed("--delta", "--out", exportZero)
	if err != nil {
		t.Fatalf("zero delta run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "delta work items:    0 new, 0 changed") ||
		!strings.Contains(out, "delta memories:     0 new, 0 changed") ||
		!strings.Contains(out, "seed-v1: done") {
		t.Errorf("zero delta run wrong:\n%s", out)
	}
}
