package migrate

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// The §10.1 four-step post-migration verification. Steps ①② read the
// Core SQLite file READ-ONLY (the tool must not write the live database;
// WAL allows a concurrent reader alongside the live server, and the
// migration window is silent by A12 anyway). Step ③ goes over the real
// HTTP search face. Step ④ re-reads the placed authority files.
//
// Any failed step is recorded in the report and fails the whole run with
// a non-zero exit (§10.1: 校验失败 → 修复后整批重跑).

// sampleRatioN/sampleRatioD fix the §10.1 ② sampling ratio at 10%.
const (
	sampleRatioN = 1
	sampleRatioD = 10
)

// seededRand is the deterministic sampler: re-runs verify the same
// sample (the migration is idempotent; the report must be reproducible).
func seededRand() *rand.Rand { return rand.New(rand.NewSource(1)) }

// verifyError aggregates the failed verification steps; the report is
// still returned so the CLI can print the full picture before exiting
// non-zero.
type verifyError struct{ failures []string }

func (e *verifyError) Error() string {
	return "verification failed: " + strings.Join(e.failures, "; ")
}

// verify runs the four steps and fills rep.Verification. The returned
// error is non-nil exactly when some step failed.
func verify(opts Options, rep *Report, placed []placedMem) error {
	db, err := openCoreReadOnly(opts.DataDir)
	if err != nil {
		// The Core file is unreachable (wrong --data-dir, or the deployment
		// never ran): the whole gate is broken, which IS a count-reconciliation
		// failure — recorded as a step result so the report still shows it.
		step := StepResult{Step: "count-reconciliation", OK: false, Detail: err.Error()}
		rep.Verification = append(rep.Verification, step)
		return &verifyError{failures: []string{step.Step + ": " + step.Detail}}
	}
	defer db.Close()

	var failures []string
	rep.Verification = nil

	// ① count reconciliation: per-entry status rows for every migrated id
	// (covers delta re-runs), plus — in FULL mode — the global row-count
	// comparison and the rebuild statistic.
	rep.Verification = append(rep.Verification, verifyCounts(db, rep, placed, opts.Since == ""))
	if last := rep.Verification[len(rep.Verification)-1]; !last.OK {
		failures = append(failures, last.Step+": "+last.Detail)
	}

	// ② content_sha sampling at the fixed ratio.
	rep.Verification = append(rep.Verification, verifyContentSHA(opts.DataDir, db, rep, placed))
	if last := rep.Verification[len(rep.Verification)-1]; !last.OK {
		failures = append(failures, last.Step+": "+last.Detail)
	}

	// ③ retrieval smoke over the real HTTP search face.
	rep.Verification = append(rep.Verification, verifyRetrieval(opts, rep, placed))
	if last := rep.Verification[len(rep.Verification)-1]; !last.OK {
		failures = append(failures, last.Step+": "+last.Detail)
	}

	// ④ blanked-list reconciliation: the report lists match the placed
	// authority files.
	rep.Verification = append(rep.Verification, verifyBlankedLists(opts.DataDir, rep, placed))
	if last := rep.Verification[len(rep.Verification)-1]; !last.OK {
		failures = append(failures, last.Step+": "+last.Detail)
	}

	if len(failures) > 0 {
		return &verifyError{failures: failures}
	}
	return nil
}

// openCoreReadOnly opens <data-dir>/aihub.db read-only. NEVER a write
// path: the only live writer is the active service (INV-2a).
func openCoreReadOnly(dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, "aihub.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("core database not found at %s: %w", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open core database read-only: %w", err)
	}
	return db, nil
}

// verifyCounts is step ①: every migrated id has a row with the expected
// status (active/redacted); in full mode the global row counts equal the
// export counts and the rebuild statistic equals the active count.
func verifyCounts(db *sql.DB, rep *Report, placed []placedMem, full bool) StepResult {
	step := StepResult{Step: "count-reconciliation", OK: true}
	var problems []string
	for _, p := range placed {
		want := "active"
		if p.rec.Status == "redacted" {
			want = "redacted"
		}
		var got string
		err := db.QueryRow(`SELECT status FROM memories WHERE id=?1`, p.rec.ID).Scan(&got)
		if err == sql.ErrNoRows {
			problems = append(problems, fmt.Sprintf("%s: no row", p.rec.ID))
		} else if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", p.rec.ID, err))
		} else if got != want {
			problems = append(problems, fmt.Sprintf("%s: status %q, want %q", p.rec.ID, got, want))
		}
	}
	if full {
		counts := map[string]int{}
		rows, err := db.Query(`SELECT status, COUNT(*) FROM memories GROUP BY status`)
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
		if counts["active"] != rep.ActiveCount {
			problems = append(problems, fmt.Sprintf("global active rows %d != exported %d", counts["active"], rep.ActiveCount))
		}
		if counts["redacted"] != rep.RedactedCount {
			problems = append(problems, fmt.Sprintf("global redacted rows %d != exported %d", counts["redacted"], rep.RedactedCount))
		}
		if rep.Rebuilt != rep.ActiveCount {
			problems = append(problems, fmt.Sprintf("rebuild-index rebuilt %d != exported active %d", rep.Rebuilt, rep.ActiveCount))
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d mismatch(es): %s", len(problems), strings.Join(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d row(s) reconciled", len(placed))
	if full {
		step.Detail += fmt.Sprintf("; global active=%d redacted=%d; rebuilt=%d",
			rep.ActiveCount, rep.RedactedCount, rep.Rebuilt)
	} else {
		step.Detail += " (delta mode: per-entry reconciliation)"
	}
	return step
}

// verifyContentSHA is step ②: for every sampled id, the live sha256 of
// the placed memory.md equals the memories.content_sha column.
func verifyContentSHA(dataDir string, db *sql.DB, rep *Report, placed []placedMem) StepResult {
	step := StepResult{Step: "content-sha-sampling", OK: true}
	sampled := sampleSet(rep.SampledIDs)
	byID := placedByIDs(placed)
	var problems []string
	checked := 0
	for _, id := range rep.SampledIDs {
		if !sampled[id] {
			continue
		}
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
		sum := sha256.Sum256(raw)
		var want string
		if err := db.QueryRow(`SELECT content_sha FROM memories WHERE id=?1`, id).Scan(&want); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		checked++
		if want != hex.EncodeToString(sum[:]) {
			problems = append(problems, fmt.Sprintf("%s: content_sha mismatch (row %s != file %s)",
				id, want, hex.EncodeToString(sum[:])))
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d mismatch(es): %s", len(problems), strings.Join(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d of %d record(s) sampled at the fixed %d%% ratio, all digests match",
		checked, len(placed), sampleRatioN*100/sampleRatioD)
	return step
}

// verifyRetrieval is step ③: for every sampled entry the operator key
// can read (active, non-private — 06 §7 pins private to the owner for
// admins too, and the tool holds no owner credentials), a 3-character
// body substring must retrieve the entry over the real HTTP search face.
// Redacted entries are unsearchable by design; private ones are noted as
// skipped rather than silently dropped from the smoke.
func verifyRetrieval(opts Options, rep *Report, placed []placedMem) StepResult {
	step := StepResult{Step: "retrieval-smoke", OK: true}
	api := newAihub(opts.AihubURL, opts.APIKey)
	byID := placedByIDs(placed)
	var problems []string
	smoked, skipped := 0, 0
	for _, id := range rep.SampledIDs {
		p, ok := byID[id]
		if !ok {
			continue
		}
		if p.rec.Status != "active" || p.rec.Visibility == "private" {
			skipped++
			continue
		}
		probe, err := smokeSubstring(p.rec.Content)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		// The probe rides the query string; the shared client URL-encodes
		// it, so CJK probes survive verbatim.
		ids, err := api.searchMemories(p.projectName, probe)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: search %q: %v", id, probe, err))
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
			problems = append(problems, fmt.Sprintf("%s: probe %q did not retrieve the entry (hits: %v)", id, probe, ids))
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d failure(s): %s", len(problems), strings.Join(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d sampled entr(ies) retrieved via HTTP search", smoked)
	if skipped > 0 {
		step.Detail += fmt.Sprintf("; %d skipped (redacted or private: not retrievable by the operator key)", skipped)
	}
	return step
}

// verifyBlankedLists is step ④: the report's blanked lists agree with the
// placed authority FILES. Every placed memory.md is re-read and its
// frontmatter compared against the expected post-blanking state —
// work_item_id absent for every migrated entry, related exactly the
// resolvable subset — so a writer bug cannot hide behind the report's own
// bookkeeping.
func verifyBlankedLists(dataDir string, rep *Report, placed []placedMem) StepResult {
	step := StepResult{Step: "blanked-list-reconciliation", OK: true}
	var problems []string
	// The report lists must be exactly the files' truth.
	reportedWI := map[string]bool{}
	for _, b := range rep.BlankedWorkItems {
		reportedWI[b.MemID] = true
	}
	reportedRel := map[string]int{}
	for _, b := range rep.BlankedRelated {
		reportedRel[b.MemID]++
	}
	for _, p := range placed {
		fm, err := readPlacedFM(dataDir, p.relPath)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if fm.ID != p.rec.ID {
			problems = append(problems, fmt.Sprintf("%s: frontmatter id %q", p.relPath, fm.ID))
		}
		hadOldWI := p.rec.WorkItemID != nil && *p.rec.WorkItemID != ""
		if fm.WorkItemID != nil {
			problems = append(problems, fmt.Sprintf("%s: work_item_id %q NOT blanked", p.rec.ID, *fm.WorkItemID))
		} else if hadOldWI != reportedWI[p.rec.ID] {
			problems = append(problems, fmt.Sprintf("%s: blanked-list mismatch (file blanked=%v, report lists=%v)",
				p.rec.ID, true, reportedWI[p.rec.ID]))
		}
		// Related: the file must carry exactly the expected survivors.
		want := p.expectedFM.Related
		if len(want) == 0 && len(fm.Related) == 0 {
			continue
		}
		if !sameStrings(want, fm.Related) {
			problems = append(problems, fmt.Sprintf("%s: related %v != expected %v", p.rec.ID, fm.Related, want))
		}
	}
	// Totals: every reported blanked related entry is truly absent.
	for memID, n := range reportedRel {
		p, ok := placedByIDs(placed)[memID]
		if !ok {
			problems = append(problems, fmt.Sprintf("blanked related lists %s which was not migrated", memID))
			continue
		}
		droppedSet := map[string]bool{}
		for _, b := range rep.BlankedRelated {
			if b.MemID == memID {
				droppedSet[b.DroppedID] = true
			}
		}
		for _, rel := range p.expectedFM.Related {
			if droppedSet[rel] {
				problems = append(problems, fmt.Sprintf("%s: dropped related id %s still present in file", memID, rel))
			}
		}
		if len(droppedSet) != n {
			problems = append(problems, fmt.Sprintf("%s: blanked related bookkeeping mismatch", memID))
		}
	}
	if len(problems) > 0 {
		step.OK = false
		step.Detail = fmt.Sprintf("%d mismatch(es): %s", len(problems), strings.Join(problems, "; "))
		return step
	}
	step.Detail = fmt.Sprintf("%d file(s) re-read; blanked work_item_id=%d, blanked related=%d, all consistent",
		len(placed), len(rep.BlankedWorkItems), len(rep.BlankedRelated))
	return step
}

func sampleSet(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func placedByIDs(placed []placedMem) map[string]placedMem {
	out := map[string]placedMem{}
	for _, p := range placed {
		out[p.rec.ID] = p
	}
	return out
}

func sameStrings(a, b []string) bool {
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
