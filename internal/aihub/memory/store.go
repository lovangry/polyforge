package memory

// Store: per-mem write mutexes, the three-step write order, the INV-7f
// serving gate, FTS5-trigram search (with the short-query widening for
// <3-rune tokens, kept from v0: trigram MATCH cannot answer those) and
// redact.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/common/api"
)

// fileStamp is the freshness stamp a memories row carries (polyforge#129):
// the authority file's observed mtime (nanoseconds) and byte size at the
// moment the row was written. Equality with a fresh Stat of the file is
// the reconcile's skip condition — the row (and its FTS row, written in
// the same transaction) then provably describe this exact file state.
type fileStamp struct {
	mtimeNS int64
	size    int64
}

// statStamp observes one file's freshness stamp. nil when the file cannot
// be stat'ed: the row is written unstamped and the next reconcile pass
// verifies it by content — a write never fails for a stamp.
func statStamp(path string) *fileStamp {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &fileStamp{mtimeNS: st.ModTime().UnixNano(), size: st.Size()}
}

// stampArgs renders a stamp as two SQL values (NULL when unstamped).
func stampArgs(st *fileStamp) (any, any) {
	if st == nil {
		return nil, nil
	}
	return st.mtimeNS, st.size
}

// Entry is the memories row as served.
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

	apath string `json:"-"` // authority_path, internal

	// fsStamp is the row's freshness stamp (polyforge#129); nil when the
	// row is unstamped (a pre-#129 row, or a write that could not stat
	// the file) and the reconcile must verify it by content.
	fsStamp *fileStamp `json:"-"`
}

// Store is the memory subsystem.
type Store struct {
	db      *core.DB
	dataDir string

	// ReconcileProgress, when set, receives periodic progress during
	// O(library) reconcile passes (polyforge#129: a long pass — first
	// boot after the upgrade, index repair — must be
	// distinguishable from a hang). Set it before the first RebuildIndex
	// call; it is invoked only from the goroutine running the pass.
	ReconcileProgress func(ProgressReport)

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-mem_id write mutex
}

func NewStore(db *core.DB, dataDir string) *Store {
	return &Store{db: db, dataDir: dataDir, locks: map[string]*sync.Mutex{}}
}

func (s *Store) authorityDir(projectID, memID string) string {
	return filepath.Join(s.dataDir, "memory", projectID, memID)
}

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

// tryMemLock is memLock without waiting: it answers false instead of
// blocking when the entry's mutex is already held. The reconcile's
// batched pass uses it to never wait on a per-mem lock while holding an
// open write transaction — the lock holder needs Core's only connection
// for its own write, so blocking there is a deadlock shape (polyforge#129:
// the startup reconcile runs concurrently with serving traffic).
func (s *Store) tryMemLock(memID string) (func(), bool) {
	s.mu.Lock()
	l, ok := s.locks[memID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[memID] = l
	}
	s.mu.Unlock()
	if !l.TryLock() {
		return nil, false
	}
	return l.Unlock, true
}

// CreateRequest is the POST /api/v1/memories body.
type CreateRequest struct {
	Project        string   `json:"project"`
	Type           string   `json:"type"`
	Title          *string  `json:"title"`
	Visibility     string   `json:"visibility"`
	Content        string   `json:"content"`
	Tags           []string `json:"tags"`
	Related        []string `json:"related"`
	WorkItemID     *string  `json:"work_item_id"`
	IdempotencyKey *string  `json:"idempotency_key"`
}

// Create runs the write order: ① tmp+fsync, ② one Core tx (row + FTS),
// ③ rename over the authority file. Between ② and ③ no half-committed
// state is ever served (INV-7f); crash residue is swept by reconcile.
func (s *Store) Create(req CreateRequest, owner string) (*Entry, error) {
	if strings.TrimSpace(req.Content) == "" {
		return nil, errf(api.ErrMemBadInput, "", "content required")
	}
	if req.Type == "" {
		return nil, errf(api.ErrMemBadInput, "", "type required")
	}
	if !visibilities[req.Visibility] {
		return nil, errf(api.ErrMemBadInput, "", "bad visibility %q", req.Visibility)
	}
	if len(req.Content) > maxContentBytes {
		return nil, errf(api.ErrMemTooLarge, "", "content exceeds 1 MiB")
	}
	var projectID string
	if err := s.db.QueryRow(`SELECT id FROM projects WHERE name=?1 OR id=?1`, req.Project).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf(api.ErrMemNotFound, "", "project %q", req.Project)
		}
		return nil, err
	}
	// Idempotent create: same key + owner + project returns the original.
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		var existing, proj, own string
		qerr := s.db.QueryRow(`SELECT id, project_id, owner_user_id FROM memories WHERE idempotency_key=?1`,
			*req.IdempotencyKey).Scan(&existing, &proj, &own)
		if qerr == nil {
			if proj != projectID || own != owner {
				return nil, errf(api.ErrMemConflict, "IDEMPOTENCY_KEY_REUSED",
					"idempotency_key belongs to another owner or project")
			}
			return s.Get(existing)
		}
		if !errors.Is(qerr, sql.ErrNoRows) {
			return nil, qerr
		}
	}
	// Client frontmatter is allowed to seed fields, but never the id.
	var body string
	if block, rest, ok := splitFrontmatter(req.Content); ok {
		raw, err := parseFrontmatterBlock(block)
		if err != nil {
			return nil, err
		}
		if _, has := raw["id"]; has {
			return nil, errf(api.ErrMemBadInput, "CLIENT_ID_NOT_ALLOWED", "content frontmatter must not carry an id")
		}
		body = rest
	} else {
		body = req.Content
	}
	memID, err := core.NewID("mem_")
	if err != nil {
		return nil, err
	}
	unlock := s.memLock(memID)
	defer unlock()
	now := core.Now()
	fm := &frontmatter{
		ID: memID, Type: req.Type, OwnerUserID: owner, Title: req.Title,
		Tags: req.Tags, Visibility: req.Visibility, WorkItemID: req.WorkItemID,
		ProjectID: projectID, IdempotencyKey: req.IdempotencyKey, Related: req.Related,
		CreatedAt: now, UpdatedAt: now,
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
		return nil, errf(api.ErrMemTooLarge, "", "authority document exceeds 1 MiB")
	}
	dir := s.authorityDir(projectID, memID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errf(api.ErrMemInternal, "", "mkdir: %v", err)
	}
	tmp := filepath.Join(dir, "memory.md.tmp")
	authority := filepath.Join(dir, "memory.md")
	// ①
	if err := writeTmpSync(tmp, authorityBytes); err != nil {
		return nil, errf(api.ErrMemInternal, "", "write authority: %v", err)
	}
	contentSHA := shaHex(authorityBytes)
	// Freshness stamp (polyforge#129): the tmp file's mtime+size ARE the
	// authority file's state after step ③ (rename preserves mtime), so
	// the stamp is taken now and commits together with the row — before
	// the rename, never after.
	fsStamp := statStamp(tmp)
	// ②
	rel := "memory/" + projectID + "/" + memID
	var idem any
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		idem = *req.IdempotencyKey
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	fsM, fsS := stampArgs(fsStamp)
	if _, err := tx.Exec(`INSERT INTO memories(id, project_id, owner_user_id, work_item_id, type, title,
		visibility, authority_path, content_sha, content_len, tags, related, idempotency_key, status, created_at, updated_at,
		fs_mtime_ns, fs_size)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,'active',?14,?14,?15,?16)`,
		memID, projectID, owner, anyPtr(req.WorkItemID), req.Type, anyPtr(req.Title), req.Visibility, rel,
		contentSHA, len(authorityBytes), jsonOrEmpty(req.Tags), jsonOrEmpty(req.Related), idem, now,
		fsM, fsS); err != nil {
		return nil, err
	}
	if err := ftsReplace(tx, memID, extractBody(string(authorityBytes))); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// ③
	if err := os.Rename(tmp, authority); err != nil {
		return nil, errf(api.ErrMemInternal, "", "rename authority: %v", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, errf(api.ErrMemInternal, "", "fsync authority dir: %v", err)
	}
	return s.load(memID)
}

// Get serves one entry through the INV-7f gate.
func (s *Store) Get(memID string) (*Entry, error) {
	unlock := s.memLock(memID)
	defer unlock()
	return s.validatedLocked(memID)
}

// Content returns the authority Markdown bytes under the same lock hold
// that validated them.
func (s *Store) Content(memID string) ([]byte, error) {
	unlock := s.memLock(memID)
	defer unlock()
	e, err := s.validatedLocked(memID)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(s.dataDir, filepath.FromSlash(e.apath), "memory.md"))
}

func (s *Store) load(memID string) (*Entry, error) {
	var e Entry
	var wi, title sql.NullString
	var tags, related string
	var fsM, fsS sql.NullInt64
	err := s.db.QueryRow(`SELECT id, project_id, owner_user_id, work_item_id, type, title, visibility,
		authority_path, content_sha, content_len, tags, related, status, created_at, updated_at,
		fs_mtime_ns, fs_size
		FROM memories WHERE id=?1`, memID).
		Scan(&e.ID, &e.ProjectID, &e.OwnerUserID, &wi, &e.Type, &title, &e.Visibility,
			&e.apath, &e.ContentSHA, &e.ContentLen, &tags, &related, &e.Status, &e.CreatedAt, &e.UpdatedAt,
			&fsM, &fsS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf(api.ErrMemNotFound, "", "memory %s", memID)
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
	if fsM.Valid && fsS.Valid {
		e.fsStamp = &fileStamp{mtimeNS: fsM.Int64, size: fsS.Int64}
	}
	return &e, nil
}

// Search runs the real-time text query: FTS5 trigram for ≥3-rune tokens,
// substring widening for shorter tokens (kept from v0; a trigram index
// cannot answer <3-rune queries, which are the common CJK shape).
// memType filters by memory type: a value ending in ".*" is a prefix
// match (X.* → prefix "X."), any other value is an exact match.
// allowed filters candidates (membership/visibility, applied by the
// server layer).
func (s *Store) Search(q string, memType string, tags []string, limit int, allowed func(*Entry) bool) ([]Entry, error) {
	return s.SearchMode(MatchAll, "", q, memType, tags, limit, allowed)
}

// SearchMode is the search face's single entry: match picks the mode
// (MatchAll / "" — Search's every-token AND; MatchAny — SearchAny's
// ranked any-term OR; anything else is MEM-400). project, when
// non-empty, is a project id narrowing the candidates INSIDE the FTS /
// scan SQL — a pure narrowing (the caller's allowed predicate still
// enforces membership, visibility and the same project), so the answer
// is identical to filtering afterwards, minus the per-candidate load
// of every other project's matches.
func (s *Store) SearchMode(match, project, q, memType string, tags []string, limit int, allowed func(*Entry) bool) ([]Entry, error) {
	switch match {
	case "", MatchAll, MatchAny:
	default:
		return nil, errf("MEM-400", "", "match must be %q or %q", MatchAll, MatchAny)
	}
	if strings.TrimSpace(q) == "" {
		return nil, errf(api.ErrMemBadInput, "", "q required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	f := searchFilter{project: project, memType: memType, tags: tags, limit: limit, allowed: allowed}
	if match == MatchAny {
		return s.searchAny(q, f)
	}
	var long, short []string
	for _, tok := range strings.Fields(q) {
		if utf8.RuneCountInString(tok) >= 3 {
			long = append(long, tok)
		} else {
			short = append(short, tok)
		}
	}
	fts := ""
	if len(long) > 0 {
		fts = ftsQuery(long)
	}
	return s.collect(fts, short, 0, f)
}

// searchFilter is one search's post-MATCH filter set plus its limit.
type searchFilter struct {
	project string // project id narrowing the SQL candidates; "" = all
	memType string
	tags    []string
	limit   int
	allowed func(*Entry) bool
}

// collect answers the filtered hits for one search. match is the FTS5
// MATCH expression ("" = no FTS step: every active body is scanned);
// short tokens must each appear as a substring of the indexed body.
// maxCands > 0 bounds the FTS candidates examined in total (one
// rank-ordered window, no further paging); 0 pages until the limit is
// filled or the matches run out.
func (s *Store) collect(match string, short []string, maxCands int, f searchFilter) ([]Entry, error) {
	memType, tags, limit, allowed := f.memType, f.tags, f.limit, f.allowed
	out := []Entry{}
	// seen: a write between two FTS pages can shift ranks, so the same
	// entry may appear on consecutive pages — it is answered once.
	seen := map[string]bool{}
	// take filters one candidate batch into out; true once out is full.
	take := func(cands []candRow) bool {
		for _, c := range cands {
			if seen[c.id] {
				continue
			}
			seen[c.id] = true
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
			if err != nil || e.Status != "active" {
				continue
			}
			e, ok := s.serveConsistent(e)
			if !ok {
				continue
			}
			if !typeMatches(e.Type, memType) {
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
				return true
			}
		}
		return false
	}
	if match == "" {
		scanned, serr := s.scanBodies(f.project)
		if serr != nil {
			return nil, serr
		}
		take(scanned)
		return out, nil
	}
	// A bounded search examines one best-ranked window and stops there.
	if maxCands > 0 {
		cands, err := s.ftsPage(match, f.project, maxCands, 0)
		if err != nil {
			return nil, err
		}
		take(cands)
		return out, nil
	}
	// FTS candidates are paged in rank order until limit entries survive
	// the filters or the matches run out: the type/tag/membership/project
	// filters run after the MATCH, so one fixed-size candidate window
	// could silently answer fewer than limit hits while more exist.
	batch := limit * 4
	for offset := 0; ; offset += batch {
		cands, err := s.ftsPage(match, f.project, batch, offset)
		if err != nil {
			return nil, err
		}
		if take(cands) || len(cands) < batch {
			return out, nil
		}
	}
}

// ftsPage reads one rank-ordered page of FTS candidates.
// project, when non-empty, keeps only that project's rows.
func (s *Store) ftsPage(match, project string, limit, offset int) ([]candRow, error) {
	rows, err := s.db.Query(`SELECT f.mem_id, f.body FROM memories m
		JOIN memory_fts f ON f.mem_id = m.id
		WHERE memory_fts MATCH ?1 AND m.status='active'
		AND (?4 = '' OR m.project_id = ?4)
		ORDER BY rank LIMIT ?2 OFFSET ?3`, match, limit, offset, project)
	if err != nil {
		// A typed refusal, never a degrade to a full scan: answering
		// every memory for a query FTS could not parse is a leak.
		return nil, errf(api.ErrMemUnavailable, "", "fts: %v", err)
	}
	defer rows.Close()
	var out []candRow
	for rows.Next() {
		var c candRow
		if err := rows.Scan(&c.id, &c.body); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// typeMatches answers the type filter for one entry. An empty filter
// passes every type. A value ending in ".*" is a prefix match on "X.":
// experience.* matches experience.debug and experience.approach but
// not fact.note (and not the bare type experience). Any other value
// stays an exact match. No whitelist — a new type prefix filters as
// soon as memories carry it.
func typeMatches(entryType, memType string) bool {
	if memType == "" {
		return true
	}
	if prefix, ok := strings.CutSuffix(memType, ".*"); ok {
		return strings.HasPrefix(entryType, prefix+".")
	}
	return entryType == memType
}

// candRow is one search candidate (mem id + indexed body).
type candRow struct{ id, body string }

// scanBodies returns every active entry's indexed body for substring
// filtering (the short-token path); project, when non-empty, keeps only
// that project's rows.
func (s *Store) scanBodies(project string) ([]candRow, error) {
	rows, err := s.db.Query(`SELECT f.mem_id, f.body FROM memories m
		JOIN memory_fts f ON f.mem_id = m.id WHERE m.status='active'
		AND (?1 = '' OR m.project_id = ?1)`, project)
	if err != nil {
		return nil, errf(api.ErrMemUnavailable, "", "scan: %v", err)
	}
	var out []candRow
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, candRow{id, body})
	}
	rows.Close()
	return out, nil
}

// ftsQuery ANDs the tokens as quoted phrases for the trigram MATCH.
func ftsQuery(tokens []string) string { return ftsJoin(tokens, " AND ") }

// ftsJoin joins the tokens with op (" AND " / " OR ") as quoted phrases.
// Every token is an FTS5 string literal (embedded `"` doubled), so
// operator characters such as - : * ( ) never reach the FTS5 expression
// parser.
func ftsJoin(tokens []string, op string) string {
	parts := make([]string, len(tokens))
	for i, p := range tokens {
		parts[i] = `body:"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, op)
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

// redactedRelOf maps an entry's recorded placement to its _redacted
// destination.
func redactedRelOf(e *Entry) string {
	rel := filepath.ToSlash(e.apath)
	i := strings.LastIndexByte(rel, '/')
	return rel[:i] + "/_redacted/" + rel[i+1:]
}

// Redact soft-deletes: ① rename the authority dir into _redacted/, ② one
// Core tx (status + FTS removal). A crash between ① and ② is
// forward-completed by reconcile from the file position.
func (s *Store) Redact(memID string) error {
	unlock := s.memLock(memID)
	defer unlock()
	e, err := s.load(memID)
	if err != nil {
		return err
	}
	if e.Status != "active" {
		return errf(api.ErrMemConflict, "", "memory is %s", e.Status)
	}
	if !s.entryConsistent(e) {
		// Crash residue on the entry being redacted: resolve it under the
		// lock we already hold before moving anything.
		if _, serr := os.Stat(filepath.Join(s.dataDir, filepath.FromSlash(redactedRelOf(e)), "memory.md")); serr == nil {
			if ferr := s.forwardCompleteRedact(e); ferr != nil {
				return ferr
			}
			return errf(api.ErrMemConflict, "", "memory is redacted")
		}
		if derr := s.deleteOrphanRow(memID); derr != nil {
			return derr
		}
		return errf(api.ErrMemNotFound, "", "memory %s", memID)
	}
	// ① file move (the file location IS the authoritative state).
	src := filepath.Join(s.dataDir, filepath.FromSlash(e.apath))
	dst := filepath.Join(s.dataDir, filepath.FromSlash(redactedRelOf(e)))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return errf(api.ErrMemInternal, "", "redact move: %v", err)
	}
	// ② metadata + FTS removal in one tx.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET status='redacted', updated_at=?1 WHERE id=?2`, core.Now(), memID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, memID); err != nil {
		return err
	}
	return tx.Commit()
}

// ftsReplace enforces the delete-then-insert invariant: each mem_id has
// at most one retrievable row, consistent with the latest authority.
func ftsReplace(tx *sql.Tx, memID, body string) error {
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE mem_id=?1`, memID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO memory_fts(mem_id, body) VALUES (?1, ?2)`, memID, body)
	return err
}

func anyPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func jsonOrEmpty(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
