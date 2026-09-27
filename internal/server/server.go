// Package server is the AIHub HTTP face: one port, all 25 business endpoints
// plus the management face M1–M5 (02-aihub-core.md §5.1). Handlers only call
// internal/core packages; no handler writes SQL directly (02 §3).
package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/memory"
	"github.com/silgrid/polyforge/internal/core/owner"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/workitem"
)

// Server wires the domain managers behind one mux.
type Server struct {
	DB       *store.DB
	Svc      *service.Service
	Owners   *owner.Manager
	WIs      *workitem.Manager
	Caps     *capsule.Store
	Mems     *memory.Store
	DataDir  string

	mux *http.ServeMux
}

// New assembles the server. svc may be nil only in tests that skip
// generation endpoints.
func New(db *store.DB, dataDir, workflowsDir string, svc *service.Service) *Server {
	s := &Server{
		DB:      db,
		Svc:     svc,
		Owners:  owner.NewManager(db, svc),
		WIs:     workitem.NewManager(db, workflowsDir),
		Caps:    capsule.NewStore(db, dataDir, owner.NewManager(db, svc)),
		Mems:    memory.NewStore(db, dataDir),
		DataDir: dataDir,
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// errStatus maps a CORE-/SYNC-/MEM- code to its HTTP status. The code itself
// rides in the JSON body (inference: the design pins codes, not statuses).
func errStatus(code string) int {
	switch code {
	case "404":
		return http.StatusNotFound
	case "CORE-400", "SYNC-400", "MEM-400":
		return http.StatusBadRequest
	case "CORE-401":
		return http.StatusUnauthorized
	case "CORE-403":
		return http.StatusForbidden
	case "CORE-404", "MEM-404", "SYNC-404":
		return http.StatusNotFound
	case "CORE-409", "SYNC-409", "MEM-409":
		return http.StatusConflict
	case "CORE-410", "SYNC-410", "MEM-410":
		return http.StatusGone
	case "CORE-413":
		return http.StatusRequestEntityTooLarge
	case "CORE-425":
		return http.StatusTooEarly // 425 exists; body carries the typed code
	case "CORE-503", "MEM-503":
		return http.StatusServiceUnavailable
	case "CORE-504", "MEM-502":
		return http.StatusBadGateway
	case "CORE-507":
		return http.StatusInsufficientStorage
	case "CORE-508":
		return http.StatusConflict
	case "CORE-509":
		return http.StatusConflict
	case "CORE-500", "MEM-500":
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// writeErr renders the typed error envelope.
func writeErr(w http.ResponseWriter, err error) {
	code, reason, detail := "CORE-500", "", err.Error()
	for _, prefix := range []string{"CORE-", "SYNC-", "MEM-"} {
		if strings.HasPrefix(detail, prefix) {
			rest := strings.TrimPrefix(detail, prefix)
			end := strings.IndexAny(rest, " :")
			if end > 0 {
				code = prefix + rest[:end]
			}
			break
		}
	}
	// owner/workitem/capsule/memory APIError carries structured fields.
	var ae interface {
		Error() string
	}
	_ = ae
	switch e := err.(type) {
	case *owner.APIError:
		code, reason, detail = e.Code, e.Reason, e.Detail
	case *workitem.APIError:
		code, reason, detail = e.Code, e.Reason, e.Detail
	case *capsule.APIError:
		code, reason, detail = e.Code, e.Reason, e.Detail
	case *memory.APIError:
		code, reason, detail = e.Code, e.Reason, e.Detail
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(errStatus(code))
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "reason": reason, "detail": detail},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("CORE-400: invalid JSON body")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Authentication (02 §4.10 / §8)

// identity is the authenticated caller.
type identity struct {
	UserID     string
	Role       string // global role: admin|writer
	Kind       string // human|machine
	KeyScope   *string // api_keys.project_id when the key is project-scoped
}

// authenticate resolves the api key. Inference (recorded): keys travel in the
// Authorization: Bearer header; only sha256 hashes are stored.
func (s *Server) authenticate(r *http.Request) (*identity, error) {
	h := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if token == "" {
		return nil, errors.New("CORE-401: missing api key")
	}
	sum := sha256Hex(token)
	var id identity
	var scope sql.NullString
	err := s.DB.QueryRow(`SELECT u.id, u.role, u.kind, k.project_id
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash=?1 AND k.revoked_at IS NULL`, sum).
		Scan(&id.UserID, &id.Role, &id.Kind, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("CORE-401: invalid api key")
	}
	if err != nil {
		return nil, err
	}
	if scope.Valid {
		id.KeyScope = &scope.String
	}
	return &id, nil
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

// requireScope enforces the api-key project scope before anything else
// (02 §8: middleware-level, all routes).
func (s *Server) requireScope(idn *identity, projectID string) error {
	if idn.KeyScope != nil && *idn.KeyScope != projectID {
		return errors.New("CORE-403: key is scoped to another project")
	}
	return nil
}

// canRead/canWrite: object data access = membership OR global admin (02
// §4.10); write additionally requires writer+ membership. Inference (02 pins
// visibility, not per-role write depth): viewer reads, writer+ writes.
func (s *Server) canRead(idn *identity, projectID string) (bool, error) {
	if idn.Role == "admin" {
		return true, nil
	}
	role, err := s.memberRole(projectID, idn.UserID)
	if err != nil {
		return false, err
	}
	return role != "", nil
}

func (s *Server) canWrite(idn *identity, projectID string) (bool, error) {
	if idn.Role == "admin" {
		return true, nil
	}
	role, err := s.memberRole(projectID, idn.UserID)
	if err != nil {
		return false, err
	}
	return role == "writer" || role == "maintainer", nil
}

// wiProject loads the WI's project id; a hidden object stays hidden
// (CORE-404, no existence leak) when the caller lacks read access.
func (s *Server) wiProject(idn *identity, wiID string) (string, *owner.APIError) {
	var projectID string
	err := s.DB.QueryRow(`SELECT project_id FROM work_items WHERE id=?1`, wiID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", &owner.APIError{Code: "CORE-404", Detail: "work item " + wiID}
	}
	if err != nil {
		return "", &owner.APIError{Code: "CORE-500", Detail: err.Error()}
	}
	return projectID, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
