package memory

// Reconcile: the INV-7f read-path gate and the rebuild procedure. One
// direction governs everything (INV-7c): when the row and the authority
// file disagree, the FILE wins and the row is corrected from it.
//
// The three scenarios the v1 lane pins:
//   - orphan ROW rollback: a row whose authority file exists nowhere is
//     deleted (create crashed between ② and ③);
//   - FILE backfill: an authority file whose row is missing is backfilled
//     entirely from its frontmatter;
//   - redacted forward-completion: a redact that moved the directory but
//     crashed before committing is completed from the file position
//     (never rolled back).

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// isNotFound reports the MEM-404 of s.load.
func isNotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Code == "MEM-404"
}

// parseAuthorityDoc parses an existing authority document: non-create
// paths must find a self-identifying frontmatter whose id equals the
// directory name; anything else is refused, never silently repaired.
func parseAuthorityDoc(raw []byte, dirMemID string) (*frontmatter, string, error) {
	block, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return nil, "", errf("MEM-400", "FRONTMATTER_INVALID", "no frontmatter block")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return nil, "", errf("MEM-400", "FRONTMATTER_INVALID", "%v", err)
	}
	if fm.ID == "" {
		return nil, "", errf("MEM-400", "FRONTMATTER_INVALID", "frontmatter id missing")
	}
	if dirMemID != "" && fm.ID != dirMemID {
		return nil, "", errf("MEM-400", "FRONTMATTER_ID_MISMATCH", "frontmatter id %q != dir %q", fm.ID, dirMemID)
	}
	if fm.Type == "" || !visibilities[fm.Visibility] || fm.OwnerUserID == "" ||
		fm.ProjectID == "" || fm.CreatedAt == "" || fm.UpdatedAt == "" {
		return nil, "", errf("MEM-400", "FRONTMATTER_INVALID", "missing/invalid required frontmatter fields")
	}
	return &fm, body, nil
}

// rowMatchesFrontmatter compares the metadata set the file can correct.
func rowMatchesFrontmatter(e *Entry, fm *frontmatter) bool {
	return strPtrEq(e.Title, fm.Title) &&
		sliceEq(e.Tags, fm.Tags) &&
		sliceEq(e.Related, fm.Related) &&
		e.Visibility == fm.Visibility &&
		e.UpdatedAt == fm.UpdatedAt
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sliceEq(a, b []string) bool {
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

// entryConsistent is the live INV-7f check: the active authority file
// exists and its digest equals the row's content_sha.
func (s *Store) entryConsistent(e *Entry) bool {
	raw, err := os.ReadFile(filepath.Join(s.dataDir, filepath.FromSlash(e.apath), "memory.md"))
	if err != nil {
		return false
	}
	return shaHex(raw) == e.ContentSHA
}

// validatedLocked is the serving gate for callers that already hold the
// entry's per-mem mutex: check → repair → re-serve.
func (s *Store) validatedLocked(memID string) (*Entry, error) {
	e, err := s.load(memID)
	if err != nil {
		return nil, err
	}
	if e.Status != "active" {
		return nil, errf("MEM-404", "", "memory %s", memID)
	}
	if s.entryConsistent(e) {
		return e, nil
	}
	if rerr := s.reconcileOne(memID); rerr != nil {
		return nil, rerr
	}
	fresh, err := s.load(memID)
	if err != nil {
		// reconcile deleted an orphan row: the entry never existed as far
		// as serving is concerned.
		return nil, errf("MEM-404", "", "memory %s", memID)
	}
	if fresh.Status != "active" || !s.entryConsistent(fresh) {
		return nil, errf("MEM-404", "", "memory %s", memID)
	}
	return fresh, nil
}

// serveConsistent is the gate for list-shaped read paths: an
// inconsistent entry is excluded from THIS result set and a best-effort
// single-entry reconcile is dispatched.
func (s *Store) serveConsistent(e *Entry) (*Entry, bool) {
	unlock := s.memLock(e.ID)
	defer unlock()
	if e.Status == "active" && s.entryConsistent(e) {
		return e, true
	}
	fresh, err := s.load(e.ID)
	if err == nil && fresh.Status == "active" && s.entryConsistent(fresh) {
		return fresh, true
	}
	_ = s.reconcileOne(e.ID)
	return nil, false
}

// reconcileOne brings ONE entry back to a servable state. The caller
// holds the per-mem mutex; everything is re-read inside.
func (s *Store) reconcileOne(memID string) error {
	e, err := s.load(memID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if e.Status != "active" {
		return nil // redacted rows are terminal
	}
	dir := filepath.Join(s.dataDir, filepath.FromSlash(e.apath))
	raw, rerr := os.ReadFile(filepath.Join(dir, "memory.md"))
	if rerr == nil {
		// File present, row drifted: the file wins.
		fm, body, perr := parseAuthorityDoc(raw, memID)
		if perr != nil {
			return perr
		}
		sha := shaHex(raw)
		if sha == e.ContentSHA && rowMatchesFrontmatter(e, fm) {
			return nil // a writer completed ③ while we waited
		}
		if uerr := s.updateRowFromFile(e, fm, raw, sha); uerr != nil {
			return uerr
		}
		s.sweepDirTransients(dir)
		_ = body
		return nil
	}
	// Active file missing: a redact that crashed between ① and ② is
	// forward-completed from the file's position; anything else is an
	// orphan row and dies with its FTS row.
	if _, serr := os.Stat(filepath.Join(s.dataDir, filepath.FromSlash(redactedRelOf(e)), "memory.md")); serr == nil {
		return s.forwardCompleteRedact(e)
	}
	s.sweepDirTransients(dir)
	return s.deleteOrphanRow(memID)
}

// updateRowFromFile rewrites the row's metadata from the parsed file and
// rebuilds the FTS row in the same transaction.
func (s *Store) updateRowFromFile(e *Entry, fm *frontmatter, raw []byte, sha string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET content_sha=?1, content_len=?2, title=?3, tags=?4,
		related=?5, visibility=?6, updated_at=?7 WHERE id=?8`,
		sha, len(raw), anyPtr(fm.Title), jsonOrEmpty(fm.Tags), jsonOrEmpty(fm.Related),
		fm.Visibility, fm.UpdatedAt, e.ID); err != nil {
		return err
	}
	if err := ftsReplace(tx, e.ID, extractBody(string(raw))); err != nil {
		return err
	}
	return tx.Commit()
}

// forwardCompleteRedact finishes a redact that moved the directory but
// crashed before committing: the row is corrected to 'redacted' and the
// FTS row removed — forward completion, never rollback.
func (s *Store) forwardCompleteRedact(e *Entry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET status='redacted' WHERE id=?1`, e.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, e.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteOrphanRow removes a row whose authority file exists nowhere,
// together with its FTS row (the whole create rolls back).
func (s *Store) deleteOrphanRow(memID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM memories WHERE id=?1`, memID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, memID); err != nil {
		return err
	}
	return tx.Commit()
}

// sweepDirTransients removes the write-order transients of one entry
// directory. Callers hold the per-mem mutex, so anything still present
// is crash residue.
func (s *Store) sweepDirTransients(dir string) {
	_ = os.Remove(filepath.Join(dir, "memory.md.tmp"))
}

// sweepTransients removes every write-order transient under the memory
// tree, each removal under the directory's mem_id mutex with an
// existence recheck (a live writer is waited out, not swept).
func (s *Store) sweepTransients() error {
	root := filepath.Join(s.dataDir, "memory")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		unlock := s.memLock(filepath.Base(filepath.Dir(path)))
		defer unlock()
		if _, serr := os.Stat(path); serr != nil {
			return nil // the live writer finished while we waited
		}
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			return rerr
		}
		return nil
	})
}

// RebuildIndex runs the full reconcile: transient sweep, file-driven
// backfill/correction, _redacted forward-completion, row-driven orphan
// rollback and an unconditional per-entry FTS rebuild. Returns the
// number of reindexed active entries.
func (s *Store) RebuildIndex() (int, error) {
	if err := s.sweepTransients(); err != nil {
		return 0, errf("MEM-502", "", "sweep: %v", err)
	}
	projDirs, err := os.ReadDir(filepath.Join(s.dataDir, "memory"))
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, errf("MEM-502", "", "walk: %v", err)
		}
		projDirs = nil
	}
	n := 0
	// Active tree: backfill or correct each entry, rebuild FTS.
	for _, pd := range projDirs {
		if !pd.IsDir() {
			continue
		}
		memDirs, werr := os.ReadDir(filepath.Join(s.dataDir, "memory", pd.Name()))
		if werr != nil {
			return 0, errf("MEM-502", "", "walk: %v", werr)
		}
		for _, md := range memDirs {
			if !md.IsDir() || md.Name() == "_redacted" {
				continue
			}
			counted, cerr := s.rebuildActiveEntry(pd.Name(), md.Name())
			if cerr != nil {
				return 0, errf("MEM-502", "", "rebuild %s: %v", md.Name(), cerr)
			}
			if counted {
				n++
			}
		}
	}
	// _redacted tree: backfill migrated stock; forward-complete crashed
	// redacts.
	for _, pd := range projDirs {
		if !pd.IsDir() {
			continue
		}
		redDirs, rerr := os.ReadDir(filepath.Join(s.dataDir, "memory", pd.Name(), "_redacted"))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue
			}
			return 0, errf("MEM-502", "", "walk: %v", rerr)
		}
		for _, md := range redDirs {
			if !md.IsDir() {
				continue
			}
			if cerr := s.rebuildRedactedEntry(pd.Name(), md.Name()); cerr != nil {
				return 0, errf("MEM-502", "", "rebuild %s: %v", md.Name(), cerr)
			}
		}
	}
	// Row-driven scan: active rows whose authority file is missing.
	rows, err := s.db.Query(`SELECT id FROM memories WHERE status='active'`)
	if err != nil {
		return 0, errf("MEM-502", "", "scan rows: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, errf("MEM-502", "", "scan rows: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		e, lerr := s.load(id)
		if lerr != nil {
			continue
		}
		if s.entryConsistent(e) {
			continue
		}
		unlock := s.memLock(id)
		if cerr := s.reconcileOne(id); cerr != nil {
			unlock()
			if ae, ok := cerr.(*APIError); ok && ae.Code == "MEM-400" {
				continue // unparseable authority file: no branch can act
			}
			return 0, errf("MEM-502", "", "reconcile %s: %v", id, cerr)
		}
		unlock()
	}
	// Dangling FTS rows die last.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id NOT IN (SELECT id FROM memories)`); err != nil {
		return 0, errf("MEM-502", "", "fts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, errf("MEM-502", "", "commit: %v", err)
	}
	return n, nil
}

// rebuildActiveEntry runs the file-driven pass for one active-tree entry
// inside its per-mem mutex: backfill a missing row, correct a drifted
// row (file wins), then unconditionally rebuild the FTS row from the
// same locked snapshot.
func (s *Store) rebuildActiveEntry(dirProject, memID string) (counted bool, err error) {
	unlock := s.memLock(memID)
	defer unlock()
	raw, rerr := os.ReadFile(filepath.Join(s.dataDir, "memory", dirProject, memID, "memory.md"))
	if rerr != nil {
		return false, nil // no authority here: the row-driven scan decides
	}
	fm, _, perr := parseAuthorityDoc(raw, memID)
	if perr != nil {
		return false, nil // invalid frontmatter never becomes an illegal row
	}
	row, lerr := s.load(memID)
	if isNotFound(lerr) {
		if !s.bindingsExist(fm) {
			return false, nil
		}
		if berr := s.backfillRow(fm, raw, dirProject, memID, "active"); berr != nil {
			return false, berr
		}
	} else if lerr != nil {
		return false, lerr
	} else if row.Status == "redacted" {
		// Terminal: the file must not resurrect the row.
		return false, nil
	} else if row.ContentSHA != shaHex(raw) || !rowMatchesFrontmatter(row, fm) {
		if cerr := s.reconcileOne(memID); cerr != nil {
			if ae, ok := cerr.(*APIError); ok && ae.Code == "MEM-400" {
				return false, nil
			}
			return false, cerr
		}
	}
	tx, terr := s.db.Begin()
	if terr != nil {
		return false, terr
	}
	defer tx.Rollback()
	if terr := ftsReplace(tx, memID, extractBody(string(raw))); terr != nil {
		return false, terr
	}
	if terr := tx.Commit(); terr != nil {
		return false, terr
	}
	return true, nil
}

// rebuildRedactedEntry runs the _redacted pass for one entry: migrated
// stock is backfilled; a row still active while the directory already
// sits in _redacted/ is forward-completed.
func (s *Store) rebuildRedactedEntry(dirProject, memID string) error {
	unlock := s.memLock(memID)
	defer unlock()
	raw, rerr := os.ReadFile(filepath.Join(s.dataDir, "memory", dirProject, "_redacted", memID, "memory.md"))
	if rerr != nil {
		return nil
	}
	fm, _, perr := parseAuthorityDoc(raw, memID)
	if perr != nil {
		return nil
	}
	row, lerr := s.load(memID)
	if isNotFound(lerr) {
		if !s.bindingsExist(fm) {
			return nil
		}
		return s.backfillRow(fm, raw, dirProject, memID, "redacted")
	}
	if lerr != nil {
		return lerr
	}
	if row.Status != "redacted" {
		return s.reconcileOne(memID)
	}
	return nil
}

// bindingsExist verifies a frontmatter's FK bindings can satisfy the DDL
// before a backfill inserts a row.
func (s *Store) bindingsExist(fm *frontmatter) bool {
	var one int
	if err := s.db.QueryRow(`SELECT 1 FROM projects WHERE id=?1`, fm.ProjectID).Scan(&one); err != nil {
		return false
	}
	if err := s.db.QueryRow(`SELECT 1 FROM users WHERE id=?1`, fm.OwnerUserID).Scan(&one); err != nil {
		return false
	}
	return true
}

// backfillRow inserts a memories row for a file whose mem_id has none:
// full frontmatter→DDL mapping, content_sha computed live.
func (s *Store) backfillRow(fm *frontmatter, raw []byte, dirProject, memID, status string) error {
	rel := "memory/" + dirProject + "/" + memID
	if status == "redacted" {
		rel = "memory/" + dirProject + "/_redacted/" + memID
	}
	var idem any
	if fm.IdempotencyKey != nil && *fm.IdempotencyKey != "" {
		idem = *fm.IdempotencyKey
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO memories(id, project_id, owner_user_id, work_item_id, type, title,
		visibility, authority_path, content_sha, content_len, tags, related, idempotency_key, status, created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16)`,
		memID, fm.ProjectID, fm.OwnerUserID, anyPtr(fm.WorkItemID), fm.Type, anyPtr(fm.Title),
		fm.Visibility, rel, shaHex(raw), len(raw), jsonOrEmpty(fm.Tags), jsonOrEmpty(fm.Related),
		idem, status, fm.CreatedAt, fm.UpdatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
