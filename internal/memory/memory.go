// Package memory implements the Core-side Memory metadata store and the
// rows 22–25 endpoint surface. 02-aihub-core.md §4.9 defines the tables, the
// authority-file layout (§4.11) and the delete-then-insert FTS invariant;
// the FULL semantic authority (three-step write order with backupBarrier,
// HTML derivation rules, rebuild tooling, migration import) is
// 06-memory-search.md and lands in the Memory-domain work item — the parts
// here that go beyond 02 are marked 06-deferred and re-verified there.
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
		return fmt.Sprintf("%s %s: %s", e.Code, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func errf(code, reason, format string, args ...any) *APIError {
	return &APIError{Code: code, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Visibility tiers (06 §2.1 vocabulary; mechanical membership-first filtering
// is applied by the server layer per 02 §4.10).
var visibilities = map[string]bool{"admin": true, "private": true, "project": true, "public": true, "team": true}

// Entry is the memories row as served by the API.
type Entry struct {
	ID          string          `json:"id"`
	ProjectID   string          `json:"project_id"`
	OwnerUserID string          `json:"owner_user_id"`
	WorkItemID  *string         `json:"work_item_id"`
	Type        string          `json:"type"`
	Title       *string         `json:"title"`
	Visibility  string          `json:"visibility"`
	ContentSHA  string          `json:"content_sha"`
	ContentLen  int64           `json:"content_len"`
	Tags        []string        `json:"tags"`
	Related     []string        `json:"related"`
	Status      string          `json:"status"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`

	apath string `json:"-"` // authority_path column, internal
}

// Store is the memory subsystem store.
type Store struct {
	db      *store.DB
	dataDir string
}

func NewStore(db *store.DB, dataDir string) *Store {
	return &Store{db: db, dataDir: dataDir}
}

func (s *Store) authorityDir(projectID, memID string) string {
	return filepath.Join(s.dataDir, "memory", projectID, memID)
}

// CreateRequest is the POST /api/v1/memories body.
type CreateRequest struct {
	Project       string   `json:"project"` // project name
	Type          string   `json:"type"`
	Title         *string  `json:"title"`
	Visibility    string   `json:"visibility"`
	Content       string   `json:"content"`
	Tags          []string `json:"tags"`
	Related       []string `json:"related"`
	WorkItemID    *string  `json:"work_item_id"`
	IdempotencyKey *string `json:"idempotency_key"`
}

// Create writes the authority Markdown and the metadata row + FTS entry.
// 06-deferred: the authoritative three-step write order under a single
// backupBarrier (06 §2.3 INV-7a) is refined in the Memory work item; the
// invariant held here is the one 02 §4.9 pins: FTS delete-then-insert inside
// the same transaction as the row write.
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

	memID, err := store.NewID("mem_")
	if err != nil {
		return nil, err
	}
	dir := s.authorityDir(projectID, memID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errf("MEM-500", "", "mkdir: %v", err)
	}
	authority := filepath.Join(dir, "memory.md")
	if err := atomicWrite(authority+".tmp", authority, []byte(req.Content)); err != nil {
		return nil, errf("MEM-500", "", "write authority: %v", err)
	}
	sum := sha256.Sum256([]byte(req.Content))

	now := store.Now()
	tags, _ := json.Marshal(req.Tags)
	if req.Tags == nil {
		tags = []byte("[]")
	}
	related, _ := json.Marshal(req.Related)
	if req.Related == nil {
		related = []byte("[]")
	}
	rel := "memory/" + projectID + "/" + memID // authority_path: data-dir-relative
	var idem any
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		idem = *req.IdempotencyKey
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var wiAny any
	if req.WorkItemID != nil {
		wiAny = *req.WorkItemID
	}
	if _, err := tx.Exec(`INSERT INTO memories(id, project_id, owner_user_id, work_item_id, type, title,
		visibility, authority_path, content_sha, content_len, tags, related, idempotency_key, status, created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,'active',?14,?14)`,
		memID, projectID, owner, wiAny, req.Type, req.Title, req.Visibility, rel,
		hex.EncodeToString(sum[:]), len(req.Content), string(tags), string(related), idem, now); err != nil {
		return nil, err
	}
	if err := ftsReplace(tx, memID, extractBody(req.Content)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(memID)
}

// Get returns one entry (format=html handled by the server layer).
func (s *Store) Get(memID string) (*Entry, error) {
	e, err := s.load(memID)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Content returns the authority Markdown bytes.
func (s *Store) Content(memID string) ([]byte, error) {
	e, err := s.load(memID)
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
		authority_path, content_sha, content_len, tags, related, status, created_at, updated_at
		FROM memories WHERE id=?1`, memID).
		Scan(&e.ID, &e.ProjectID, &e.OwnerUserID, &wi, &e.Type, &title, &e.Visibility,
			&e.apath, &e.ContentSHA, &e.ContentLen, &tags, &related, &e.Status, &e.CreatedAt, &e.UpdatedAt)
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

// Update rewrites the authority content and replaces the FTS row
// (delete-then-insert in one transaction, 02 §4.9).
func (s *Store) Update(memID, content string, title *string, tags []string) (*Entry, error) {
	e, err := s.load(memID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) == "" {
		return nil, errf("MEM-400", "", "content required")
	}
	if e.Status != "active" {
		return nil, errf("MEM-409", "", "memory is %s", e.Status)
	}
	authority := filepath.Join(s.dataDir, filepath.FromSlash(e.apath), "memory.md")
	if err := atomicWrite(authority+".tmp", authority, []byte(content)); err != nil {
		return nil, errf("MEM-500", "", "write authority: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	var titleAny any
	if title != nil {
		titleAny = *title
	} else if e.Title != nil {
		titleAny = *e.Title
	}
	var tagsBlob any
	if tags != nil {
		b, _ := json.Marshal(tags)
		tagsBlob = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET content_sha=?1, content_len=?2, title=?3, tags=COALESCE(?4, tags), updated_at=?5 WHERE id=?6`,
		hex.EncodeToString(sum[:]), len(content), titleAny, tagsBlob, store.Now(), memID); err != nil {
		return nil, err
	}
	if err := ftsReplace(tx, memID, extractBody(content)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(memID)
}

// Search runs the FTS query and returns candidate entries in rank order.
// Membership-first filtering (project membership before visibility tiers)
// is applied by the server layer per 02 §4.10 / 06 §4.5 — 06-deferred there.
func (s *Store) Search(q string, memType string, tags []string, limit int, allowed func(*Entry) bool) ([]Entry, error) {
	if strings.TrimSpace(q) == "" {
		return nil, errf("MEM-400", "", "q required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT m.id FROM memories m
		JOIN memory_fts f ON f.mem_id = m.id
		WHERE memory_fts MATCH ?1 AND m.status='active'
		ORDER BY rank LIMIT ?2`, ftsQuery(q), limit*4)
	if err != nil {
		return nil, errf("MEM-503", "", "fts: %v", err)
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

// ftsQuery quotes the phrase for MATCH.
func ftsQuery(q string) string {
	parts := strings.Fields(q)
	for i, p := range parts {
		if !strings.ContainsAny(p, ":-*\"()") {
			parts[i] = `"` + p + `"`
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
		if allowed != nil && !allowed(e) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

// Redact soft-deletes: moves the authority dir to _redacted/ and marks the
// row + drops FTS in one transaction (06 §2.3 redact 两步式样 shape).
func (s *Store) Redact(memID string) error {
	e, err := s.load(memID)
	if err != nil {
		return err
	}
	if e.Status != "active" {
		return errf("MEM-409", "", "memory is %s", e.Status)
	}
	src := filepath.Join(s.dataDir, "memory", e.ProjectID, memID)
	dst := filepath.Join(s.dataDir, "memory", e.ProjectID, "_redacted", memID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return errf("MEM-500", "", "redact move: %v", err)
	}
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

// Share issues an opaque, unguessable token for one memory. Stateless form:
// token = mem_id + "." + HMAC(secret-share-key, mem_id) — 06-deferred
// (06 may add persistence/revocation; the v1 shape is verifiable without
// storage and grants read on exactly this one entry).
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

// RebuildIndex re-derives the whole FTS table from the authority files
// (02 §4.9: 派生物可从权威 Markdown 全量重建).
func (s *Store) RebuildIndex() (int, error) {
	rows, err := s.db.Query(`SELECT id, authority_path FROM memories WHERE status='active'`)
	if err != nil {
		return 0, errf("MEM-502", "", "scan: %v", err)
	}
	type item struct{ id, path string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.path); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM memory_fts`); err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		content, err := os.ReadFile(filepath.Join(s.dataDir, filepath.FromSlash(it.path), "memory.md"))
		if err != nil {
			continue // authority missing ⇒ not searchable until repaired
		}
		if err := ftsReplace(tx, it.id, extractBody(string(content))); err != nil {
			return 0, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
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

// extractBody derives the searchable text from the authority Markdown.
// 06-deferred: 06 §4.4 owns the extraction rules; v1 strips a leading YAML
// frontmatter block and searches the remainder verbatim.
func extractBody(content string) string {
	if strings.HasPrefix(content, "---\n") {
		if end := strings.Index(content[4:], "\n---"); end >= 0 {
			return content[4+end+4:]
		}
	}
	return content
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
