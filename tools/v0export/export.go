// Package v0export reads a v0 pilot aihub deployment (SQLite + authority
// memory files + the capsule revision chain) READ-ONLY and exports the
// v1 seeding inputs:
//
//	work-items.jsonl        one JSON object per work item, all columns
//	memory/memories.jsonl   one JSON object per memory, dual-source
//	                        (table row + authority file) reconciled
//	capsule-snapshots/<wi>/ the latest published capsule head of every
//	                        work item, replayed from manifests + CAS
//	                        blocks into a plain file tree
//	export-report.json      counts and the reconciliation findings
//
// The source is never written: the database opens mode=ro and the
// authority/capsule trees are only read. The emitted JSONL shapes are the
// frozen input contracts of `pf migrate-work-items` and
// `pf migrate-memories` (internal/polyforge/cli).
package v0export

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Options is the resolved v0-export invocation.
type Options struct {
	DBPath   string // v0 aihub.db (opened read-only)
	DataDir  string // v0 aihub data dir (authority memory/ tree, capsules/)
	Out      string // output directory (created)
	SkipCapsules bool
}

// Report is the export report (also written as export-report.json).
type Report struct {
	WorkItems        int      `json:"work_items"`
	Memories         int      `json:"memories"`
	OrphanFiles      []string `json:"orphan_files"`       // authority files with no table row (exported from the file)
	ShaMismatches    []string `json:"sha_mismatches"`      // row content_sha != live file digest (the FILE body exported)
	RedactedMemories int      `json:"redacted_memories"`
	CapsuleSnapshots []string `json:"capsule_snapshots"`  // "<wi>@seq<n> (x files)"
	ManifestShaMismatch []string `json:"manifest_sha_mismatch"`
}

// Run executes the export.
func Run(opts Options) (*Report, error) {
	if strings.TrimSpace(opts.DBPath) == "" {
		return nil, fmt.Errorf("--db (the v0 aihub.db) is required")
	}
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("--data-dir (the v0 aihub data dir) is required")
	}
	if strings.TrimSpace(opts.Out) == "" {
		return nil, fmt.Errorf("--out (the output dir) is required")
	}
	db, err := openReadOnly(opts.DBPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := os.MkdirAll(opts.Out, 0o750); err != nil {
		return nil, err
	}
	rep := &Report{}
	if err := exportWorkItems(db, opts, rep); err != nil {
		return nil, err
	}
	if err := exportMemories(db, opts, rep); err != nil {
		return nil, err
	}
	if !opts.SkipCapsules {
		if err := exportCapsuleSnapshots(db, opts, rep); err != nil {
			return nil, err
		}
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.Out, "export-report.json"), append(raw, '\n'), 0o640); err != nil {
		return nil, err
	}
	return rep, nil
}

// openReadOnly opens the source database mode=ro. NEVER a write path.
func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("v0 database not found at %s: %v", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open v0 database read-only: %v (a hot WAL a live writer owns can refuse read-only opens; stop the v0 service first)", err)
	}
	return db, nil
}

// ---------------------------------------------------------------------------
// Work items

// WIRecord is one emitted work-items.jsonl line. The key set is the
// pf migrate-work-items input contract; attrs/labels/budget ride as raw
// JSON so the old world's bytes pass through undisturbed.
type WIRecord struct {
	ID             string          `json:"id"`
	Seq            *int64          `json:"seq"`
	Project        string          `json:"project"`
	Goal           string          `json:"goal"`
	Content        *string         `json:"content"`
	Status         string          `json:"status"`
	Priority       string          `json:"priority"`
	Milestone      *string         `json:"milestone"`
	Labels         json.RawMessage `json:"labels"`
	Attrs          json.RawMessage `json:"attrs"`
	Reporter       string          `json:"reporter"`
	Assignee       *string         `json:"assignee"`
	WaitingOn      *string         `json:"waiting_on"`
	BundleName     *string         `json:"bundle_name"`
	BundleDigest   *string         `json:"bundle_digest"`
	BudgetBaseline json.RawMessage `json:"workflow_budget_baseline"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	ClosedAt       *string         `json:"closed_at"`
}

func exportWorkItems(db *sql.DB, opts Options, rep *Report) error {
	rows, err := db.Query(`SELECT w.id, w.seq, p.name, w.goal, w.content, w.status, w.priority,
		w.milestone, w.labels, w.attrs, w.reporter, w.assignee, w.waiting_on,
		w.bundle_name, w.bundle_digest, w.workflow_budget_baseline,
		w.created_at, w.updated_at, w.closed_at
		FROM work_items w JOIN projects p ON p.id=w.project_id ORDER BY w.seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r WIRecord
		var seq int64
		var content, milestone, assignee, waitingOn, bundle, bundleDigest, budget, closed sql.NullString
		var labels, attrs string
		if err := rows.Scan(&r.ID, &seq, &r.Project, &r.Goal, &content, &r.Status, &r.Priority,
			&milestone, &labels, &attrs, &r.Reporter, &assignee, &waitingOn,
			&bundle, &bundleDigest, &budget, &r.CreatedAt, &r.UpdatedAt, &closed); err != nil {
			return err
		}
		r.Seq = &seq
		r.Content = nullStrPtr(content)
		r.Milestone = nullStrPtr(milestone)
		r.Assignee = nullStrPtr(assignee)
		r.WaitingOn = nullStrPtr(waitingOn)
		r.BundleName = nullStrPtr(bundle)
		r.BundleDigest = nullStrPtr(bundleDigest)
		r.BudgetBaseline = rawOrEmptyJSON(budget)
		r.ClosedAt = nullStrPtr(closed)
		r.Labels = json.RawMessage(labels)
		r.Attrs = json.RawMessage(attrs)
		if len(r.Labels) == 0 {
			r.Labels = json.RawMessage("[]")
		}
		if len(r.Attrs) == 0 {
			r.Attrs = json.RawMessage("{}")
		}
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		out = append(out, string(line))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(opts.Out, "work-items.jsonl"), out); err != nil {
		return err
	}
	rep.WorkItems = len(out)
	return nil
}

// ---------------------------------------------------------------------------
// Memories: table + authority file dual-source reconciliation

// MemRecord is one emitted memories.jsonl line — the v0/v1
// pf migrate-memories input contract.
type MemRecord struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Title      *string  `json:"title"`
	Tags       []string `json:"tags"`
	Visibility string   `json:"visibility"`
	Content    string   `json:"content"`
	Project    string   `json:"project"`
	Owner      string   `json:"owner"`
	OwnerKind  string   `json:"owner_kind"`
	WorkItemID *string  `json:"work_item_id"`
	Related    []string `json:"related"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	Status     string   `json:"status"`
}

// usersHaveEmail reports whether the v0 users table carries an email
// column (the pilot's does not; a fuller v0 install may).
func usersHaveEmail(db *sql.DB) bool {
	rows, err := db.Query(`PRAGMA table_info(users)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil && name == "email" {
			return true
		}
	}
	return false
}

func exportMemories(db *sql.DB, opts Options, rep *Report) error {
	withEmail := usersHaveEmail(db)
	ownerExpr := "u.display_name"
	if withEmail {
		ownerExpr = "COALESCE(u.email, u.display_name)"
	}
	q := `SELECT m.id, m.type, m.title, m.tags, m.visibility, p.name,
		m.owner_user_id, ` + ownerExpr + `, u.kind, m.work_item_id, m.related,
		m.status, m.created_at, m.updated_at, m.content_sha, m.authority_path
		FROM memories m
		JOIN projects p ON p.id = m.project_id
		JOIN users u ON u.id = m.owner_user_id
		ORDER BY m.id`
	rows, err := db.Query(q)
	if err != nil {
		return err
	}
	type rowSrc struct {
		rec     MemRecord
		sha     string
		authRel string
	}
	var srcs []rowSrc
	defer rows.Close()
	for rows.Next() {
		var s rowSrc
		var title, wi sql.NullString
		var tags, related string
		var ownerName, kind string
		if err := rows.Scan(&s.rec.ID, &s.rec.Type, &title, &tags, &s.rec.Visibility, &s.rec.Project,
			&s.rec.Owner, &ownerName, &kind, &wi, &related,
			&s.rec.Status, &s.rec.CreatedAt, &s.rec.UpdatedAt, &s.sha, &s.authRel); err != nil {
			return err
		}
		_ = s.rec.Owner // owner_user_id kept in .Owner; identifier below
		s.rec.Owner = ownerName
		s.rec.OwnerKind = kind
		s.rec.Title = nullStrPtr(title)
		if wi.Valid {
			v := wi.String
			s.rec.WorkItemID = &v
		}
		_ = json.Unmarshal([]byte(tags), &s.rec.Tags)
		if s.rec.Tags == nil {
			s.rec.Tags = []string{}
		}
		_ = json.Unmarshal([]byte(related), &s.rec.Related)
		if s.rec.Related == nil {
			s.rec.Related = []string{}
		}
		srcs = append(srcs, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	memDir := filepath.Join(opts.Out, "memory")
	if err := os.MkdirAll(memDir, 0o750); err != nil {
		return err
	}
	byID := map[string]bool{}
	var out []string
	for _, s := range srcs {
		// The authority file is the content source (INV-7c: the file wins);
		// the row carries status and identity.
		fileRel := s.authRel
		if fileRel == "" {
			return fmt.Errorf("memory %s: empty authority_path in the row", s.rec.ID)
		}
		raw, err := os.ReadFile(filepath.Join(opts.DataDir, filepath.FromSlash(fileRel), "memory.md"))
		if err != nil {
			return fmt.Errorf("memory %s: authority file missing (%s): %v — the dual source is broken, refusing to invent content", s.rec.ID, fileRel, err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != s.sha {
			rep.ShaMismatches = append(rep.ShaMismatches,
				fmt.Sprintf("%s: row content_sha != live file digest (file body exported)", s.rec.ID))
		}
		_, body, ok := splitFrontmatter(string(raw))
		if !ok {
			return fmt.Errorf("memory %s: authority file has no frontmatter block", s.rec.ID)
		}
		s.rec.Content = body
		byID[s.rec.ID] = true
		if s.rec.Status == "redacted" {
			rep.RedactedMemories++
		}
		line, err := json.Marshal(s.rec)
		if err != nil {
			return err
		}
		out = append(out, string(line))
	}

	// Orphan authority files (no table row): exported from the file's own
	// frontmatter, status=active (an active-tree file is active stock; a
	// _redacted one is redacted stock).
	orphans, err := scanAuthorityTree(opts.DataDir)
	if err != nil {
		return err
	}
	for _, o := range orphans {
		if byID[o.memID] {
			continue
		}
		rec, err := recordFromAuthorityFile(opts.DataDir, o)
		if err != nil {
			return fmt.Errorf("orphan authority file %s: %v", o.rel, err)
		}
		rep.OrphanFiles = append(rep.OrphanFiles, o.rel)
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		out = append(out, string(line))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if err := writeJSONL(filepath.Join(memDir, "memories.jsonl"), out); err != nil {
		return err
	}
	rep.Memories = len(out)
	return nil
}

// recordFromAuthorityFile builds a JSONL record from an authority file's
// own frontmatter (orphan stock: the row is gone, the file is the only
// source). The project name falls back to the directory's project id —
// the importer keys on --project anyway.
func recordFromAuthorityFile(dataDir string, o memFileRef) (MemRecord, error) {
	var rec MemRecord
	raw, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(o.rel), "memory.md"))
	if err != nil {
		return rec, err
	}
	block, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return rec, errors.New("no frontmatter block")
	}
	var fm struct {
		ID          string   `yaml:"id"`
		Type        string   `yaml:"type"`
		OwnerUserID string   `yaml:"owner_user_id"`
		Title       *string  `yaml:"title"`
		Tags        []string `yaml:"tags"`
		Visibility  string   `yaml:"visibility"`
		WorkItemID  *string  `yaml:"work_item_id"`
		ProjectID   string   `yaml:"project_id"`
		Related     []string `yaml:"related"`
		CreatedAt   string   `yaml:"created_at"`
		UpdatedAt   string   `yaml:"updated_at"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return rec, err
	}
	if fm.ID == "" || fm.ID != o.memID {
		return rec, fmt.Errorf("frontmatter id %q != dir %q", fm.ID, o.memID)
	}
	rec = MemRecord{
		ID: fm.ID, Type: fm.Type, Title: fm.Title, Tags: orEmptyTags(fm.Tags),
		Visibility: fm.Visibility, Content: body, Project: o.projectDir,
		Owner: fm.OwnerUserID, OwnerKind: "", WorkItemID: fm.WorkItemID,
		Related: orEmptyTags(fm.Related), CreatedAt: fm.CreatedAt, UpdatedAt: fm.UpdatedAt,
		Status: o.status,
	}
	return rec, nil
}

// memFileRef locates one authority file in the source tree.
type memFileRef struct {
	memID      string
	projectDir string
	rel        string // data-dir-relative directory of memory.md
	status     string // active | redacted
}

// scanAuthorityTree walks the v0 memory tree (memory/<pj>/<mem>/memory.md
// and memory/<pj>/_redacted/<mem>/memory.md).
func scanAuthorityTree(dataDir string) ([]memFileRef, error) {
	root := filepath.Join(dataDir, "memory")
	var out []memFileRef
	projDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, pd := range projDirs {
		if !pd.IsDir() {
			continue
		}
		memDirs, merr := os.ReadDir(filepath.Join(root, pd.Name()))
		if merr != nil {
			continue
		}
		for _, md := range memDirs {
			if !md.IsDir() || md.Name() == "_redacted" {
				continue
			}
			if _, serr := os.Stat(filepath.Join(root, pd.Name(), md.Name(), "memory.md")); serr == nil {
				out = append(out, memFileRef{memID: md.Name(), projectDir: pd.Name(),
					rel: "memory/" + pd.Name() + "/" + md.Name(), status: "active"})
			}
		}
		redDirs, rerr := os.ReadDir(filepath.Join(root, pd.Name(), "_redacted"))
		if rerr != nil {
			continue
		}
		for _, md := range redDirs {
			if !md.IsDir() {
				continue
			}
			if _, serr := os.Stat(filepath.Join(root, pd.Name(), "_redacted", md.Name(), "memory.md")); serr == nil {
				out = append(out, memFileRef{memID: md.Name(), projectDir: pd.Name(),
					rel: "memory/" + pd.Name() + "/_redacted/" + md.Name(), status: "redacted"})
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Capsule snapshots: replay manifests + CAS blocks into a file tree

// capsuleOp is one manifest op (the v0 revision wire shape).
type capsuleOp struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Mode   *int64 `json:"mode,omitempty"`
	Target string `json:"target,omitempty"`
}

// capsuleManifest is the rev/<seq>/manifest.json object.
type capsuleManifest struct {
	WorkItemID string      `json:"work_item_id"`
	Seq        int64       `json:"seq"`
	Parent     *int64      `json:"parent"`
	Generation int64       `json:"generation"`
	Kind       string      `json:"kind"`
	Ops        []capsuleOp `json:"ops"`
	BuiltAt    string      `json:"built_at"`
}

// headRow is one capsule_heads row.
type headRow struct {
	WorkItemID string
	Generation int64
	Seq        int64
	UpdatedAt  string
}

// exportCapsuleSnapshots materializes the latest published head of every
// work item that has one: the manifest chain 0..head replays onto a
// plain file tree (put/delete/symlink/chmod; blocks are zstd-compressed
// CAS content, the digest addressing the ORIGINAL bytes).
func exportCapsuleSnapshots(db *sql.DB, opts Options, rep *Report) error {
	rows, err := db.Query(`SELECT work_item_id, generation, seq, updated_at FROM capsule_heads`)
	if err != nil {
		return err
	}
	var heads []headRow
	for rows.Next() {
		var h headRow
		if err := rows.Scan(&h.WorkItemID, &h.Generation, &h.Seq, &h.UpdatedAt); err != nil {
			rows.Close()
			return err
		}
		heads = append(heads, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].WorkItemID < heads[j].WorkItemID })
	outRoot := filepath.Join(opts.Out, "capsule-snapshots")
	for _, h := range heads {
		nfiles, err := exportOneCapsule(db, opts, outRoot, h, rep)
		if err != nil {
			return fmt.Errorf("capsule %s: %v", h.WorkItemID, err)
		}
		rep.CapsuleSnapshots = append(rep.CapsuleSnapshots,
			fmt.Sprintf("%s@seq%d (%d files)", h.WorkItemID, h.Seq, nfiles))
	}
	return nil
}

func exportOneCapsule(db *sql.DB, opts Options, outRoot string, h headRow, rep *Report) (int, error) {
	dst := filepath.Join(outRoot, h.WorkItemID)
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return 0, err
	}
	casDir := filepath.Join(opts.DataDir, "capsules", h.WorkItemID, "rev", "_cas")
	nfiles := 0
	for seq := int64(0); seq <= h.Seq; seq++ {
		manifestPath := filepath.Join(opts.DataDir, "capsules", h.WorkItemID, "rev",
			fmt.Sprintf("%d", seq), "manifest.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return nfiles, fmt.Errorf("manifest %d unreadable: %v", seq, err)
		}
		var m capsuleManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nfiles, fmt.Errorf("manifest %d unparsable: %v", seq, err)
		}
		if m.WorkItemID != h.WorkItemID || m.Seq != seq {
			return nfiles, fmt.Errorf("manifest %d identity %s/%d", seq, m.WorkItemID, m.Seq)
		}
		// The DB row pins the manifest digest: verify the audit copy.
		var want string
		if qerr := db.QueryRow(`SELECT manifest_sha FROM capsule_revisions WHERE work_item_id=?1 AND seq=?2`,
			h.WorkItemID, seq).Scan(&want); qerr == nil {
			got := computeManifestSha(raw)
			if got != want {
				rep.ManifestShaMismatch = append(rep.ManifestShaMismatch,
					fmt.Sprintf("%s seq %d: db %s != manifest %s", h.WorkItemID, seq, want, got))
			}
		}
		for _, op := range m.Ops {
			if err := applyCapsuleOp(dst, casDir, op); err != nil {
				return nfiles, err
			}
		}
		if seq == h.Seq {
			// The head manifest rides beside the tree for provenance.
			if err := os.WriteFile(filepath.Join(outRoot, h.WorkItemID+".head-manifest.json"), raw, 0o640); err != nil {
				return nfiles, err
			}
		}
	}
	// Count the materialized live set.
	err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			nfiles++
		}
		return err
	})
	if err != nil {
		return nfiles, err
	}
	return nfiles, nil
}

// applyCapsuleOp materializes one manifest op. put: zstd-decode the CAS
// block, verify the digest addresses the ORIGINAL bytes, write + chmod.
func applyCapsuleOp(dst, casDir string, op capsuleOp) error {
	switch op.Op {
	case "put":
		block, err := os.ReadFile(filepath.Join(casDir, op.Sha256[:2], op.Sha256))
		if err != nil {
			return fmt.Errorf("block %s: %v", op.Sha256, err)
		}
		content, err := zstdDecode(block)
		if err != nil {
			return fmt.Errorf("block %s: %v", op.Sha256, err)
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != op.Sha256 {
			return fmt.Errorf("block %s: content does not match its sha256", op.Sha256)
		}
		full := filepath.Join(dst, filepath.FromSlash(op.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o640); err != nil {
			return err
		}
		if op.Mode != nil {
			_ = os.Chmod(full, os.FileMode(*op.Mode&0o7777))
		}
	case "delete":
		if err := os.RemoveAll(filepath.Join(dst, filepath.FromSlash(op.Path))); err != nil {
			return err
		}
	case "symlink":
		full := filepath.Join(dst, filepath.FromSlash(op.Path))
		_ = os.RemoveAll(full)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return err
		}
		if err := os.Symlink(op.Target, full); err != nil {
			return err
		}
	case "chmod":
		if op.Mode != nil {
			_ = os.Chmod(filepath.Join(dst, filepath.FromSlash(op.Path)), os.FileMode(*op.Mode&0o7777))
		}
	default:
		return fmt.Errorf("unknown op %q", op.Op)
	}
	return nil
}

// computeManifestSha recomputes the v0 manifest identity: the manifest
// object minus built_at, 2-space-indent JSON with keys in code-point
// order, sha256 → "sha256:<hex>".
func computeManifestSha(raw []byte) string {
	var tree map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return ""
	}
	delete(tree, "built_at")
	b, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func zstdDecode(block []byte) ([]byte, error) {
	if len(block) < 4 || string(block[:4]) != "\x28\xb5\x2f\xfd" {
		return block, nil // non-zstd payload (older writers): raw
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(block, nil)
}

// ---------------------------------------------------------------------------
// shared helpers

func splitFrontmatter(content string) (block, body string, ok bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content, false
	}
	idx := strings.Index(content[4:], "\n---")
	if idx < 0 {
		return "", content, false
	}
	end := 4 + idx
	rest := content[end+4:]
	if rest != "" && rest[0] != '\n' && rest[0] != '\r' {
		return "", content, false
	}
	return content[4:end], rest, true
}

func nullStrPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func rawOrEmptyJSON(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}

func orEmptyTags(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func writeJSONL(path string, lines []string) error {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o640)
}
