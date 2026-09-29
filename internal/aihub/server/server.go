// Package server is the v1 AIHub HTTP face: the business endpoints
// (v0 openapi surface minus every SYNC-*/revision-chain route), the admin
// face, and the /git/{wi}.git/... proxy mount. Every error is the typed
// common/api errStatus envelope; auth is the Bearer API key.
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/aihub/memory"
	"github.com/silgrid/polyforge/internal/common/api"
)

// Server wires the domain managers behind one mux.
type Server struct {
	DB     *core.DB
	WIs    *core.WIManager
	Owners *core.OwnerManager
	Admin  *core.AdminManager
	Mems   *memory.Store
	Git    *git.Manager

	mux *http.ServeMux
}

// New assembles the server.
func New(db *core.DB, dataDir string, gitMgr *git.Manager) *Server {
	s := &Server{
		DB:     db,
		WIs:    core.NewWIManager(db),
		Owners: core.NewOwnerManager(db),
		Admin:  core.NewAdminManager(db),
		Mems:   memory.NewStore(db, dataDir),
		Git:    gitMgr,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := http.NewServeMux()

	// Business face (02 §5.1 minus SYNC rows; git replaces the chain).
	m.HandleFunc("POST /api/v1/work-items", s.auth(s.hCreateWI))
	m.HandleFunc("GET /api/v1/work-items", s.auth(s.hListWIs))
	m.HandleFunc("GET /api/v1/work-items/{id}", s.auth(s.hGetWI))
	m.HandleFunc("PATCH /api/v1/work-items/{id}", s.auth(s.hPatchWI))
	m.HandleFunc("POST /api/v1/work-items/{id}/cancel", s.auth(s.hCancelWI))
	m.HandleFunc("POST /api/v1/work-items/{id}/claim", s.auth(s.hClaim))
	m.HandleFunc("POST /api/v1/work-items/{id}/end-generation", s.auth(s.hEndGeneration))
	m.HandleFunc("POST /api/v1/work-items/{id}/dependencies", s.auth(s.hAddDep))
	m.HandleFunc("GET /api/v1/work-items/{id}/dependencies", s.auth(s.hDeps))
	m.HandleFunc("DELETE /api/v1/work-items/{id}/dependencies/{blocking}/{kind}", s.auth(s.hDelDep))
	m.HandleFunc("GET /api/v1/projects/{project}/ready-queue", s.auth(s.hReadyQueue))
	m.HandleFunc("POST /api/v1/machines/heartbeat", s.auth(s.hHeartbeat))

	// Memory face.
	m.HandleFunc("POST /api/v1/memories", s.auth(s.hCreateMemory))
	m.HandleFunc("GET /api/v1/memories", s.auth(s.hListMemories))
	m.HandleFunc("POST /api/v1/memories/search", s.auth(s.hSearchMemoryPOST))
	m.HandleFunc("GET /api/v1/memories/search", s.auth(s.hSearchMemory))
	m.HandleFunc("POST /api/v1/memories/rebuild-index", s.authAdmin(s.hRebuildIndex))
	m.HandleFunc("GET /api/v1/memories/{id}", s.auth(s.hGetMemory))
	m.HandleFunc("PATCH /api/v1/memories/{id}", s.auth(s.hPatchMemory))
	m.HandleFunc("POST /api/v1/memories/{id}/redact", s.auth(s.hRedactMemory))

	// Admin face.
	m.HandleFunc("POST /api/v1/users", s.authAdmin(s.hCreateUser))
	m.HandleFunc("GET /api/v1/users", s.authAdmin(s.hListUsers))
	m.HandleFunc("POST /api/v1/api-keys", s.authAdmin(s.hCreateAPIKey))
	m.HandleFunc("DELETE /api/v1/api-keys/{id}", s.authAdmin(s.hRevokeAPIKey))
	m.HandleFunc("POST /api/v1/projects", s.authAdmin(s.hCreateProject))
	m.HandleFunc("GET /api/v1/projects", s.authAdmin(s.hListProjects))
	m.HandleFunc("GET /api/v1/projects/{id}", s.authAdmin(s.hGetProject))
	m.HandleFunc("POST /api/v1/projects/{id}/rotate-identifier", s.authAdmin(s.hRotateIdentifier))

	// Git proxy (own auth: attempt-scoped Basic credential).
	m.Handle("/git/", http.HandlerFunc(s.Git.HandleGit))

	s.mux = m
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---------------------------------------------------------------------------
// Errors and JSON

// writeErr renders the typed envelope from any error shape.
func writeErr(w http.ResponseWriter, err error) {
	switch e := err.(type) {
	case *api.Error:
		writeJSON(w, e.HTTPStatus(), e)
	case *memory.APIError:
		detail := e.Detail
		if e.Reason != "" {
			detail = e.Reason + ": " + e.Detail
		}
		writeJSON(w, (&api.Error{Code: e.Code, Detail: detail}).HTTPStatus(),
			&api.Error{Code: e.Code, Detail: e.Detail,
				Extra: reasonExtra(e.Reason)})
	default:
		writeJSON(w, http.StatusInternalServerError, &api.Error{Code: api.ErrCoreInternal, Detail: err.Error()})
	}
}

func reasonExtra(reason string) json.RawMessage {
	if reason == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]string{"reason": reason})
	return raw
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	if err := dec.Decode(v); err != nil {
		return core.Fail(api.ErrCoreBadInput, "", "invalid JSON body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Authentication and authorization

// identity is the authenticated caller.
type identity struct {
	UserID   string
	Role     string // admin|writer
	Kind     string // human|machine
	KeyScope *string
}

// authenticate resolves the Bearer API key (hash-only storage).
func (s *Server) authenticate(r *http.Request) (*identity, error) {
	h := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if token == "" {
		return nil, core.Fail(api.ErrAuthUnauthorized, "", "missing api key")
	}
	var id identity
	var scope sql.NullString
	err := s.DB.QueryRow(`SELECT u.id, u.role, u.kind, k.project_id
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash=?1 AND k.revoked_at IS NULL`, core.Sha256Hex(token)).
		Scan(&id.UserID, &id.Role, &id.Kind, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.Fail(api.ErrAuthUnauthorized, "", "invalid api key")
	}
	if err != nil {
		return nil, err
	}
	if scope.Valid {
		id.KeyScope = &scope.String
	}
	return &id, nil
}

// auth wraps a handler with key authentication.
func (s *Server) auth(next func(http.ResponseWriter, *http.Request, *identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idn, err := s.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		next(w, r, idn)
	}
}

// authAdmin additionally requires the global admin role.
func (s *Server) authAdmin(next func(http.ResponseWriter, *http.Request, *identity)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, idn *identity) {
		if idn.Role != "admin" {
			writeErr(w, core.Fail(api.ErrAuthForbidden, "", "admin role required"))
			return
		}
		next(w, r, idn)
	})
}

// memberRole returns the caller's project role ("" when not a member).
func (s *Server) memberRole(projectID, userID string) (string, error) {
	var role string
	err := s.DB.QueryRow(`SELECT role FROM project_members WHERE project_id=?1 AND user_id=?2`,
		projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// projectIDOf resolves a project name-or-id to its id.
func (s *Server) projectIDOf(nameOrID string) (string, error) {
	var id string
	err := s.DB.QueryRow(`SELECT id FROM projects WHERE name=?1 OR id=?1`, nameOrID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.Fail(api.ErrCoreNotFound, "", "project %q", nameOrID)
	}
	return id, err
}

// scopeCheck enforces the api-key project scope (middleware-level).
func scopeCheck(idn *identity, projectID string) error {
	if idn.KeyScope != nil && *idn.KeyScope != projectID {
		return core.Fail(api.ErrAuthForbidden, "", "api key is scoped to another project")
	}
	return nil
}

// canRead: object data access = membership OR global admin.
func (s *Server) canRead(idn *identity, projectID string) (bool, error) {
	if err := scopeCheck(idn, projectID); err != nil {
		return false, err
	}
	if idn.Role == "admin" {
		return true, nil
	}
	role, err := s.memberRole(projectID, idn.UserID)
	return role != "", err
}

// canWrite: writer+ membership OR admin.
func (s *Server) canWrite(idn *identity, projectID string) (bool, error) {
	if err := scopeCheck(idn, projectID); err != nil {
		return false, err
	}
	if idn.Role == "admin" {
		return true, nil
	}
	role, err := s.memberRole(projectID, idn.UserID)
	return role == "writer" || role == "maintainer", err
}

// wiProject loads a WI's project id (hidden objects stay hidden).
func (s *Server) wiProject(idn *identity, idOrSlug string) (string, string, error) {
	id, err := s.WIs.Resolve(idOrSlug)
	if err != nil {
		return "", "", err
	}
	var projectID string
	if err := s.DB.QueryRow(`SELECT project_id FROM work_items WHERE id=?1`, id).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", core.Fail(api.ErrCoreNotFound, "", "work item %s", idOrSlug)
		}
		return "", "", err
	}
	return id, projectID, nil
}

// ---------------------------------------------------------------------------
// Work items

func (s *Server) hCreateWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req core.CreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.Reporter = idn.UserID
	projectID, err := s.projectIDOf(req.Project)
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canWrite(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "writer role on the project required"))
		return
	}
	wi, err := s.WIs.Create(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wi)
}

func (s *Server) hListWIs(w http.ResponseWriter, r *http.Request, idn *identity) {
	q := r.URL.Query()
	items, err := s.WIs.List(core.ListFilters{
		Project:   q.Get("project"),
		Status:    q.Get("status"),
		Assignee:  q.Get("assignee"),
		Priority:  q.Get("priority"),
		Milestone: q.Get("milestone"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// Permission trim: non-admins see only their projects.
	out := make([]api.WorkItem, 0, len(items))
	for i := range items {
		if ok, err := s.canRead(idn, items[i].ProjectID); err == nil && ok {
			out = append(out, items[i])
		}
	}
	writeJSON(w, http.StatusOK, api.ListResponse[api.WorkItem]{Items: out, Total: len(out)})
}

func (s *Server) hGetWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canRead(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrCoreNotFound, "", "work item %s", r.PathValue("id")))
		return
	}
	opts := core.GetOptions{}
	for _, inc := range strings.Split(r.URL.Query().Get("include"), ",") {
		switch strings.TrimSpace(inc) {
		case "events":
			opts.IncludeEvents = true
		case "deps":
			opts.IncludeDeps = true
		case "":
		default:
			writeErr(w, core.Fail(api.ErrCoreBadInput, "", "unknown include value %q", inc))
			return
		}
	}
	wi, err := s.WIs.Get(id, opts)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wi)
}

func (s *Server) hPatchWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canWrite(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "writer role on the project required"))
		return
	}
	var fields map[string]json.RawMessage
	if err := decodeBody(r, &fields); err != nil {
		writeErr(w, err)
		return
	}
	wi, err := s.WIs.Patch(id, fields)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wi)
}

func (s *Server) hCancelWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Cancel authorization: reporter, project maintainer, or admin (02 §6.5).
	allowed := idn.Role == "admin"
	if !allowed {
		var reporter string
		if rerr := s.DB.QueryRow(`SELECT reporter FROM work_items WHERE id=?1`, id).Scan(&reporter); rerr == nil {
			allowed = reporter == idn.UserID
		}
		role, merr := s.memberRole(projectID, idn.UserID)
		if merr == nil && role == "maintainer" {
			allowed = true
		}
	}
	if !allowed {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "cancel requires reporter, maintainer or admin"))
		return
	}
	wi, err := s.WIs.Cancel(id, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wi)
}

// hClaim serves claim/resume/takeover. claim and resume are the machine
// execution surface (any valid key, scope-checked); takeover requires
// admin or project maintainer (a human decision, v0 §7).
func (s *Server) hClaim(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	var req core.ClaimRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Intent == core.IntentTakeover && idn.Role != "admin" {
		role, merr := s.memberRole(projectID, idn.UserID)
		if merr != nil || role != "maintainer" {
			writeErr(w, core.Fail(api.ErrAuthForbidden, "", "takeover requires admin or project maintainer"))
			return
		}
	}
	res, err := s.Owners.Claim(id, req, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The bare repo is part of the claim contract: the chain begins here.
	if err := s.Git.EnsureBare(id); err != nil {
		writeErr(w, core.Fail(api.ErrCoreInternal, "", "create bare repo: %v", err))
		return
	}
	wi, err := s.WIs.Get(id, core.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Claim      api.Claim          `json:"claim"`
		Credential api.PushCredential `json:"credential"`
		WorkItem   api.WorkItem       `json:"work_item"`
	}{res.Claim, res.Credential, *wi})
}

func (s *Server) hEndGeneration(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	var req struct {
		AttemptID  string `json:"attempt_id"`
		Generation int64  `json:"generation"`
		Status     string `json:"status"`
		Note       string `json:"note,omitempty"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Owners.EndGeneration(id, req.AttemptID, req.Generation, req.Status, req.Note); err != nil {
		writeErr(w, err)
		return
	}
	wi, err := s.WIs.Get(id, core.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wi)
}

// ---------------------------------------------------------------------------
// Dependencies

func (s *Server) hAddDep(w http.ResponseWriter, r *http.Request, idn *identity) {
	_, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canWrite(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "writer role on the project required"))
		return
	}
	var req struct {
		BlockingWIID string `json:"blocking_wi_id"`
		Kind         string `json:"kind,omitempty"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	blocked, err := s.WIs.Resolve(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.WIs.AddDependency(req.BlockingWIID, blocked, req.Kind); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.depsView(blocked))
}

func (s *Server) hDeps(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, rerr := s.canRead(idn, projectID)
	if rerr != nil {
		writeErr(w, rerr)
		return
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrCoreNotFound, "", "work item %s", id))
		return
	}
	writeJSON(w, http.StatusOK, s.depsView(id))
}

func (s *Server) depsView(wiID string) *core.Deps {
	deps, err := s.WIs.Dependencies(wiID)
	if err != nil {
		return &core.Deps{Blocking: []core.Edge{}, BlockedBy: []core.Edge{}}
	}
	return deps
}

func (s *Server) hDelDep(w http.ResponseWriter, r *http.Request, idn *identity) {
	_, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canWrite(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "writer role on the project required"))
		return
	}
	blocked, err := s.WIs.Resolve(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	blocking, err := s.WIs.Resolve(r.PathValue("blocking"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.WIs.RemoveDependency(blocking, blocked, r.PathValue("kind")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.depsView(blocked))
}

// ---------------------------------------------------------------------------
// Ready queue

func (s *Server) hReadyQueue(w http.ResponseWriter, r *http.Request, idn *identity) {
	project := r.PathValue("project")
	projectID, err := s.projectIDOf(project)
	if err != nil {
		writeErr(w, err)
		return
	}
	ok, err := s.canRead(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		// No existence leak for hidden projects.
		writeErr(w, core.Fail(api.ErrCoreNotFound, "", "project %q", project))
		return
	}
	q, err := s.WIs.ReadyQueue(project)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// ---------------------------------------------------------------------------
// Machines

func (s *Server) hHeartbeat(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req core.MachineHeartbeat
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Admin.Heartbeat(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// Memory

// memAllowed builds the visibility predicate for the caller.
func (s *Server) memAllowed(idn *identity) func(*memory.Entry) bool {
	return func(e *memory.Entry) bool {
		switch e.Visibility {
		case "admin":
			return idn.Role == "admin"
		case "private":
			return e.OwnerUserID == idn.UserID || idn.Role == "admin"
		default: // project | team | public: any project reader
			if idn.Role == "admin" {
				return true
			}
			ok, err := s.canRead(idn, e.ProjectID)
			return err == nil && ok
		}
	}
}

func (s *Server) memReadAllowed(idn *identity, projectID string) error {
	if idn.Role == "admin" {
		return nil
	}
	ok, err := s.canRead(idn, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return core.Fail(api.ErrMemForbidden, "", "no access to this project's memories")
	}
	return nil
}

func (s *Server) hCreateMemory(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req memory.CreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	projectID, err := s.projectIDOf(req.Project)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.memWriteAllowed(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	e, err := s.Mems.Create(req, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) memWriteAllowed(idn *identity, projectID string) error {
	if err := scopeCheck(idn, projectID); err != nil {
		return err
	}
	if idn.Role == "admin" {
		return nil
	}
	role, err := s.memberRole(projectID, idn.UserID)
	if err != nil {
		return err
	}
	if role != "writer" && role != "maintainer" {
		return core.Fail(api.ErrMemForbidden, "", "writer role on the project required")
	}
	return nil
}

func (s *Server) hGetMemory(w http.ResponseWriter, r *http.Request, idn *identity) {
	e, content, err := s.memRead(r.PathValue("id"), idn)
	if err != nil {
		writeErr(w, err)
		return
	}
	if r.URL.Query().Get("format") == "content" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// memRead loads an entry and enforces read access in one shot.
func (s *Server) memRead(idOrSlug string, idn *identity) (*memory.Entry, []byte, error) {
	memID := idOrSlug
	if !strings.HasPrefix(memID, "mem_") {
		// Memories are addressed by id only; anything else is a 404.
		return nil, nil, core.Fail(api.ErrMemNotFound, "", "memory %q", idOrSlug)
	}
	e, err := s.Mems.Get(memID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.memReadAllowed(idn, e.ProjectID); err != nil {
		return nil, nil, err
	}
	allowed := s.memAllowed(idn)
	if !allowed(e) {
		return nil, nil, core.Fail(api.ErrMemNotFound, "", "memory %q", memID)
	}
	content, err := s.Mems.Content(memID)
	if err != nil {
		return nil, nil, err
	}
	return e, content, nil
}

func (s *Server) hPatchMemory(w http.ResponseWriter, r *http.Request, idn *identity) {
	memID := r.PathValue("id")
	e, _, err := s.memRead(memID, idn)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.memWriteAllowed(idn, e.ProjectID); err != nil {
		writeErr(w, err)
		return
	}
	var req memory.UpdateRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	updated, err := s.Mems.Update(memID, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) hRedactMemory(w http.ResponseWriter, r *http.Request, idn *identity) {
	memID := r.PathValue("id")
	e, _, err := s.memRead(memID, idn)
	if err != nil {
		writeErr(w, err)
		return
	}
	if e.OwnerUserID != idn.UserID && idn.Role != "admin" {
		writeErr(w, core.Fail(api.ErrMemForbidden, "", "only the owner (or admin) can redact"))
		return
	}
	if err := s.Mems.Redact(memID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": memID, "status": "redacted"})
}

func (s *Server) hListMemories(w http.ResponseWriter, r *http.Request, idn *identity) {
	project := r.URL.Query().Get("project")
	if project == "" {
		writeErr(w, core.Fail(api.ErrMemBadInput, "", "project query parameter required"))
		return
	}
	projectID, err := s.projectIDOf(project)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.memReadAllowed(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	items, err := s.Mems.List(projectID, s.memAllowed(idn))
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []memory.Entry{}
	}
	writeJSON(w, http.StatusOK, api.ListResponse[memory.Entry]{Items: items, Total: len(items)})
}

func (s *Server) hSearchMemory(w http.ResponseWriter, r *http.Request, idn *identity) {
	q := r.URL.Query()
	var tags []string
	if t := q.Get("tags"); t != "" {
		tags = strings.Split(t, ",")
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	s.hSearchRun(w, q.Get("q"), q.Get("type"), tags, limit, idn)
}

func (s *Server) hSearchMemoryPOST(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req struct {
		Q     string   `json:"q"`
		Type  string   `json:"type,omitempty"`
		Tags  []string `json:"tags,omitempty"`
		Limit int      `json:"limit,omitempty"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	s.hSearchRun(w, req.Q, req.Type, req.Tags, req.Limit, idn)
}

func (s *Server) hSearchRun(w http.ResponseWriter, query, memType string, tags []string, limit int, idn *identity) {
	// Membership-first filtering: restrict to the projects the caller can
	// read before visibility tiers apply.
	allowed := s.memAllowed(idn)
	baseAllowed := allowed
	allowed = func(e *memory.Entry) bool {
		if idn.Role != "admin" {
			ok, err := s.canRead(idn, e.ProjectID)
			if err != nil || !ok {
				return false
			}
		}
		return baseAllowed(e)
	}
	items, err := s.Mems.Search(query, memType, tags, limit, allowed)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []memory.Entry{}
	}
	writeJSON(w, http.StatusOK, api.ListResponse[memory.Entry]{Items: items, Total: len(items)})
}

func (s *Server) hRebuildIndex(w http.ResponseWriter, r *http.Request, idn *identity) {
	n, err := s.Mems.RebuildIndex()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"reindexed": n})
}

// ---------------------------------------------------------------------------
// Admin face

func (s *Server) hCreateUser(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req struct {
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
		Role        string `json:"role"`
		UserType    string `json:"user_type"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	u, err := s.Admin.CreateUser(req.DisplayName, req.Email, req.Role, req.UserType)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) hListUsers(w http.ResponseWriter, r *http.Request, idn *identity) {
	users, err := s.Admin.ListUsers()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListResponse[api.User]{Items: users, Total: len(users)})
}

func (s *Server) hCreateAPIKey(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req struct {
		UserID       string `json:"user_id"`
		Name         string `json:"name"`
		ProjectScope string `json:"project_scope,omitempty"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	key, err := s.Admin.CreateAPIKey(req.UserID, req.Name, req.ProjectScope)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, key) // plaintext rides once, here
}

func (s *Server) hRevokeAPIKey(w http.ResponseWriter, r *http.Request, idn *identity) {
	if err := s.Admin.RevokeAPIKey(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) hCreateProject(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req core.CreateProjectRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	p, err := s.Admin.CreateProject(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p) // identifier plaintext rides once
}

func (s *Server) hListProjects(w http.ResponseWriter, r *http.Request, idn *identity) {
	projects, err := s.Admin.ListProjects()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListResponse[api.Project]{Items: projects, Total: len(projects)})
}

func (s *Server) hGetProject(w http.ResponseWriter, r *http.Request, idn *identity) {
	p, err := s.Admin.GetProject(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) hRotateIdentifier(w http.ResponseWriter, r *http.Request, idn *identity) {
	plain, err := s.Admin.RotateIdentifier(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"identifier": plain}) // once
}
