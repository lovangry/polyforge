package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/core/workitem"
)

// routes registers the canonical 25-row inventory (02 §5.1) plus M1–M5.
func (s *Server) routes() {
	s.mux = http.NewServeMux()

	// A. WI business (rows 1–6, data plane).
	s.mux.HandleFunc("POST /api/v1/work-items", s.hCreateWI)
	s.mux.HandleFunc("GET /api/v1/work-items", s.hListWIs)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}", s.hGetWI)
	s.mux.HandleFunc("PATCH /api/v1/work-items/{id}", s.hPatchWI)
	s.mux.HandleFunc("GET /api/v1/ready-queue", s.hReadyQueue)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/cancel", s.hCancelWI)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}/progress", s.hProgress)

	// B. Execution right (rows 7–13; no membership filtering, §8).
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/claim", s.hClaim)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/force-takeover", s.hForceTakeover)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/end-generation", s.hEndGeneration)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/unblock", s.hUnblock)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/budget-increments", s.hBudgetIncrement)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}/budget", s.hGetBudget)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}/repo-inventory", s.hRepoInventory)

	// C. Sync (rows 14–20; Daemon publishes, admin deletes).
	s.mux.HandleFunc("PUT /api/v1/work-items/{id}/revisions/{seq}/blocks/{sha}", s.hPutBlock)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/revisions/{seq}/manifest", s.hPostManifest)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}/head", s.hHead)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}/revisions/{seq}/blocks/{sha}", s.hGetBlock)
	s.mux.HandleFunc("DELETE /api/v1/work-items/{id}/capsule-data", s.hTombstone)
	s.mux.HandleFunc("GET /api/v1/work-items/{id}/revisions", s.hRevisionsSince)
	s.mux.HandleFunc("POST /api/v1/work-items/{id}/capsule-data/revert-tombstone", s.hRevertTombstone)

	// D. Machines (row 21).
	s.mux.HandleFunc("POST /api/v1/machines/heartbeat", s.hHeartbeat)

	// E. Memory (rows 22–25; full 06 semantics land in the Memory WI).
	s.mux.HandleFunc("POST /api/v1/memories", s.hCreateMemory)
	s.mux.HandleFunc("GET /api/v1/memories", s.hListMemories)
	s.mux.HandleFunc("GET /api/v1/memories/search", s.hSearchMemories)
	s.mux.HandleFunc("POST /api/v1/memories/rebuild-index", s.hRebuildIndex)
	s.mux.HandleFunc("GET /api/v1/memories/{id}", s.hGetMemory)
	s.mux.HandleFunc("PATCH /api/v1/memories/{id}", s.hPatchMemory)
	s.mux.HandleFunc("POST /api/v1/memories/{id}/redact", s.hRedactMemory)
	s.mux.HandleFunc("POST /api/v1/memories/{id}/share", s.hShareMemory)
	s.mux.HandleFunc("GET /api/v1/share/{token}", s.hConsumeShare) // anonymous read-only

	// F. Management face (M1–M5; admin).
	s.mux.HandleFunc("POST /api/v1/users", s.hCreateUser)
	s.mux.HandleFunc("GET /api/v1/users", s.hListUsers)
	s.mux.HandleFunc("GET /api/v1/users/{id}", s.hGetUser)
	s.mux.HandleFunc("PATCH /api/v1/users/{id}", s.hPatchUser)
	s.mux.HandleFunc("POST /api/v1/api-keys", s.hCreateAPIKey)
	s.mux.HandleFunc("GET /api/v1/api-keys", s.hListAPIKeys)
	s.mux.HandleFunc("DELETE /api/v1/api-keys/{id}", s.hRevokeAPIKey)
	s.mux.HandleFunc("POST /api/v1/projects", s.hCreateProject)
	s.mux.HandleFunc("GET /api/v1/projects", s.hListProjects)
	s.mux.HandleFunc("GET /api/v1/projects/{id}", s.hGetProject)
	s.mux.HandleFunc("PATCH /api/v1/projects/{id}", s.hPatchProject)
	s.mux.HandleFunc("POST /api/v1/admin/backup", s.hBackup)
	s.mux.HandleFunc("POST /api/v1/service/rotate-secret", s.hRotateSecret)
}

// --- A group handlers ---

func (s *Server) hCreateWI(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req createWIRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1`, req.Project).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("CORE-404", "project "+req.Project))
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
		writeErr(w, ownerAPI("CORE-403", "forbidden: no write membership"))
		return
	}
	req.Reporter = idn.UserID
	d, err := s.WIs.Create(req.CreateRequest)
	writeResult(w, err, func() { writeJSON(w, http.StatusCreated, d) })
}

type createWIRequest struct {
	workitem.CreateRequest
}

func (s *Server) hListWIs(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	f := workitem.ListFilters{
		Status:    r.URL.Query().Get("status"),
		Assignee:  r.URL.Query().Get("assignee"),
		WaitingOn: r.URL.Query().Get("waiting_on"),
		Parent:    r.URL.Query().Get("parent"),
		Milestone: r.URL.Query().Get("milestone"),
	}
	projectName := r.URL.Query().Get("project")
	if projectName == "" {
		writeErr(w, ownerAPI("CORE-400", "project= required"))
		return
	}
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1`, projectName).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("CORE-404", "project "+projectName))
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
		writeErr(w, ownerAPI("CORE-404", "project "+projectName))
		return
	}
	f.Project = projectName
	items, err := s.WIs.List(f)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"items": items}) })
}

func (s *Server) hGetWI(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	projectID, aerr := s.wiProject(idn, wiID)
	if aerr != nil {
		writeErr(w, aerr)
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
		writeErr(w, ownerAPI("CORE-404", "work item "+wiID))
		return
	}
	opts := workitem.GetOptions{}
	for _, inc := range strings.Split(r.URL.Query().Get("include"), ",") {
		switch strings.TrimSpace(inc) {
		case "events":
			opts.Events = true
		case "deps":
			opts.Deps = true
		case "children":
			opts.Children = true
		case "":
		default:
			writeErr(w, ownerAPI("CORE-400", "unknown include value "+inc))
			return
		}
	}
	d, err := s.WIs.Get(wiID, opts)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, d) })
}

func (s *Server) hPatchWI(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	projectID, aerr := s.wiProject(idn, wiID)
	if aerr != nil {
		writeErr(w, aerr)
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
	var fields map[string]json.RawMessage
	if err := decodeBody(r, &fields); err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.WIs.Patch(wiID, fields)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, d) })
}

func (s *Server) hReadyQueue(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	projectName := r.URL.Query().Get("project")
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1`, projectName).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("CORE-404", "project "+projectName))
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
		writeErr(w, ownerAPI("CORE-404", "project "+projectName))
		return
	}
	q, err := s.WIs.ReadyQueue(projectName)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, q) })
}

func (s *Server) hCancelWI(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	projectID, aerr := s.wiProject(idn, wiID)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	// §6.5 authorization: reporter OR project maintainer OR admin.
	var reporter string
	if err := s.DB.QueryRow(`SELECT reporter FROM work_items WHERE id=?1`, wiID).Scan(&reporter); err != nil {
		writeErr(w, ownerAPI("CORE-404", "work item "+wiID))
		return
	}
	role, err := s.memberRole(projectID, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" && idn.UserID != reporter && role != "maintainer" {
		writeErr(w, ownerAPI("CORE-403", "forbidden"))
		return
	}
	d, err := s.WIs.Cancel(wiID, idn.UserID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, d) })
}

func (s *Server) hProgress(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	projectID, aerr := s.wiProject(idn, wiID)
	if aerr != nil {
		writeErr(w, aerr)
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
		writeErr(w, ownerAPI("CORE-404", "work item "+wiID))
		return
	}
	seq, gen, updated, body, err := s.Caps.Progress(wiID)
	writeResult(w, err, func() {
		if seq == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"work_item_id": wiID, "head_seq": nil, "head_generation": nil,
				"progress": nil, "head_updated_at": nil,
				"freshness_note": "no published revision yet",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"work_item_id": wiID, "head_seq": *seq, "head_generation": *gen,
			"progress": jsonRaw(body), "head_updated_at": *updated,
			"freshness_note": "数据截至最近一次发布",
		})
	})
}

// --- helpers ---

func ownerAPI(code, detail string) *ownerAPIError { return &ownerAPIError{code: code, detail: detail} }

type ownerAPIError struct {
	code   string
	reason string
	detail string
}

func (e *ownerAPIError) Error() string {
	if e.reason != "" {
		return e.code + " " + e.reason + ": " + e.detail
	}
	return e.code + ": " + e.detail
}

func writeResult(w http.ResponseWriter, err error, ok func()) {
	if err != nil {
		writeErr(w, err)
		return
	}
	ok()
}

func pathSeq(r *http.Request) (int64, error) {
	v := r.PathValue("seq")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, &ownerAPIError{code: "SYNC-400", detail: "bad seq " + v}
	}
	return n, nil
}
