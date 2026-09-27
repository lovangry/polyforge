package server

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/memory"
)

// F group (M1–M5): the pf management CLI face. Admin unless noted (M3 also
// allows the project's maintainer).
//
// Project-scope enforcement (02 §8): an api_keys.project_id-scoped key is
// rejected for every out-of-scope project object, on the management face
// exactly as on the data plane — scope only narrows, never widens. Handlers
// whose objects carry a project dimension (M2 key scope, M3 projects)
// enforce it below. M1 (users), M4 (backup) and M5 (rotate-secret) operate
// on global, non-project objects; 02 §8 frames scope over project objects
// only, so those stay reachable — but a scoped key can never parley them
// into out-of-scope reach: M2 below refuses to mint anything but in-scope
// keys, membership is never granted by a key (02 §4.10), and M1 refuses to
// ELEVATE a user to role='admin' from a scoped key (hPatchUser) — admin is
// a global authority grant, and the composed chain scoped key -> register
// machine (global key, handlers_exec.go) -> PATCH role=admin is closed at
// BOTH links (machine registration itself refuses scoped keys).

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
	idn, ok := requireAdmin(s, w, r)
	if !ok {
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
	// 02 §8: scope only narrows, never widens. role='admin' is a GLOBAL
	// authority grant (the membership exemption of 02 §4.10 applies to
	// every project), so elevating any user to admin from a project-scoped
	// key mints authority outside its scope. In particular this closes the
	// composition where a scoped admin registers a machine — its api_key is
	// global (02 §4.10) — and then patches that machine user to admin,
	// turning the machine key into an unscoped admin credential.
	if idn.KeyScope != nil && body.Role == "admin" {
		writeErr(w, ownerAPI("CORE-403", "scoped key cannot grant the admin role"))
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
	idn, ok := requireAdmin(s, w, r)
	if !ok {
		return
	}
	var body struct {
		UserID    string  `json:"user_id"`
		Name      string  `json:"name"`
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
	// 02 §8: scope only narrows. A scoped admin key may mint keys for its
	// own project only — a NULL (unscoped) or out-of-scope project_id would
	// hand out authority wider than the caller's own key and is refused.
	if idn.KeyScope != nil && (body.ProjectID == nil || *body.ProjectID != *idn.KeyScope) {
		writeErr(w, ownerAPI("CORE-403", "scoped key may only issue keys for its own project"))
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
	idn, ok := requireAdmin(s, w, r)
	if !ok {
		return
	}
	// A scoped key lists only its own scope's keys (02 §8: out-of-scope
	// objects are rejected even in list/filter requests).
	query := `SELECT id, user_id, project_id, revoked_at FROM api_keys`
	args := []any{}
	if idn.KeyScope != nil {
		query += ` WHERE project_id=?1`
		args = append(args, *idn.KeyScope)
	}
	query += ` ORDER BY id`
	rows, err := s.DB.Query(query, args...)
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
	idn, ok := requireAdmin(s, w, r)
	if !ok {
		return
	}
	// A scoped key may only revoke keys inside its own scope (02 §8); a
	// NULL-scope target key is a global object, outside a scoped caller's
	// narrowed face.
	if idn.KeyScope != nil {
		var proj sql.NullString
		err := s.DB.QueryRow(`SELECT project_id FROM api_keys WHERE id=?1`, r.PathValue("id")).Scan(&proj)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, ownerAPI("CORE-404", "api key"))
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		if !proj.Valid || proj.String != *idn.KeyScope {
			writeErr(w, ownerAPI("CORE-403", "scoped key may only revoke keys in its own project"))
			return
		}
	}
	if _, err := s.DB.Exec(`UPDATE api_keys SET revoked_at=?1 WHERE id=?2`, store.Now(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// M3: projects CRUD + membership management (admin or project maintainer).
func (s *Server) hCreateProject(w http.ResponseWriter, r *http.Request) {
	idn, ok := requireAdmin(s, w, r)
	if !ok {
		return
	}
	// A newly created project is outside any existing key scope by
	// definition; a scoped key's narrowed face must not extend (02 §8).
	if idn.KeyScope != nil {
		writeErr(w, ownerAPI("CORE-403", "scoped key cannot create projects outside its scope"))
		return
	}
	var body struct {
		Name    string `json:"name"`
		Visible *bool  `json:"visible"`
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
		WHERE (p.visible=1 OR m.user_id IS NOT NULL OR ?2='admin') AND (?3 IS NULL OR p.id=?3)`,
		idn.UserID, idn.Role, idn.KeyScope)
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
	// Key scope precedes the membership read check (02 §8: a scoped key is
	// rejected for out-of-scope projects on every route, scope only narrows).
	if err := s.requireScope(idn, id); err != nil {
		writeErr(w, err)
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
	// Key scope precedes the admin/maintainer check (02 §8: a scoped key
	// is rejected for out-of-scope projects on every route).
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
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
		// RemoveMembers (fix #18, M3): explicit removal list — deliberately
		// simpler than an expected_removals/CAS scheme; a member listed here is
		// physically deleted from project_members. Gated by the same
		// admin-or-maintainer check as the rest of this endpoint.
		RemoveMembers []string `json:"remove_members"`
		Visible       *bool    `json:"visible"`
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
	// Removal runs AFTER the upsert loop so an explicit removal wins if the
	// same user_id somehow appears in both lists of one request.
	for _, uid := range body.RemoveMembers {
		if _, err := s.DB.Exec(`DELETE FROM project_members WHERE project_id=?1 AND user_id=?2`, projectID, uid); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// M4: on-demand backup snapshot (VACUUM INTO under <data_dir>/backups).
// The write side of the backupBarrier (07 §6.2): it blocks until every
// memory write path (06 §4.6 create/update/redact, each holding the read
// side across its whole write order) has drained, and no new one enters
// until the SQLite snapshot is taken — so the snapshot can never observe
// a "files changed but Core not committed" (or inverse) pair.
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
	memory.BackupBarrier.Lock()
	defer memory.BackupBarrier.Unlock()
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
