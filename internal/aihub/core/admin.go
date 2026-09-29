package core

// Admin face: users, api keys, machines, projects and the project
// identifier rotation. v0 semantics: secrets are stored hash-only and
// returned in plaintext exactly once at issuance.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// AdminManager serves the management endpoints (M1–M5 shape, v1 subset).
type AdminManager struct{ db *DB }

func NewAdminManager(db *DB) *AdminManager { return &AdminManager{db: db} }

// ---------------------------------------------------------------------------
// Users

// CreateUser inserts a human or machine identity. Email is required for
// humans, generated for machines.
func (m *AdminManager) CreateUser(displayName, email, role, userType string) (*api.User, error) {
	if strings.TrimSpace(displayName) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "display_name required")
	}
	if role == "" {
		role = "writer"
	}
	if role != "admin" && role != "writer" {
		return nil, Fail(api.ErrCoreBadInput, "", "role must be admin or writer")
	}
	if userType == "" {
		userType = "human"
	}
	if userType != "human" && userType != "machine" {
		return nil, Fail(api.ErrCoreBadInput, "", "user_type must be human or machine")
	}
	if userType == "human" && strings.TrimSpace(email) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "email required for human users")
	}
	id, err := NewID("usr_")
	if err != nil {
		return nil, err
	}
	var emailAny any
	if email != "" {
		emailAny = email
	}
	if _, err := m.db.Exec(`INSERT INTO users(id, display_name, email, kind, role, created_at)
		VALUES (?1,?2,?3,?4,?5,?6)`, id, displayName, emailAny, userType, role, Now()); err != nil {
		return nil, err
	}
	return m.GetUser(id)
}

func (m *AdminManager) GetUser(id string) (*api.User, error) {
	var u api.User
	var email sql.NullString
	err := m.db.QueryRow(`SELECT id, display_name, email, kind, role, created_at FROM users WHERE id=?1`, id).
		Scan(&u.ID, &u.DisplayName, &email, &u.UserType, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "user %s", id)
	}
	if err != nil {
		return nil, err
	}
	u.Email = email.String
	u.APITokens = []api.APIToken{}
	return &u, nil
}

// ListUsers returns users (with token metadata, never hashes).
func (m *AdminManager) ListUsers() ([]api.User, error) {
	rows, err := m.db.Query(`SELECT id FROM users ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []api.User{}
	for _, id := range ids {
		u, err := m.GetUser(id)
		if err != nil {
			return nil, err
		}
		rows, err := m.db.Query(`SELECT id, name, created_at FROM api_keys WHERE user_id=?1 AND revoked_at IS NULL`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t api.APIToken
			if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			u.APITokens = append(u.APITokens, t)
		}
		rows.Close()
		out = append(out, *u)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// API keys

// CreateAPIKey issues a key for a user; plaintext is returned ONCE.
type IssuedKey struct {
	Token api.APIToken `json:"token"`
	Plain string       `json:"plain"` // shown exactly once
}

func (m *AdminManager) CreateAPIKey(userID, name, projectScope string) (*IssuedKey, error) {
	var one int
	if err := m.db.QueryRow(`SELECT 1 FROM users WHERE id=?1`, userID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "user %s", userID)
		}
		return nil, err
	}
	if projectScope != "" {
		if err := m.db.QueryRow(`SELECT 1 FROM projects WHERE id=?1 OR name=?1`, projectScope).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, Fail(api.ErrCoreNotFound, "", "project %q", projectScope)
			}
			return nil, err
		}
		var scopeID string
		if err := m.db.QueryRow(`SELECT id FROM projects WHERE id=?1 OR name=?1`, projectScope).Scan(&scopeID); err != nil {
			return nil, err
		}
		projectScope = scopeID
	}
	id, err := NewID("key_")
	if err != nil {
		return nil, err
	}
	plain, err := NewSecret(24)
	if err != nil {
		return nil, err
	}
	var scopeAny any
	if projectScope != "" {
		scopeAny = projectScope
	}
	if _, err := m.db.Exec(`INSERT INTO api_keys(id, user_id, project_id, name, key_hash, created_at)
		VALUES (?1,?2,?3,?4,?5,?6)`, id, userID, scopeAny, name, sha256Hex(plain), Now()); err != nil {
		return nil, err
	}
	return &IssuedKey{
		Token: api.APIToken{ID: id, Name: name, CreatedAt: Now()},
		Plain: plain,
	}, nil
}

func (m *AdminManager) RevokeAPIKey(keyID string) error {
	res, err := m.db.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=?1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either absent or already revoked: both are terminal for the key.
		var one int
		if err := m.db.QueryRow(`SELECT 1 FROM api_keys WHERE id=?1`, keyID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Fail(api.ErrCoreNotFound, "", "api key %s", keyID)
			}
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Machines

// MachineHeartbeat is the machines/heartbeat body. First call registers
// (creates the machine user + issues its key, plaintext once); repeats
// only refresh the timestamp.
type MachineHeartbeat struct {
	MachineID      string `json:"machine_id"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
}

type HeartbeatResult struct {
	Registered bool       `json:"registered"` // first call created the identity
	MachineID  string     `json:"machine_id"`
	UserID     string     `json:"user_id"`
	Key        *IssuedKey `json:"api_key,omitempty"` // set on first registration only
}

func (m *AdminManager) Heartbeat(req MachineHeartbeat) (*HeartbeatResult, error) {
	if strings.TrimSpace(req.MachineID) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "machine_id required")
	}
	// Existing machine: refresh only.
	var userID string
	err := m.db.QueryRow(`SELECT user_id FROM machines WHERE id=?1`, req.MachineID).Scan(&userID)
	if err == nil {
		var rv any
		if req.RuntimeVersion != "" {
			rv = req.RuntimeVersion
		}
		if _, err := m.db.Exec(`UPDATE machines SET last_heartbeat_at=?, runtime_version=COALESCE(?, runtime_version)
			WHERE id=?1`, Now(), rv, req.MachineID); err != nil {
			return nil, err
		}
		return &HeartbeatResult{MachineID: req.MachineID, UserID: userID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Register: machine user + machines row + first key, one transaction.
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	uid, err := NewID("mac_")
	if err != nil {
		return nil, err
	}
	display := req.DisplayName
	if display == "" {
		display = "machine " + req.MachineID
	}
	email := uid + "@machines.local"
	if _, err := tx.Exec(`INSERT INTO users(id, display_name, email, kind, role, created_at)
		VALUES (?1,?2,?3,'machine','writer',?4)`, uid, display, email, Now()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO machines(id, user_id, runtime_version, last_heartbeat_at)
		VALUES (?1,?2,?3,?4)`, req.MachineID, uid, req.RuntimeVersion, Now()); err != nil {
		return nil, err
	}
	keyID, err := NewID("key_")
	if err != nil {
		return nil, err
	}
	plain, err := NewSecret(24)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO api_keys(id, user_id, project_id, name, key_hash, created_at)
		VALUES (?1,?2,NULL,'machine registration',?3,?4)`, keyID, uid, sha256Hex(plain), Now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &HeartbeatResult{
		Registered: true,
		MachineID:  req.MachineID,
		UserID:     uid,
		Key: &IssuedKey{
			Token: api.APIToken{ID: keyID, Name: "machine registration", CreatedAt: Now()},
			Plain: plain,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Projects

var projectNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-_]{0,39}$`)

// CreateProjectRequest is the projects create body.
type CreateProjectRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Repos       []api.ProjectRepo `json:"repos"`
	Visible     *bool             `json:"visible"`
}

// CreateProject inserts a project and mints its identifier token
// (plaintext returned once; hash stored — the same discipline as api keys).
type ProjectWithSecret struct {
	Project    api.Project `json:"project"`
	Identifier string      `json:"identifier"` // plaintext, shown once
}

func (m *AdminManager) CreateProject(req CreateProjectRequest) (*ProjectWithSecret, error) {
	if !projectNameRe.MatchString(req.Name) {
		return nil, Fail(api.ErrCoreBadInput, "", "project name must be 1-40 chars of [a-z0-9-_]")
	}
	if req.Repos == nil {
		req.Repos = []api.ProjectRepo{}
	}
	repos, err := json.Marshal(req.Repos)
	if err != nil {
		return nil, err
	}
	visible := true
	if req.Visible != nil {
		visible = *req.Visible
	}
	id, err := NewID("prj_")
	if err != nil {
		return nil, err
	}
	identifier, err := NewSecret(24)
	if err != nil {
		return nil, err
	}
	now := Now()
	if _, err := m.db.Exec(`INSERT INTO projects(id, name, description, repos, visible, identifier_hash, created_at, updated_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?7)`,
		id, req.Name, req.Description, string(repos), visible, sha256Hex(identifier), now); err != nil {
		if isUniqueViolation(err) {
			return nil, Fail(api.ErrCoreConflict, "", "project %q already exists", req.Name)
		}
		return nil, err
	}
	p, err := m.GetProject(id)
	if err != nil {
		return nil, err
	}
	return &ProjectWithSecret{Project: *p, Identifier: identifier}, nil
}

func (m *AdminManager) GetProject(idOrName string) (*api.Project, error) {
	var p api.Project
	var visible int
	var repos string
	var scenario sql.NullString
	err := m.db.QueryRow(`SELECT id, name, description, repos, visible, scenario, created_at, updated_at
		FROM projects WHERE id=?1 OR name=?1`, idOrName).
		Scan(&p.ID, &p.Name, &p.Description, &repos, &visible, &scenario, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "project %q", idOrName)
	}
	if err != nil {
		return nil, err
	}
	p.Visible = visible != 0
	_ = json.Unmarshal([]byte(repos), &p.Repos)
	if p.Repos == nil {
		p.Repos = []api.ProjectRepo{}
	}
	if scenario.Valid {
		s := scenario.String
		_ = s // scenario kept for forward compatibility; not exposed on the v1 wire
	}
	p.Members, err = m.projectMembers(p.ID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *AdminManager) projectMembers(projectID string) ([]api.ProjectMember, error) {
	rows, err := m.db.Query(`SELECT user_id, role FROM project_members WHERE project_id=?1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.ProjectMember{}
	for rows.Next() {
		var mem api.ProjectMember
		if err := rows.Scan(&mem.UserID, &mem.Role); err != nil {
			return nil, err
		}
		out = append(out, mem)
	}
	return out, rows.Err()
}

// ListProjects returns every project (name/visibility only; the caller's
// object access is decided per object at the business endpoints).
func (m *AdminManager) ListProjects() ([]api.Project, error) {
	rows, err := m.db.Query(`SELECT id FROM projects ORDER BY created_at LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []api.Project{}
	for _, id := range ids {
		p, err := m.GetProject(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

// AddMember inserts/updates a membership row.
func (m *AdminManager) AddMember(projectID, userID, role string) error {
	switch role {
	case "viewer", "writer", "maintainer":
	default:
		return Fail(api.ErrCoreBadInput, "", "role must be viewer|writer|maintainer")
	}
	if _, err := m.db.Exec(`INSERT INTO project_members(project_id, user_id, role, created_at)
		VALUES (?1,?2,?3,?4) ON CONFLICT(project_id, user_id) DO UPDATE SET role=excluded.role`,
		projectID, userID, role, Now()); err != nil {
		return err
	}
	return nil
}

// RotateIdentifier mints a fresh project identifier; the plaintext is
// returned once, only the hash is kept (the old identifier dies at once).
func (m *AdminManager) RotateIdentifier(idOrName string) (string, error) {
	plain, err := NewSecret(24)
	if err != nil {
		return "", err
	}
	res, err := m.db.Exec(`UPDATE projects SET identifier_hash=?, updated_at=? WHERE id=? OR name=?`,
		sha256Hex(plain), Now(), idOrName, idOrName)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", Fail(api.ErrCoreNotFound, "", "project %q", idOrName)
	}
	return plain, nil
}
