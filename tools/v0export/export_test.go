package v0export

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The export reads a synthesized v0 fixture (fixture.go) and must emit
// the exact v1 seeding inputs: full-field work-item JSONL, dual-source
// memory JSONL (the authority FILE is the content source; the row's sha
// cross-checks it) and the replayed capsule head tree.

func TestExportFixtureFullChain(t *testing.T) {
	dir := t.TempDir()
	f, err := BuildFixture(dir)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	out := filepath.Join(dir, "export")
	rep, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir, Out: out})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if rep.WorkItems != 4 {
		t.Errorf("work items = %d, want 4", rep.WorkItems)
	}
	if rep.Memories != 3 {
		t.Errorf("memories = %d, want 3", rep.Memories)
	}
	if rep.RedactedMemories != 1 {
		t.Errorf("redacted = %d, want 1", rep.RedactedMemories)
	}
	if len(rep.ShaMismatches) != 0 || len(rep.OrphanFiles) != 0 || len(rep.ManifestShaMismatch) != 0 {
		t.Errorf("reconciliation findings not empty: %+v", rep)
	}
	if len(rep.CapsuleSnapshots) != 1 || !strings.Contains(rep.CapsuleSnapshots[0], "@seq1") {
		t.Errorf("capsule snapshots = %v", rep.CapsuleSnapshots)
	}

	// work-items.jsonl: statuses, ids, closed_at, and the v0 cancelled
	// spelling pass through verbatim (the importer normalizes).
	raw, err := os.ReadFile(filepath.Join(out, "work-items.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("work item lines = %d", len(lines))
	}
	seen := map[string]string{}
	for _, l := range lines {
		var r WIRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("wi line: %v (%s)", err, l)
		}
		seen[r.Status] = r.ID
		if r.Seq == nil || *r.Seq < 1 {
			t.Errorf("%s: seq = %v", r.ID, r.Seq)
		}
		if r.Reporter != "usr_root" || r.Project != "fixture-pilot" {
			t.Errorf("%s: reporter/project = %q/%q", r.ID, r.Reporter, r.Project)
		}
	}
	if seen["cancelled"] != "wi_cancelled0001" {
		t.Errorf("cancelled record missing: %v", seen)
	}
	if seen["wrapped"] != "wi_wrapped000001" || seen["blocked"] != "wi_blocked000001" || seen["failed"] != "wi_failed0000001" {
		t.Errorf("record ids wrong: %v", seen)
	}

	// memories.jsonl: content is the authority-file BODY (frontmatter
	// stripped), owner identifier = display name, related survives.
	raw, err = os.ReadFile(filepath.Join(out, "memory", "memories.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	mlines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(mlines) != 3 {
		t.Fatalf("memory lines = %d", len(mlines))
	}
	byID := map[string]MemRecord{}
	for _, l := range mlines {
		var r MemRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("mem line: %v (%s)", err, l)
		}
		byID[r.ID] = r
	}
	m1 := byID["mem_fix00000001"]
	if strings.Contains(m1.Content, "id: mem_fix") {
		t.Errorf("content still carries frontmatter: %q", m1.Content)
	}
	if !strings.Contains(m1.Content, "ZebraFix") {
		t.Errorf("content body lost: %q", m1.Content)
	}
	if m1.Owner != "fixture owner" || m1.OwnerKind != "human" {
		t.Errorf("owner = %q/%q", m1.Owner, m1.OwnerKind)
	}
	if m1.WorkItemID == nil || *m1.WorkItemID != "wi_wrapped000001" {
		t.Errorf("work_item_id = %v", m1.WorkItemID)
	}
	if len(m1.Related) != 1 || m1.Related[0] != "mem_fix00000002" {
		t.Errorf("related = %v", m1.Related)
	}
	if byID["mem_fix00000003"].Status != "redacted" {
		t.Errorf("redacted status lost: %+v", byID["mem_fix00000003"])
	}

	// capsule snapshot: the head tree has main.md replaced and gone.txt
	// deleted by the seq-1 incremental, plus the execution.json.
	snap := filepath.Join(out, "capsule-snapshots", "wi_wrapped000001")
	mainB, err := os.ReadFile(filepath.Join(snap, "docs", "main.md"))
	if err != nil {
		t.Fatalf("snapshot main.md: %v", err)
	}
	if string(mainB) != "replacement content v2\n" {
		t.Errorf("main.md = %q (the seq-1 put did not apply)", mainB)
	}
	if _, err := os.Stat(filepath.Join(snap, "docs", "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt survived the seq-1 delete")
	}
	if _, err := os.Stat(filepath.Join(snap, "runtime-state", "execution.json")); err != nil {
		t.Errorf("execution.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "capsule-snapshots", "wi_wrapped000001.head-manifest.json")); err != nil {
		t.Errorf("head manifest copy: %v", err)
	}
}

// The dual-source gate: a row whose authority file is missing refuses to
// export (never invent content), and a live file whose digest drifted
// from the row's content_sha is flagged while the FILE body still wins.
func TestExportMemorySourceGates(t *testing.T) {
	dir := t.TempDir()
	f, err := BuildFixture(dir)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// Drift one file's bytes (sha mismatch → reported, file body wins).
	target := filepath.Join(f.Dir, "memory", f.Project, "mem_fix00000002", "memory.md")
	if err := os.WriteFile(target, []byte(strings.Replace(string(mustRead(t, target)),
		"ZebraFix", "DriftedTok", 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "export")
	rep, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir, Out: out, SkipCapsules: true})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(rep.ShaMismatches) != 1 || !strings.Contains(rep.ShaMismatches[0], "mem_fix00000002") {
		t.Errorf("sha mismatches = %v", rep.ShaMismatches)
	}
	raw, err := os.ReadFile(filepath.Join(out, "memory", "memories.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "DriftedTok") {
		t.Errorf("the drifted FILE body did not win")
	}

	// Missing authority file: the export refuses.
	if err := os.RemoveAll(filepath.Join(f.Dir, "memory", f.Project, "mem_fix00000001")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir, Out: filepath.Join(dir, "export2"), SkipCapsules: true}); err == nil {
		t.Errorf("missing authority file did not fail the export")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// readState loads a written hash state file.
func readState(t *testing.T, path string) *HashState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st HashState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state %s: %v", path, err)
	}
	if st.WorkItems == nil {
		st.WorkItems = map[string]string{}
	}
	if st.Memories == nil {
		st.Memories = map[string]string{}
	}
	return &st
}

// jsonlIDs returns the record ids of a JSONL file, in order.
func jsonlIDs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var r struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ids = append(ids, r.ID)
	}
	return ids
}

// Delta mode: a full run writes the candidate FULL hash set; a delta run
// against the promoted baseline emits only the new and changed records
// (changed ones INCLUDED), skips the capsule snapshots, and its own
// candidate still carries every record the source has now.
func TestExportDeltaMode(t *testing.T) {
	dir := t.TempDir()
	f, err := BuildFixture(dir)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	out := filepath.Join(dir, "export")
	if _, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir, Out: out}); err != nil {
		t.Fatalf("full export: %v", err)
	}
	candPath := filepath.Join(out, CandidateStateFile)
	cand := readState(t, candPath)
	if len(cand.WorkItems) != 4 || len(cand.Memories) != 3 {
		t.Fatalf("full candidate hashes: wi=%d mem=%d, want 4/3", len(cand.WorkItems), len(cand.Memories))
	}

	// seed-v1.sh promotes the candidate to the persistent state file.
	statePath := filepath.Join(dir, "seed-v1.state.json")
	if err := os.WriteFile(statePath, mustRead(t, candPath), 0o640); err != nil {
		t.Fatal(err)
	}

	// Mutate the v0 world after the first run (the cutover window):
	// one changed WI, one brand-new WI, one changed memory body, one
	// new orphan authority file.
	db, err := sql.Open("sqlite", "file:"+f.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE work_items SET goal='fixture goal CHANGED' WHERE id='wi_wrapped000001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO work_items(id, seq, project_id, goal, content, status, priority,
		labels, attrs, reporter, waiting_on, bundle_name, bundle_digest, workflow_budget_baseline,
		created_at, updated_at, closed_at)
		VALUES ('wi_newafter0001', 5, ?1, 'a brand new record', NULL, 'wrapped', 'normal',
		'[]','{}','usr_root',NULL,NULL,NULL,NULL,'2026-09-03T08:00:00Z','2026-09-03T09:00:00Z','2026-09-03T09:00:00Z')`,
		f.Project); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mem2 := filepath.Join(f.Dir, "memory", f.Project, "mem_fix00000002", "memory.md")
	if err := os.WriteFile(mem2, []byte(strings.Replace(string(mustRead(t, mem2)),
		"ZebraFix", "EditedAfterFirstRun", 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	orphanDir := filepath.Join(f.Dir, "memory", f.Project, "mem_orphan00001")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "memory.md"), []byte(
		"---\nid: mem_orphan00001\ntype: fact.note\nowner_user_id: usr_owner00aa\n"+
			"visibility: project\nproject_id: "+f.Project+"\nrelated: []\n"+
			"created_at: \"2026-09-03T08:00:00.000000Z\"\nupdated_at: \"2026-09-03T08:00:00.000000Z\"\n---\n"+
			"a brand new orphan body\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	out2 := filepath.Join(dir, "export2")
	rep2, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir, Out: out2,
		Delta: true, BaselineFile: statePath})
	if err != nil {
		t.Fatalf("delta export: %v", err)
	}
	if rep2.Delta == nil {
		t.Fatal("delta report missing")
	}
	if rep2.Delta.NewWorkItems != 1 || rep2.Delta.ChangedWorkItems != 1 {
		t.Errorf("wi delta = %d new / %d changed, want 1/1", rep2.Delta.NewWorkItems, rep2.Delta.ChangedWorkItems)
	}
	if rep2.Delta.NewMemories != 1 || rep2.Delta.ChangedMemories != 1 {
		t.Errorf("mem delta = %d new / %d changed, want 1/1", rep2.Delta.NewMemories, rep2.Delta.ChangedMemories)
	}
	if !rep2.Delta.CapsulesSkipped {
		t.Errorf("capsule snapshots not reported as skipped")
	}
	// The emitted delta files carry ONLY the new/changed records, changed
	// ones included.
	wiIDs := jsonlIDs(t, filepath.Join(out2, "work-items.jsonl"))
	if len(wiIDs) != 2 || !contains(wiIDs, "wi_wrapped000001") || !contains(wiIDs, "wi_newafter0001") {
		t.Errorf("delta work-items.jsonl = %v, want the changed + new ids only", wiIDs)
	}
	memIDs := jsonlIDs(t, filepath.Join(out2, "memory", "memories.jsonl"))
	if len(memIDs) != 2 || !contains(memIDs, "mem_fix00000002") || !contains(memIDs, "mem_orphan00001") {
		t.Errorf("delta memories.jsonl = %v, want the changed + new ids only", memIDs)
	}
	// Capsule snapshot replay skipped entirely in delta mode.
	if _, err := os.Stat(filepath.Join(out2, "capsule-snapshots")); !os.IsNotExist(err) {
		t.Errorf("delta export replayed capsule snapshots: %v", err)
	}
	// The delta candidate still carries the FULL current hash set (the
	// next baseline must not lose the unchanged records).
	cand2 := readState(t, filepath.Join(out2, CandidateStateFile))
	if len(cand2.WorkItems) != 5 || len(cand2.Memories) != 4 {
		t.Errorf("delta candidate hashes: wi=%d mem=%d, want 5/4", len(cand2.WorkItems), len(cand2.Memories))
	}
	// A second delta against the just-computed candidate is empty: the
	// promotion keeps the next run honest.
	state2 := filepath.Join(dir, "seed-v1.state.2.json")
	if err := os.WriteFile(state2, mustRead(t, filepath.Join(out2, CandidateStateFile)), 0o640); err != nil {
		t.Fatal(err)
	}
	rep3, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir, Out: filepath.Join(dir, "export3"),
		Delta: true, BaselineFile: state2})
	if err != nil {
		t.Fatalf("second delta export: %v", err)
	}
	if rep3.WorkItems != 0 || rep3.Memories != 0 ||
		rep3.Delta.NewWorkItems+rep3.Delta.ChangedWorkItems+rep3.Delta.NewMemories+rep3.Delta.ChangedMemories != 0 {
		t.Errorf("second delta not empty: %+v", rep3.Delta)
	}
}

// --delta without a baseline is a hard error, never a silent full export.
func TestExportDeltaRequiresBaseline(t *testing.T) {
	dir := t.TempDir()
	f, err := BuildFixture(dir)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir,
		Out: filepath.Join(dir, "out"), Delta: true}); err == nil {
		t.Fatal("delta without --baseline accepted")
	}
	if _, err := Run(Options{DBPath: f.DBPath, DataDir: f.Dir,
		Out: filepath.Join(dir, "out"), Delta: true,
		BaselineFile: filepath.Join(dir, "missing.state.json")}); err == nil {
		t.Fatal("delta with a missing baseline accepted")
	}
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
