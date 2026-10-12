// Package server is the v1 AIHub HTTP face: the business endpoints
// (v0 openapi surface minus every SYNC-*/revision-chain route), the
// admin face, the v1.5 session endpoints (claim/session-verify/
// session-attach/force-end-session/session-end), and the /git/{wi}.git/...
// proxy mount. Every error is the typed common/api errStatus envelope;
// auth is the Bearer API key.
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/aihub/memory"
	"github.com/silgrid/polyforge/internal/common/api"
)

// Server wires the domain managers behind one mux.
type Server struct {
	DB       *core.DB
	WIs      *core.WIManager
	Owners   *core.OwnerManager
	Sessions *core.SessionManager
	Admin    *core.AdminManager
	Mems     *memory.Store
	Ships    *core.ShipLedgerManager
	Git      *git.Manager
	DataDir  string

	// StaleAfter is the machines liveness threshold used by the machines
	// read face: a machine whose last_heartbeat_at is older than this is
	// reported stale. Zero keeps the default (5× the 60s heartbeat
	// default = 5m); cmd/aihub wires it to -machines-stale-after.
	StaleAfter time.Duration

	mux *http.ServeMux
}

// New assembles the server.
func New(db *core.DB, dataDir string, gitMgr *git.Manager) *Server {
	s := &Server{
		DB:       db,
		WIs:      core.NewWIManager(db),
		Owners:   core.NewOwnerManager(db),
		Sessions: core.NewSessionManager(db),
		Admin:    core.NewAdminManager(db),
		Mems:     memory.NewStore(db, dataDir),
		Ships:    core.NewShipLedgerManager(db, core.NewWIManager(db)),
		Git:      gitMgr,
		DataDir:  dataDir,
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
	m.HandleFunc("POST /api/v1/work-items/{id}/claim", s.auth(s.hSessionClaim))
	m.HandleFunc("POST /api/v1/work-items/{id}/session-verify", s.auth(s.hSessionVerify))
	m.HandleFunc("POST /api/v1/work-items/{id}/session-attach", s.auth(s.hSessionAttach))
	m.HandleFunc("POST /api/v1/work-items/{id}/session-end", s.auth(s.hSessionEnd))
	// Recovery door: an admin/maintainer force-ends a stuck holder's session
	// (WI -> queued); any daemon then claims it normally.
	m.HandleFunc("POST /api/v1/work-items/{id}/force-end-session", s.auth(s.hForceEndSession))
	// polyforge#116: the §6.4 owner doors on a workflow parked blocked —
	// resume (the owner's re-dispatch-in-place verdict, claimed back
	// immediately) and the terminate verdict (pf wrap --fail on a parked WI).
	m.HandleFunc("POST /api/v1/work-items/{id}/resume-blocked", s.auth(s.hResumeBlockedWI))
	m.HandleFunc("POST /api/v1/work-items/{id}/terminate-blocked", s.auth(s.hTerminateBlockedWI))
	// The read-only authorization probe of the abandon-a-repair-pending-WI
	// door (the daemon ends the session; the human is checked here first).
	m.HandleFunc("GET /api/v1/work-items/{id}/close-auth", s.auth(s.hCloseAuthWI))
	m.HandleFunc("POST /api/v1/work-items/{id}/dependencies", s.auth(s.hAddDep))
	m.HandleFunc("GET /api/v1/work-items/{id}/dependencies", s.auth(s.hDeps))
	// Ship ledger (polyforge#123, P1b): the ship bridge records each
	// pushed repo's branch top; anyone who can read the WI queries it.
	m.HandleFunc("POST /api/v1/work-items/{id}/ship-ledger", s.auth(s.hRecordShipLedger))
	m.HandleFunc("DELETE /api/v1/work-items/{id}/dependencies/{blocking}/{kind}", s.auth(s.hDelDep))
	m.HandleFunc("GET /api/v1/projects/{project}/ready-queue", s.auth(s.hReadyQueue))
	m.HandleFunc("POST /api/v1/machines/heartbeat", s.auth(s.hHeartbeat))
	m.HandleFunc("GET /api/v1/machines", s.authAdmin(s.hListMachines))

	// Memory face.
	m.HandleFunc("POST /api/v1/memories", s.auth(s.hCreateMemory))
	m.HandleFunc("GET /api/v1/memories/search", s.auth(s.hSearchMemory))
	m.HandleFunc("POST /api/v1/memories/rebuild-index", s.authAdmin(s.hRebuildIndex))
	m.HandleFunc("GET /api/v1/memories/{id}", s.auth(s.hGetMemory))
	m.HandleFunc("POST /api/v1/memories/{id}/redact", s.auth(s.hRedactMemory))

	// Admin face.
	m.HandleFunc("POST /api/v1/users", s.authAdmin(s.hCreateUser))
	m.HandleFunc("GET /api/v1/users", s.authAdmin(s.hListUsers))
	m.HandleFunc("GET /api/v1/users/{id}", s.authAdmin(s.hGetUser))
	m.HandleFunc("PATCH /api/v1/users/{id}", s.authAdmin(s.hPatchUser))
	m.HandleFunc("POST /api/v1/api-keys", s.authAdmin(s.hCreateAPIKey))
	m.HandleFunc("GET /api/v1/api-keys", s.authAdmin(s.hListAPIKeys))
	m.HandleFunc("DELETE /api/v1/api-keys/{id}", s.authAdmin(s.hRevokeAPIKey))
	m.HandleFunc("POST /api/v1/projects", s.authAdmin(s.hCreateProject))
	m.HandleFunc("GET /api/v1/projects", s.authAdmin(s.hListProjects))
	m.HandleFunc("GET /api/v1/projects/{id}", s.authAdmin(s.hGetProject))
	m.HandleFunc("PATCH /api/v1/projects/{id}", s.auth(s.hPatchProject))
	m.HandleFunc("POST /api/v1/admin/backup", s.authAdmin(s.hBackup))

	// Git proxy (own auth: session-scoped Basic credential).
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

// requireWrite enforces write access to projectID: on error or denial it
// writes the typed refusal envelope itself and answers false.
func (s *Server) requireWrite(w http.ResponseWriter, idn *identity, projectID string) bool {
	ok, err := s.canWrite(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "writer role on the project required"))
		return false
	}
	return true
}

// requireRead enforces read access to projectID, hiding the object
// behind the caller-supplied not-found detail on denial.
func (s *Server) requireRead(w http.ResponseWriter, idn *identity, projectID, notFound string, args ...any) bool {
	ok, err := s.canRead(idn, projectID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if !ok {
		writeErr(w, core.Fail(api.ErrCoreNotFound, "", notFound, args...))
		return false
	}
	return true
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
	if !s.requireWrite(w, idn, projectID) {
		return
	}
	wi, err := s.WIs.Create(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.writeWI(w, http.StatusCreated, wi)
}

// writeWI wraps a WorkItem with its public session audit block (m4:
// session_fingerprint / session_machine / session_owner /
// session_started_at / clean_pause; NEVER session_hash, never the
// secret) and writes it.
func (s *Server) writeWI(w http.ResponseWriter, status int, wi *api.WorkItem) {
	view, err := s.WIs.View(wi)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, status, view)
}

func (s *Server) hListWIs(w http.ResponseWriter, r *http.Request, idn *identity) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit")) // 0/absent = core default; clamped there
	items, err := s.WIs.List(core.ListFilters{
		Project:   q.Get("project"),
		Status:    q.Get("status"),
		Assignee:  q.Get("assignee"),
		Priority:  q.Get("priority"),
		Milestone: q.Get("milestone"),
		Limit:     limit,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// Permission trim: non-admins see only their projects.
	trimmed := make([]api.WorkItem, 0, len(items))
	for i := range items {
		if ok, err := s.canRead(idn, items[i].ProjectID); err == nil && ok {
			trimmed = append(trimmed, items[i])
		}
	}
	// m4: each row carries the public session audit block.
	views, err := s.WIs.Views(trimmed)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items []core.WIView `json:"items"`
		Total int           `json:"total"`
	}{Items: views, Total: len(views)})
}

func (s *Server) hGetWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireRead(w, idn, projectID, "work item %s", r.PathValue("id")) {
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
	s.writeWI(w, http.StatusOK, wi)
}

func (s *Server) hPatchWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireWrite(w, idn, projectID) {
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
	s.writeWI(w, http.StatusOK, wi)
}

// ownerMaintainerOrAdmin is the one authorization of the human decisions on
// a work item — cancel (abandon) and the blocked exits (continue /
// terminate; v2-converge §7, §11 item 7): the WI's reporter (the task
// owner), a project maintainer, or a global admin.
func (s *Server) ownerMaintainerOrAdmin(idn *identity, projectID, wiID string) bool {
	if idn.Role == "admin" {
		return true
	}
	var reporter string
	if err := s.DB.QueryRow(`SELECT reporter FROM work_items WHERE id=?1`, wiID).Scan(&reporter); err == nil && reporter == idn.UserID {
		return true
	}
	role, err := s.memberRole(projectID, idn.UserID)
	return err == nil && role == "maintainer"
}

func (s *Server) hCancelWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	// Cancel authorization: reporter, project maintainer, or admin (02 §6.5).
	if !s.ownerMaintainerOrAdmin(idn, projectID, id) {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "cancel requires reporter, maintainer or admin"))
		return
	}
	wi, err := s.WIs.Cancel(id, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.writeWI(w, http.StatusOK, wi)
}

// ---------------------------------------------------------------------------
// Ship ledger (polyforge#123, P1b): record on ship, queryable per WI.

// hRecordShipLedger serves POST /api/v1/work-items/{id}/ship-ledger: the
// ship bridge's per-run recording face. Authorization is the ordinary
// object-data WRITE rule (the same face the daemon's other reporting
// calls use). Recording is an upsert per (WI, repo): the ledger answers
// the LATEST delivery, not the history — the push history lives in git.
func (s *Server) hRecordShipLedger(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireWrite(w, idn, projectID) {
		return
	}
	var req api.ShipLedgerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, core.Fail(api.ErrCoreBadInput, "", "ship-ledger body is not valid JSON: %v", err))
		return
	}
	if err := s.Ships.Record(id, req.Branch, req.Repos); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded", "work_item_id": id})
}

// ---------------------------------------------------------------------------
// v1.5 session endpoints (contract §1). The claim path is the machine
// execution surface (any valid key, scope-checked); force-end-session keeps
// the human decision gate (admin or project maintainer).

// sessionClaimEnvelope is the claim/attach response: the SAME
// shape as api.SessionClaimResponse (which cannot gain fields — Phase 0
// owns it), with the work item served as the m4 audit view so its JSON
// carries the public session block.
type sessionClaimEnvelope struct {
	Claim    api.SessionClaim `json:"claim"`
	WorkItem core.WIView      `json:"work_item"`
}

// sessionClaimResponse runs the claim half, ensures the bare repo (the
// chain begins here, as in v1) and assembles the envelope.
func (s *Server) sessionClaimResponse(w http.ResponseWriter, id string, claim *api.SessionClaim, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Git.EnsureBare(id); err != nil {
		writeErr(w, core.Fail(api.ErrCoreInternal, "", "create bare repo: %v", err))
		return
	}
	wi, err := s.WIs.Get(id, core.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	view, err := s.WIs.View(wi)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionClaimEnvelope{Claim: *claim, WorkItem: *view})
}

// hSessionClaim serves POST /api/v1/work-items/{id}/claim (the v1.5 CAS
// session claim; it replaced the deleted v1 generation-era intent
// handler).
func (s *Server) hSessionClaim(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireWrite(w, idn, projectID) {
		return
	}
	var req api.SessionClaimRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.WorkItemID = id
	claim, err := s.Sessions.Claim(id, req, idn.UserID)
	s.sessionClaimResponse(w, id, claim, err)
}

// hSessionVerify serves the compare-only holder check (B3: the hub
// stores only the hash; the secret never echoes, not even on a match).
func (s *Server) hSessionVerify(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	var req api.SessionVerifyRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	resp, err := s.Sessions.Verify(id, req.SessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// hSessionAttach serves the same-session resume of a paused WI (M3).
func (s *Server) hSessionAttach(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	var req api.SessionAttachRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.WorkItemID = id
	claim, err := s.Sessions.Attach(id, req, idn.UserID)
	s.sessionClaimResponse(w, id, claim, err)
}

// hForceEndSession serves the admin/maintainer force-end of a held session
// (WI -> queued, session cleared, the old secret fenced). Authorization is
// admin or project maintainer: the human recovery decision.
func (s *Server) hForceEndSession(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	if !s.adminOrMaintainer(idn, projectID) {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "end-session requires admin or project maintainer"))
		return
	}
	var req api.ForceEndSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Sessions.ForceEnd(id, req.Reason, idn.UserID); err != nil {
		writeErr(w, err)
		return
	}
	wi, err := s.WIs.Get(id, core.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	view, err := s.WIs.View(wi)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// adminOrMaintainer is the human recovery gate (no reporter admission).
func (s *Server) adminOrMaintainer(idn *identity, projectID string) bool {
	if idn.Role == "admin" {
		return true
	}
	role, err := s.memberRole(projectID, idn.UserID)
	return err == nil && role == "maintainer"
}

// hSessionEnd serves the session-fenced terminal verdict (wrapped|
// failed|blocked) and — the merge amend — the M3 pause transition
// ("paused": the session is KEPT, clean_pause rides the same fenced
// update). It fully replaces the deleted v1 /end-generation endpoint.
func (s *Server) hSessionEnd(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	var req api.SessionEndRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Sessions.End(id, req, idn.UserID); err != nil {
		writeErr(w, err)
		return
	}
	wi, err := s.WIs.Get(id, core.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	view, err := s.WIs.View(wi)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// hResumeBlockedWI serves the §6.4 owner resume verdict's hub door
// (polyforge#116): the door `pf resume` exercises on a workflow parked
// blocked on an honest incomplete result. Authorization (v2-converge §7):
// the WI's reporter, a project maintainer, or an admin — the same gate as
// terminate-blocked and cancel.
func (s *Server) hResumeBlockedWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	if !s.ownerMaintainerOrAdmin(idn, projectID, id) {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "resume-blocked requires the work item's reporter, a project maintainer or admin"))
		return
	}
	wi, err := s.WIs.ResumeBlocked(id, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	view, err := s.WIs.View(wi)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// hTerminateBlockedWI serves the §6.4 owner terminate verdict's hub door
// (polyforge#116): `pf wrap --fail` on a parked workflow. Authorization is
// the unified blocked-exit gate: reporter, project maintainer, or admin.
func (s *Server) hTerminateBlockedWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	if !s.ownerMaintainerOrAdmin(idn, projectID, id) {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "terminate-blocked requires the work item's reporter, a project maintainer or admin"))
		return
	}
	wi, err := s.WIs.TerminateBlocked(id, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	view, err := s.WIs.View(wi)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// hCloseAuthWI serves GET /api/v1/work-items/{id}/close-auth: a read-only
// probe of the human close authorization (reporter, project maintainer or
// admin — the same gate as cancel and the blocked exits). It mutates
// nothing. The CLI runs it with the human's own key before asking the local
// daemon to abandon a repair_pending WI, whose session only the daemon can
// end (the hub's cancel refuses a running WI).
func (s *Server) hCloseAuthWI(w http.ResponseWriter, r *http.Request, idn *identity) {
	id, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	if !s.ownerMaintainerOrAdmin(idn, projectID, id) {
		writeErr(w, core.Fail(api.ErrAuthForbidden, "", "closing a work item requires its reporter, a project maintainer or admin"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "work_item_id": id})
}

// ---------------------------------------------------------------------------
// Dependencies

func (s *Server) hAddDep(w http.ResponseWriter, r *http.Request, idn *identity) {
	_, projectID, err := s.wiProject(idn, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireWrite(w, idn, projectID) {
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
	if !s.requireRead(w, idn, projectID, "work item %s", id) {
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
	if !s.requireWrite(w, idn, projectID) {
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
	if !s.requireRead(w, idn, projectID, "project %q", project) {
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

// defaultMachinesStaleAfter is 5× the daemon heartbeat default (60s): a
// machine missing five consecutive heartbeats is stale.
const defaultMachinesStaleAfter = 5 * time.Minute

// authorizeMachineRegistration gates the FIRST-registration branch of the
// heartbeat path (the M3 folded finding): minting a machine identity — a
// machine_id with no machines row — is a privileged act, reserved for the
// global admin or a maintainer of a configured project (the heartbeat body
// carries no project context, so any project_members row with the
// maintainer role qualifies). Refresh of an EXISTING machine row stays
// open to any authenticated key: the gate fires only when the row is
// absent, before core performs any write. This mirrors the admin-or-
// project-maintainer convention of hPatchProject, but conditionally.
func (s *Server) authorizeMachineRegistration(idn *identity, machineID string) error {
	if strings.TrimSpace(machineID) == "" {
		return nil // core rejects the empty id with bad input; nothing to gate
	}
	if idn.Role == "admin" {
		return nil
	}
	var exists int
	err := s.DB.QueryRow(`SELECT 1 FROM machines WHERE id=?1`, machineID).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if exists == 1 {
		return nil // refresh path: open to any authenticated key, unchanged
	}
	var maintainer int
	if err := s.DB.QueryRow(`SELECT 1 FROM project_members WHERE user_id=?1 AND role='maintainer' LIMIT 1`,
		idn.UserID).Scan(&maintainer); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if maintainer == 1 {
		return nil
	}
	return core.Fail(api.ErrAuthForbidden, "", "machine registration requires admin or project maintainer")
}

func (s *Server) hHeartbeat(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req core.MachineHeartbeat
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.authorizeMachineRegistration(idn, req.MachineID); err != nil {
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

// machinesStaleAfter resolves the configured threshold (0 = default).
func (s *Server) machinesStaleAfter() time.Duration {
	if s.StaleAfter <= 0 {
		return defaultMachinesStaleAfter
	}
	return s.StaleAfter
}

// machineStatus is one machines read-face row: the stored liveness
// projection plus the server-computed stale boolean and age. An
// unparseable last_heartbeat_at reports stale=false with no age —
// "cannot judge" is not "dead".
type machineStatus struct {
	ID                  string   `json:"id"`
	UserID              string   `json:"user_id"`
	RuntimeVersion      string   `json:"runtime_version,omitempty"`
	Mode                string   `json:"mode,omitempty"`
	LiveWIIDs           []string `json:"live_wi_ids"`
	LastHeartbeatAt     string   `json:"last_heartbeat_at"`
	HeartbeatAgeSeconds *int64   `json:"heartbeat_age_seconds,omitempty"`
	Stale               bool     `json:"stale"`
}

// hListMachines is the operators' liveness read face (admin): every
// registered machine with its last_heartbeat_at and the stale projection
// computed against StaleAfter. This is the read half the M3 gate metric
// judges against (--m3-heartbeat-max-age reads the same table offline).
func (s *Server) hListMachines(w http.ResponseWriter, r *http.Request, idn *identity) {
	rows, err := s.Admin.Machines()
	if err != nil {
		writeErr(w, err)
		return
	}
	staleAfter := s.machinesStaleAfter()
	now := time.Now()
	out := make([]machineStatus, 0, len(rows))
	for _, m := range rows {
		st := machineStatus{
			ID:              m.ID,
			UserID:          m.UserID,
			RuntimeVersion:  m.RuntimeVersion,
			Mode:            m.Mode,
			LiveWIIDs:       m.LiveWIIDs,
			LastHeartbeatAt: m.LastHeartbeatAt,
		}
		if hb, err := time.Parse(time.RFC3339, m.LastHeartbeatAt); err == nil {
			age := int64(now.Sub(hb).Seconds())
			if age < 0 {
				age = 0
			}
			st.HeartbeatAgeSeconds = &age
			st.Stale = time.Duration(age)*time.Second > staleAfter
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machines":            out,
		"stale_after_seconds": int64(staleAfter.Seconds()),
		"count":               len(out),
	})
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
		_, _ = w.Write([]byte(memory.Body(content)))
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

func (s *Server) hSearchMemory(w http.ResponseWriter, r *http.Request, idn *identity) {
	q := r.URL.Query()
	var tags []string
	if t := q.Get("tags"); t != "" {
		tags = strings.Split(t, ",")
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	excerpt, _ := strconv.Atoi(q.Get("excerpt"))
	s.hSearchRun(w, q.Get("q"), q.Get("type"), tags, limit, excerpt, idn)
}

// maxMemoryExcerptRunes bounds the content excerpt a search answer may
// carry (the F6 recall faces ask for bounded excerpts; the cap keeps a
// hostile/absent caller from turning search into a full-content dump).
const maxMemoryExcerptRunes = 8192

// memoryHit is one search answer row with a content excerpt attached:
// every memory.Entry field (embedded, marshaled flat) plus the first
// excerpt runes of the authority content. Only served when the caller
// asked for an excerpt — the plain search answer stays the v0 shape.
type memoryHit struct {
	memory.Entry
	ContentExcerpt string `json:"content_excerpt,omitempty"`
}

// memoryHitList is the excerpt search answer (ListResponse shape).
type memoryHitList struct {
	Items []memoryHit `json:"items"`
	Total int         `json:"total"`
}

func (s *Server) hSearchRun(w http.ResponseWriter, query, memType string, tags []string, limit, excerpt int, idn *identity) {
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
	// The excerpt extension (F6): a caller that names an excerpt length
	// gets each hit with a bounded content prefix attached. Purely
	// additive — an absent/zero excerpt keeps the exact v0 answer shape.
	if excerpt > 0 {
		if excerpt > maxMemoryExcerptRunes {
			excerpt = maxMemoryExcerptRunes
		}
		hits := make([]memoryHit, 0, len(items))
		for i := range items {
			h := memoryHit{Entry: items[i]}
			if content, cerr := s.Mems.Content(items[i].ID); cerr == nil {
				h.ContentExcerpt = excerptBody(string(content), excerpt)
			}
			hits = append(hits, h)
		}
		writeJSON(w, http.StatusOK, memoryHitList{Items: hits, Total: len(hits)})
		return
	}
	writeJSON(w, http.StatusOK, api.ListResponse[memory.Entry]{Items: items, Total: len(items)})
}

// excerptBody answers the first n runes of the memory's BODY: the
// authority file's leading frontmatter block (the id/type/title rows
// the hit already carries as fields) is skipped, so the excerpt is
// content, not metadata echo.
func excerptBody(authority string, n int) string {
	return firstRunes(strings.TrimLeft(memory.Body([]byte(authority)), "\n"), n)
}

// firstRunes answers the first n runes of s (shorter strings pass
// through; n <= 0 answers "").
func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	c := 0
	for i := range s {
		if c == n {
			return s[:i]
		}
		c++
	}
	return s
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
		UserID    string  `json:"user_id"`
		Name      string  `json:"name"`
		ProjectID *string `json:"project_id,omitempty"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	scope := ""
	if req.ProjectID != nil {
		scope = *req.ProjectID
	}
	key, err := s.Admin.CreateAPIKey(req.UserID, req.Name, scope)
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
	writeJSON(w, http.StatusCreated, p)
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

func (s *Server) hGetUser(w http.ResponseWriter, r *http.Request, idn *identity) {
	u, err := s.Admin.GetUser(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) hPatchUser(w http.ResponseWriter, r *http.Request, idn *identity) {
	var req struct {
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	u, err := s.Admin.PatchUser(r.PathValue("id"), req.DisplayName, req.Role)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) hListAPIKeys(w http.ResponseWriter, r *http.Request, idn *identity) {
	keys, err := s.Admin.ListAPIKeys()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListResponse[core.APIKeyInfo]{Items: keys, Total: len(keys)})
}

// hPatchProject serves PATCH /api/v1/projects/{id} (M3): description,
// visibility and membership. Authorization: global admin or a maintainer
// of THAT project (02 M3); a project-scoped key may only touch its own.
func (s *Server) hPatchProject(w http.ResponseWriter, r *http.Request, idn *identity) {
	projectID, err := s.projectIDOf(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := scopeCheck(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" {
		role, merr := s.memberRole(projectID, idn.UserID)
		if merr != nil || role != "maintainer" {
			writeErr(w, core.Fail(api.ErrAuthForbidden, "", "project patch requires admin or project maintainer"))
			return
		}
	}
	var req core.ProjectPatchRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	p, err := s.Admin.PatchProject(r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) hBackup(w http.ResponseWriter, r *http.Request, idn *identity) {
	name, err := s.Admin.Backup(s.DataDir)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"backup": name})
}
