package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/memory"
)

// E group (rows 22–25). The HTTP surface is 02's; the 06 semantics live
// in internal/memory: three-step write order under the shared backupBarrier
// (06 §5.2), HTML derivation with the double-digest staleness check
// (06 §4.3), short-query retrieval widening, and the A18 share ruling.

// memoryAllowed applies the 06 §7 phase-1 visibility matrix INSIDE the
// project-eligible candidate set. 06 §7 is strictly TWO layers, and the
// admin role exception belongs to the FIRST layer only:
//
//	“第一层按 project 访问资格过滤：候选集 = 调用方有 membership 的
//	project ∪ 全局 admin 可见的全部 project … 第二层才在候选集内
//	应用可见性分级”
//
// The second layer (the matrix below) knows no admin exception:
//
//	“private = 仅 owner_user_id（创建者…）可读；… admin = 仅
//	users.role='admin' 可读”
//
// — private is owner-only for EVERYONE, a global admin included; the
// admin ROLE only buys the project-membership tier 'admin'. The project
// gate has already filtered by membership OR global admin before this
// predicate runs; team ≡ project in phase 1.
func (s *Server) memoryAllowed(idn *identity, projectID string) func(*memory.Entry) bool {
	return func(e *memory.Entry) bool {
		switch e.Visibility {
		case "private":
			// 06 §7 matrix: private = owner only — for admins too. The admin
			// role is a layer-1 (project eligibility) exception only and
			// never reaches into the visibility matrix.
			return e.OwnerUserID == idn.UserID
		case "admin":
			// 06 §7: the admin tier is readable by users.role='admin' only.
			return idn.Role == "admin"
		}
		// project|team|public: readable by the whole candidate set
		// (membership already enforced at the project gate).
		return true
	}
}

func (s *Server) hCreateMemory(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req memory.CreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1`, req.Project).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("MEM-404", "project "+req.Project))
		return
	}
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canWrite(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, ownerAPI("CORE-403", "forbidden"))
		return
	}
	e, err := s.Mems.Create(req, idn.UserID)
	writeResult(w, err, func() { writeJSON(w, http.StatusCreated, e) })
}

// memForRead authorizes the METADATA-ONLY memory routes (PATCH/redact/
// share: they decide on owner/scope/project, all immutable-by-PATCH
// fields, so a concurrent write cannot invalidate the decision). The
// content-disclosing detail GET does NOT use it anymore: it authorizes
// and reads from one snapshot via Mems.ReadView (R2C-F3, hGetMemory).
func (s *Server) memForRead(w http.ResponseWriter, r *http.Request, memID string) (*memory.Entry, *identity, bool) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	e, err := s.Mems.Get(memID)
	if err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	// Key scope first (02 §8: enforced on every route before any object
	// logic; a scoped key must see nothing of out-of-scope memories even
	// when its user is a member or a global admin — scope only narrows).
	if err := s.requireScope(idn, e.ProjectID); err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	ok, err := s.canRead(idn, e.ProjectID)
	if err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	if !ok || !s.memoryAllowed(idn, e.ProjectID)(e) {
		writeErr(w, ownerAPI("MEM-404", "memory "+memID))
		return nil, nil, false
	}
	return e, idn, true
}

// memForShare authorizes the SHARE route (06 §5.1 A18, 02 row 24).
// R3C-F4: share is an explicit owner-authorized grant channel OUTSIDE the
// visibility tiers — “分享链接只能由该条 Memory 的 owner 经显式动作生成”
// — so the ordinary read-tier predicate must not gate it. Going through
// memForRead rejected a non-admin OWNER of a visibility=admin entry at
// the admin-tier predicate before ownership was ever considered, so the
// one principal the A18 ruling names could never mint a share. The
// authorization here is key scope + project membership + OWNERSHIP:
//
//   - the OWNER passes even for admin-tier (and private) entries;
//   - a non-owner who could not have read the entry is refused with the
//     no-existence-leak MEM-404 (the same code the detail route answers);
//   - a non-owner who could read it reaches the caller's owner-only
//     check and is refused MEM-403 there — including a non-owner admin,
//     per the A18 owner-only ruling (02 row 24).
func (s *Server) memForShare(w http.ResponseWriter, r *http.Request, memID string) (*memory.Entry, *identity, bool) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	e, err := s.Mems.Get(memID)
	if err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	// Key scope first (02 §8: enforced on every route before any object
	// logic; a scoped key must see nothing of out-of-scope memories even
	// when its user is a member or a global admin — scope only narrows).
	if err := s.requireScope(idn, e.ProjectID); err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	ok, err := s.canRead(idn, e.ProjectID)
	if err != nil {
		writeErr(w, err)
		return nil, nil, false
	}
	if !ok || (e.OwnerUserID != idn.UserID && !s.memoryAllowed(idn, e.ProjectID)(e)) {
		// A caller who could not have read the entry learns nothing of its
		// existence from a share attempt: the same MEM-404 the read routes
		// answer. The owner is exempt from the tier screen (A18 exception).
		writeErr(w, ownerAPI("MEM-404", "memory "+memID))
		return nil, nil, false
	}
	return e, idn, true
}

// hGetMemory serves row 22 GET: Markdown authority + metadata; with
// format=html the DERIVED view (06 §4.3) — the memory store applies the
// source_markdown_sha/html_sha staleness check and re-derives if stale
// (INV-7d), so a stale or out-of-band-edited memory.html is never served.
//
// R2C-F3: authorization and bytes come from ONE snapshot. The previous
// shape authorized against memForRead's Get metadata (lock window 1) and
// then read Content/HTML under separate windows — a concurrent owner
// PATCH to visibility=private with a new body in between disclosed the
// NEW private body to a non-owner. The tier decision now runs inside the
// store's single memLock hold (Mems.ReadView), against the exact entry
// whose bytes are served; scope (CORE-403) precedes membership and the
// tier matrix exactly as memForRead ordered them.
func (s *Server) hGetMemory(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wantHTML := r.URL.Query().Get("format") == "html"
	e, content, html, err := s.Mems.ReadView(r.PathValue("id"), func(e *memory.Entry) (bool, error) {
		// Key scope first (02 §8: enforced on every route before any object
		// logic; a scoped key must see nothing of out-of-scope memories even
		// when its user is a member or a global admin — scope only narrows).
		if serr := s.requireScope(idn, e.ProjectID); serr != nil {
			return false, serr
		}
		ok, rerr := s.canRead(idn, e.ProjectID)
		if rerr != nil {
			return false, rerr
		}
		if !ok {
			return false, nil
		}
		return s.memoryAllowed(idn, e.ProjectID)(e), nil
	}, wantHTML)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := map[string]any{"entry": e, "content": string(content)}
	if wantHTML {
		// Inference (recorded): memory.html holds the rendered body
		// fragment; the consuming UI wraps it in its own document shell.
		resp["html"] = string(html)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) hListMemories(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	projectName := r.URL.Query().Get("project")
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1`, projectName).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("MEM-404", "project "+projectName))
		return
	}
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canRead(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, ownerAPI("MEM-404", "project "+projectName))
		return
	}
	items, err := s.Mems.List(projectID, s.memoryAllowed(idn, projectID))
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"items": items}) })
}

func (s *Server) hPatchMemory(w http.ResponseWriter, r *http.Request) {
	e, idn, ok := s.memForRead(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if idn.Role != "admin" && e.OwnerUserID != idn.UserID {
		writeErr(w, ownerAPI("MEM-403", "only the owner may update"))
		return
	}
	var body struct {
		Content string   `json:"content"`
		Title   *string  `json:"title"`
		Tags    []string `json:"tags"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	upd, err := s.Mems.Update(e.ID, memory.UpdateRequest{
		Content: body.Content, Title: body.Title, Tags: body.Tags,
	})
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, upd) })
}

func (s *Server) hSearchMemories(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	projectName := q.Get("project")
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1`, projectName).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("MEM-404", "project "+projectName))
		return
	}
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canRead(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, ownerAPI("MEM-404", "project "+projectName))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	tags := []string{}
	if t := q.Get("tags"); t != "" {
		tags = strings.Split(t, ",")
	}
	// Membership-first (02 §4.10): restrict candidates to this project, then
	// apply visibility tiers inside it.
	items, err := s.Mems.Search(q.Get("q"), q.Get("type"), tags, limit,
		func(e *memory.Entry) bool {
			return e.ProjectID == projectID && s.memoryAllowed(idn, projectID)(e)
		})
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"items": items}) })
}

func (s *Server) hRedactMemory(w http.ResponseWriter, r *http.Request) {
	e, idn, ok := s.memForRead(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if idn.Role != "admin" && e.OwnerUserID != idn.UserID {
		writeErr(w, ownerAPI("MEM-403", "only the owner may redact"))
		return
	}
	if err := s.Mems.Redact(e.ID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) hShareMemory(w http.ResponseWriter, r *http.Request) {
	// R3C-F4: memForShare, NOT memForRead — share authorization is
	// scope/project + OWNERSHIP (the A18 owner-authorized exception),
	// independently of the ordinary read-tier predicate.
	e, idn, ok := s.memForShare(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// 06 §5.1 A18 ruling, owner-only with NO admin exception: “分享链接
	// 只能由该条 Memory 的 owner 经显式动作生成—— POST …/share，非
	// owner 调用返回 MEM-403”（02 row 24: callers “share 仅该条 Memory
	// 的 owner”）. Share is an owner-explicit grant channel outside the
	// visibility tiers; a non-owner admin therefore cannot mint a share
	// link for somebody else's entry.
	if e.OwnerUserID != idn.UserID {
		writeErr(w, ownerAPI("MEM-403", "share is owner-only"))
		return
	}
	// FB-10 fix (02 row 24: the token is RANDOMLY issued at share-generation
	// time and non-enumerable). The previous construction — mem_id +
	// "." + HMAC(service secret, mem_id) — was deterministic: every
	// issuance of the same memory produced the identical token. The token
	// keeps the <mem_id> "." <22 lowercase hex> shape, but the suffix is now
	// 11 crypto/rand bytes (hex-encoded), and the sha256 of the full token
	// is persisted in share_tokens so the anonymous consume route can
	// resolve it. Only the hash is stored: a DB read yields no usable token.
	// 06 §5.1 (A18) pins no revocation; redaction hides the entry itself
	// (Mems.Get 404s non-active rows), which kills consumption.
	suffix, err := newSecret(11)
	if err != nil {
		writeErr(w, err)
		return
	}
	token := e.ID + "." + suffix
	if _, err := s.DB.Exec(`INSERT INTO share_tokens(token_hash, mem_id, created_at) VALUES (?1,?2,?3)`,
		sha256Hex(token), e.ID, nowStamp()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"share_token": token})
}

// hConsumeShare serves GET /api/v1/share/{token} anonymously: read-only view
// of exactly one memory, no list, no search (row 24, A18). The middleware
// skips api_key/membership for this path by design; the token resolves to
// at most the one memory it was issued for and leaks no other object's
// existence (unknown or malformed tokens answer MEM-404). Entry and bytes
// come from the same single-snapshot read as the authorized detail route
// (R2C-F3): the share grant is the token itself, so no predicate is needed,
// but the entry/bytes pair still cannot straddle a concurrent write.
func (s *Server) hConsumeShare(w http.ResponseWriter, r *http.Request) {
	memID, err := s.resolveShareToken(r.PathValue("token"))
	if err != nil {
		writeErr(w, err)
		return
	}
	e, content, _, err := s.Mems.ReadView(memID, nil, false)
	writeResult(w, err, func() {
		writeJSON(w, http.StatusOK, map[string]any{"entry": e, "content": string(content)})
	})
}

func (s *Server) hRebuildIndex(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" {
		writeErr(w, ownerAPI("CORE-403", "forbidden: admin only"))
		return
	}
	n, err := s.Mems.RebuildIndex()
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"rebuilt": n}) })
}

// resolveShareToken maps a share token back to its memory: the token must
// carry the issued shape (<mem_id> "." <22 lowercase hex>) and its sha256
// must be a stored issuance. Anything else is MEM-404 — no existence
// information beyond the single shared entry leaks (02 row 24).
func (s *Server) resolveShareToken(token string) (string, error) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 || len(token)-i-1 != 22 {
		return "", ownerAPI("MEM-404", "bad share token")
	}
	for _, c := range token[i+1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", ownerAPI("MEM-404", "bad share token")
		}
	}
	var memID string
	err := s.DB.QueryRow(`SELECT mem_id FROM share_tokens WHERE token_hash=?1`, sha256Hex(token)).Scan(&memID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ownerAPI("MEM-404", "bad share token")
	}
	if err != nil {
		return "", err
	}
	return memID, nil
}

// ensureShareTokens installs the server-owned share issuance table at boot.
// The share surface is an HTTP-face concern (02 row 24); the table holds
// only token hashes — never tokens — so the authority store stays the
// single place a raw share token exists.
func (s *Server) ensureShareTokens() error {
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS share_tokens (
		token_hash TEXT PRIMARY KEY,
		mem_id     TEXT NOT NULL,
		created_at TEXT NOT NULL)`)
	return err
}
