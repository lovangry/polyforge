// Package testsupport provides shared fixtures for core-domain tests:
// base seed data (project/users), a valid demo workflow bundle, and file-tree
// helpers. Test-only; never imported by production code.
package testsupport

import (
	"database/sql"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/core/store"
)

// SeedBase creates the minimal identity/project fixture every core test uses.
func SeedBase(db *store.DB) error {
	if _, err := db.Exec(`INSERT INTO projects(id, name) VALUES ('pj1','p1')`); err != nil {
		return err
	}
	users := []struct{ id, name, kind, role string }{
		{"usr_worker", "worker", "human", "writer"},
		{"usr_other", "other", "human", "writer"},
		{"usr_admin", "admin", "human", "admin"},
	}
	for _, u := range users {
		if _, err := db.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1,?2,?3,?4)`,
			u.id, u.name, u.kind, u.role); err != nil {
			return err
		}
	}
	for _, m := range []struct{ uid, name string }{
		{"usr_worker", "worker"}, {"usr_other", "other"}, {"usr_admin", "admin"},
	} {
		if _, err := db.Exec(`INSERT INTO project_members(project_id, user_id, role, created_at) VALUES ('pj1', ?1, 'writer', '2026-01-01T00:00:00Z')`, m.uid); err != nil {
			return err
		}
	}
	// Machine users + registrations (claims carry machine_id bound here).
	for _, m := range []struct{ mid, uid, name string }{
		{"mac_a", "mac_user_a", "machine-a"},
		{"mac_b", "mac_user_b", "machine-b"},
	} {
		if _, err := db.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1, ?2, 'machine', 'writer')`,
			m.uid, m.name); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO machines(id, user_id, last_heartbeat_at) VALUES (?1, ?2, '2026-01-01T00:00:00Z')`,
			m.mid, m.uid); err != nil {
			return err
		}
	}
	return nil
}

// MakeDemoBundle writes a minimal valid workflow bundle (name "demo") under
// wfDir/workflows/demo with one code step.
func MakeDemoBundle(wfDir string) error {
	return WriteFileTree(wfDir, map[string]string{
		"workflows/demo/workflow.md":  "---\nname: demo\nbudget: { requests: 100 }\nsteps:\n  - id: code\n    kind: code\n---\nbody\n",
		"workflows/demo/agents/code.md": "---\nname: code\ndescription: c\ntools: bash\noutput: {type: object}\n---\n",
	})
}

// WriteFileTree writes every rel→content pair under root (mkdir -p).
func WriteFileTree(root string, files map[string]string) error {
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// OpenTestDB opens a migrated Core DB in a temp dir and registers the
// service (lock file is not taken; service.Register only self-registers).
func OpenTestDB(dir string) (*store.DB, error) { return store.Open(dir) }

var _ = sql.Open
