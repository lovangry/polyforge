package cli

// migrate_verify.go: the four-step post-import verification (the v0
// §10.1 semantics, adapted). Steps ①② read the target Core SQLite file
// READ-ONLY — the tool must not write the live database; the live server
// is the only writer (the reconcile it just ran is the thing under
// test). Step ③ goes over the real HTTP search face. Step ④ re-reads the
// placed authority files and judges them against the conversion report.
//
// Any failed step is recorded in the report and fails the whole run.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// migOpenCoreReadOnly opens <data-dir>/aihub.db read-only. NEVER a write
// path.
func migOpenCoreReadOnly(dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, "aihub.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("target core database not found at %s (wrong --data-dir?): %v", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open target core database read-only: %v", err)
	}
	return db, nil
}

// migVerifyMemories runs the four steps and fills rep.Verification. The
// returned error is non-nil exactly when some step failed.
func migVerifyMemories(opts MigMemoriesOptions, rep *MigMemoriesReport, placed []placedMem, client *aihubclient.Client) error {
	db, err := migOpenCoreReadOnly(opts.DataDir)
	if err != nil {
		// The core file is unreachable: the whole gate is broken, which IS
		// a count-reconciliation failure.
		step := StepResult{Step: "count-reconciliation", OK: false, Detail: err.Error()}
		rep.Verification = append(rep.Verification, step)
		return &verifyError{failures: []string{step.Step + ": " + step.Detail}}
	}
	defer db.Close()

	var failures []string
	appendStep := func(step StepResult) {
		rep.Verification = append(rep.Verification, step)
		if !step.OK {
			failures = append(failures, step.Step+": "+step.Detail)
		}
	}
	appendStep(migVerifyCounts(db, opts, rep, placed))
	appendStep(migVerifyContentSHA(opts.DataDir, db, rep, placed))
	appendStep(migVerifyRetrieval(rep, placed, client))
	appendStep(migVerifyConversions(opts.DataDir, rep, placed))
	if len(failures) > 0 {
		return &verifyError{failures: failures}
	}
	return nil
}

// migVerifyCounts is step ①: every migrated id has a row with the
// expected status, the TARGET PROJECT's row counts equal the placed
// authority tree's counts and the (project-scoped) reconcile statistic
// equals the tree's active count. Scoped, not global (seq-fix lane 3c):
// the target carries OTHER projects' stock in a real cutover rig, and
// comparing the whole memories table against ONE project's tree reported
// 1165 != 139 on the real export. The placed tree (not this run's import
// count) is the project's stock: a delta run feeds a subset, and earlier
// runs' placements persist — so the tree is what the project's rows must
// add up to.
func migVerifyCounts(db *sql.DB, opts MigMemoriesOptions, rep *MigMemoriesReport, placed []placedMem) StepResult {
	step := StepResult{Step: "count-reconciliation", OK: true}
	var problems []string
	for _, p := range placed {
		want := p.rec.Status
		var got string
		err := db.QueryRow(`SELECT status FROM memories WHERE id=?1`, p.rec.ID).Scan(&got)
		if err == sql.ErrNoRows {
			problems = append(problems, fmt.Sprintf("%s: no row (reconcile skipped the file?)", p.rec.ID))
		} else if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", p.rec.ID, err))
		} else if got != want {
			problems = append(problems, fmt.Sprintf("%s: status %q, want %q", p.rec.ID, got, want))
		}
	}
	counts := map[string]int{}
	rows, err := db.Query(`SELECT status, COUNT(*) FROM memories WHERE project_id=?1 GROUP BY status`,
		rep.TargetProject)
	if err != nil {
		return StepResult{Step: step.Step, OK: false, Detail: "count rows: " + err.Error()}
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return StepResult{Step: step.Step, OK: false, Detail: "count rows: " + err.Error()}
		}
		counts[st] = n
	}
	rows.Close()
	treeActive, treeRedacted, terr := migPlacedTreeCounts(opts.DataDir, rep.TargetProject)
	if terr != nil {
		return StepResult{Step: step.Step, OK: false, Detail: "count placed tree: " + terr.Error()}
	}
	if counts["active"] != treeActive {
		problems = append(problems, fmt.Sprintf("project active rows %d != placed tree %d", counts["active"], treeActive))
	}
	if counts["redacted"] != treeRedacted {
		problems = append(problems, fmt.Sprintf("project redacted rows %d != placed tree %d", counts["redacted"], treeRedacted))
	}
	if rep.Rebuilt != treeActive {
		problems = append(problems, fmt.Sprintf("rebuild-index rebuilt %d != placed tree active %d", rep.Rebuilt, treeActive))
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d mismatch(es): %s", len(problems), joinNonEmpty(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d row(s) reconciled; project-scoped active=%d redacted=%d; rebuilt=%d",
		len(placed), treeActive, treeRedacted, rep.Rebuilt)
	return step
}

// migPlacedTreeCounts counts the authority-file stock the target tree
// carries for the project: active memory/<pj>/<mem>/ directories plus
// the _redacted ones. The tree IS the global placed set (files placed
// by earlier runs persist), which is what the global row counts must
// equal — for a full run and a delta subset alike.
func migPlacedTreeCounts(dataDir, projectID string) (active, redacted int, err error) {
	root := filepath.Join(dataDir, "memory", projectID)
	memDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, md := range memDirs {
		if !md.IsDir() {
			continue
		}
		if md.Name() == "_redacted" {
			redDirs, rerr := os.ReadDir(filepath.Join(root, "_redacted"))
			if rerr != nil {
				continue
			}
			for _, rd := range redDirs {
				if rd.IsDir() {
					redacted++
				}
			}
			continue
		}
		active++
	}
	return active, redacted, nil
}

// migVerifyContentSHA is step ②: for every sampled id, the live sha256
// of the placed memory.md equals the memories.content_sha column.
func migVerifyContentSHA(dataDir string, db *sql.DB, rep *MigMemoriesReport, placed []placedMem) StepResult {
	step := StepResult{Step: "content-sha-sampling", OK: true}
	sampled := map[string]bool{}
	for _, id := range rep.SampledIDs {
		sampled[id] = true
	}
	byID := map[string]placedMem{}
	for _, p := range placed {
		byID[p.rec.ID] = p
	}
	var problems []string
	checked := 0
	for _, id := range rep.SampledIDs {
		p, ok := byID[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: sampled id not placed", id))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(p.relPath), "memory.md"))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		var want string
		if err := db.QueryRow(`SELECT content_sha FROM memories WHERE id=?1`, id).Scan(&want); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		checked++
		sum := sha256.Sum256(raw)
		if want != hex.EncodeToString(sum[:]) {
			problems = append(problems, fmt.Sprintf("%s: content_sha mismatch (row %s != file %s)",
				id, want, hex.EncodeToString(sum[:])))
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d mismatch(es): %s", len(problems), joinNonEmpty(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d of %d record(s) sampled at the fixed %d%% ratio, all digests match",
		checked, len(placed), migSampleRatioN*100/migSampleRatioD)
	return step
}

// migVerifyRetrieval is step ③: for every sampled ACTIVE entry the admin
// key can search, a LONG distinctive content substring must retrieve the
// entry over the real HTTP search face. v1 aihub retrieval is lexical
// (FTS5 trigram) — there is no embedding provider, so "semantic recall"
// never happens and the verification is exact content-match by design
// (seq-fix lane 3b): the probe is a up-to-48-rune run of the entry's own
// body, batch-unique when one exists. Redacted entries are unsearchable
// by design; private ones are noted as skipped.
func migVerifyRetrieval(rep *MigMemoriesReport, placed []placedMem, client *aihubclient.Client) StepResult {
	step := StepResult{Step: "retrieval-smoke", OK: true}
	byID := map[string]placedMem{}
	for _, p := range placed {
		byID[p.rec.ID] = p
	}
	var problems []string
	smoked, skipped := 0, 0
	for _, id := range rep.SampledIDs {
		p, ok := byID[id]
		if !ok {
			continue
		}
		if p.rec.Status != "active" || p.rec.Visibility == "private" || p.probe == "" {
			skipped++
			continue
		}
		ids, err := migSearchHitIDs(client, p.probe)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: search %q: %v", id, p.probe, err))
			continue
		}
		smoked++
		found := false
		for _, hit := range ids {
			if hit == id {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s: probe %q did not retrieve the entry (hits: %v)", id, p.probe, ids))
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d failure(s): %s", len(problems), joinNonEmpty(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d sampled entr(ies) retrieved via HTTP search (long content-substring probes)", smoked)
	if skipped > 0 {
		step.Detail += fmt.Sprintf("; %d skipped (redacted, private or unprobeable)", skipped)
	}
	return step
}

// migSearchHitIDs runs the memory search and returns the hit ids.
func migSearchHitIDs(client *aihubclient.Client, probe string) ([]string, error) {
	raw, err := client.SearchMemories("", probe, "", nil, 200)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := unmarshalStrict(raw, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		out = append(out, it.ID)
	}
	return out, nil
}

// migVerifyConversions is step ④: the report's conversion lists agree
// with the placed authority FILES. Every placed memory.md is re-read and
// its frontmatter compared against the expected post-conversion state —
// id, target project/owner, preserved-or-blank work_item_id and the
// related survivors — so a writer bug cannot hide behind the report's
// own bookkeeping.
func migVerifyConversions(dataDir string, rep *MigMemoriesReport, placed []placedMem) StepResult {
	step := StepResult{Step: "conversion-reconciliation", OK: true}
	var problems []string
	for _, p := range placed {
		fm, err := migReadPlacedFM(dataDir, p.relPath)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if fm.ID != p.rec.ID {
			problems = append(problems, fmt.Sprintf("%s: frontmatter id %q", p.rec.ID, fm.ID))
		}
		if fm.ProjectID != p.expectedFM.ProjectID {
			problems = append(problems, fmt.Sprintf("%s: project_id %q != %q", p.rec.ID, fm.ProjectID, p.expectedFM.ProjectID))
		}
		if fm.OwnerUserID != p.expectedFM.OwnerUserID {
			problems = append(problems, fmt.Sprintf("%s: owner_user_id %q != %q", p.rec.ID, fm.OwnerUserID, p.expectedFM.OwnerUserID))
		}
		if !migPtrEq(fm.WorkItemID, p.expectedFM.WorkItemID) {
			problems = append(problems, fmt.Sprintf("%s: work_item_id %v != expected %v",
				p.rec.ID, strOrNone(fm.WorkItemID), strOrNone(p.expectedFM.WorkItemID)))
		}
		if !migSliceEq(fm.Related, p.expectedFM.Related) {
			problems = append(problems, fmt.Sprintf("%s: related %v != expected %v", p.rec.ID, fm.Related, p.expectedFM.Related))
		}
	}
	// Every blanked related entry is truly absent from its file.
	for _, c := range rep.BlankedRelated {
		for _, p := range placed {
			if p.rec.ID != c.MemID {
				continue
			}
			for _, rel := range p.expectedFM.Related {
				if rel == c.Old {
					problems = append(problems, fmt.Sprintf("%s: blanked related id %s still present in file", c.MemID, c.Old))
				}
			}
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d mismatch(es): %s", len(problems), joinNonEmpty(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d file(s) re-read; conversions all consistent", len(placed))
	return step
}

func migPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func migSliceEq(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func strOrNone(p *string) string {
	if p == nil {
		return "(none)"
	}
	return *p
}
