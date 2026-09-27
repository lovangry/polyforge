// Package capsule implements the Core side of Revision storage: CAS block
// intake, the manifest accept chain (fencing → idempotency → HEAD bootstrap →
// manifest_sha recompute → SYNC-INV-3 → block integrity), HEAD pointer
// management, deletion protection and the progress read-through
// reconstruction. Design authority: docs/design/polyforge-vnext/02-aihub-core.md
// §4.8/§5.2/§6.3 (Core side) cross-referenced with
// 05-sync-revision-recovery.md §2.6/§2.8/§2.9/§2.10/§4/§5.1/§7.4.
package capsule

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/silgrid/polyforge/internal/core/owner"
	"github.com/silgrid/polyforge/internal/core/store"
)

// APIError mirrors owner.APIError.
type APIError struct {
	Code   string
	Reason string
	Detail string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func errf(code, reason, format string, args ...any) *APIError {
	return &APIError{Code: code, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Op is one manifest operation (05 §4.2).
type Op struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Mode   *int64 `json:"mode,omitempty"`
	Target string `json:"target,omitempty"`
}

// Manifest is the rev/<seq>/manifest.json object (05 §4.2).
type Manifest struct {
	WorkItemID             string `json:"work_item_id"`
	Seq                    int64  `json:"seq"`
	Parent                 *int64 `json:"parent"`
	Generation             int64  `json:"generation"`
	Kind                   string `json:"kind"`
	Ops                    []Op   `json:"ops"`
	LiveFiles              int64  `json:"live_files"`
	DataBytes              int64  `json:"data_bytes"`
	ExecutionStateIncluded bool   `json:"execution_state_included"`
	BuiltAt                string `json:"built_at"`
}

// Store is the Core-side capsule data store.
type Store struct {
	db     *store.DB
	dataDir string
	owners *owner.Manager
}

func NewStore(db *store.DB, dataDir string, owners *owner.Manager) *Store {
	return &Store{db: db, dataDir: dataDir, owners: owners}
}

func (s *Store) capsuleDir(wiID string) string {
	return filepath.Join(s.dataDir, "capsules", wiID)
}

func (s *Store) casPath(wiID, sha string) string {
	return filepath.Join(s.capsuleDir(wiID), "rev", "_cas", sha[:2], sha)
}

func (s *Store) manifestPath(wiID string, seq int64) string {
	return filepath.Join(s.capsuleDir(wiID), "rev", fmt.Sprintf("%d", seq), "manifest.json")
}

// ---------------------------------------------------------------------------
// Block upload (row 14)

// PutBlock writes one CAS block idempotently. Block-level fencing (05 §2.9):
// the generation token must validate for the WI's active generation BEFORE
// any write, so a fenced-out old owner cannot produce orphan blocks.
//
// Digest verification (05 §5.1 row 14, fix #6): the presented shaHex is
// ALWAYS checked against sha256(body) — or, for a zstd-compressed block,
// against sha256 of the decompressed original — even on the CAS-hit path;
// a mismatch is SYNC-400 and nothing is written or treated as a no-op.
func (s *Store) PutBlock(wiID, shaHex string, body []byte, tokenHex string) error {
	if err := validateWiID(wiID); err != nil {
		return err
	}
	if err := validateShaHex(shaHex); err != nil {
		return err
	}
	if err := s.fence(wiID, tokenHex); err != nil {
		return err
	}
	if err := verifyBlockDigest(body, shaHex); err != nil {
		return err
	}
	dst := s.casPath(wiID, shaHex)
	if _, err := os.Stat(dst); err == nil {
		return nil // CAS hit: idempotent no-op, only after the digest matched above
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return errf("CORE-507", "", "mkdir: %v", err)
	}
	tmp := dst + ".tmp"
	if err := atomicWrite(tmp, dst, body); err != nil {
		return errf("CORE-507", "", "write block: %v", err)
	}
	return nil
}

// verifyBlockDigest checks sha256(content) == shaHex, where content is body
// itself for a non-zstd payload or the zstd-decompressed original otherwise
// (decodeBlock handles both transparently; 05 §4.2: digest addresses the
// ORIGINAL content, not the compressed wire bytes).
func verifyBlockDigest(body []byte, shaHex string) error {
	content, err := decodeBlock(body)
	if err != nil {
		return errf("SYNC-400", "DIGEST_MISMATCH", "zstd decode failed: %v", err)
	}
	sum := sha256.Sum256(content)
	got := hex.EncodeToString(sum[:])
	if got != shaHex {
		return errf("SYNC-400", "DIGEST_MISMATCH", "sha256(content) %s != presented %s", got, shaHex)
	}
	return nil
}

// GetBlock streams one CAS block back (row 17).
func (s *Store) GetBlock(wiID, shaHex string) ([]byte, error) {
	if err := validateWiID(wiID); err != nil {
		return nil, err
	}
	if err := validateShaHex(shaHex); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.casPath(wiID, shaHex))
	if os.IsNotExist(err) {
		return nil, errf("SYNC-404", "", "block %s", shaHex)
	}
	return b, err
}

// ---------------------------------------------------------------------------
// Manifest submit (row 15)

// PostManifest runs the full accept chain of 05 §5.1 row 15 / §2.8 / §2.6:
// fencing → idempotency → HEAD bootstrap/parent → manifest_sha recompute →
// SYNC-INV-3 → block integrity, then INSERT revision + INSERT/UPDATE HEAD in
// one transaction (先数据后状态: blocks were uploaded first).
//
// Validation order (fix #10, pinned to 05 §5.1 row 15): fencing →
// idempotency → HEAD bootstrap/parent → manifest_sha recompute →
// SYNC-INV-3 (execution_state_included) → block integrity. A stale parent
// AND a bad hash in the same request therefore surfaces SYNC-409
// (SEQ_CONFLICT), not the hash mismatch — HEAD is checked first and HEAD
// never moves on any later failure either.
func (s *Store) PostManifest(wiID string, manifest Manifest, manifestSha string, tokenHex string) (headSeq int64, err error) {
	if manifest.WorkItemID != wiID {
		return 0, errf("SYNC-400", "", "manifest work_item_id mismatch")
	}
	gen, err := s.fenceGen(wiID, tokenHex)
	if err != nil {
		return 0, err
	}
	if manifest.Generation != gen {
		return 0, errf("SYNC-403", "stale_generation", "manifest generation %d ≠ active %d", manifest.Generation, gen)
	}

	// Idempotency FIRST — before any HEAD comparison (05 §2.8).
	var existing int64
	qerr := s.db.QueryRow(`SELECT seq FROM capsule_revisions
		WHERE work_item_id=?1 AND seq=?2 AND manifest_sha=?3`, wiID, manifest.Seq, manifestSha).Scan(&existing)
	if qerr == nil {
		cur, _ := s.HeadSeq(wiID)
		return cur, nil
	}
	if !errors.Is(qerr, sql.ErrNoRows) {
		return 0, qerr
	}

	// HEAD bootstrap / chain validation (05 §2.6) — moved ahead of the
	// manifest_sha recompute and block-integrity checks (fix #10).
	var headSeqCur sql.NullInt64
	if qerr := s.db.QueryRow(`SELECT seq FROM capsule_heads WHERE work_item_id=?1`, wiID).Scan(&headSeqCur); qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return 0, qerr
	}
	if !headSeqCur.Valid {
		if manifest.Seq != 0 || manifest.Parent != nil {
			return 0, errf("SYNC-409", "SEQ_CONFLICT", "bootstrap requires seq=0 and parent=null")
		}
	} else {
		if manifest.Parent == nil || *manifest.Parent != headSeqCur.Int64 {
			return 0, errf("SYNC-409", "SEQ_CONFLICT", "parent must equal current HEAD %d", headSeqCur.Int64)
		}
		if manifest.Seq != headSeqCur.Int64+1 {
			return 0, errf("SYNC-409", "SEQ_CONFLICT", "seq must be HEAD+1 = %d", headSeqCur.Int64+1)
		}
	}

	// manifest_sha recompute over the idempotent view (built_at stripped).
	want := ComputeManifestSha(manifest)
	if want != manifestSha {
		return 0, errf("SYNC-400", "MANIFEST_HASH_MISMATCH", "recomputed %s ≠ presented %s", want, manifestSha)
	}

	// SYNC-INV-3.
	if !manifest.ExecutionStateIncluded {
		return 0, errf("SYNC-400", "EXECUTION_STATE_MISSING", "execution_state_included must be true")
	}
	if manifest.Kind != "incremental" && manifest.Kind != "checkpoint" {
		return 0, errf("SYNC-400", "", "kind must be incremental|checkpoint")
	}

	// Block integrity + path safety.
	for _, op := range manifest.Ops {
		if err := validateOp(op); err != nil {
			return 0, err
		}
		if op.Op == "put" {
			if op.Mode == nil {
				return 0, errf("SYNC-400", "MODE_MISSING", "put %s lacks mode", op.Path)
			}
			if _, err := os.Stat(s.casPath(wiID, op.Sha256)); err != nil {
				return 0, errf("SYNC-400", "BLOCK_MISSING", "block %s for %s not uploaded", op.Sha256, op.Path)
			}
		}
	}

	// Persist manifest file + row + HEAD. The manifest.json file is written
	// BEFORE tx.Commit (fix #8): read-through reconstruction (Progress) depends
	// on it being present for every committed revision, so a write failure
	// here must abort the whole publish (tx.Rollback via defer) rather than
	// leave a capsule_revisions row with no matching manifest file. The
	// on-disk copy keeps built_at (audit); the stored manifest_sha is the
	// idempotent-view digest.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := store.Now()
	if _, err := tx.Exec(`INSERT INTO capsule_revisions(work_item_id, seq, parent, generation, manifest_sha, data_bytes, published_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7)`, wiID, manifest.Seq, manifest.Parent, manifest.Generation,
		manifestSha, manifest.DataBytes, now); err != nil {
		return 0, err
	}
	if !headSeqCur.Valid {
		if _, err := tx.Exec(`INSERT INTO capsule_heads(work_item_id, generation, seq, updated_at)
			VALUES (?1,?2,?3,?4)`, wiID, manifest.Generation, manifest.Seq, now); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.Exec(`UPDATE capsule_heads SET generation=?1, seq=?2, updated_at=?3 WHERE work_item_id=?4`,
			manifest.Generation, manifest.Seq, now, wiID); err != nil {
			return 0, err
		}
	}
	mblob, merr := json.MarshalIndent(manifest, "", "  ")
	if merr != nil {
		return 0, merr
	}
	mpath := s.manifestPath(wiID, manifest.Seq)
	if err := os.MkdirAll(filepath.Dir(mpath), 0o750); err != nil {
		return 0, errf("CORE-507", "", "mkdir manifest dir: %v", err)
	}
	if err := atomicWrite(mpath+".tmp", mpath, mblob); err != nil {
		return 0, errf("CORE-507", "", "write manifest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return manifest.Seq, nil
}

// fence validates the generation token against the WI's active generation
// and returns the active generation number.
func (s *Store) fence(wiID, tokenHex string) error {
	_, err := s.fenceGen(wiID, tokenHex)
	return err
}

func (s *Store) fenceGen(wiID, tokenHex string) (int64, error) {
	gen, ok, reason, err := s.owners.CheckTokenForWI(wiID, tokenHex)
	if err != nil {
		return 0, err
	}
	if !ok {
		return gen, errf("SYNC-403", reason, "stale owner")
	}
	return gen, nil
}

// ComputeManifestSha computes "sha256:"+hex over the manifest's idempotent
// view (built_at stripped) canonicalized with the 05 §4.2 rule: UTF-8, keys
// in code point ascending order, fixed 2-space indent, no trailing newline.
func ComputeManifestSha(m Manifest) string {
	raw, _ := json.Marshal(m)
	var tree map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	_ = dec.Decode(&tree)
	delete(tree, "built_at")
	b, _ := json.MarshalIndent(tree, "", "  ")
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func validateOp(op Op) error {
	switch op.Op {
	case "put", "delete", "symlink", "chmod":
	default:
		return errf("SYNC-400", "", "unknown op %q", op.Op)
	}
	if err := validateRelPath(op.Path); err != nil {
		return err
	}
	if op.Op == "symlink" {
		if err := validateRelPath(op.Target); err != nil {
			return errf("SYNC-400", "PATH_ESCAPE", "symlink target %q: %v", op.Target, err)
		}
	}
	return nil
}

func validateRelPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "\x00") {
		return errf("SYNC-400", "PATH_ESCAPE", "path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errf("SYNC-400", "PATH_ESCAPE", "path %q", p)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HEAD / listing (rows 16, 19)

// Head is the GET head response.
type Head struct {
	Seq         int64  `json:"seq"`
	Generation  int64  `json:"generation"`
	ManifestSha string `json:"manifest_sha"`
	UpdatedAt   string `json:"updated_at"`
}

func (s *Store) Head(wiID string) (*Head, error) {
	if err := validateWiID(wiID); err != nil {
		return nil, err
	}
	var h Head
	err := s.db.QueryRow(`SELECT h.seq, h.generation, r.manifest_sha, h.updated_at
		FROM capsule_heads h JOIN capsule_revisions r ON r.work_item_id=h.work_item_id AND r.seq=h.seq
		WHERE h.work_item_id=?1`, wiID).Scan(&h.Seq, &h.Generation, &h.ManifestSha, &h.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("404", "", "no HEAD")
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// everClaimed reports whether the WI has ever had an owner_generations row
// (claimed at least once), regardless of whether that generation is still
// active (fix #16).
func (s *Store) everClaimed(wiID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1`, wiID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) HeadSeq(wiID string) (int64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRow(`SELECT seq FROM capsule_heads WHERE work_item_id=?1`, wiID).Scan(&seq); err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

// RevisionMeta is one row of the since-listing (row 19).
type RevisionMeta struct {
	Seq         int64  `json:"seq"`
	Parent      *int64 `json:"parent"`
	Generation  int64  `json:"generation"`
	ManifestSha string `json:"manifest_sha"`
	DataBytes   int64  `json:"data_bytes"`
	PublishedAt string `json:"published_at"`

	// Ops-bearing extension (02 §5.1 row 19 amendment, polyforge#14):
	// populated ONLY for include=ops listings — kind and ops are read
	// from the rev/<seq>/manifest.json audit copy so 05 §7.4 recovery can
	// replay over HTTP. The metadata fields above are unchanged.
	Kind string `json:"kind,omitempty"`
	Ops  []Op   `json:"ops,omitempty"`
}

// RevisionsSince lists revisions with seq >= since (row 19). With
// includeOps the rows additionally carry each revision's manifest audit
// content (kind + ops); the default listing stays metadata-only.
func (s *Store) RevisionsSince(wiID string, since int64, includeOps bool) ([]RevisionMeta, error) {
	if err := validateWiID(wiID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT seq, parent, generation, manifest_sha, data_bytes, published_at
		FROM capsule_revisions WHERE work_item_id=?1 AND seq >= ?2 ORDER BY seq`, wiID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RevisionMeta
	for rows.Next() {
		var r RevisionMeta
		var parent sql.NullInt64
		if err := rows.Scan(&r.Seq, &parent, &r.Generation, &r.ManifestSha, &r.DataBytes, &r.PublishedAt); err != nil {
			return nil, err
		}
		if parent.Valid {
			p := parent.Int64
			r.Parent = &p
		}
		if includeOps {
			m, err := s.loadManifest(wiID, r)
			if err != nil {
				return nil, err
			}
			r.Kind = m.Kind
			r.Ops = m.Ops
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Deletion protection (rows 18, 20)

// Tombstone writes the deletion-protection row (05 §2.10): only terminal WIs,
// explicit confirm token, 24h revert window.
func (s *Store) Tombstone(wiID, confirm string) error {
	if err := validateWiID(wiID); err != nil {
		return err
	}
	if confirm != wiID {
		return errf("SYNC-400", "", "confirm token must repeat the work item id")
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errf("CORE-404", "", "work item %s", wiID)
		}
		return err
	}
	switch status {
	case "wrapped", "failed", "cancelled":
	default:
		return errf("SYNC-409", "", "only terminal work items may be tombstoned (status %s)", status)
	}
	now := time.Now().UTC()
	hard := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	// fix #14: a WI whose latest tombstone row was reverted must be
	// re-tombstoneable with a FRESH 24h window rather than silently no-op
	// (the previous ON CONFLICT DO NOTHING left it permanently un-deletable
	// after one revert). A row already 'pending' or 'hard_deleted' is left
	// alone by the WHERE guard: re-tombstoning an already-pending row does
	// not reset its window, and a hard-deleted row cannot be resurrected.
	if _, err := s.db.Exec(`INSERT INTO capsule_tombstones(work_item_id, tombstoned_at, hard_delete_at, status, confirm_wi_id)
		VALUES (?1, ?2, ?3, 'pending', ?4)
		ON CONFLICT(work_item_id) DO UPDATE SET
			tombstoned_at=excluded.tombstoned_at,
			hard_delete_at=excluded.hard_delete_at,
			status='pending',
			confirm_wi_id=excluded.confirm_wi_id
		WHERE capsule_tombstones.status='reverted'`, wiID, now.Format(time.RFC3339Nano), hard, confirm); err != nil {
		return err
	}
	return nil
}

// RevertTombstone cancels a pending tombstone inside the 24h window.
//
// Window check (fix #14): 05 §2.10 defines the 24h window but does not name
// an error code for a revert attempted after hard_delete_at has elapsed.
// SYNC-409 (state conflict: the capsule is already committed to deletion) is
// used rather than SYNC-410 (which this package already uses for "no pending
// tombstone exists at all" — a materially different condition: here a row
// DOES exist, it is simply too late to revert it).
func (s *Store) RevertTombstone(wiID string) error {
	if err := validateWiID(wiID); err != nil {
		return err
	}
	var hardDeleteAt string
	err := s.db.QueryRow(`SELECT hard_delete_at FROM capsule_tombstones
		WHERE work_item_id=?1 AND status='pending'`, wiID).Scan(&hardDeleteAt)
	if errors.Is(err, sql.ErrNoRows) {
		return errf("SYNC-410", "", "no pending tombstone to revert")
	}
	if err != nil {
		return err
	}
	if hd, perr := time.Parse(time.RFC3339Nano, hardDeleteAt); perr == nil && !time.Now().UTC().Before(hd) {
		return errf("SYNC-409", "WINDOW_ELAPSED", "revert window elapsed at %s", hardDeleteAt)
	}
	res, err := s.db.Exec(`UPDATE capsule_tombstones SET status='reverted'
		WHERE work_item_id=?1 AND status='pending'`, wiID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errf("SYNC-410", "", "no pending tombstone to revert")
	}
	return nil
}

// SweepTombstones hard-deletes capsule data whose revert window elapsed.
// Blocks reachable from a live HEAD are never deleted independently — v1
// deletes the whole capsule directory of a terminal WI past its window
// (block-level GC with the 90-day checkpoint retention is WI ④ scope).
func (s *Store) SweepTombstones() error {
	rows, err := s.db.Query(`SELECT t.work_item_id, t.hard_delete_at FROM capsule_tombstones t
		JOIN work_items w ON w.id = t.work_item_id
		WHERE t.status='pending' AND t.hard_delete_at <= ?1`, store.Now())
	if err != nil {
		return err
	}
	var due []string
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	for _, id := range due {
		if err := os.RemoveAll(s.capsuleDir(id)); err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE capsule_tombstones SET status='hard_deleted' WHERE work_item_id=?1`, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read-through progress (02 §5.2/§6.3; reconstruction per 05 §7.4)

// Progress reconstructs the runtime-state/execution.json content at HEAD and
// returns the raw JSON bytes plus head metadata. No Core-side materialization
// or cache (INV-1: every call re-reads through).
func (s *Store) Progress(wiID string) (headSeq *int64, headGen *int64, updatedAt *string, execJSON []byte, err error) {
	h, herr := s.Head(wiID)
	if herr != nil {
		var ae *APIError
		if errors.As(herr, &ae) && ae.Code == "404" {
			// No HEAD. 02 §5.2 distinguishes "never claimed" (head_seq: null is
			// a legitimate answer) from "claimed but nothing published yet",
			// which must surface CORE-504 instead of silently looking identical
			// (fix #16). errors.As replaces an unchecked type assertion that
			// could panic on a plain DB error.
			claimed, cerr := s.everClaimed(wiID)
			if cerr != nil {
				return nil, nil, nil, nil, cerr
			}
			if claimed {
				return nil, nil, nil, nil, errf("CORE-504", "", "work item %s was claimed but has no published HEAD", wiID)
			}
			return nil, nil, nil, nil, nil // never claimed/published: head_seq null
		}
		return nil, nil, nil, nil, herr
	}
	// Collect manifests since the latest checkpoint ≤ head (or from 0).
	revs, err := s.revisionChain(wiID, h.Seq)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	files := map[string]string{} // path → sha256 (live set)
	for _, m := range revs {
		switch m.Kind {
		case "checkpoint":
			files = map[string]string{}
			for _, op := range m.Ops {
				if op.Op == "put" {
					files[op.Path] = op.Sha256
				}
			}
		case "incremental":
			for _, op := range m.Ops {
				switch op.Op {
				case "put":
					files[op.Path] = op.Sha256
				case "delete":
					delete(files, op.Path)
				}
			}
		}
	}
	sha, ok := files["runtime-state/execution.json"]
	if !ok {
		return &h.Seq, &h.Generation, &h.UpdatedAt, nil,
			errf("CORE-504", "", "execution.json not present at HEAD")
	}
	block, err := s.GetBlock(wiID, sha)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	content, err := decodeBlock(block)
	if err != nil {
		return nil, nil, nil, nil, errf("CORE-504", "", "block %s undecodable: %v", sha, err)
	}
	return &h.Seq, &h.Generation, &h.UpdatedAt, content, nil
}

// revisionChain loads the manifests to replay: the newest checkpoint ≤ head,
// then every incremental after it (05 §7.4).
func (s *Store) revisionChain(wiID string, head int64) ([]Manifest, error) {
	// Manifests are replayed from the on-disk manifest.json audit copies
	// (written at accept time); the DB rows carry the metadata.
	var bestCheckpoint int64 = -1
	all, err := s.RevisionsSince(wiID, 0, false)
	if err != nil {
		return nil, err
	}
	var chain []Manifest
	for _, r := range all {
		if r.Seq > head {
			break
		}
		m, err := s.loadManifest(wiID, r)
		if err != nil {
			return nil, err
		}
		if m.Kind == "checkpoint" {
			bestCheckpoint = m.Seq
		}
		chain = append(chain, *m)
	}
	if bestCheckpoint >= 0 {
		for i, m := range chain {
			if m.Seq >= bestCheckpoint {
				return chain[i:], nil
			}
		}
	}
	return chain, nil
}

func (s *Store) loadManifest(wiID string, r RevisionMeta) (*Manifest, error) {
	raw, err := os.ReadFile(s.manifestPath(wiID, r.Seq))
	if err != nil {
		return nil, errf("CORE-504", "", "manifest %d unreadable: %v", r.Seq, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errf("CORE-504", "", "manifest %d unparsable: %v", r.Seq, err)
	}
	return &m, nil
}

// decodeBlock returns the file content behind a CAS block: 05 §4.2 stores
// blocks zstd-compressed; non-zstd payloads (injected by tests / older
// writers) are returned raw.
func decodeBlock(block []byte) ([]byte, error) {
	if len(block) < 4 || string(block[:4]) != "\x28\xb5\x2f\xfd" {
		return block, nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(block, nil)
}

func isHexLower(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// validateWiID is the shared path-safety guard (fix #1) for every function
// that builds a filesystem path from a caller-supplied work item id
// (capsuleDir/casPath/manifestPath): charset [A-Za-z0-9_-] only, no "/", no
// "..", no encoded separators — a traversal attempt ("../", a %2F-smuggled
// segment that decodes to a slash, an over-length id) is rejected SYNC-400
// before touching the filesystem. Length is bounded generously (Core ids are
// "wi_" + 12 chars; 128 leaves headroom without being unbounded).
func validateWiID(id string) error {
	if id == "" || len(id) > 128 {
		return errf("SYNC-400", "", "invalid work item id")
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return errf("SYNC-400", "", "invalid work item id")
		}
	}
	return nil
}

// validateShaHex guards the other filesystem-path component derived from
// caller input (casPath uses sha[:2]); a short or malformed value must be
// rejected here rather than panic on the slice.
func validateShaHex(sha string) error {
	if len(sha) != 64 || !isHexLower(sha) {
		return errf("SYNC-400", "", "block id must be 64-hex sha256")
	}
	return nil
}

func atomicWrite(tmp, dst string, body []byte) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
