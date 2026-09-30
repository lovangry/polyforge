package cli

// migrate_authority.go: the authority-file layer of migrate-memories.
//
// The frontmatter struct below mirrors internal/aihub/memory's private
// frontmatter EXACTLY (same fields, same order, same yaml tags): the
// migration renders authority bytes that the server's parseAuthorityDoc
// accepts, and the placed file passes the INV-7f digest gate. Any drift
// would leave the file skipped by the reconcile ("invalid frontmatter
// never becomes an illegal row"), which the count verification then
// catches — the format is mirrored, never improvised.
//
// The v0 pilot's authority files were produced by the same struct (v1 is
// a semantic port), so --from-dir validation is a field-by-field
// comparison against THIS struct: unknown keys, wrong order notes and
// missing required fields all surface as conversion-report entries, and
// the id/project/owner identity remap is logged per file.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// migFrontmatter is the 06 §4.2 frontmatter field set.
type migFrontmatter struct {
	ID             string   `yaml:"id"`
	Type           string   `yaml:"type"`
	OwnerUserID    string   `yaml:"owner_user_id"`
	Title          *string  `yaml:"title,omitempty"`
	Tags           []string `yaml:"tags,flow,omitempty"`
	Visibility     string   `yaml:"visibility"`
	WorkItemID     *string  `yaml:"work_item_id,omitempty"`
	ProjectID      string   `yaml:"project_id"`
	IdempotencyKey *string  `yaml:"idempotency_key,omitempty"`
	Related        []string `yaml:"related,flow,omitempty"`
	CreatedAt      string   `yaml:"created_at"`
	UpdatedAt      string   `yaml:"updated_at"`
}

// migFrontmatterKeys is the v1 key set in serialization order.
var migFrontmatterKeys = []string{
	"id", "type", "owner_user_id", "title", "tags", "visibility",
	"work_item_id", "project_id", "idempotency_key", "related",
	"created_at", "updated_at",
}

var migFrontmatterKeySet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range migFrontmatterKeys {
		m[k] = true
	}
	return m
}()

// migVisibilities mirrors internal/aihub/memory's tier set.
var migVisibilities = map[string]bool{
	"private": true, "project": true, "team": true, "public": true, "admin": true,
}

// migRenderAuthority renders the full authority document with the same
// framing the server's buildAuthorityFile produces.
func migRenderAuthority(fm *migFrontmatter, body string) ([]byte, error) {
	y, err := yaml.Marshal(fm)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(y)
	if !strings.HasSuffix(sb.String(), "\n") {
		sb.WriteByte('\n')
	}
	sb.WriteString("---\n")
	sb.WriteString(body)
	return []byte(sb.String()), nil
}

// migSplitFrontmatter mirrors internal/aihub/memory's framing rules.
func migSplitFrontmatter(content string) (block, body string, ok bool) {
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

// migRelPath is the data-dir-relative placement of one migrated memory:
// active stock in the project tree, redacted stock under _redacted/.
func migRelPath(projectID, memID string, redacted bool) string {
	if redacted {
		return "memory/" + projectID + "/_redacted/" + memID
	}
	return "memory/" + projectID + "/" + memID
}

// migPlaceAuthority writes one memory.md directly under the data dir
// (CLI直写 — no memory.md.tmp, no write-order transients; the migration
// runs in the deployment's silent window, and the server's reconcile is
// the only thing that turns files into rows). Durable write (fsync file
// + parent dir).
func migPlaceAuthority(dataDir, relPath string, content []byte) error {
	dir := filepath.Join(dataDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	full := filepath.Join(dir, "memory.md")
	f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return migSyncDir(dir)
}

func migSyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// migReadPlacedFM re-reads one placed authority file's frontmatter — the
// verification judges the FILES, not the tool's in-memory intent.
func migReadPlacedFM(dataDir, relPath string) (*migFrontmatter, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(relPath), "memory.md"))
	if err != nil {
		return nil, err
	}
	block, _, ok := migSplitFrontmatter(string(raw))
	if !ok {
		return nil, fmt.Errorf("%s: no frontmatter block", relPath)
	}
	var fm migFrontmatter
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return nil, fmt.Errorf("%s: %v", relPath, err)
	}
	return &fm, nil
}

// migRemoveStalePlacement deletes the OPPOSITE-tree placement of one
// mem_id before (re)placing the current one, so a re-import whose status
// flipped never leaves a stale directory behind (a redacted row never
// resurrects from an active-tree file).
func migRemoveStalePlacement(dataDir, projectID, memID string, redacted bool) error {
	other := migRelPath(projectID, memID, !redacted)
	if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(other))); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(filepath.Join(dataDir, filepath.FromSlash(other)))
}

// migScanPlacedIDs walks the target memory tree and returns every mem_id
// directory name already placed (active and _redacted alike — a redacted
// entry's mem_id is still a resolvable reference target).
func migScanPlacedIDs(dataDir string) (map[string]bool, error) {
	out := map[string]bool{}
	root := filepath.Join(dataDir, "memory")
	projDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
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
			if md.IsDir() && md.Name() != "_redacted" {
				out[md.Name()] = true
			}
		}
		redDirs, rerr := os.ReadDir(filepath.Join(root, pd.Name(), "_redacted"))
		if rerr != nil {
			continue
		}
		for _, md := range redDirs {
			if md.IsDir() {
				out[md.Name()] = true
			}
		}
	}
	return out, nil
}

// migFormatDiff is one frontmatter difference between a v0 source file
// and the v1 authority format: an unknown key, or a required field the
// source lacked (and the conversion filled/blanked).
type migFormatDiff struct {
	MemID string `json:"mem_id"`
	Field string `json:"field"`
	Note  string `json:"note"`
}

// migOrderMatches reports whether the present known keys appear in the
// v1 serialization order (a subsequence check: omitted optional keys are
// fine, present keys must not swap).
func migOrderMatches(seen []string) bool {
	idx := 0
	for _, k := range migFrontmatterKeys {
		if idx < len(seen) && seen[idx] == k {
			idx++
		}
	}
	return idx == len(seen)
}

// migSourceMem is one parsed source memory, uniform over both input
// paths (dir file or JSONL record).
type migSourceMem struct {
	ID          string
	Type        string
	Title       *string
	Tags        []string
	Visibility  string
	Content     string // body markdown (frontmatter stripped in dir mode)
	OldProject  string // old identifier, informational
	OldOwner    string // old identifier (old usr_/mac_ id or email/actor)
	OwnerKind   string // human|machine|"" (inferred)
	WorkItemID  *string
	Related     []string
	CreatedAt   string
	UpdatedAt   string
	Status      string // active|redacted
	FormatDiffs []migFormatDiff
}

// migReadDirMemories parses a v0 authority-file tree. dir may be either
// the memory ROOT (memory/<pj_id>/<mem_id>/memory.md) or a single project
// dir (<mem_id>/memory.md); _redacted subtrees mark redacted stock.
// Every file's frontmatter is validated against the v1 field set;
// structural differences (unknown keys, out-of-order keys, missing
// required fields) are collected per file for the conversion report.
func migReadDirMemories(dir string) ([]migSourceMem, []migFormatDiff, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("--from-dir: %v", err)
	}
	// Layout detection: a project dir has <child>/memory.md directly;
	// a memory root has <child>/<grandchild>/memory.md.
	projectDir := ""
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "_redacted" {
			continue
		}
		if _, serr := os.Stat(filepath.Join(dir, e.Name(), "memory.md")); serr == nil {
			projectDir = dir
			break
		}
	}
	var roots []string
	if projectDir != "" {
		roots = []string{projectDir}
	} else {
		for _, e := range entries {
			if e.IsDir() {
				roots = append(roots, filepath.Join(dir, e.Name()))
			}
		}
	}
	var out []migSourceMem
	var diffs []migFormatDiff
	for _, root := range roots {
		memDirs, rerr := os.ReadDir(root)
		if rerr != nil {
			continue
		}
		for _, md := range memDirs {
			if !md.IsDir() || md.Name() == "_redacted" {
				continue
			}
			m, d, perr := migParseAuthorityFile(filepath.Join(root, md.Name(), "memory.md"), md.Name(), false)
			if perr != nil {
				return nil, diffs, perr
			}
			if m != nil {
				out = append(out, *m)
				diffs = append(diffs, d...)
			}
		}
		redDirs, rerr := os.ReadDir(filepath.Join(root, "_redacted"))
		if rerr == nil {
			for _, md := range redDirs {
				if !md.IsDir() {
					continue
				}
				m, d, perr := migParseAuthorityFile(filepath.Join(root, "_redacted", md.Name(), "memory.md"), md.Name(), true)
				if perr != nil {
					return nil, diffs, perr
				}
				if m != nil {
					out = append(out, *m)
					diffs = append(diffs, d...)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("--from-dir %s: no authority files found (expected memory/<pj>/<mem_id>/memory.md layout)", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, diffs, nil
}

// migParseAuthorityFile parses one v0 authority file into the uniform
// source shape. A missing file is skipped (nil mem): the dir walk meets
// placement gaps; a present-but-invalid file is an error (input
// corruption is never silently dropped).
func migParseAuthorityFile(path, memID string, redacted bool) (*migSourceMem, []migFormatDiff, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	block, body, ok := migSplitFrontmatter(string(raw))
	if !ok {
		return nil, nil, fmt.Errorf("%s: no frontmatter block", path)
	}
	// Field-by-field comparison against the v1 format: raw map (unknown
	// keys), yaml node (serialization order) and the typed decode
	// (domains and required fields).
	var rawMap map[string]any
	if err := yaml.Unmarshal([]byte(block), &rawMap); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(block), &node); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	var diffs []migFormatDiff
	addDiff := func(field, note string) {
		diffs = append(diffs, migFormatDiff{MemID: memID, Field: field, Note: note})
	}
	// Serialization order: the sequence of KNOWN keys must match the v1
	// struct order (unknown keys are reported separately below).
	if len(node.Content) > 0 {
		var seen []string
		for _, kv := range node.Content[0].Content {
			if kv.Tag != "!!str" {
				continue
			}
			if migFrontmatterKeySet[kv.Value] {
				seen = append(seen, kv.Value)
			}
		}
		if !migOrderMatches(seen) {
			addDiff("(order)", fmt.Sprintf("key order %v deviates from the v1 serialization order; re-rendered canonically", seen))
		}
	}
	var keys []string
	for k := range rawMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !migFrontmatterKeySet[k] {
			addDiff(k, "unknown in the v1 frontmatter set; dropped on conversion")
		}
	}
	var fm migFrontmatter
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	if fm.ID == "" {
		return nil, diffs, fmt.Errorf("%s: frontmatter id missing", path)
	}
	if fm.ID != memID {
		return nil, diffs, fmt.Errorf("%s: frontmatter id %q != dir %q", path, fm.ID, memID)
	}
	if fm.Type == "" {
		return nil, diffs, fmt.Errorf("%s: frontmatter type missing", path)
	}
	if !migVisibilities[fm.Visibility] {
		return nil, diffs, fmt.Errorf("%s: bad visibility %q", path, fm.Visibility)
	}
	if fm.OwnerUserID == "" {
		return nil, diffs, fmt.Errorf("%s: frontmatter owner_user_id missing", path)
	}
	if fm.ProjectID == "" {
		return nil, diffs, fmt.Errorf("%s: frontmatter project_id missing", path)
	}
	if _, terr := migParseRFC3339(fm.CreatedAt); terr != nil {
		return nil, diffs, fmt.Errorf("%s: created_at: %v", path, terr)
	}
	if _, terr := migParseRFC3339(fm.UpdatedAt); terr != nil {
		return nil, diffs, fmt.Errorf("%s: updated_at: %v", path, terr)
	}
	m := &migSourceMem{
		ID: fm.ID, Type: fm.Type, Title: fm.Title, Tags: fm.Tags,
		Visibility: fm.Visibility, Content: body,
		OldProject: fm.ProjectID, OldOwner: fm.OwnerUserID, OwnerKind: "",
		WorkItemID: fm.WorkItemID, Related: fm.Related,
		CreatedAt: fm.CreatedAt, UpdatedAt: fm.UpdatedAt,
	}
	if redacted {
		m.Status = "redacted"
	} else {
		m.Status = "active"
	}
	// Structural notes the conversion report carries per file.
	if fm.IdempotencyKey != nil && *fm.IdempotencyKey != "" {
		addDiff("idempotency_key", "old-world key kept verbatim (no collision semantics on import)")
	}
	return m, diffs, nil
}
