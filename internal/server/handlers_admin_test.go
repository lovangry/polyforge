package server_test

// FA-4 regressions: project-scope enforcement on the management face
// (M2/M3). 02 §8 pins that an api_keys.project_id-scoped key is rejected
// for every out-of-scope project object — including list/filter requests —
// and that scope only ever narrows. A scoped ADMIN key is the sharpest
// probe: its user satisfies every role check, so only the scope checks
// below can stop it.

import (
	"testing"
)

func TestAdminScopedKeyScopeEnforcement(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.Exec(`INSERT INTO projects(id, name) VALUES ('pj2','p2')`); err != nil {
		t.Fatal(err)
	}
	pj1 := "pj1"
	scopedAdmin := e.mintKey(t, e.db, "usr_admin", &pj1)

	// M2 create: an unscoped (NULL project_id) key would widen authority
	// beyond the caller's own key — refused.
	e.doWantStatus(t, "scoped admin minting unscoped key", 403, "POST", "/api/v1/api-keys",
		scopedAdmin, map[string]any{"user_id": "usr_other"})
	// M2 create: out-of-scope project_id — refused.
	e.doWantStatus(t, "scoped admin minting out-of-scope key", 403, "POST", "/api/v1/api-keys",
		scopedAdmin, map[string]any{"user_id": "usr_other", "project_id": "pj2"})
	// M2 create: in-scope project_id — allowed.
	st, m, _ := e.do(t, "POST", "/api/v1/api-keys", scopedAdmin,
		map[string]any{"user_id": "usr_other", "project_id": "pj1"})
	if st != 201 {
		t.Fatalf("scoped admin minting in-scope key: %d %v", st, m)
	}
	inScopeKey := m["id"].(string)

	// M2 list: only the scope's keys are visible — no NULL-scope keys.
	st, m, _ = e.do(t, "GET", "/api/v1/api-keys", scopedAdmin, nil)
	if st != 200 {
		t.Fatalf("scoped admin list keys: %d %v", st, m)
	}
	items, _ := m["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("scoped admin list unexpectedly empty: %v", m)
	}
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row["project_id"] != "pj1" {
			t.Fatalf("scoped admin sees out-of-scope key: %v", row)
		}
	}

	// M2 revoke: NULL-scope target — refused; unknown id — 404; in-scope
	// target — allowed.
	if _, err := e.db.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES ('key_victim','usr_other',NULL,'deadbeef')`); err != nil {
		t.Fatal(err)
	}
	e.doWantStatus(t, "scoped admin revoking unscoped key", 403, "DELETE", "/api/v1/api-keys/key_victim",
		scopedAdmin, nil)
	e.doWantStatus(t, "scoped admin revoking unknown key", 404, "DELETE", "/api/v1/api-keys/key_nope",
		scopedAdmin, nil)
	e.doWantStatus(t, "scoped admin revoking in-scope key", 200, "DELETE", "/api/v1/api-keys/"+inScopeKey,
		scopedAdmin, nil)

	// M3 create project: a new project is outside the narrowed scope by
	// definition — refused.
	e.doWantStatus(t, "scoped admin creating project", 403, "POST", "/api/v1/projects",
		scopedAdmin, map[string]any{"name": "p3"})

	// M3 get: out-of-scope project — refused; in-scope — serves.
	e.doWantStatus(t, "scoped admin get out-of-scope project", 403, "GET", "/api/v1/projects/p2",
		scopedAdmin, nil)
	e.doWantStatus(t, "scoped admin get in-scope project", 200, "GET", "/api/v1/projects/p1",
		scopedAdmin, nil)

	// M3 list: only the scope project is visible.
	st, m, _ = e.do(t, "GET", "/api/v1/projects", scopedAdmin, nil)
	if st != 200 {
		t.Fatalf("scoped admin list projects: %d %v", st, m)
	}
	items, _ = m["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "pj1" {
		t.Fatalf("scoped admin project list not narrowed to scope: %v", items)
	}

	// M3 patch: out-of-scope project — refused before any role logic; an
	// empty in-scope patch still serves.
	e.doWantStatus(t, "scoped admin patch out-of-scope project", 403, "PATCH", "/api/v1/projects/p2",
		scopedAdmin, map[string]any{})
	e.doWantStatus(t, "scoped admin patch in-scope project", 200, "PATCH", "/api/v1/projects/p1",
		scopedAdmin, map[string]any{})

	// Sanity: an UNSCOPED admin key keeps the full management face.
	e.doWantStatus(t, "unscoped admin minting unscoped key", 201, "POST", "/api/v1/api-keys",
		e.admin, map[string]any{"user_id": "usr_other"})
	e.doWantStatus(t, "unscoped admin get p2", 200, "GET", "/api/v1/projects/p2", e.admin, nil)
	e.doWantStatus(t, "unscoped admin revoke global key", 200, "DELETE", "/api/v1/api-keys/key_victim",
		e.admin, nil)
}

// TestScopedAdminCannotMintUnscopedAdminCredential (R2C-F1 regression):
// a project-scoped admin key must never mint or elevate anything outside
// its scope (02 §8). The composed escalation chain under test: scoped
// admin -> POST /machines/heartbeat (registers a machine and hands back
// the plaintext of a GLOBAL, NULL-scope machine key, 02 §4.10) ->
// PATCH /users/{machine-user} role=admin -> the machine key now
// authenticates an unscoped admin. Both links are closed: registration
// refuses scoped keys, and the role PATCH refuses admin grants from
// scoped keys.
func TestScopedAdminCannotMintUnscopedAdminCredential(t *testing.T) {
	e := newEnv(t)
	pj1 := "pj1"
	scopedAdmin := e.mintKey(t, e.db, "usr_admin", &pj1)

	// Link 1: scoped key registering a machine — refused. No machine row
	// and, critically, no machine api_key plaintext may be handed out.
	e.doWantStatus(t, "scoped admin registered a machine", 403, "POST", "/api/v1/machines/heartbeat",
		scopedAdmin, map[string]any{"machine_id": "mac_evil", "runtime_version": "v0"})
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE id='mac_evil'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("machine row created despite the refused registration")
	}

	// Link 2: scoped key elevating the pre-existing machine user (mac_user_a,
	// whose api_key is global) to admin — refused; the grant is a global
	// authority change outside any single project scope.
	e.doWantStatus(t, "scoped admin granted role=admin", 403, "PATCH", "/api/v1/users/mac_user_a",
		scopedAdmin, map[string]any{"role": "admin"})
	var role string
	if err := e.db.QueryRow(`SELECT role FROM users WHERE id='mac_user_a'`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "writer" {
		t.Fatalf("machine user elevated despite refusal: %s", role)
	}
	// The refusal is specific to the admin grant: display_name edits and
	// writer role from the scoped key keep working (M1 stays reachable for
	// global non-authority CRUD, 02 §8 frames scope over project objects).
	e.doWantStatus(t, "scoped admin writer role PATCH", 200, "PATCH", "/api/v1/users/mac_user_a",
		scopedAdmin, map[string]any{"role": "writer"})

	// Sanity, the unscoped deployment operator path (02 §4.10 row 21)
	// still registers: the machine key comes back exactly once and the
	// machine user stays role='writer' (no self-escalation surface).
	st, m, _ := e.do(t, "POST", "/api/v1/machines/heartbeat", e.admin,
		map[string]any{"machine_id": "mac_fresh", "runtime_version": "v1"})
	if st != 200 {
		t.Fatalf("unscoped admin registration: %d %v", st, m)
	}
	machineKey, _ := m["api_key"].(string)
	if machineKey == "" {
		t.Fatalf("registration returned no api_key: %v", m)
	}
	if err := e.db.QueryRow(`SELECT role FROM users WHERE id=?1`, m["user_id"]).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "writer" {
		t.Fatalf("registered machine user role: %s", role)
	}
	// And an unscoped admin may still grant role=admin on M1 (the whole
	// chain is closed only against SCOPED keys).
	e.doWantStatus(t, "unscoped admin role grant", 200, "PATCH", "/api/v1/users/mac_user_a",
		e.admin, map[string]any{"role": "admin"})
}
