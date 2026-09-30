package v0export

import (
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
