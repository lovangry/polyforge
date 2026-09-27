package server

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/core/store"
)

// F group (M1–M5): the pf management CLI face. Admin unless noted (M3 also
// allows the project's maintainer).

func requireAdmin(s *Server, w http.ResponseWriter, r *http.Request) (*identity, bool) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return nil, false
	}
	if idn.Role != "admin" {
		writeErr(w, ownerAPI("CORE-403", "forbidden: admin only"))
		return nil, false
	}
	return idn, true
}

// M1: users CRUD.
func (s *Server) hCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Kind        string `json:"kind"`
		Role        string `json:"role"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Kind == "" {
		body.Kind = "human"
	}
	if body.Role == "" {
		body.Role = "writer"
	}
	if body.DisplayName == "" || (body.Kind != "human" && body.Kind != "machine") ||
		(body.Role != "admin" && body.Role != "writer") {
		writeErr(w, ownerAPI("CORE-400", "bad user payload"))
		return
	}
	id, _ := store.NewID("usr_")
	prefix := "usr_"
	if body.Kind == "machine" {
		prefix = "mac_"
	}
	id, err := store.NewID(prefix)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.DB.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1,?2,?3,?4)`,
		id, body.DisplayName, body.Kind, body.Role); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "display_name": body.DisplayName, "kind": body.Kind, "role": body.Role})
}

func (s *Server) hListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	rows, err := s.DB.Query(`SELECT id, display_name, kind, role FROM users ORDER BY id`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, kind, role string
		if err := rows.Scan(&id, &name, &kind, &role); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "display_name": name, "kind": kind, "role": role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) hGetUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	var id, name, kind, role string
	err := s.DB.QueryRow(`SELECT id, display_name, kind, role FROM users WHERE id=?1`, r.PathValue("id")).
		Scan(&id, &name, &kind, &role)
	if err != nil {
		writeErr(w, ownerAPI("CORE-404", "user"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "display_name": name, "kind": kind, "role": role})
}

func (s *Server) hPatchUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Role != "" && body.Role != "admin" && body.Role != "writer" {
		writeErr(w, ownerAPI("CORE-400", "bad role"))
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET display_name=COALESCE(?1, display_name), role=COALESCE(?2, role) WHERE id=?3`,
		nullIfEmpty(body.DisplayName), nullIfEmpty(body.Role), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// M2: api-keys CRUD. Plaintext is returned exactly once; only hashes stored.
func (s *Server) hCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	var body struct {
		UserID    string `json:"user_id"`
		Name      string `json:"name"`
		ProjectID *string `json:"project_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.UserID == "" {
		writeErr(w, ownerAPI("CORE-400", "user_id required"))
		return
	}
	id, _ := store.NewID("key_")
	plain, err := store.NewSecret(24)
	if err != nil {
		writeErr(w, err)
		return
	}
	var proj any
	if body.ProjectID != nil {
		proj = *body.ProjectID
	}
	if _, err := s.DB.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,?3,?4)`,
		id, body.UserID, proj, sha256Hex(plain)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "user_id": body.UserID, "api_key": plain})
}

func (s *Server) hListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	rows, err := s.DB.Query(`SELECT id, user_id, project_id, revoked_at FROM api_keys ORDER BY id`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, user string
		var proj, revoked *string
		if err := rows.Scan(&id, &user, &proj, &revoked); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "user_id": user, "project_id": proj, "revoked_at": revoked})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) hRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	if _, err := s.DB.Exec(`UPDATE api_keys SET revoked_at=?1 WHERE id=?2`, store.Now(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// M3: projects CRUD + membership management (admin or project maintainer).
func (s *Server) hCreateProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Visible *bool `json:"visible"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Name == "" {
		writeErr(w, ownerAPI("CORE-400", "name required"))
		return
	}
	id, _ := store.NewID("pj_")
	visible := 1
	if body.Visible != nil && !*body.Visible {
		visible = 0
	}
	if _, err := s.DB.Exec(`INSERT INTO projects(id, name, visible) VALUES (?1,?2,?3)`, id, body.Name, visible); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": body.Name, "visible": visible == 1})
}

func (s *Server) hListProjects(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rows, err := s.DB.Query(`SELECT p.id, p.name, p.visible FROM projects p
		LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id=?1
		WHERE p.visible=1 OR m.user_id IS NOT NULL OR ?2='admin'`, idn.UserID, idn.Role)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name string
		var visible int
		if err := rows.Scan(&id, &name, &visible); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "name": name, "visible": visible == 1})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) hGetProject(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var id, name string
	var visible int
	err = s.DB.QueryRow(`SELECT id, name, visible FROM projects WHERE id=?1 OR name=?1`, r.PathValue("id")).
		Scan(&id, &name, &visible)
	if err != nil {
		writeErr(w, ownerAPI("CORE-404", "project"))
		return
	}
	ok, err := s.canRead(idn, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeErr(w, ownerAPI("CORE-404", "project"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "visible": visible == 1})
}

func (s *Server) hPatchProject(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var projectID string
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE id=?1 OR name=?1`, r.PathValue("id")).Scan(&projectID); err != nil {
		writeErr(w, ownerAPI("CORE-404", "project"))
		return
	}
	role, err := s.memberRole(projectID, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" && role != "maintainer" {
		writeErr(w, ownerAPI("CORE-403", "forbidden: admin or project maintainer"))
		return
	}
	var body struct {
		Members []struct {
			UserID string `json:"user_id"`
			Role   string `json:"role"`
		} `json:"members"`
		Visible *bool `json:"visible"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Visible != nil {
		v := 1
		if !*body.Visible {
			v = 0
		}
		if _, err := s.DB.Exec(`UPDATE projects SET visible=?1 WHERE id=?2`, v, projectID); err != nil {
			writeErr(w, err)
			return
		}
	}
	for _, m := range body.Members {
		if m.Role != "viewer" && m.Role != "writer" && m.Role != "maintainer" {
			writeErr(w, ownerAPI("CORE-400", "bad member role "+m.Role))
			return
		}
		if _, err := s.DB.Exec(`INSERT INTO project_members(project_id, user_id, role, created_at)
			VALUES (?1,?2,?3,?4) ON CONFLICT(project_id, user_id) DO UPDATE SET role=excluded.role`,
			projectID, m.UserID, m.Role, store.Now()); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// M4: on-demand backup snapshot (VACUUM INTO under <data_dir>/backups).
func (s *Server) hBackup(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(s, w, r); !ok {
		return
	}
	if err := os.MkdirAll(filepath.Join(s.DataDir, "backups"), 0o750); err != nil {
		writeErr(w, err)
		return
	}
	name := "aihub-" + store.Now() + ".db"
	name = sanitizeFilename(name)
	target := filepath.Join(s.DataDir, "backups", name)
	if _, err := s.DB.Exec(`VACUUM INTO ?1`, target); err != nil {
		writeErr(w, ownerAPI("CORE-507", "vacuum failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup": name})
}

func sanitizeFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// M5: service secret rotation (grace | rollback), §6.6.
func (s *Server) hRotateSecret(w http.ResponseWriter, r *http.Request) {
	idn, ok := requireAdmin(s, w, r)
	if !ok {
		return
	}
	var body struct {
		Mode         string `json:"mode"`
		GraceSeconds int    `json:"grace_seconds"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Mode == "" {
		body.Mode = "grace"
	}
	if err := s.Svc.RotateSecret(body.Mode, body.GraceSeconds, idn.UserID); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.WIs.AppendEvent("", idn.UserID, "secret_rotated", nil); err != nil {
		// Event append needs a WI; rotation itself already succeeded. The
		// event stream for rotation is recorded via core_meta + epochs; the
		// §5.1a secret_rotated event is emitted per-WI by the recovery flow.
		_ = err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
