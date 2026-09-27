package memory

// The 06 §5.3 reconcile/rebuild procedure and the INV-7f read-path gate
// (06 §2.3, §6). One direction governs everything here (INV-7c): when the
// memories row and the authority file disagree, the FILE is the authority
// and the row is corrected from it — never the other way around.
//
//   - RebuildIndex runs the full numbered §5.3 procedure (transient sweep,
//     file-driven metadata backfill/correction, _redacted forward-completion,
//     row-driven orphan rollback, unconditional per-entry FTS rebuild). It
//     is the one procedure behind the rebuild-index endpoint, the startup
//     reconcile and the scheduled task (06 §6 "对账式样均在 §5.3 内实现").
//   - validated/reconcileOne implement the single-entry read-path repair of
//     06 §6: verify the row's content_sha against the LIVE file digest
//     before serving (INV-7f); on mismatch recheck under the per-mem write
//     mutex — an in-flight ②→③ writer heals itself and IS served — and only
//     a surviving mismatch (crash residue or an out-of-band edit) runs the
//     single-entry reconcile before the entry is served again.

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// isNotFound reports whether err is the MEM-404 that s.load returns for a
// missing row. load wraps sql.ErrNoRows into an *APIError, so errors.Is
// against the bare driver error never matches — every caller that needs
// the "row absent" branch must test the wrapper's code instead.
func isNotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Code == "MEM-404"
}

// ---------- authority document parsing (non-create paths) ----------

// parseAuthorityDoc parses an existing authority document read from disk.
// 06 §2.2: non-create paths must find a self-identifying frontmatter whose
// id equals the directory name; anything else is refused, never silently
// repaired into a row. dirMemID is the mem directory name ("" skips the
// directory-identity check).
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
	// NOT NULL columns (02 §4.9) must all be derivable from the file
	// (06 §5.3 step 3 branch 1): an invalid frontmatter never produces an
	// illegal row. project_id is REQUIRED here (06 §4.2: it is a 必填
	// frontmatter field and the authority for the row's project binding —
	// the directory only fixes the file location, never the binding).
	if fm.Type == "" || !visibilities[fm.Visibility] || fm.OwnerUserID == "" ||
		fm.ProjectID == "" || fm.CreatedAt == "" || fm.UpdatedAt == "" {
		return nil, "", errf("MEM-400", "FRONTMATTER_INVALID", "missing/invalid required frontmatter fields")
	}
	return &fm, body, nil
}

// rowMatchesFrontmatter compares the §5.3 step-3 metadata set of a row
// against the parsed frontmatter (nil and empty slices are equal: the
// machine-written file omits empty lists, the row stores "[]").
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

// ---------- INV-7f read-path gate ----------

// entryConsistent is the live INV-7f check: the active authority file
// exists and its digest equals the row's content_sha. A missing active
// memory.md IS the inconsistency — no cached column is ever trusted.
func (s *Store) entryConsistent(e *Entry) bool {
	raw, err := os.ReadFile(filepath.Join(s.dataDir, filepath.FromSlash(e.apath), "memory.md"))
	if err != nil {
		return false
	}
	return shaHex(raw) == e.ContentSHA
}

// validated is the serving gate for every single-entry read path
// (Get/Content/HTML, 06 §2.3 INV-7f, §6 execution detail). The whole gate
// — row load, live digest check and any repair — runs INSIDE the entry's
// per-mem write mutex, and the byte-reading callers (Content, HTML) keep
// the same lock hold across their file reads, so the row that was
// validated and the bytes that are served are always the same committed
// state. The earlier shape ran the digest check BEFORE acquiring the
// lock and served bytes captured pre-lock: a concurrent PATCH landing
// between the check and the read served a row/bytes pair that had never
// been validated together (TOCTOU vs concurrent writes and out-of-band
// edits, 06 §6). Holding the lock from the start also subsumes the §6
// mismatch-recheck: no writer can sit in its ②→③ window while the mutex
// is held, so a surviving mismatch is genuine crash residue or an
// out-of-band edit and runs the single-entry reconcile (same logic as
// §5.3) before the entry is served. A redacted (terminal) entry is not
// served — the active-path file is gone by design and 06 §5.2 pins
// detail reads of that state to MEM-404.
func (s *Store) validated(memID string) (*Entry, error) {
	unlock := s.memLock(memID)
	defer unlock()
	return s.validatedLocked(memID)
}

// validatedLocked is validated for a CALLER that already holds the
// entry's per-mem write mutex (Content and HTML keep the hold across
// their byte reads; the reconcile passes hold it across their per-entry
// work). No writer can be in flight, so the gate is check → repair →
// re-serve with no in-flight recheck needed.
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
		// reconcile deleted an orphan row: the entry never existed
		// as far as serving is concerned (06 §6).
		return nil, errf("MEM-404", "", "memory %s", memID)
	}
	if fresh.Status != "active" || !s.entryConsistent(fresh) {
		return nil, errf("MEM-404", "", "memory %s", memID)
	}
	return fresh, nil
}

// serveConsistent is the INV-7f gate for the list-shaped read paths
// (Search candidates, List items, 06 §6). The consistency check runs
// INSIDE the entry's per-mem write mutex — not before it: the passed-in
// candidate row was loaded pre-lock, so it is judged against the live
// file under the lock, and a row reloaded there is what gets served when
// the candidate merely went stale while queueing for the mutex. An
// inconsistent entry is excluded from THIS result set, and a best-effort
// single-entry reconcile is dispatched (synchronously here — v1 has no
// background queue) so the entry returns on the next call.
func (s *Store) serveConsistent(e *Entry) (*Entry, bool) {
	unlock := s.memLock(e.ID)
	defer unlock()
	if e.Status == "active" && s.entryConsistent(e) {
		return e, true
	}
	// The pre-lock candidate is not servable as-is. Reload under the
	// lock: either the candidate went stale (a writer completed ③ while
	// we queued) — the fresh row is served — or the entry is genuine
	// crash residue / out-of-band drift and is excluded.
	fresh, err := s.load(e.ID)
	if err == nil && fresh.Status == "active" && s.entryConsistent(fresh) {
		return fresh, true
	}
	_ = s.reconcileOne(e.ID) // best-effort: the entry is excluded regardless
	return nil, false
}

// ---------- single-entry reconcile (06 §6, same logic as §5.3) ----------

// reconcileOne brings ONE entry back to a servable state using the §5.3
// per-entry branches. The CALLER holds the mem_id write mutex (06 §6: the
// read-path repair must recheck under the lock so an in-flight write is
// never mistaken for residue). Everything is re-read inside: the state may
// have changed while waiting for the mutex.
func (s *Store) reconcileOne(memID string) error {
	e, err := s.load(memID)
	if err != nil {
		if isNotFound(err) {
			return nil // row already gone: nothing to reconcile
		}
		return err
	}
	if e.Status != "active" {
		return nil // redacted rows are terminal; nothing servable to fix
	}
	dir := filepath.Join(s.dataDir, filepath.FromSlash(e.apath))
	raw, rerr := os.ReadFile(filepath.Join(dir, "memory.md"))
	if rerr == nil {
		// File present, row drifted: the file wins (INV-7c; §5.3 step 3
		// branch "row exists but content mismatches" — for the B5 ②→③
		// crash window this is the whole-operation rollback).
		fm, body, perr := parseAuthorityDoc(raw, memID)
		if perr != nil {
			return perr // an invalid file is never turned into a row
		}
		sha := shaHex(raw)
		if sha == e.ContentSHA && rowMatchesFrontmatter(e, fm) {
			return nil // a writer completed ③ while we waited
		}
		shaChanged := sha != e.ContentSHA
		if uerr := s.updateRowFromFile(e, fm, raw, sha); uerr != nil {
			return uerr
		}
		s.sweepDirTransients(dir) // tmp residue of the rolled-back write
		if shaChanged {
			// §5.3 step 3: force a re-derivation, never trusting the old
			// source_markdown_sha (INV-7d). deriveHTML writes memory.html
			// inside the authority area → barrier read lock.
			BackupBarrier.RLock()
			_, derr := s.deriveHTML(e, raw, body)
			BackupBarrier.RUnlock()
			if derr != nil {
				// MEM-501-class: authority + metadata are committed; the
				// next format=html read re-derives (06 §4.3).
				_ = derr
			}
		}
		return nil
	}
	// Active file missing: a redact that crashed between ① and ② is
	// forward-completed from the file's position (06 §5.2); anything else
	// is an orphan row (create ②→③ crash) and dies with its FTS row. The
	// _redacted probe is derived from the recorded placement
	// (redactedRelOf), never from project_id: a cross-bound entry lives
	// in the tree its authority_path names.
	if _, serr := os.Stat(filepath.Join(s.dataDir, filepath.FromSlash(redactedRelOf(e)), "memory.md")); serr == nil {
		return s.forwardCompleteRedact(e)
	}
	s.sweepDirTransients(dir)
	return s.deleteOrphanRow(memID)
}

// reconcileEntry wraps reconcileOne with the per-mem mutex and is the form
// the full §5.3 pass uses (06 §6: every reconcile correction happens under
// the lock, with a recheck).
func (s *Store) reconcileEntry(memID string) error {
	unlock := s.memLock(memID)
	defer unlock()
	return s.reconcileOne(memID)
}

// updateRowFromFile rewrites the §5.3 step-3 metadata set of the row from
// the parsed file (content_sha/content_len/title/tags/related/visibility/
// updated_at — the file is the authority) and rebuilds the entry's FTS row
// in the same transaction.
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
// crashed before committing the metadata (06 §5.2/§5.3 step 5): the file
// location IS the authoritative state, so the row is corrected to
// 'redacted' and the FTS row removed — forward completion, never a
// rollback that would widen the half-committed window.
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

// deleteOrphanRow removes a row whose authority file exists nowhere (the
// create ②→③ crash window: metadata committed, file never placed) together
// with its FTS row — the whole create rolls back (06 §5.3 step 6).
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

// ---------- transient sweep (06 §5.3 step 1) ----------

// sweepDirTransients removes the write-order transients of one entry
// directory. Callers hold the per-mem mutex (or know no writer can be in
// flight), so anything still present is crash residue.
func (s *Store) sweepDirTransients(dir string) {
	for _, name := range []string{"memory.md.tmp", "memory.html.tmp"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			_ = err // best-effort: residue is inert, the next sweep retries
		}
	}
	s.sweepOrphanHistory(dir)
}

// sweepTransients removes every write-order transient under the memory
// tree — tmp files AND the orphan history snapshots of crashed updates
// (06 §5.3 step 1). A tmp file can only outlive its writer when the
// writer died before ③; each removal happens under the directory's mem_id
// mutex with an existence recheck, so a live writer holding its ①→③ window
// is waited out instead of swept.
func (s *Store) sweepTransients() error {
	root := filepath.Join(s.dataDir, "memory")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") {
			unlock := s.memLock(filepath.Base(filepath.Dir(path)))
			defer unlock()
			if _, serr := os.Stat(path); serr != nil {
				return nil // the live writer finished ③ while we waited
			}
			if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
				return rerr
			}
			return nil
		}
		// Orphan history snapshot (§5.3 step 1): judged under the entry's
		// mem_id mutex like any other residue, with the same existence
		// recheck that waits out a live update writer.
		if strings.HasSuffix(d.Name(), ".md") && filepath.Base(filepath.Dir(path)) == "history" {
			memDir := filepath.Dir(filepath.Dir(path))
			unlock := s.memLock(filepath.Base(memDir))
			defer unlock()
			if _, serr := os.Stat(path); serr != nil {
				return nil // a live writer holds the window: not residue
			}
			s.sweepOrphanHistory(memDir)
		}
		return nil
	})
}

// sweepOrphanHistory removes the crash-residue history snapshots of one
// entry directory (06 §5.3 step 1: “清扫崩溃残留的孤儿 history/*.md 快照（不参与
// 任何判定）”). Inference (recorded): a snapshot is orphan residue exactly
// when its bytes equal the CURRENT authority file — the update that took
// it died before ③, so the outgoing version it captured is still the
// surviving authority and the copy is redundant; a snapshot of a
// genuinely superseded version differs from the current authority and
// survives. Either way snapshots never participate in any consistency
// judgment. Callers hold the per-mem mutex (or know no writer is in
// flight); removal is best-effort like the tmp sweep.
func (s *Store) sweepOrphanHistory(dir string) {
	auth, err := os.ReadFile(filepath.Join(dir, "memory.md"))
	if err != nil {
		return // no authority here: nothing to judge snapshots against
	}
	names, gerr := filepath.Glob(filepath.Join(dir, "history", "*.md"))
	if gerr != nil || len(names) == 0 {
		return
	}
	sha := shaHex(auth)
	for _, name := range names {
		if b, rerr := os.ReadFile(name); rerr == nil && shaHex(b) == sha {
			_ = os.Remove(name)
		}
	}
}

// ---------- full §5.3 rebuild/reconcile ----------

// RebuildIndex runs the full 06 §5.3 procedure. It supersedes the bare
// FTS rebuild: every numbered step runs on each call, so the same method
// serves the admin endpoint, the startup reconcile and the scheduled task.
// The return value is the number of reindexed active entries (06 §5.3
// step 7 rebuild statistics).
//
// Lock discipline (06 §6): every per-entry action of the file-driven
// passes — snapshot (file read + frontmatter parse), the step-3 metadata
// branch and the step-4 unconditional FTS rewrite — happens inside the
// entry's per-mem write mutex, in ONE lock hold per entry. The earlier
// shape cached the walked bytes and rewrote FTS from them in a later
// batch transaction; a PATCH/redact committing between the walk and that
// rewrite left the row's content_sha and the FTS body permanently
// disagreeing (INV-7c). Holding the memLock across snapshot→rewrite makes
// the interleaving impossible: a concurrent writer either finishes before
// the lock is taken (snapshot sees the new state) or waits until the
// entry's whole rebuild section is done.
func (s *Store) RebuildIndex() (int, error) {
	// Step 1 (B5): sweep write-order transients — they can only be ②-side
	// crash residue; the authority path was never touched by them.
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

	// Steps 2–4, one locked section per entry (see the lock-discipline note
	// above).n counts the entries whose FTS row was rebuilt.
	n := 0
	for _, pd := range projDirs {
		if !pd.IsDir() {
			continue
		}
		dirProject := pd.Name()
		memDirs, werr := os.ReadDir(filepath.Join(s.dataDir, "memory", dirProject))
		if werr != nil {
			return 0, errf("MEM-502", "", "walk: %v", werr)
		}
		for _, md := range memDirs {
			if !md.IsDir() || md.Name() == "_redacted" {
				continue
			}
			counted, cerr := s.rebuildActiveEntry(dirProject, md.Name())
			if cerr != nil {
				return 0, errf("MEM-502", "", "rebuild %s: %v", md.Name(), cerr)
			}
			if counted {
				n++
			}
		}
	}

	// Step 5: _redacted pass — migrated redacted stock is backfilled (no
	// FTS); a redact that crashed between ① and ② is forward-completed.
	// Same per-entry lock discipline as the active pass.
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

	// Step 6 (B5): row-driven scan — active rows whose authority file is
	// missing at the active path: in _redacted/ → forward-complete;
	// nowhere → the orphan row (create ②→③ crash) dies with its FTS row.
	// reconcileEntry takes the per-mem mutex and rechecks inside, so a
	// live writer is waited out rather than mistaken for residue.
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
		if cerr := s.reconcileEntry(id); cerr != nil {
			if ae, ok := cerr.(*APIError); ok && ae.Code == "MEM-400" {
				continue // authority file unparseable: no §5.3 branch can act
			}
			return 0, errf("MEM-502", "", "reconcile %s: %v", id, cerr)
		}
	}

	// Dangling FTS rows (index entries whose backing memories row is gone)
	// are dropped in one final transaction, taken with NO per-entry lock
	// held — never a memLock while a transaction is open, the same
	// single-writer ordering every other path obeys.
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

// rebuildActiveEntry runs the §5.3 steps 2–4 for ONE active-tree entry,
// entirely inside the entry's per-mem write mutex (06 §6). counted
// reports whether the step-4 FTS rebuild ran for this entry (the §5.3
// step-7 statistic).
func (s *Store) rebuildActiveEntry(dirProject, memID string) (counted bool, err error) {
	unlock := s.memLock(memID)
	defer unlock()

	// Steps 2–3: snapshot the authority file and parse its frontmatter —
	// UNDER the lock, so the bytes indexed below are the bytes judged.
	raw, rerr := os.ReadFile(filepath.Join(s.dataDir, "memory", dirProject, memID, "memory.md"))
	if rerr != nil {
		return false, nil // no authority file here: the row-driven scan decides the row's fate
	}
	fm, _, perr := parseAuthorityDoc(raw, memID)
	if perr != nil {
		// 06 §5.3: an invalid frontmatter never becomes an illegal row. The
		// file is skipped; an existing row is left untouched (there is no
		// branch that could act on it).
		return false, nil
	}
	row, lerr := s.load(memID)
	if isNotFound(lerr) {
		// File without row (manual placement/import, 06 §10.1): the row is
		// backfilled entirely from the frontmatter — but only if the
		// frontmatter's bindings can satisfy the 02 §4.9 DDL constraints;
		// otherwise the file is skipped like any other invalid frontmatter
		// (one bad planted file must never abort the whole rebuild).
		if !s.bindingsExist(fm) {
			return false, nil
		}
		if berr := s.backfillRow(fm, raw, dirProject, memID, "active"); berr != nil {
			return false, berr
		}
	} else if lerr != nil {
		return false, lerr
	} else if row.Status == "redacted" {
		// Inference (recorded): an active-tree placement whose row is
		// terminal-redacted has no §5.3 branch. Redact is terminal
		// (06 §5.4 MEM-409 终态不可写), so the file must NOT resurrect
		// the row; it is ignored and never indexed.
		return false, nil
	} else if row.ContentSHA != shaHex(raw) || !rowMatchesFrontmatter(row, fm) {
		// Row drifted from the file (B5 ②→③ crash window or an
		// out-of-band edit — including direct row edits, the
		// "row columns vs written frontmatter" backfill direction):
		// the file wins. The lock is already held, so reconcileOne runs
		// inline with its recheck-on-entry semantics (06 §6).
		if cerr := s.reconcileOne(memID); cerr != nil {
			if ae, ok := cerr.(*APIError); ok && ae.Code == "MEM-400" {
				return false, nil // file went invalid mid-rebuild: do not index it
			}
			return false, cerr
		}
	}

	// Step 4: unconditional FTS rebuild from the SAME locked snapshot —
	// deliberately NOT gated on which branch step 3 took, so a pure index
	// loss (Markdown and row fully consistent, FTS corrupt/missing) is
	// repaired even when no metadata needed touching (06 §9 仅FTS丢失
	// 专项). One short per-entry transaction, opened while only this
	// entry's memLock is held.
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

// rebuildRedactedEntry runs the §5.3 step-5 branch for ONE _redacted/
// entry, inside the entry's per-mem write mutex: migrated redacted stock
// is backfilled (never indexed); a row still active while the directory
// already sits in _redacted/ is forward-completed from the file position
// (a redact that crashed between ① and ②, 06 §5.2).
func (s *Store) rebuildRedactedEntry(dirProject, memID string) error {
	unlock := s.memLock(memID)
	defer unlock()

	raw, rerr := os.ReadFile(filepath.Join(s.dataDir, "memory", dirProject, "_redacted", memID, "memory.md"))
	if rerr != nil {
		return nil // no authority here: nothing to backfill or correct
	}
	fm, _, perr := parseAuthorityDoc(raw, memID)
	if perr != nil {
		return nil // invalid frontmatter: never an illegal row
	}
	row, lerr := s.load(memID)
	if isNotFound(lerr) {
		// Migrated redacted stock: same §4.2/§5.3 frontmatter-authority
		// and DDL-constraint rules as the active backfill (bindings must
		// reference real rows, or the file is skipped, not inserted).
		if !s.bindingsExist(fm) {
			return nil
		}
		return s.backfillRow(fm, raw, dirProject, memID, "redacted")
	}
	if lerr != nil {
		return lerr
	}
	if row.Status != "redacted" {
		// File already in _redacted/ ⇒ the redact is authoritative; the
		// lagging Core row is corrected (forward completion). The lock is
		// already held: reconcileOne runs inline with its recheck.
		return s.reconcileOne(memID)
	}
	return nil
}

// bindingsExist verifies that a frontmatter's row bindings can satisfy
// the 02 §4.9 DDL constraints before a §5.3 backfill inserts a row:
// memories.project_id and memories.owner_user_id are FOREIGN KEY columns
// (foreign_keys=ON), so the frontmatter's project (06 §4.2: the AUTHORITY
// for the project_id column — the walk directory fixes only the file
// location) and owner must name real rows. A planted file whose bindings
// dangle is treated exactly like any other invalid frontmatter: it is
// skipped — never an illegal row, and never a reason to abort the whole
// rebuild with MEM-502.
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

// backfillRow inserts a memories row for a file whose mem_id has none
// (06 §5.3 step 3 branch 1 / step 5): the FULL frontmatter→DDL mapping,
// status from the caller ('active' from the active tree, 'redacted' from
// _redacted/), content_sha computed live, digest pair NULL (the lazy
// §4.3 read-time derivation handles the html).
//
// Binding vs placement (06 §4.2/§5.3): the row's project_id column is
// read from the FRONTMATTER — frontmatter is the authority for the
// metadata columns, and §5.3 step 3 branch 1 names project_id among the
// fields “均从 frontmatter 读出” — while authority_path records where the
// file actually IS (dirProject, the directory the walk found it in), so
// the directory remains the file-location authority and the two can
// legitimately differ for a hand-planted file. Callers hold the entry's
// per-mem mutex, have validated the frontmatter (parseAuthorityDoc, incl.
// the required project_id) and checked bindingsExist.
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
	return tx.Commit()
}
