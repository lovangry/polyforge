// Package memory implements the Core-side Memory store and the rows 22–25
// endpoint surface. 02-aihub-core.md §4.9 defines the tables, the
// authority-file layout (§4.11) and the delete-then-insert FTS invariant;
// 06-memory-search.md is the full semantic authority implemented here:
//
//   - three-step write order under a single backupBarrier read lock
//     (06 §2.3 INV-7a, §4.6, §5.2),
//   - Markdown→HTML derivation with the source_markdown_sha/html_sha
//     double-digest staleness check (06 §4.3, INV-7d),
//   - the §4.4 body extraction rule for the FTS index,
//   - short-query (<3-character, incl. 2-char CJK) retrieval widening
//     beside the FTS5 trigram MATCH path,
//   - INV-7f read-path serving validation and the full §5.3 reconcile
//     procedure (reconcile.go: transient sweep, orphan handling both
//     ways, _redacted forward-completion, frontmatter metadata backfill,
//     unconditional FTS rebuild),
//   - the 06 §9 B5 crash-injection hooks on every numbered boundary of
//     the §5.2 write order (crashHook below).
package memory

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/silgrid/polyforge/internal/core/store"
)

// APIError mirrors the other core packages.
type APIError struct {
	Code   string
	Reason string
	Detail string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return e.Code + " " + e.Reason + ": " + e.Detail
	}
	return e.Code + ": " + e.Detail
}

func errf(code, reason, format string, args ...any) *APIError {
	return &APIError{Code: code, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// BackupBarrier is the process-level consistency barrier of 07 §6.2: the
// admin backup procedure takes the WRITE side (a snapshot must observe a
// "no in-flight writes" point covering BOTH SQLite and the memory/ files),
// and the three authority-file write paths of this package (create /
// update / redact, 06 §4.6) take the READ side, held from BEFORE the first
// byte is written to the authority area until the whole write order (tmp
// write → Core tx commit → rename → HTML derivation) is done.
//
// It is a package-level shared lock so the backup handler (server layer,
// VACUUM INTO) and the memory write paths coordinate through one barrier —
// 07 §6.2 is explicit that there is no second barrier mechanism. Memory's
// internal Core transactions never touch this lock themselves (07 §6.2
// rule 7: one READ acquisition per operation, taken at the API entry
// layer, the non-reentrant RWMutex must not be re-locked inside the
// transaction layer).
//
// Simplicity ruling (recorded): 07 §6.2 describes hardlink directory
// snapshots inside the write-lock window; the current backup handler only
// performs VACUUM INTO. The lock is therefore held around the VACUUM INTO
// (and, once directory snapshots exist, around those too); nothing else in
// this process takes the write side.
var BackupBarrier sync.RWMutex

// Visibility tiers (06 §4.2 vocabulary; membership-first filtering is
// applied by the server layer per 02 §4.10 / 06 §7).
var visibilities = map[string]bool{"admin": true, "private": true, "project": true, "public": true, "team": true}

// crashHook is the 06 §9 B5 test-time crash-injection hook. It is nil in
// production; the write paths call crashpoint at every numbered boundary
// of the 06 §5.2 write order so a test can kill the operation exactly
// where a real process would die. A test hook typically panics with a
// sentinel; the defers that run during the unwind (SQLite rolling an
// uncommitted transaction back, lock release) leave behind exactly the
// durable state a hard crash leaves — the tmp file and any committed rows
// survive, nothing else changes.
var crashHook func(stage string)

// Write-order stages passed to crashHook (06 §9 B5):
//
//	create/update: "…:pre-write"   before ① — no authority byte written yet
//	               "…:post-tmp"    ① done, ② not started
//	               "…:post-commit" ② done, ③ not started
//	redact:        "redact:pre-move" / "redact:post-move" (① done, ② not)
func crashpoint(stage string) {
	if crashHook != nil {
		crashHook(stage)
	}
}

// maxContentBytes is the 06 §5.4 MEM-413 body limit (≤ 1 MiB).
const maxContentBytes = 1 << 20

// Entry is the memories row as served by the API.
type Entry struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	OwnerUserID string   `json:"owner_user_id"`
	WorkItemID  *string  `json:"work_item_id"`
	Type        string   `json:"type"`
	Title       *string  `json:"title"`
	Visibility  string   `json:"visibility"`
	ContentSHA  string   `json:"content_sha"`
	ContentLen  int64    `json:"content_len"`
	Tags        []string `json:"tags"`
	Related     []string `json:"related"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`

	apath  string         `json:"-"` // authority_path column, internal
	smsSHA sql.NullString `json:"-"` // source_markdown_sha (06 §2.1 double digest)
	hSHA   sql.NullString `json:"-"` // html_sha (06 §2.1 double digest)
}

// Store is the memory subsystem store.
type Store struct {
	db      *store.DB
	dataDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-mem_id write mutex (06 §6)
}

func NewStore(db *store.DB, dataDir string) *Store {
	return &Store{db: db, dataDir: dataDir, locks: map[string]*sync.Mutex{}}
}

func (s *Store) authorityDir(projectID, memID string) string {
	return filepath.Join(s.dataDir, "memory", projectID, memID)
}

// memLock takes the per-mem_id in-process write mutex (06 §6): concurrent
// requests for the same mem_id serialize BEFORE any file writing and hold
// until the whole operation (write order + HTML derivation) completes. It
// is a different layer from BackupBarrier (per-entry serialization vs
// write-vs-backup consistency); the two are acquired independently in a
// FIXED order everywhere (memLock first, barrier read lock second — the
// barrier's write side is never held while waiting on a memLock, so no
// cycle can form). create takes its memLock right after the id is
// allocated, before the first authority byte — from that point on the id
// is known for the whole write order (06 §6: "before entering file writing").
//
// Entries are never evicted (bounded by the number of distinct mem_ids in
// the process lifetime; 06 §6 scopes the lock table to one AIHub process).
func (s *Store) memLock(memID string) func() {
	s.mu.Lock()
	l, ok := s.locks[memID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[memID] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// CreateRequest is the POST /api/v1/memories body.
type CreateRequest struct {
	Project        string   `json:"project"` // project name
	Type           string   `json:"type"`
	Title          *string  `json:"title"`
	Visibility     string   `json:"visibility"`
	Content        string   `json:"content"`
	Tags           []string `json:"tags"`
	Related        []string `json:"related"`
	WorkItemID     *string  `json:"work_item_id"`
	IdempotencyKey *string  `json:"idempotency_key"`
}

// Create runs the 06 §5.2 create write order under a single backupBarrier
// read lock (INV-7a/INV-7e):
//
//	① write memory.md.tmp + fsync (file and parent dir)
//	② one Core tx: memories row (content_sha = sha of the ① bytes) + FTS
//	③ rename tmp over the authority memory.md + fsync dir, then derive
//	   memory.html and record the source_markdown_sha/html_sha pair
//
// Between ② and ③ there is no externally servable half-committed state:
// before ② the authority file does not exist; after ② a reader that finds
// the file missing treats the entry as inconsistent (INV-7f, not served).
// Crash residue (an orphan tmp) is swept by the §5.3 reconcile task.
func (s *Store) Create(req CreateRequest, owner string) (*Entry, error) {
	if strings.TrimSpace(req.Content) == "" {
		return nil, errf("MEM-400", "", "content required")
	}
	if req.Type == "" {
		return nil, errf("MEM-400", "", "type required")
	}
	if !visibilities[req.Visibility] {
		return nil, errf("MEM-400", "", "bad visibility %q", req.Visibility)
	}
	if len(req.Content) > maxContentBytes {
		return nil, errf("MEM-413", "", "content exceeds 1 MiB")
	}
	var projectID string
	if err := s.db.QueryRow(`SELECT id FROM projects WHERE name=?1`, req.Project).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("MEM-404", "", "project %q", req.Project)
		}
		return nil, err
	}

	// Idempotent create: UNIQUE(idempotency_key) returns the original entry.
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		var existing string
		qerr := s.db.QueryRow(`SELECT id FROM memories WHERE idempotency_key=?1`, *req.IdempotencyKey).Scan(&existing)
		if qerr == nil {
			return s.Get(existing)
		}
		if !errors.Is(qerr, sql.ErrNoRows) {
			return nil, qerr
		}
	}

	// Content may carry a frontmatter block; the server owns the id (06
	// §2.2: a client-supplied id is CLIENT_ID_NOT_ALLOWED) and merges the
	// client's frontmatter fields with the validated request fields — the
	// request fields win (inference: the typed API body is the validated
	// surface; free-form frontmatter in content only seeds what the body
	// does not repeat).
	var body string
	if block, rest, ok := splitFrontmatter(req.Content); ok {
		raw, err := parseFrontmatterBlock(block)
		if err != nil {
			return nil, err
		}
		if _, has := raw["id"]; has {
			return nil, errf("MEM-400", "CLIENT_ID_NOT_ALLOWED", "content frontmatter must not carry an id")
		}
		body = rest
	} else {
		body = req.Content
	}

	memID, err := store.NewID("mem_")
	if err != nil {
		return nil, err
	}
	// The per-mem write mutex is held from before the first authority byte
	// (06 §6: the lock is taken “before entering file writing” — the id IS
	// known by then) so the INV-7f read-path recheck and the §5.3 sweep can
	// distinguish a live ①→③ window from crash residue.
	unlockMem := s.memLock(memID)
	defer unlockMem()
	now := store.Now()
	fm := &frontmatter{
		ID:             memID,
		Type:           req.Type,
		OwnerUserID:    owner, // authenticated identity, never a client field (06 §5.2 step 5)
		Title:          req.Title,
		Tags:           req.Tags,
		Visibility:     req.Visibility,
		WorkItemID:     req.WorkItemID,
		ProjectID:      projectID,
		IdempotencyKey: req.IdempotencyKey,
		Related:        req.Related,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if fm.Tags == nil {
		fm.Tags = []string{}
	}
	if fm.Related == nil {
		fm.Related = []string{}
	}
	authorityBytes, err := buildAuthorityFile(fm, body)
	if err != nil {
		return nil, err
	}
	if len(authorityBytes) > maxContentBytes {
		return nil, errf("MEM-413", "", "authority document exceeds 1 MiB")
	}

	dir := s.authorityDir(projectID, memID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errf("MEM-500", "", "mkdir: %v", err)
	}

	// INV-7e: the read lock is taken BEFORE the first authority-area byte
	// is written and held through tx commit, rename and HTML derivation.
	BackupBarrier.RLock()
	defer BackupBarrier.RUnlock()

	tmp := filepath.Join(dir, "memory.md.tmp")
	authority := filepath.Join(dir, "memory.md")
	// ① tmp + fsync — the authority path is still untouched.
	crashpoint("create:pre-write") // 06 §9: dies before any byte
	if err := writeTmpSync(tmp, authorityBytes); err != nil {
		return nil, errf("MEM-500", "", "write authority: %v", err)
	}
	contentSHA := shaHex(authorityBytes)
	crashpoint("create:post-tmp") // 06 §9: ① done, ② not started

	// ② one Core tx: metadata row + FTS (02 §4.9 delete-then-insert).
	rel := "memory/" + projectID + "/" + memID // authority_path, data-dir-relative
	var idem any
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		idem = *req.IdempotencyKey
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO memories(id, project_id, owner_user_id, work_item_id, type, title,
		visibility, authority_path, content_sha, content_len, tags, related, idempotency_key, status, created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,'active',?14,?14)`,
		memID, projectID, owner, anyPtr(req.WorkItemID), req.Type, req.Title, req.Visibility, rel,
		contentSHA, len(authorityBytes), jsonOrEmpty(req.Tags), jsonOrEmpty(req.Related), idem, now); err != nil {
		return nil, err
	}
	if err := ftsReplace(tx, memID, extractBody(string(authorityBytes))); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	crashpoint("create:post-commit") // 06 §9: ② done, ③ not started

	// ③ rename over the authority file, fsync dir, then derive.
	if err := os.Rename(tmp, authority); err != nil {
		return nil, errf("MEM-500", "", "rename authority: %v", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, errf("MEM-500", "", "fsync authority dir: %v", err)
	}

	// Derivation is best-effort on the write path (06 §4.3): a derive
	// failure is a MEM-501-class warning — the authority and metadata are
	// committed and searchable; the next format=html read re-derives.
	e := &Entry{ID: memID, ProjectID: projectID, OwnerUserID: owner, apath: rel,
		ContentSHA: contentSHA, ContentLen: int64(len(authorityBytes))}
	_, _ = s.deriveHTML(e, authorityBytes, body)
	// Raw load, not the INV-7f gate: this path just completed ③ and holds
	// the memLock — re-validating here would deadlock on the mutex.
	return s.load(memID)
}

// Get returns one entry through the INV-7f serving gate (06 §2.3/§6):
// the row's content_sha is verified against the live authority file
// digest before anything is served, and a mismatch runs the single-entry
// reconcile (file wins) instead of serving a half-committed state.
func (s *Store) Get(memID string) (*Entry, error) {
	return s.validated(memID)
}

// Content returns the authority Markdown bytes.
func (s *Store) Content(memID string) ([]byte, error) {
	e, err := s.validated(memID)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(s.dataDir, filepath.FromSlash(e.apath), "memory.md"))
}

func (s *Store) load(memID string) (*Entry, error) {
	var e Entry
	var wi, title sql.NullString
	var tags, related string
	err := s.db.QueryRow(`SELECT id, project_id, owner_user_id, work_item_id, type, title, visibility,
		authority_path, content_sha, content_len, tags, related, status, created_at, updated_at,
		source_markdown_sha, html_sha
		FROM memories WHERE id=?1`, memID).
		Scan(&e.ID, &e.ProjectID, &e.OwnerUserID, &wi, &e.Type, &title, &e.Visibility,
			&e.apath, &e.ContentSHA, &e.ContentLen, &tags, &related, &e.Status, &e.CreatedAt, &e.UpdatedAt,
			&e.smsSHA, &e.hSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("MEM-404", "", "memory %s", memID)
	}
	if err != nil {
		return nil, err
	}
	if wi.Valid {
		e.WorkItemID = &wi.String
	}
	if title.Valid {
		e.Title = &title.String
	}
	_ = json.Unmarshal([]byte(tags), &e.Tags)
	if e.Tags == nil {
		e.Tags = []string{}
	}
	_ = json.Unmarshal([]byte(related), &e.Related)
	if e.Related == nil {
		e.Related = []string{}
	}
	return &e, nil
}

// UpdateRequest carries the PATCH fields; fields left nil/empty keep the
// current values.
type UpdateRequest struct {
	Content string
	Title   *string
	Tags    []string
}

// Update runs the 06 §5.2 update write order: same three-step shape as
// create, plus the pre-① history snapshot of the outgoing authority file.
// Metadata columns (content_sha, title, tags, related, visibility,
// updated_at) all follow the new content (06 §5.2 step 4: omitting
// visibility here would leave a public→private downgrade stale in Core
// until the next rebuild — a privilege-escalation window).
func (s *Store) Update(memID string, req UpdateRequest) (*Entry, error) {
	unlock := s.memLock(memID)
	defer unlock()
	BackupBarrier.RLock()
	defer BackupBarrier.RUnlock()

	e, err := s.load(memID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Content) == "" {
		return nil, errf("MEM-400", "", "content required")
	}
	if e.Status != "active" {
		return nil, errf("MEM-409", "", "memory is %s", e.Status)
	}
	if len(req.Content) > maxContentBytes {
		return nil, errf("MEM-413", "", "content exceeds 1 MiB")
	}
	dir := filepath.Join(s.dataDir, filepath.FromSlash(e.apath))
	authority := filepath.Join(dir, "memory.md")
	crashpoint("update:pre-write") // 06 §9: before any byte (incl. the history snapshot)

	// Seed the new frontmatter from the CURRENT row, then apply the new
	// content's frontmatter (if any) and the PATCH fields.
	fm := &frontmatter{
		ID: memID, Type: e.Type, OwnerUserID: e.OwnerUserID, Title: e.Title,
		Tags: e.Tags, Visibility: e.Visibility, WorkItemID: e.WorkItemID,
		ProjectID: e.ProjectID, Related: e.Related,
		CreatedAt: e.CreatedAt, UpdatedAt: store.Now(),
	}
	var body string
	if block, rest, ok := splitFrontmatter(req.Content); ok {
		raw, perr := parseFrontmatterBlock(block)
		if perr != nil {
			return nil, perr
		}
		if idv, has := raw["id"]; has {
			if s, _ := idv.(string); s != memID {
				return nil, errf("MEM-400", "FRONTMATTER_ID_MISMATCH", "frontmatter id %q != %s", s, memID)
			}
		}
		// Frontmatter seeds fields; explicit PATCH fields (below) win.
		if v, ok := rawString(raw, "title"); ok {
			fm.Title = &v
		}
		if v, ok := rawStrings(raw, "tags"); ok {
			fm.Tags = v
		}
		if v, ok := rawStrings(raw, "related"); ok {
			fm.Related = v
		}
		if v, ok := rawString(raw, "visibility"); ok {
			if !visibilities[v] {
				return nil, errf("MEM-400", "FRONTMATTER_INVALID", "bad visibility %q", v)
			}
			fm.Visibility = v
		}
		body = rest
	} else {
		body = req.Content
	}
	if req.Title != nil {
		fm.Title = req.Title
	}
	if req.Tags != nil {
		fm.Tags = req.Tags
	}

	// Outgoing-version snapshot (06 §5.2 update step 2): copy the current
	// authority file into history/ BEFORE step ① touches anything. Orphan
	// history files from a crashed update are inert (not part of any
	// judgment) and are swept by the §5.3 reconcile task.
	if old, rerr := os.ReadFile(authority); rerr == nil {
		histDir := filepath.Join(dir, "history")
		if merr := os.MkdirAll(histDir, 0o750); merr == nil {
			name := strings.Map(func(r rune) rune {
				if r == ':' || r == '.' {
					return '-'
				}
				return r
			}, fm.UpdatedAt) + ".md"
			_ = os.WriteFile(filepath.Join(histDir, name), old, 0o640)
		}
	}

	authorityBytes, err := buildAuthorityFile(fm, body)
	if err != nil {
		return nil, err
	}
	if len(authorityBytes) > maxContentBytes {
		return nil, errf("MEM-413", "", "authority document exceeds 1 MiB")
	}

	// ① tmp + fsync.
	tmp := authority + ".tmp"
	if err := writeTmpSync(tmp, authorityBytes); err != nil {
		return nil, errf("MEM-500", "", "write authority: %v", err)
	}
	contentSHA := shaHex(authorityBytes)
	crashpoint("update:post-tmp") // 06 §9: ① done, ② not started

	// ② one Core tx: full metadata update + FTS delete-then-insert.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET content_sha=?1, content_len=?2, title=?3, tags=?4,
		related=?5, visibility=?6, updated_at=?7 WHERE id=?8`,
		contentSHA, len(authorityBytes), anyPtr(fm.Title), jsonOrEmpty(fm.Tags), jsonOrEmpty(fm.Related),
		fm.Visibility, fm.UpdatedAt, memID); err != nil {
		return nil, err
	}
	if err := ftsReplace(tx, memID, extractBody(string(authorityBytes))); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	crashpoint("update:post-commit") // 06 §9: ② done, ③ not started (the B5 window)

	// ③ rename over the authority file, fsync dir, then re-derive.
	if err := os.Rename(tmp, authority); err != nil {
		return nil, errf("MEM-500", "", "rename authority: %v", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, errf("MEM-500", "", "fsync authority dir: %v", err)
	}
	_, _ = s.deriveHTML(e, authorityBytes, body)
	// Raw load, not the INV-7f gate: the memLock is still held here.
	return s.load(memID)
}

// HTML returns the derived view for format=html reads, applying the 06
// §4.3 three-condition staleness check (INV-7d): the artifact is fresh only
// if (1) memory.html exists, (2) its recorded source_markdown_sha equals
// the sha256 of the authority file AS CURRENTLY ON DISK (computed live,
// not via the cached content_sha column), and (3) its recorded html_sha
// equals the sha256 of the memory.html bytes as currently on disk. Any
// miss → one re-derivation, writing memory.html plus BOTH digests.
//
// Derivation mutates derived (never authority) state, so this read path
// still takes the barrier read lock while it writes and the per-mem mutex
// to serialize against a concurrent update of the same entry.
func (s *Store) HTML(memID string) ([]byte, error) {
	// INV-7f gate first: the authority file digest is verified against the
	// row before the derived view is even considered.
	e, err := s.validated(memID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.dataDir, filepath.FromSlash(e.apath))
	authorityBytes, err := os.ReadFile(filepath.Join(dir, "memory.md"))
	if err != nil {
		return nil, errf("MEM-404", "", "authority file missing for %s", memID)
	}
	mdSHA := shaHex(authorityBytes)
	htmlPath := filepath.Join(dir, "memory.html")
	htmlBytes, ioerr := os.ReadFile(htmlPath)
	fresh := ioerr == nil && e.smsSHA.Valid && e.smsSHA.String == mdSHA &&
		e.hSHA.Valid && e.hSHA.String == shaHex(htmlBytes)
	if fresh {
		return htmlBytes, nil
	}

	unlock := s.memLock(memID)
	defer unlock()
	BackupBarrier.RLock()
	defer BackupBarrier.RUnlock()

	_, body, _ := splitFrontmatter(string(authorityBytes))
	out, derr := s.deriveHTML(e, authorityBytes, body)
	if derr != nil {
		return nil, derr
	}
	return out, nil
}

// deriveHTML renders the body of the given authority bytes, writes
// memory.html atomically and records BOTH digests in the same operation
// (06 §4.3: the html write and the source_markdown_sha/html_sha pair land
// together, never split across commits). Callers hold the barrier read
// lock and the per-mem mutex (write paths) or acquire them (HTML reads).
func (s *Store) deriveHTML(e *Entry, authorityBytes []byte, body string) ([]byte, error) {
	htmlBytes, err := renderMarkdown(body)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.dataDir, filepath.FromSlash(e.apath))
	if err := atomicWrite(filepath.Join(dir, "memory.html.tmp"), filepath.Join(dir, "memory.html"), htmlBytes); err != nil {
		return nil, errf("MEM-501", "DERIVE_FAILED", "write html: %v", err)
	}
	_, err = s.db.Exec(`UPDATE memories SET source_markdown_sha=?1, html_sha=?2 WHERE id=?3`,
		shaHex(authorityBytes), shaHex(htmlBytes), e.ID)
	if err != nil {
		return nil, err
	}
	return htmlBytes, nil
}

// Search runs the real-time text query (06 §4.4/§4.5: FTS5 trigram is the
// one v1 retrieval path) and returns candidate entries in rank order.
// Membership-first filtering (project membership before visibility tiers)
// is applied by the server layer per 02 §4.10 / 06 §7.
//
// Short-query widening: the trigram tokenizer cannot serve a token of
// fewer than 3 characters (a measured SQLite limitation — such MATCHes
// return nothing, notably 2-character CJK terms). Tokens shorter than 3
// runes are answered with a substring scan over the indexed bodies
// instead: if ALL tokens are short the scan is the query; if long tokens
// exist they run as the FTS MATCH and the short tokens filter the
// candidate set. Substring semantics for ≥3-rune text remain trigram
// MATCH, per 06 §4.4.
func (s *Store) Search(q string, memType string, tags []string, limit int, allowed func(*Entry) bool) ([]Entry, error) {
	if strings.TrimSpace(q) == "" {
		return nil, errf("MEM-400", "", "q required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var long, short []string
	for _, tok := range strings.Fields(q) {
		if utf8.RuneCountInString(tok) >= 3 {
			long = append(long, tok)
		} else {
			short = append(short, tok)
		}
	}

	type cand struct{ id, body string }
	var cands []cand
	if len(long) > 0 {
		rows, err := s.db.Query(`SELECT f.mem_id, f.body FROM memories m
			JOIN memory_fts f ON f.mem_id = m.id
			WHERE memory_fts MATCH ?1 AND m.status='active'
			ORDER BY rank LIMIT ?2`, ftsQuery(long), limit*4)
		if err != nil {
			// FTS could not serve this expression at all → degrade to the
			// substring scan so retrieval still answers (06 §4.4 substring
			// intent) instead of failing the whole search.
			scanned, serr := s.scanBodies()
			if serr != nil {
				return nil, errf("MEM-503", "", "fts: %v", err)
			}
			for _, c := range scanned {
				cands = append(cands, cand{c.id, c.body})
			}
		} else {
			for rows.Next() {
				var c cand
				if err := rows.Scan(&c.id, &c.body); err != nil {
					rows.Close()
					return nil, err
				}
				cands = append(cands, c)
			}
			rows.Close()
		}
	} else {
		scanned, serr := s.scanBodies()
		if serr != nil {
			return nil, serr
		}
		for _, c := range scanned {
			cands = append(cands, cand{c.id, c.body})
		}
	}

	out := []Entry{}
	for _, c := range cands {
		stale := false
		for _, tok := range short {
			if !strings.Contains(c.body, tok) {
				stale = true
				break
			}
		}
		if stale {
			continue
		}
		e, err := s.load(c.id)
		if err != nil {
			continue
		}
		if e.Status != "active" || !s.serveConsistent(e) {
			// INV-7f: an inconsistent hit is dropped from THIS result set
			// (its reconcile is dispatched inside serveConsistent).
			continue
		}
		if memType != "" && e.Type != memType {
			continue
		}
		if len(tags) > 0 && !containsAll(e.Tags, tags) {
			continue
		}
		if allowed != nil && !allowed(e) {
			continue
		}
		out = append(out, *e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// scanBodies returns every active entry's indexed body for substring
// filtering (the short-token path; scale is v1 single-project team size).
func (s *Store) scanBodies() ([]struct{ id, body string }, error) {
	rows, err := s.db.Query(`SELECT f.mem_id, f.body FROM memories m
		JOIN memory_fts f ON f.mem_id = m.id WHERE m.status='active'`)
	if err != nil {
		return nil, errf("MEM-503", "", "scan: %v", err)
	}
	var out []struct{ id, body string }
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, struct{ id, body string }{id, body})
	}
	rows.Close()
	return out, nil
}

// ftsQuery ANDs the tokens as quoted phrases for the trigram MATCH.
func ftsQuery(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, p := range tokens {
		if !strings.ContainsAny(p, ":-*\"()") {
			parts[i] = `"` + p + `"`
		} else {
			parts[i] = p
		}
	}
	return "body:" + strings.Join(parts, " AND body:")
}

func containsAll(have, want []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// List returns the project's active memories (visibility filtering by caller).
func (s *Store) List(projectID string, allowed func(*Entry) bool) ([]Entry, error) {
	rows, err := s.db.Query(`SELECT id FROM memories WHERE project_id=?1 AND status='active' ORDER BY created_at DESC LIMIT 500`, projectID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Entry{}
	for _, id := range ids {
		e, err := s.load(id)
		if err != nil {
			continue
		}
		if e.Status != "active" || !s.serveConsistent(e) {
			continue // INV-7f: not served in a half-committed state
		}
		if allowed != nil && !allowed(e) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

// Redact soft-deletes (06 §5.2 redact): the two-step shape under a single
// backupBarrier read lock —
//
//	① rename the authority dir into <project>/_redacted/ (the file
//	   location IS the authoritative state; moving first means a crash
//	   before ② is forward-completed by reconcile from the file position,
//	   never rolled back into a longer half-committed window)
//	② one Core tx: status='redacted' + FTS delete
func (s *Store) Redact(memID string) error {
	unlock := s.memLock(memID)
	defer unlock()
	BackupBarrier.RLock()
	defer BackupBarrier.RUnlock()

	e, err := s.load(memID)
	if err != nil {
		return err
	}
	if e.Status != "active" {
		return errf("MEM-409", "", "memory is %s", e.Status)
	}
	if !s.entryConsistent(e) {
		// Crash residue on the very entry being redacted: resolve it under
		// the lock we already hold before moving anything (06 §6). A
		// redact that crashed between ① and ② forward-completes here; a
		// create/update orphan (authority file nowhere) is rolled back and
		// the redact target simply does not exist.
		if _, serr := os.Stat(filepath.Join(s.dataDir, "memory", e.ProjectID, "_redacted", memID, "memory.md")); serr == nil {
			if ferr := s.forwardCompleteRedact(e); ferr != nil {
				return ferr
			}
			return errf("MEM-409", "", "memory is redacted")
		}
		if derr := s.deleteOrphanRow(memID); derr != nil {
			return derr
		}
		return errf("MEM-404", "", "memory %s", memID)
	}
	// ① file move.
	crashpoint("redact:pre-move")
	src := filepath.Join(s.dataDir, "memory", e.ProjectID, memID)
	dst := filepath.Join(s.dataDir, "memory", e.ProjectID, "_redacted", memID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return errf("MEM-500", "", "redact move: %v", err)
	}
	crashpoint("redact:post-move") // 06 §9: ① done, ② not started
	// ② metadata + FTS removal in one tx.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET status='redacted', updated_at=?1 WHERE id=?2`, store.Now(), memID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, memID); err != nil {
		return err
	}
	return tx.Commit()
}

// Share issues an opaque, unguessable token for one memory.
//
// Ruling (recorded, deliverable 4): 06 §5.1 (A18) is the share-link
// authority and pins ONLY these semantics: owner-explicit generation
// (MEM-403 otherwise), an access channel OUTSIDE the five visibility
// tiers (it does not add a sixth), and anonymous single-entry read-only
// consumption (02 row 24). It mandates NO persistence and NO revocation
// for the token itself, so the stateless construction is kept: the token
// is mem_id + "." + HMAC(secret, mem_id), verifiable without storage and
// granting read on exactly one entry. (02 row 24's "随机签发" wording is
// descriptive of the pf-deploy-era shape; the non-enumerability it asks
// for is provided by the keyed HMAC — without the service secret no
// token can be produced for a chosen mem_id. The shareKey itself is
// HMAC-derived from the rotatable service secret by the server layer.)
// Revocation, if ever required, is the rotate-the-secret hammer: rotating
// the epoch invalidates every outstanding token at once.
func (s *Store) Share(memID string, secret []byte) (string, error) {
	if _, err := s.load(memID); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(memID))
	sig := hex.EncodeToString(mac.Sum(nil))[:22]
	return memID + "." + sig, nil
}

// ResolveShare validates a share token and returns the mem id.
func (s *Store) ResolveShare(token string, secret []byte) (string, error) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 {
		return "", errf("MEM-404", "", "bad share token")
	}
	memID := token[:i]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(memID))
	want := hex.EncodeToString(mac.Sum(nil))[:22]
	if !hmac.Equal([]byte(token[i+1:]), []byte(want)) {
		return "", errf("MEM-404", "", "bad share token")
	}
	return memID, nil
}

// ftsReplace enforces the 02 §4.9 invariant: delete-then-insert so each
// mem_id has at most one retrievable row, consistent with the latest
// authority content.
func ftsReplace(tx *sql.Tx, memID, body string) error {
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, memID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO memory_fts(mem_id, body) VALUES (?1, ?2)`, memID, body)
	return err
}

// anyPtr converts a *string to an any usable as a SQL NULL-able argument.
func anyPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// jsonOrEmpty marshals a slice as a JSON array, never nil (NOT NULL columns).
func jsonOrEmpty(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func rawString(raw map[string]any, key string) (string, bool) {
	v, ok := raw[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func rawStrings(raw map[string]any, key string) ([]string, bool) {
	v, ok := raw[key]
	if !ok {
		return nil, false
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}
