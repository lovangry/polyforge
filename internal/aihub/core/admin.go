package core

// Admin face: users, api keys, machines, projects and the project
// identifier rotation. v0 semantics: secrets are stored hash-only and
// returned in plaintext exactly once at issuance.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// PatchUser updates display_name and/or the global role (M1). Empty
// fields mean "leave unchanged".
func (m *AdminManager) PatchUser(id, displayName, role string) (*api.User, error) {
	if strings.TrimSpace(displayName) == "" && role == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "nothing to update: display_name and/or role required")
	}
	if role != "" && role != "admin" && role != "writer" {
		return nil, Fail(api.ErrCoreBadInput, "", "role must be admin or writer")
	}
	res, err := m.db.Exec(`UPDATE users SET display_name=CASE WHEN ?1<>'' THEN ?1 ELSE display_name END,
		role=CASE WHEN ?2<>'' THEN ?2 ELSE role END WHERE id=?3`, displayName, role, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreNotFound, "", "user %s", id)
	}
	return m.GetUser(id)
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
		VALUES (?1,?2,?3,?4,?5,?6)`, id, userID, scopeAny, name, Sha256Hex(plain), Now()); err != nil {
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

// APIKeyInfo is one admin-list row (M2 list): metadata only — never the
// hash, never material.
type APIKeyInfo struct {
	ID        string  `json:"id"`
	UserID    string  `json:"user_id"`
	ProjectID *string `json:"project_id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

// ListAPIKeys lists key metadata (M2), newest first, revoked rows last
// within a user. Only metadata is returned; plaintext exists exactly once
// at issuance and the hash never leaves the store.
func (m *AdminManager) ListAPIKeys() ([]APIKeyInfo, error) {
	rows, err := m.db.Query(`SELECT id, user_id, project_id, name, created_at, revoked_at
		FROM api_keys ORDER BY created_at DESC, id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKeyInfo{}
	for rows.Next() {
		var k APIKeyInfo
		var projectID, revoked sql.NullString
		if err := rows.Scan(&k.ID, &k.UserID, &projectID, &k.Name, &k.CreatedAt, &revoked); err != nil {
			return nil, err
		}
		if projectID.Valid {
			p := projectID.String
			k.ProjectID = &p
		}
		if revoked.Valid {
			r := revoked.String
			k.RevokedAt = &r
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Machines

// MachineHeartbeat is the machines/heartbeat body. First call registers
// (creates the machine user + issues its key, plaintext once); repeats
// only refresh the timestamp and the liveness projection. There is no
// client timestamp field: last_heartbeat_at is stamped by the SERVER
// (core.Now, RFC3339Nano, single-writer SQLite) — client clocks are not
// trusted for liveness. Mode/liveWIIDs are the M3 daemon liveness
// projection (mode empty = keep the stored value; live_wi_ids nil = keep,
// non-nil including empty = replace).
type MachineHeartbeat struct {
	MachineID      string   `json:"machine_id"`
	RuntimeVersion string   `json:"runtime_version,omitempty"`
	DisplayName    string   `json:"display_name,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	LiveWIIDs      []string `json:"live_wi_ids,omitempty"`
}

// HeartbeatResult is the heartbeat response.
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
	live := nullableJSON(req.LiveWIIDs)
	// Existing machine: refresh only.
	var userID string
	err := m.db.QueryRow(`SELECT user_id FROM machines WHERE id=?1`, req.MachineID).Scan(&userID)
	if err == nil {
		var rv, md any
		if req.RuntimeVersion != "" {
			rv = req.RuntimeVersion
		}
		if req.Mode != "" {
			md = req.Mode
		}
		if _, err := m.db.Exec(`UPDATE machines SET last_heartbeat_at=?2, runtime_version=COALESCE(?3, runtime_version),
			mode=COALESCE(?4, mode), live_wi_ids=COALESCE(?5, live_wi_ids)
			WHERE id=?1`, req.MachineID, Now(), rv, md, live); err != nil {
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
	if _, err := tx.Exec(`INSERT INTO machines(id, user_id, runtime_version, mode, live_wi_ids, last_heartbeat_at)
		VALUES (?1,?2,?3,?4,?5,?6)`, req.MachineID, uid, req.RuntimeVersion, req.Mode, live, Now()); err != nil {
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
		VALUES (?1,?2,NULL,'machine registration',?3,?4)`, keyID, uid, Sha256Hex(plain), Now()); err != nil {
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

// MachineRow is one machines read-model row (the admin machines face).
type MachineRow struct {
	ID              string   `json:"id"`
	UserID          string   `json:"user_id"`
	RuntimeVersion  string   `json:"runtime_version,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	LiveWIIDs       []string `json:"live_wi_ids"`
	LastHeartbeatAt string   `json:"last_heartbeat_at"`
}

// Machines lists every registered machine oldest-heartbeat-first (the
// admin read face behind GET /api/v1/machines; the server computes the
// stale projection — staleness is presentation, not storage).
func (m *AdminManager) Machines() ([]MachineRow, error) {
	rows, err := m.db.Query(`SELECT id, user_id, COALESCE(runtime_version,''), COALESCE(mode,''),
		COALESCE(live_wi_ids,'[]'), last_heartbeat_at FROM machines ORDER BY last_heartbeat_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MachineRow
	for rows.Next() {
		var r MachineRow
		var live string
		if err := rows.Scan(&r.ID, &r.UserID, &r.RuntimeVersion, &r.Mode, &live, &r.LastHeartbeatAt); err != nil {
			return nil, err
		}
		r.LiveWIIDs = []string{}
		_ = json.Unmarshal([]byte(live), &r.LiveWIIDs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullableJSON marshals ids for a nullable JSON-array column: nil stays
// NULL ("keep the stored value" in the upsert's COALESCE), an empty slice
// stores [] ("replace with nothing held live").
func nullableJSON(ids []string) any {
	if ids == nil {
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil // []string always marshals; defensive only
	}
	return string(b)
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
		id, req.Name, req.Description, string(repos), visible, Sha256Hex(identifier), now); err != nil {
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
		Sha256Hex(plain), Now(), idOrName, idOrName)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", Fail(api.ErrCoreNotFound, "", "project %q", idOrName)
	}
	return plain, nil
}

// ---------------------------------------------------------------------------
// Project patch (M3): description/visible plus membership management.

// ProjectPatchRequest is the PATCH /api/v1/projects/{id} body. The wire
// shape is pinned by the client (aihubclient.ProjectPatchRequest):
// `members` is an explicit UPSERT list (not a wholesale replacement) and
// `remove_members` is an explicit remove list.
type ProjectPatchRequest struct {
	Description   *string              `json:"description,omitempty"`
	Members       *[]api.ProjectMember `json:"members,omitempty"`
	RemoveMembers *[]string            `json:"remove_members,omitempty"`
	Visible       *bool                `json:"visible,omitempty"`
	// Repos replaces the whole task-repo inventory (explicit full
	// replacement; every entry is named by the caller, same tradeoff as
	// the membership rows below).
	Repos *[]api.ProjectRepo `json:"repos,omitempty"`
}

// PatchProject applies a project patch. Membership CAS tradeoff vs v0:
// v0 guarded a wholesale member-list replacement with members_version
// plus expected_removals (a shorter-than-intended list could otherwise
// silently drop members). The v1 wire shape cannot express that hazard —
// every mutation names its user explicitly — so the version token is
// dropped: upserts and removes are per-row idempotent operations and
// concurrent patches converge row-wise under the single SQLite writer.
// Removal runs AFTER the upsert loop, so an explicit removal wins when
// the same user appears in both lists of one request.
func (m *AdminManager) PatchProject(idOrName string, req ProjectPatchRequest) (*api.Project, error) {
	var projectID string
	err := m.db.QueryRow(`SELECT id FROM projects WHERE id=?1 OR name=?1`, idOrName).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "project %q", idOrName)
	}
	if err != nil {
		return nil, err
	}
	if req.Members != nil {
		for _, mem := range *req.Members {
			switch mem.Role {
			case "viewer", "writer", "maintainer":
			default:
				return nil, Fail(api.ErrCoreBadInput, "", "member role must be viewer|writer|maintainer (got %q)", mem.Role)
			}
			// Pre-check the user so a bad id answers 404, not an FK 500.
			var one int
			if err := m.db.QueryRow(`SELECT 1 FROM users WHERE id=?1`, mem.UserID).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, Fail(api.ErrCoreNotFound, "", "user %s", mem.UserID)
				}
				return nil, err
			}
			if err := m.AddMember(projectID, mem.UserID, mem.Role); err != nil {
				return nil, err
			}
		}
	}
	if req.RemoveMembers != nil {
		for _, uid := range *req.RemoveMembers {
			if _, err := m.db.Exec(`DELETE FROM project_members WHERE project_id=?1 AND user_id=?2`,
				projectID, uid); err != nil {
				return nil, err
			}
		}
	}
	sets := []string{"updated_at=?"}
	args := []any{Now()}
	if req.Description != nil {
		sets = append(sets, "description=?")
		args = append(args, *req.Description)
	}
	if req.Visible != nil {
		sets = append(sets, "visible=?")
		args = append(args, *req.Visible)
	}
	if req.Repos != nil {
		blob, err := json.Marshal(*req.Repos)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "repos=?")
		args = append(args, string(blob))
	}
	args = append(args, projectID)
	if _, err := m.db.Exec(`UPDATE projects SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return nil, err
	}
	return m.GetProject(projectID)
}

// ---------------------------------------------------------------------------
// Backup (M4) and service-secret rotation (M5)

// sanitizeFilename keeps a backup name filesystem-safe.
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

// Backup takes an on-demand SQLite snapshot (M4) via VACUUM INTO under
// <dataDir>/backups and returns the snapshot file name. Scope: the SQLite
// file only — memory content files (dataDir/memory) are NOT part of this
// snapshot (the v0 backupBarrier machinery is not part of the v1 lane).
func (m *AdminManager) Backup(dataDir string) (string, error) {
	if dataDir == "" {
		return "", Fail(api.ErrCoreInternal, "", "backup: data dir not configured")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "backups"), 0o750); err != nil {
		return "", err
	}
	name := sanitizeFilename("aihub-" + Now() + ".db")
	target := filepath.Join(dataDir, "backups", name)
	if _, err := m.db.Exec(`VACUUM INTO ?1`, target); err != nil {
		return "", Fail("CORE-507", "", "vacuum into %s: %v", target, err)
	}
	return name, nil
}

// RotateSecret records a service-secret epoch rotation (M5, 02 §6.6).
// Session-model pushes are fenced by the per-WI session hash on
// work_items — no validation path consumes a shared service secret — so
// rotation maintains the epoch LEDGER only: the M5 wire contract and
// its audit trail. mode=grace requires grace_seconds > 0
// and retires the superseded epoch at now+grace; mode=rollback forces
// grace to zero and retires every older live epoch immediately.
func (m *AdminManager) RotateSecret(mode string, graceSeconds int64, rotatedBy string) (int, error) {
	if mode == "" {
		mode = "grace"
	}
	now := Now()
	var retireExpr string
	var retireArgs []any
	switch mode {
	case "grace":
		if graceSeconds <= 0 {
			return 0, Fail(api.ErrCoreBadInput, "", "grace_seconds must be > 0 for mode=grace")
		}
		retireExpr = `?`
		expire := time.Now().UTC().Add(time.Duration(graceSeconds) * time.Second).Format(time.RFC3339Nano)
		retireArgs = append(retireArgs, expire)
	case "rollback":
		if graceSeconds != 0 {
			return 0, Fail(api.ErrCoreBadInput, "", "grace_seconds must be 0 for mode=rollback")
		}
		retireExpr = `?`
		retireArgs = append(retireArgs, now)
	default:
		return 0, Fail(api.ErrCoreBadInput, "", "mode must be grace or rollback")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var maxEpoch sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(epoch) FROM service_secret_epochs`).Scan(&maxEpoch); err != nil {
		return 0, err
	}
	epoch := maxEpoch.Int64 + 1
	var actor any
	if rotatedBy != "" {
		actor = rotatedBy
	}
	if _, err := tx.Exec(`INSERT INTO service_secret_epochs(epoch, mode, created_at, rotated_by)
		VALUES (?1,?2,?3,?4)`, epoch, mode, now, actor); err != nil {
		return 0, err
	}
	// Retire the superseded epochs: for grace, at the end of the window;
	// for rollback, immediately and permanently.
	args := append([]any{}, retireArgs...)
	args = append(args, epoch)
	if _, err := tx.Exec(`UPDATE service_secret_epochs SET retired_at=`+retireExpr+`
		WHERE epoch < ?2 AND retired_at IS NULL`, args...); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO core_meta(key, value) VALUES ('current_secret_epoch', ?1)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(epoch, 10)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(epoch), nil
}
