package v0export

// fixture.go: a deterministic synthesizer of a miniature v0 pilot world
// — the v0 schema subset (users, projects, work_items, memories,
// capsule_revisions, capsule_heads), authority memory files in the exact
// v0 frontmatter format, and a capsule revision chain (manifests +
// zstd-compressed CAS blocks). The committed integration rehearsal
// (test/v1_s15) builds its fixture with this so CI never depends on the
// real pilot directory; the real rehearsal against
// /root/polyforge-pilot/aihub-data is a manual foreground run.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

// Fixture is the synthesized v0 world.
type Fixture struct {
	Dir     string // the v0 "data dir": aihub.db + memory/ + capsules/
	DBPath  string
	Users   []string // old user ids
	Project string   // old project id
	WIs     int
	Mems    int
}

// v0DDL is the v0 schema subset the export reads.
const v0DDL = `
CREATE TABLE users (
  id           TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('human','machine')),
  role         TEXT NOT NULL DEFAULT 'writer' CHECK (role IN ('admin','writer'))
);
CREATE TABLE projects (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  visible      INTEGER NOT NULL DEFAULT 1,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE TABLE work_items (
  id           TEXT PRIMARY KEY,
  seq          INTEGER NOT NULL UNIQUE,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  goal         TEXT NOT NULL,
  content      TEXT,
  status       TEXT NOT NULL DEFAULT 'queued'
               CHECK (status IN ('queued','running','paused','blocked','wrapped','failed','cancelled')),
  priority     TEXT NOT NULL DEFAULT 'normal',
  milestone    TEXT,
  labels       TEXT NOT NULL DEFAULT '[]',
  attrs        TEXT NOT NULL DEFAULT '{}',
  reporter     TEXT NOT NULL REFERENCES users(id),
  assignee     TEXT REFERENCES users(id),
  waiting_on   TEXT,
  bundle_name  TEXT,
  bundle_digest TEXT,
  workflow_budget_baseline TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  closed_at    TEXT
);
CREATE TABLE memories (
  id           TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  owner_user_id TEXT NOT NULL REFERENCES users(id),
  work_item_id TEXT REFERENCES work_items(id),
  type         TEXT NOT NULL,
  title        TEXT,
  visibility   TEXT NOT NULL,
  authority_path TEXT NOT NULL,
  content_sha  TEXT NOT NULL,
  content_len  INTEGER NOT NULL,
  tags         TEXT NOT NULL DEFAULT '[]',
  related      TEXT NOT NULL DEFAULT '[]',
  idempotency_key TEXT UNIQUE,
  status       TEXT NOT NULL DEFAULT 'active',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE TABLE capsule_revisions (
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  seq          INTEGER NOT NULL,
  parent       INTEGER,
  generation   INTEGER NOT NULL,
  manifest_sha TEXT NOT NULL,
  data_bytes   INTEGER NOT NULL,
  published_at TEXT NOT NULL,
  PRIMARY KEY (work_item_id, seq)
);
CREATE TABLE capsule_heads (
  work_item_id TEXT PRIMARY KEY REFERENCES work_items(id),
  generation   INTEGER NOT NULL,
  seq          INTEGER NOT NULL,
  updated_at   TEXT NOT NULL,
  FOREIGN KEY (work_item_id, seq) REFERENCES capsule_revisions(work_item_id, seq)
);
`

// BuildFixture synthesizes the v0 world under dir. Deterministic content,
// no timestamps beyond fixed literals.
func BuildFixture(dir string) (*Fixture, error) {
	f := &Fixture{Dir: dir, DBPath: filepath.Join(dir, "aihub.db"), Project: "pj_fixture0001"}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+f.DBPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.Exec(v0DDL); err != nil {
		return nil, err
	}
	f.Users = []string{"usr_root", "usr_owner00aa"}
	users := [][]string{
		{"usr_root", "fixture admin", "human"},
		{"usr_owner00aa", "fixture owner", "human"},
	}
	for _, u := range users {
		if _, err := db.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1,?2,?3,'admin')`, u[0], u[1], u[2]); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(`INSERT INTO projects(id, name, visible, created_at, updated_at)
		VALUES (?1,'fixture-pilot',1,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`, f.Project); err != nil {
		return nil, err
	}

	// Work items: one per record status the migration carries.
	wis := []struct {
		id, status, closed string
	}{
		{"wi_wrapped000001", "wrapped", "2026-09-02T10:00:00Z"},
		{"wi_failed0000001", "failed", "2026-09-02T11:00:00Z"},
		{"wi_blocked000001", "blocked", ""},
		{"wi_cancelled0001", "cancelled", "2026-09-02T12:00:00Z"}, // v0 double-l vocabulary
	}
	for i, wi := range wis {
		var closed any
		if wi.closed != "" {
			closed = wi.closed
		}
		if _, err := db.Exec(`INSERT INTO work_items(id, seq, project_id, goal, content, status, priority,
			labels, attrs, reporter, waiting_on, bundle_name, bundle_digest, workflow_budget_baseline,
			created_at, updated_at, closed_at)
			VALUES (?1,?2,?3,?4,?5,?6,'normal','["fixture"]','{}','usr_root',NULL,'quick-fix',NULL,NULL,
			'2026-09-01T08:00:00Z','2026-09-02T10:00:00Z',?7)`,
			wi.id, i+1, f.Project, "fixture goal "+wi.id, "# background\nfixture content for "+wi.id,
			wi.status, closed); err != nil {
			return nil, err
		}
	}
	f.WIs = len(wis)

	// Memories: active stock with a resolvable WI ref, redacted stock,
	// related links, tags. Authority files in the exact v0 format.
	memDirs := filepath.Join(dir, "memory", f.Project)
	type memSpec struct {
		id, typ, wi, status string
		tags, related       []string
	}
	mems := []memSpec{
		{"mem_fix00000001", "experience.debug", "wi_wrapped000001", "active",
			[]string{"fixture"}, []string{"mem_fix00000002"}},
		{"mem_fix00000002", "rule.work", "", "active", []string{}, []string{"mem_fix00000001"}},
		{"mem_fix00000003", "fact.architecture", "wi_blocked000001", "redacted", []string{}, []string{}},
	}
	for _, m := range mems {
		body := fmt.Sprintf("# fixture memory %s\n\n调试记录 fixture body 内容 with searchable token ZebraFix%d.\n", m.id, len(m.id))
		fm := "---\nid: " + m.id + "\ntype: " + m.typ + "\nowner_user_id: usr_owner00aa\n"
		if len(m.tags) > 0 {
			fm += "tags: [" + joinTags(m.tags) + "]\n"
		}
		fm += "visibility: project\n"
		if m.wi != "" {
			fm += "work_item_id: " + m.wi + "\n"
		}
		fm += "project_id: " + f.Project + "\nrelated: [" + joinTags(m.related) + "]\n" +
			"created_at: \"2026-09-01T09:00:00.123456Z\"\nupdated_at: \"2026-09-01T09:30:00.654321Z\"\n---\n"
		content := fm + body
		sub := filepath.Join(memDirs, m.id)
		if m.status == "redacted" {
			sub = filepath.Join(memDirs, "_redacted", m.id)
		}
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(sub, "memory.md"), []byte(content), 0o640); err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(content))
		rel := "memory/" + f.Project + "/" + m.id
		status := m.status
		if m.status == "redacted" {
			rel = "memory/" + f.Project + "/_redacted/" + m.id
		}
		var wiRef any
		if m.wi != "" {
			wiRef = m.wi
		}
		if _, err := db.Exec(`INSERT INTO memories(id, project_id, owner_user_id, work_item_id, type,
			visibility, authority_path, content_sha, content_len, tags, related, status, created_at, updated_at)
			VALUES (?1,?2,'usr_owner00aa',?3,?4,'project',?5,?6,?7,?8,?9,?10,
			'2026-09-01T09:00:00.123456Z','2026-09-01T09:30:00.654321Z')`,
			m.id, f.Project, wiRef, m.typ, rel, hex.EncodeToString(sum[:]), len(content),
			tagJSON(m.tags), tagJSON(m.related), status); err != nil {
			return nil, err
		}
	}
	f.Mems = len(mems)

	// Capsule chain for the wrapped ticket: seq 0 (full puts), seq 1
	// (incremental: one put + one delete), head=1.
	if err := buildCapsuleChain(dir, "wi_wrapped000001"); err != nil {
		return nil, err
	}
	return f, nil
}

func joinTags(tags []string) string {
	out := ""
	for i, t := range tags {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}

func tagJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

// buildCapsuleChain writes rev/0 + rev/1 manifests and zstd CAS blocks,
// registers the rows, sets head=1.
func buildCapsuleChain(dir, wi string) error {
	revDir := filepath.Join(dir, "capsules", wi, "rev")
	casDir := filepath.Join(revDir, "_cas")
	if err := os.MkdirAll(casDir, 0o750); err != nil {
		return err
	}
	blockSha := func(content string) (string, error) {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			return "", err
		}
		defer enc.Close()
		compressed := enc.EncodeAll([]byte(content), nil)
		sum := sha256.Sum256([]byte(content))
		sha := hex.EncodeToString(sum[:])
		sub := filepath.Join(casDir, sha[:2])
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(sub, sha), compressed, 0o640); err != nil {
			return "", err
		}
		return sha, nil
	}
	mode := int64(0o640)
	execJSON := "runtime-state/execution.json"
	execSha, err := blockSha(`{"steps": []}`)
	if err != nil {
		return err
	}
	mainSha, err := blockSha("fixture capsule main file\n")
	if err != nil {
		return err
	}
	goneSha, err := blockSha("stale file that revision 1 deletes\n")
	if err != nil {
		return err
	}
	rev0 := map[string]any{
		"work_item_id": wi, "seq": 0, "parent": nil, "generation": 1,
		"kind": "incremental",
		"ops": []map[string]any{
			{"op": "put", "path": "docs/main.md", "sha256": mainSha, "bytes": 26, "mode": mode},
			{"op": "put", "path": "docs/gone.txt", "sha256": goneSha, "bytes": 40, "mode": mode},
			{"op": "put", "path": execJSON, "sha256": execSha, "bytes": 14, "mode": mode},
		},
		"live_files": 3, "data_bytes": 80, "execution_state_included": true,
		"built_at": "2026-09-01T10:00:00.000000001Z",
	}
	newSha, err := blockSha("replacement content v2\n")
	if err != nil {
		return err
	}
	rev1 := map[string]any{
		"work_item_id": wi, "seq": 1, "parent": 0, "generation": 1,
		"kind": "incremental",
		"ops": []map[string]any{
			{"op": "put", "path": "docs/main.md", "sha256": newSha, "bytes": 21, "mode": mode},
			{"op": "delete", "path": "docs/gone.txt"},
			{"op": "put", "path": execJSON, "sha256": execSha, "bytes": 14, "mode": mode},
		},
		"live_files": 2, "data_bytes": 35, "execution_state_included": true,
		"built_at": "2026-09-01T11:00:00.000000001Z",
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "aihub.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	for _, rev := range []map[string]any{rev0, rev1} {
		raw, err := json.Marshal(rev)
		if err != nil {
			return err
		}
		sha := computeManifestSha(raw)
		seq := int64(rev["seq"].(int))
		if err := os.MkdirAll(filepath.Join(revDir, fmt.Sprintf("%d", seq)), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(revDir, fmt.Sprintf("%d", seq), "manifest.json"), raw, 0o640); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO capsule_revisions(work_item_id, seq, parent, generation, manifest_sha, data_bytes, published_at)
			VALUES (?1,?2,?3,1,?4,?5,'2026-09-01T10:30:00Z')`,
			wi, seq, rev["parent"], sha, int64(rev["data_bytes"].(int))); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`INSERT INTO capsule_heads(work_item_id, generation, seq, updated_at)
		VALUES (?1,1,1,'2026-09-01T11:00:00Z')`, wi); err != nil {
		return err
	}
	return nil
}
