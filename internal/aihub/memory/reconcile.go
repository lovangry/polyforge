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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/silgrid/polyforge/internal/common/api"
)

// isNotFound reports the MEM-404 of s.load.
func isNotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Code == api.ErrMemNotFound
}

// parseAuthorityDoc parses an existing authority document: non-create
// paths must find a self-identifying frontmatter whose id equals the
// directory name; anything else is refused, never silently repaired.
func parseAuthorityDoc(raw []byte, dirMemID string) (*frontmatter, string, error) {
	block, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return nil, "", errf(api.ErrMemBadInput, "FRONTMATTER_INVALID", "no frontmatter block")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return nil, "", errf(api.ErrMemBadInput, "FRONTMATTER_INVALID", "%v", err)
	}
	if fm.ID == "" {
		return nil, "", errf(api.ErrMemBadInput, "FRONTMATTER_INVALID", "frontmatter id missing")
	}
	if dirMemID != "" && fm.ID != dirMemID {
		return nil, "", errf(api.ErrMemBadInput, "FRONTMATTER_ID_MISMATCH", "frontmatter id %q != dir %q", fm.ID, dirMemID)
	}
	if fm.Type == "" || !visibilities[fm.Visibility] || fm.OwnerUserID == "" ||
		fm.ProjectID == "" || fm.CreatedAt == "" || fm.UpdatedAt == "" {
		return nil, "", errf(api.ErrMemBadInput, "FRONTMATTER_INVALID", "missing/invalid required frontmatter fields")
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
		return nil, errf(api.ErrMemNotFound, "", "memory %s", memID)
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
		return nil, errf(api.ErrMemNotFound, "", "memory %s", memID)
	}
	if fresh.Status != "active" || !s.entryConsistent(fresh) {
		return nil, errf(api.ErrMemNotFound, "", "memory %s", memID)
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
	fpath := filepath.Join(dir, "memory.md")
	// Stat BEFORE read (both under the caller's per-mem lock; polyforge#129
	// ordering rule): a stamp taken before the body it describes can only
	// be stale — the next pass re-verifies — never wrongly fresh.
	st, _ := os.Stat(fpath)
	raw, rerr := os.ReadFile(fpath)
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
		if uerr := s.updateRowFromFile(e, fm, raw, sha, st); uerr != nil {
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
// rebuilds the FTS row in the same transaction. polyforge#129: the row's
// freshness stamp is recorded with it — this IS a row write from this
// exact file state, so the next pass can skip the entry on the stamp
// alone. st may be nil (the stat failed): the row is then written
// unstamped and the next pass re-verifies it.
func (s *Store) updateRowFromFile(e *Entry, fm *frontmatter, raw []byte, sha string, st os.FileInfo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var fsM, fsS any
	if st != nil {
		fsM, fsS = st.ModTime().UnixNano(), st.Size()
	}
	if _, err := tx.Exec(`UPDATE memories SET content_sha=?1, content_len=?2, title=?3, tags=?4,
		related=?5, visibility=?6, updated_at=?7, fs_mtime_ns=?8, fs_size=?9 WHERE id=?10`,
		sha, len(raw), anyPtr(fm.Title), jsonOrEmpty(fm.Tags), jsonOrEmpty(fm.Related),
		fm.Visibility, fm.UpdatedAt, fsM, fsS, e.ID); err != nil {
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

// ftsRebuildBatch caps how many FTS rows one rebuild transaction
// carries. Every row is a DELETE+INSERT inside a shared transaction: at
// real-library scale (thousands of entries) the old per-entry transaction
// made the rebuild one fsync PER ENTRY and blew past any sane client
// timeout (seq-fix lane 3a: the 3760-entry rebuild exceeded the CLI's
// 60s). The batch holds Core's ONLY connection for its full duration, so
// a request arriving mid-batch — read or write — queues until it
// commits: the cap is deliberately small (polyforge#129 ruling b① — a
// concurrent request may wait a few hundred milliseconds, never the
// batch count × per-entry cost) and ftsRebuildHold bounds the hold in
// wall time besides.
const ftsRebuildBatch = 32

// ftsRebuildHold is the wall-time budget of one batch transaction of the
// FTS rebuild (polyforge#129 ruling b①): as soon as the open batch has
// held Core's only connection this long it commits and yields. A row
// count cap alone cannot bound the hold — per-entry index cost grows
// with the corpus (~15ms/entry locally, ~0.1s/entry at production
// scale), so a large batch would still mean seconds-to-minutes of
// queueing. With the budget, a concurrent request's worst wait is
// roughly ftsRebuildHold plus one entry's write, independent of corpus
// size.
const ftsRebuildHold = 250 * time.Millisecond

// ftsRebuildYield is the pause between two batch transactions of the FTS
// rebuild — the writer yield (polyforge#129): each batch holds Core's
// only connection for its full duration, and the startup reconcile now
// runs concurrently with serving traffic, so the yield lets queued API
// writers commit between batches instead of waiting out the whole pass.
const ftsRebuildYield = 2 * time.Millisecond

// progressEvery is how many scanned entries lie between two progress
// reports of an O(library) reconcile pass (polyforge#129: a long pass —
// first boot after the upgrade, index repair — must be
// distinguishable from a hang). The batched index-write phase reports on
// top of this, per batch rotation; see rebuildFTSBatched.
const progressEvery = 256

// ProgressReport is one reconcile progress observation.
type ProgressReport struct {
	Scanned   int           // authority entries examined so far
	Reindexed int           // entries brought current so far (see RebuildIndex)
	Elapsed   time.Duration // time since the pass began
}

// rebuildOutcome is what the file-driven pass decided for one active-tree
// entry.
type rebuildOutcome int

const (
	rebuildUnresolved rebuildOutcome = iota // the walk could not decide: the row-driven scan owns it
	rebuildSkip                             // nothing to do (fresh stamp, terminal redacted, unplaceable file)
	rebuildRefresh                          // drift observed, content verified equal: stamp refreshed, counted
	rebuildQueue                            // index write queued for the batched pass, counted
	rebuildIndexed                          // row corrected from the file, index rewritten inline, counted
)

// RebuildIndex runs the reconcile: transient sweep, file-driven
// backfill/correction, _redacted forward-completion, row-driven orphan
// rollback and the batched index writes.
//
// Incremental by freshness stamp (polyforge#129): every memories row
// carries the mtime+size of the authority file it was written from, and
// the pass consults that stamp before ANY per-entry work — a match skips
// the entry entirely (no read, no hash, no index write); a mismatch, or a
// missing stamp (pre-#129 rows, backfills awaiting their batched index
// write), is verified by content sha before anything acts. A pre-#129
// database therefore degrades to one full pass — every row verified and
// stamped, every FTS row rewritten once — and is incremental from the
// next pass on.
//
// The returned count is the entries the pass brought current: index rows
// rewritten (inline or batched) plus stamp-only refreshes after observed
// drift. A steady-state (zero-change) pass answers 0 — the old full-
// rebuild count was structurally the whole library and carried no
// information.
func (s *Store) RebuildIndex() (int, error) { return s.rebuildIndex() }

// rebuildIndex is the reconcile body (every project).
func (s *Store) rebuildIndex() (int, error) {
	start := time.Now()
	scanned := 0
	counted := 0
	report := func() {
		if s.ReconcileProgress != nil {
			s.ReconcileProgress(ProgressReport{Scanned: scanned, Reindexed: counted, Elapsed: time.Since(start)})
		}
	}
	if err := s.sweepTransients(); err != nil {
		return 0, errf(api.ErrMemUpstream, "", "sweep: %v", err)
	}
	var projDirs []string
	all, err := os.ReadDir(filepath.Join(s.dataDir, "memory"))
	if err != nil && !os.IsNotExist(err) {
		return 0, errf(api.ErrMemUpstream, "", "walk: %v", err)
	}
	for _, d := range all {
		if d.IsDir() {
			projDirs = append(projDirs, d.Name())
		}
	}
	// Active tree: each entry is decided from its freshness stamp
	// (polyforge#129) — stamp match ⇒ skip (no read, no hash, no index
	// write); mismatch ⇒ verify by content and act only on the drifted
	// entries. Only entries still needing an index write are queued for
	// the batched pass afterwards.
	var queue []rebuildEntry
	resolved := make(map[string]bool)
	for _, dirProject := range projDirs {
		memDirs, werr := os.ReadDir(filepath.Join(s.dataDir, "memory", dirProject))
		if werr != nil {
			return 0, errf(api.ErrMemUpstream, "", "walk: %v", werr)
		}
		for _, md := range memDirs {
			if !md.IsDir() || md.Name() == "_redacted" {
				continue
			}
			scanned++
			out, cerr := s.rebuildActiveEntry(dirProject, md.Name())
			if cerr != nil {
				return 0, errf(api.ErrMemUpstream, "", "rebuild %s: %v", md.Name(), cerr)
			}
			switch out {
			case rebuildQueue:
				queue = append(queue, rebuildEntry{id: md.Name(), dirProject: dirProject})
			case rebuildIndexed, rebuildRefresh:
				counted++
			}
			if out != rebuildUnresolved {
				resolved[md.Name()] = true
			}
			if scanned%progressEvery == 0 {
				report()
			}
		}
	}
	// _redacted tree: backfill missing rows; forward-complete crashed
	// redacts.
	for _, dirProject := range projDirs {
		redDirs, rerr := os.ReadDir(filepath.Join(s.dataDir, "memory", dirProject, "_redacted"))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue
			}
			return 0, errf(api.ErrMemUpstream, "", "walk: %v", rerr)
		}
		for _, md := range redDirs {
			if !md.IsDir() {
				continue
			}
			if cerr := s.rebuildRedactedEntry(dirProject, md.Name()); cerr != nil {
				return 0, errf(api.ErrMemUpstream, "", "rebuild %s: %v", md.Name(), cerr)
			}
		}
	}
	// Row-driven scan, folded into the stamped walk (polyforge#129 scope
	// guidance): the walk resolved every entry it could decide; only the
	// remainder — active rows whose authority dir/file is missing,
	// unreadable or unparseable — is checked here, so the healthy
	// majority no longer pays a per-row load + full file read + sha on
	// every pass.
	rows, err := s.db.Query(`SELECT id FROM memories WHERE status='active'`)
	if err != nil {
		return 0, errf(api.ErrMemUpstream, "", "scan rows: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, errf(api.ErrMemUpstream, "", "scan rows: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if resolved[id] {
			continue // the stamped walk decided this entry already
		}
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
			if ae, ok := cerr.(*APIError); ok && ae.Code == api.ErrMemBadInput {
				continue // unparseable authority file: no branch can act
			}
			return 0, errf(api.ErrMemUpstream, "", "reconcile %s: %v", id, cerr)
		}
		unlock()
	}
	// The batched index writes — only for the entries this pass queued
	// (backfills, unstamped rows); every other entry was either skipped
	// on its stamp or already written inline by its correction. The phase
	// reports progress per batch rotation (folded into Reindexed): on a
	// full-rebuild pass — first boot after the upgrade, index repair —
	// this phase is the bulk of the wall time, and the
	// walk's reports stop once the walk is done, so it must not go
	// silent (polyforge#129 ruling c).
	n, err := s.rebuildFTSBatched(queue, func(written int) {
		if s.ReconcileProgress != nil {
			s.ReconcileProgress(ProgressReport{
				Scanned:   scanned,
				Reindexed: counted + written,
				Elapsed:   time.Since(start),
			})
		}
	})
	if err != nil {
		return 0, errf(api.ErrMemUpstream, "", "fts: %v", err)
	}
	counted += n
	// Dangling FTS rows die last.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id NOT IN (SELECT id FROM memories)`); err != nil {
		return 0, errf(api.ErrMemUpstream, "", "fts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, errf(api.ErrMemUpstream, "", "commit: %v", err)
	}
	report() // final report: totals for the whole pass
	return counted, nil
}

// rebuildEntry is one active-tree entry queued for the batched FTS
// rebuild.
type rebuildEntry struct{ id, dirProject string }

// rebuildFTSBatched rewrites the FTS row of every given active entry,
// ftsRebuildBatch rows per transaction, and records the entry's freshness
// stamp with it (polyforge#129: for a backfilled row the stamp lands with
// the index write, so a crash before this transaction leaves the row
// unstamped — the next pass re-verifies and re-queues it; for an unstamped
// pre-#129 row this transaction is the one-time full-pass write that
// makes every later pass incremental). Each body is stat'ed then read
// under the entry's per-mem lock immediately before its write — never a
// stale pre-read — and an entry whose authority vanished mid-rebuild is
// skipped (the row-driven scan owns that case). The lock is taken with
// tryMemLock and a deferral to the next round instead of blocking: the
// lock holder needs Core's only connection for its own write, so
// blocking on a per-mem lock while holding an open write transaction is
// a deadlock shape now that the startup reconcile runs concurrently with
// serving traffic. No store call runs inside the open batch transaction:
// Core's pool is one connection, and a nested write there would deadlock
// it. Between batches the pass sleeps ftsRebuildYield so queued writers
// can commit.
//
// polyforge#129 rewind (owner rulings b① + c): a batch transaction holds
// Core's ONLY connection for its full duration, so it commits as soon as
// EITHER ftsRebuildBatch rows or ftsRebuildHold of wall time are in —
// whichever fires first — bounding a concurrent request's worst wait to
// roughly the hold plus one entry instead of the whole batch. After
// every commit progress fires (called with the entries written so far)
// so this phase — the bulk of a full-rebuild pass — keeps emitting
// scanned/reindexed/elapsed instead of going silent for minutes. A
// rotation in which every entry was lock-deferred sleeps before the next
// round instead of spinning on empty transactions.
func (s *Store) rebuildFTSBatched(entries []rebuildEntry, progress func(written int)) (int, error) {
	n := 0
	pending := entries
	for len(pending) > 0 {
		var deferred []rebuildEntry
		tx, err := s.db.Begin()
		if err != nil {
			return 0, err
		}
		batchStart := time.Now() // the open transaction's hold clock
		rotWrote, batchWrote := 0, 0
		// fail rolls back the CURRENT batch transaction (the closure captures
		// the variable, so it stays correct across batch rotations).
		fail := func(e error) (int, error) {
			_ = tx.Rollback()
			return 0, e
		}
		// closeBatch commits the open batch transaction and reports the
		// rotation: the commit hands Core's only connection back to the
		// pool (queued requests — reads and writes — run here), progress
		// fires for a batch that actually wrote something, and the yield
		// gives waiting writers their turn before the caller re-opens the
		// next batch.
		closeBatch := func() error {
			if err := tx.Commit(); err != nil {
				return err
			}
			if progress != nil && batchWrote > 0 {
				progress(n)
			}
			time.Sleep(ftsRebuildYield)
			return nil
		}
		for _, e := range pending {
			unlock, ok := s.tryMemLock(e.id)
			if !ok {
				deferred = append(deferred, e) // a writer is mid-flight on this entry
				continue
			}
			path := filepath.Join(s.dataDir, "memory", e.dirProject, e.id, "memory.md")
			st, serr := os.Stat(path)
			raw, rerr := os.ReadFile(path)
			unlock()
			if rerr != nil || serr != nil {
				continue // vanished mid-rebuild: the row-driven scan owns it
			}
			if err := ftsReplace(tx, e.id, extractBody(string(raw))); err != nil {
				return fail(err)
			}
			if _, err := tx.Exec(`UPDATE memories SET fs_mtime_ns=?1, fs_size=?2 WHERE id=?3`,
				st.ModTime().UnixNano(), st.Size(), e.id); err != nil {
				return fail(err)
			}
			n++
			rotWrote++
			batchWrote++
			// Batch boundary: row count OR hold budget, whichever fires
			// first (polyforge#129 ruling b① — the open transaction is
			// Core's only connection; every concurrent request queues
			// behind it until this commit).
			if n%ftsRebuildBatch == 0 || time.Since(batchStart) >= ftsRebuildHold {
				if cerr := closeBatch(); cerr != nil {
					return fail(cerr)
				}
				tx, err = s.db.Begin()
				if err != nil {
					return 0, err
				}
				batchStart = time.Now()
				batchWrote = 0
			}
		}
		if err := closeBatch(); err != nil {
			return fail(err)
		}
		if rotWrote == 0 && len(deferred) > 0 {
			// Every entry of this rotation was lock-deferred: without a
			// pause the pass would spin on empty transactions between two
			// reports while a writer holds the per-mem lock.
			time.Sleep(ftsRebuildYield)
		}
		pending = deferred
	}
	return n, nil
}

// stampMatches reports whether the row's freshness stamp equals the
// authority file's current observed state (polyforge#129). A match is
// the reconcile's skip condition: the row and its FTS row were written
// from exactly this file state in one transaction, so no per-entry work
// can change anything. A coarse-mtime filesystem could in principle hide
// a same-tick, same-size out-of-band modification — the INV-7f serving
// gate (sha-verified reads) stays the correctness net for serving, and a
// later out-of-band change breaks the stamp again.
func stampMatches(e *Entry, st os.FileInfo) bool {
	if e.fsStamp == nil {
		return false
	}
	return e.fsStamp.mtimeNS == st.ModTime().UnixNano() && e.fsStamp.size == st.Size()
}

// stampRow records a fresh freshness stamp for one row whose content was
// just verified equal to the file (a touched, unmodified file). No index
// write: the FTS row already matches the verified content.
func (s *Store) stampRow(memID string, st os.FileInfo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET fs_mtime_ns=?1, fs_size=?2 WHERE id=?3`,
		st.ModTime().UnixNano(), st.Size(), memID); err != nil {
		return err
	}
	return tx.Commit()
}

// rebuildActiveEntry runs the file-driven pass for one active-tree entry
// inside its per-mem mutex. It decides from the row's freshness stamp
// (polyforge#129):
//
//   - stamp match: the row (and its FTS row, written in the same
//     transaction) was written from exactly this file state — the entry
//     is skipped with no read, no hash and no index write;
//   - stamp mismatch or missing stamp: the file is read and verified by
//     content — a sha/metadata difference corrects the row from the file
//     (file wins, INV-7c; updateRowFromFile rewrites row, stamp and FTS
//     row in one transaction), verified-equal content only refreshes the
//     stamp (a touched, unmodified file), except for an unstamped row
//     (pre-#129 database), whose one-time full index write is queued for
//     the batched pass;
//   - missing row: the file backfills one (its index write is queued for
//     the batched pass too).
//
// Unparseable files never become illegal rows; unresolved entries
// (missing/unreadable/unparseable authority) are left to the caller's
// row-driven scan.
func (s *Store) rebuildActiveEntry(dirProject, memID string) (rebuildOutcome, error) {
	unlock := s.memLock(memID)
	defer unlock()
	path := filepath.Join(s.dataDir, "memory", dirProject, memID, "memory.md")
	// Stat BEFORE read (the ordering rule from reconcileOne): a stamp
	// taken before the body it describes can only be stale, never
	// wrongly fresh.
	st, serr := os.Stat(path)
	if serr != nil {
		return rebuildUnresolved, nil // no authority here: the row-driven scan decides
	}
	row, lerr := s.load(memID)
	if isNotFound(lerr) {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rebuildUnresolved, nil
		}
		fm, _, perr := parseAuthorityDoc(raw, memID)
		if perr != nil {
			return rebuildUnresolved, nil // invalid frontmatter never becomes an illegal row
		}
		if !s.bindingsExist(fm) {
			return rebuildSkip, nil
		}
		if berr := s.backfillRow(fm, raw, dirProject, memID, "active"); berr != nil {
			return rebuildUnresolved, berr
		}
		// Backfilled row: index write + stamp land in the batched pass,
		// in one transaction — a crash in between leaves the row
		// unstamped, which the next pass re-verifies and re-queues.
		return rebuildQueue, nil
	}
	if lerr != nil {
		return rebuildUnresolved, lerr
	}
	if row.Status == "redacted" {
		// Terminal: the file must not resurrect the row.
		return rebuildSkip, nil
	}
	if stampMatches(row, st) {
		// polyforge#129 fast path: nothing per-entry can change anything.
		return rebuildSkip, nil
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return rebuildUnresolved, nil // vanished mid-pass: the row-driven scan decides
	}
	fm, _, perr := parseAuthorityDoc(raw, memID)
	if perr != nil {
		return rebuildUnresolved, nil // invalid frontmatter never becomes an illegal row
	}
	if row.ContentSHA != shaHex(raw) || !rowMatchesFrontmatter(row, fm) {
		if cerr := s.reconcileOne(memID); cerr != nil {
			if ae, ok := cerr.(*APIError); ok && ae.Code == api.ErrMemBadInput {
				return rebuildUnresolved, nil
			}
			return rebuildUnresolved, cerr
		}
		return rebuildIndexed, nil // corrected from the file: row, stamp and index current
	}
	// Content verified equal — only the stamp was missing or stale.
	if row.fsStamp == nil {
		// Pre-#129 row (first pass after the upgrade): the batched pass
		// rewrites its FTS row once and stamps it — the one-time full
		// pass the upgrade degrades to (this also heals an index row an
		// older build's crashed rebuild could have left behind).
		return rebuildQueue, nil
	}
	if uerr := s.stampRow(memID, st); uerr != nil {
		return rebuildUnresolved, uerr
	}
	return rebuildRefresh, nil // touched, not modified: counted, no index write
}

// rebuildRedactedEntry runs the _redacted pass for one entry: a missing
// row is backfilled; a row still active while the directory already
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
// full frontmatter→DDL mapping, content_sha computed live. The row is
// deliberately written UNSTAMPED (polyforge#129): its index write and
// stamp land together in the batched pass, so a crash in between leaves
// the row unstamped — the next pass re-verifies and re-queues it — never
// stamped with no index row.
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
