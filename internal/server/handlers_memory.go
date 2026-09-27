package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/memory"
)

// E group (rows 22–25). The HTTP surface is 02's; the 06 semantics live
// in internal/memory: three-step write order under the shared backupBarrier
// (06 §5.2), HTML derivation with the double-digest staleness check
// (06 §4.3), short-query retrieval widening, and the A18 share ruling.

func (s *Server) memoryAllowed(idn *identity, projectID string) func(*memory.Entry) bool {
	return func(e *memory.Entry) bool {
		if idn.Role == "admin" {
			return true
		}
		if e.Visibility == "private" {
			return e.OwnerUserID == idn.UserID
		}
		return true // membership already enforced at the project gate
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

// hGetMemory serves row 22 GET: Markdown authority + metadata; with
// format=html the DERIVED view (06 §4.3) — the memory store applies the
// source_markdown_sha/html_sha staleness check and re-derives if stale
// (INV-7d), so a stale or out-of-band-edited memory.html is never served.
func (s *Server) hGetMemory(w http.ResponseWriter, r *http.Request) {
	e, _, ok := s.memForRead(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	content, err := s.Mems.Content(e.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := map[string]any{"entry": e, "content": string(content)}
	if r.URL.Query().Get("format") == "html" {
		html, err := s.Mems.HTML(e.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
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
	e, idn, ok := s.memForRead(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if idn.Role != "admin" && e.OwnerUserID != idn.UserID {
		writeErr(w, ownerAPI("MEM-403", "share is owner-only"))
		return
	}
	secret, err := s.shareKey()
	if err != nil {
		writeErr(w, err)
		return
	}
	// Stateless token per the recorded ruling in memory.Store.Share (06
	// §5.1 A18 pins no persistence/revocation for tokens).
	token, err := s.Mems.Share(e.ID, secret)
	writeResult(w, err, func() {
		writeJSON(w, http.StatusOK, map[string]any{"share_token": token})
	})
}

// hConsumeShare serves GET /api/v1/share/{token} anonymously: read-only view
// of exactly one memory, no list, no search (row 24, A18).
func (s *Server) hConsumeShare(w http.ResponseWriter, r *http.Request) {
	secret, err := s.shareKey()
	if err != nil {
		writeErr(w, err)
		return
	}
	memID, err := s.Mems.ResolveShare(r.PathValue("token"), secret)
	if err != nil {
		writeErr(w, err)
		return
	}
	e, err := s.Mems.Get(memID)
	if err != nil {
		writeErr(w, err)
		return
	}
	content, err := s.Mems.Content(memID)
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

// shareKey derives the stateless share-signing key from the CURRENT service
// secret epoch (fix #4: previously a hardcoded public string "pf-share-"+epoch,
// which anyone holding a mem_id could reproduce to forge an anonymous share
// token; it is now HMAC-derived from the actual rotatable secret file, which
// only the aihub process can read).
func (s *Server) shareKey() ([]byte, error) {
	epoch, err := s.Svc.CurrentEpoch()
	if err != nil {
		return nil, err
	}
	return s.Svc.ShareKey(epoch)
}
