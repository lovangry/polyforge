package migrate

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options is the resolved `pf migrate-memories` invocation.
type Options struct {
	DataDir        string // aihub data dir (authority tree root)
	AihubURL       string // live server base URL
	APIKey         string // operator admin api key
	InputPath      string // exported JSONL (see package doc)
	Since          string // optional RFC3339 delta filter on updated_at
	ProjectMapPath string // optional JSON {"old name": "new pj_ id"}
	UserMapPath    string // optional JSON {"old identifier": "new usr_/mac_ id"}
}

// ProjectMapping is one row of the old→new project mapping table (B03:
// the mapping table is produced and archived). NewName is the TARGET
// project's name — the search face keys on names, and a mapped project
// may carry a different name than the old world did.
type ProjectMapping struct {
	Old     string `json:"old_project_name"`
	New     string `json:"new_project_id"`
	NewName string `json:"new_project_name"`
	Created bool   `json:"placeholder_created"`
}

// UserMapping is one row of the old→new user mapping table. DisplayName
// is the target user's display_name (for placeholders: "<old>
// (imported)", the deterministic name re-runs re-resolve by).
type UserMapping struct {
	Old         string `json:"old_owner_identifier"`
	New         string `json:"new_user_id"`
	DisplayName string `json:"new_display_name"`
	Created     bool   `json:"placeholder_created"`
}

// BlankedWorkItem is one entry of the blanked work_item_id list (B03:
// old WI ids never migrate; the report records mem_id + old value for a
// human to re-attach later).
type BlankedWorkItem struct {
	MemID    string `json:"mem_id"`
	OldValue string `json:"old_work_item_id"`
}

// BlankedRelated is one dropped related id (resolves in neither the batch
// nor the already-placed file tree).
type BlankedRelated struct {
	MemID     string `json:"mem_id"`
	DroppedID string `json:"dropped_related_id"`
}

// StepResult is the outcome of one of the four §10.1 verification steps.
type StepResult struct {
	Step   string // "count-reconciliation" | "content-sha-sampling" | "retrieval-smoke" | "blanked-list-reconciliation"
	OK     bool
	Detail string
}

// Report is the migration report the verb prints and the mapping tables it
// archives under <data-dir>/migration/.
type Report struct {
	InputLines       int
	FilteredBySince  int
	Migrated         int
	ActiveCount      int
	RedactedCount    int
	Rebuilt          int
	ProjectMap       []ProjectMapping
	UserMap          []UserMapping
	BlankedWorkItems []BlankedWorkItem
	BlankedRelated   []BlankedRelated
	Verification     []StepResult
	SampledIDs       []string
}

// placedMem carries one written entry's authority placement plus the
// values the verification steps need (the expected frontmatter, kept
// independent from the file on disk so step ④ judges the file, not the
// writer's intent).
type placedMem struct {
	rec         Record
	relPath     string // data-dir-relative, no trailing memory.md
	expectedFM  authorityFM
	projectName string // TARGET project name (the search face keys on names)
}

// ResolveError is a fatal resolution/config failure (map naming a
// nonexistent id, unreachable server) — distinct from verification
// failures, which only surface after the import itself landed.
type ResolveError struct{ msg string }

func (e *ResolveError) Error() string { return e.msg }

func resolveErrf(format string, args ...any) error {
	return &ResolveError{msg: fmt.Sprintf(format, args...)}
}

// Run executes the full §10.1 import: parse → resolve identities → place
// files → rebuild → verify. On success it returns the report; a
// verification failure returns the report AND a non-nil error (the CLI
// prints the report, then exits non-zero).
func Run(opts Options) (*Report, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	projMapFile, userMapFile, err := loadMaps(opts)
	if err != nil {
		return nil, err
	}
	records, dropped, total, err := readRecords(opts.InputPath, opts.Since)
	if err != nil {
		return nil, err
	}

	rep := &Report{InputLines: total, FilteredBySince: dropped, Migrated: len(records)}

	// The related-resolution set: ids in this batch plus every mem_id
	// already placed under the data dir (a delta re-run must keep
	// references into previously migrated stock resolvable).
	batchIDs := map[string]bool{}
	for _, r := range records {
		batchIDs[r.ID] = true
	}
	placedIDs, err := scanPlacedIDs(opts.DataDir)
	if err != nil {
		return nil, err
	}

	api := newAihub(opts.AihubURL, opts.APIKey)
	res := newResolver(api, projMapFile, userMapFile)
	if err := res.preload(); err != nil {
		return nil, err
	}

	// Import step ① + ②: resolve identities per record (cached), blank the
	// never-migrating associations, place each authority file.
	var placed []placedMem
	projSeen, userSeen := map[string]bool{}, map[string]bool{}
	for i := range records {
		r := &records[i]
		pm, err := res.project(r.Project)
		if err != nil {
			return nil, err
		}
		if !projSeen[r.Project] {
			projSeen[r.Project] = true
			rep.ProjectMap = append(rep.ProjectMap, pm)
		}
		um, err := res.user(r.Owner, r.humanOwner())
		if err != nil {
			return nil, err
		}
		if !userSeen[r.Owner] {
			userSeen[r.Owner] = true
			rep.UserMap = append(rep.UserMap, um)
		}

		// B03 blanking: work_item_id is ALWAYS blanked (old WI execution
		// state never migrates); related ids that resolve in neither the
		// batch nor the placed tree are blanked. Both are logged.
		if r.WorkItemID != nil && *r.WorkItemID != "" {
			rep.BlankedWorkItems = append(rep.BlankedWorkItems, BlankedWorkItem{MemID: r.ID, OldValue: *r.WorkItemID})
		}
		related := []string{}
		for _, rel := range r.Related {
			if batchIDs[rel] || placedIDs[rel] {
				related = append(related, rel)
			} else {
				rep.BlankedRelated = append(rep.BlankedRelated, BlankedRelated{MemID: r.ID, DroppedID: rel})
			}
		}

		fm := authorityFM{
			ID:          r.ID,
			Type:        r.Type,
			OwnerUserID: um.New,
			Title:       r.Title,
			Tags:        nilIfEmpty(r.Tags),
			Visibility:  r.Visibility,
			ProjectID:   pm.New,
			Related:     nilIfEmpty(related),
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		}
		if r.Status == "redacted" {
			rep.RedactedCount++
		} else {
			rep.ActiveCount++
		}
		rel := authorityRelPath(pm.New, r.ID, r.Status == "redacted")
		// The input's status is the placement authority: a re-import whose
		// status flipped must not leave the OTHER tree's stale directory
		// behind (the server's redact is a move — the active path ends up
		// empty; and a redacted row never resurrects from an active-tree
		// file, 06 §5.3, so the stale placement would otherwise poison the
		// rebuild AND the verification).
		if err := removeStalePlacement(opts.DataDir, pm.New, r.ID, r.Status == "redacted"); err != nil {
			return nil, err
		}
		content, err := renderAuthorityFile(&fm, r.Content)
		if err != nil {
			return nil, err
		}
		if err := placeAuthorityFile(opts.DataDir, rel, content); err != nil {
			return nil, fmt.Errorf("place %s: %w", rel, err)
		}
		placed = append(placed, placedMem{rec: *r, relPath: rel, expectedFM: fm, projectName: pm.NewName})
	}

	// Import step ③: the live server rebuilds Core + FTS from the files.
	rep.Rebuilt, err = api.rebuildIndex()
	if err != nil {
		return nil, err
	}

	// Import step ④: the four-step verification (06 §10.1). Failures are
	// collected in the report; any failure makes the run exit non-zero.
	rep.SampledIDs = sampleIDs(placed, opts.Since != "")
	if err := verify(opts, rep, placed); err != nil {
		return rep, err
	}

	// Archive the mapping tables (B03: 产出映射表并留档) under
	// <data-dir>/migration/ — deterministic, overwritten per run.
	if err := archiveMappings(opts.DataDir, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func (o *Options) validate() error {
	if strings.TrimSpace(o.DataDir) == "" {
		return fmt.Errorf("--data-dir is required")
	}
	if strings.TrimSpace(o.AihubURL) == "" {
		return fmt.Errorf("--aihub-url is required (or set server in the config)")
	}
	if strings.TrimSpace(o.APIKey) == "" {
		return fmt.Errorf("an admin api key is required (--api-key, PF_API_KEY or the config file)")
	}
	if strings.TrimSpace(o.InputPath) == "" {
		return fmt.Errorf("input JSONL path is required")
	}
	if o.Since != "" {
		if _, err := parseRFC3339(o.Since); err != nil {
			return fmt.Errorf("--since: %w", err)
		}
	}
	return nil
}

// loadMaps reads the optional --project-map / --user-map files: JSON
// objects mapping old identifiers to new ids. Map entries naming a new id
// that does not exist are a fatal CONFIG error, never silently demoted
// to a placeholder (the operator said "this IS the row"; honoring a
// dangling pointer would place files bound to nothing).
func loadMaps(opts Options) (proj, user map[string]string, err error) {
	proj, err = readMapFile(opts.ProjectMapPath)
	if err != nil {
		return nil, nil, fmt.Errorf("--project-map: %w", err)
	}
	user, err = readMapFile(opts.UserMapPath)
	if err != nil {
		return nil, nil, fmt.Errorf("--user-map: %w", err)
	}
	return proj, user, nil
}

func readMapFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// scanPlacedIDs walks the memory tree and returns every mem_id directory
// name already placed (active tree and _redacted/ alike — a redacted
// entry's mem_id is still a valid, resolvable reference target).
func scanPlacedIDs(dataDir string) (map[string]bool, error) {
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

// resolver caches old→new identity resolution. Import order ① (B03):
// all identities exist before any memory file is placed, and re-runs
// re-find placeholders created by earlier runs, never duplicating them.
type resolver struct {
	api         *aihub
	projMapFile map[string]string
	userMapFile map[string]string

	projects []projectView
	users    []userView
	projByID map[string]projectView
	projDone map[string]ProjectMapping
	userDone map[string]UserMapping
}

func newResolver(api *aihub, projMapFile, userMapFile map[string]string) *resolver {
	return &resolver{
		api: api, projMapFile: projMapFile, userMapFile: userMapFile,
		projByID: map[string]projectView{}, projDone: map[string]ProjectMapping{},
		userDone: map[string]UserMapping{},
	}
}

func (r *resolver) preload() error {
	var err error
	r.projects, err = r.api.listProjects()
	if err != nil {
		return resolveErrf("list projects: %v", err)
	}
	for _, p := range r.projects {
		r.projByID[p.ID] = p
	}
	r.users, err = r.api.listUsers()
	if err != nil {
		return resolveErrf("list users: %v", err)
	}
	return nil
}

// project resolves one old project name. Order: the --project-map entry
// (must name an existing row), then an existing project of the same NAME
// (pre-created stock binds by name), then a placeholder created with
// visible=false (B03).
func (r *resolver) project(oldName string) (ProjectMapping, error) {
	if m, ok := r.projDone[oldName]; ok {
		return m, nil
	}
	if newID, ok := r.projMapFile[oldName]; ok {
		p, ok := r.projByID[newID]
		if !ok {
			return ProjectMapping{}, resolveErrf("project map: %q → %q: no such project in the target system", oldName, newID)
		}
		m := ProjectMapping{Old: oldName, New: p.ID, NewName: p.Name}
		r.projDone[oldName] = m
		return m, nil
	}
	for _, p := range r.projects {
		if p.Name == oldName {
			m := ProjectMapping{Old: oldName, New: p.ID, NewName: p.Name}
			r.projDone[oldName] = m
			return m, nil
		}
	}
	created, err := r.api.createProject(oldName, false)
	if err != nil {
		return ProjectMapping{}, resolveErrf("create placeholder project %q: %v", oldName, err)
	}
	r.projects = append(r.projects, created)
	r.projByID[created.ID] = created
	m := ProjectMapping{Old: oldName, New: created.ID, NewName: created.Name, Created: true}
	r.projDone[oldName] = m
	return m, nil
}

// placeholderName is the deterministic display_name of a placeholder
// machine user (B03: 旧名+"(imported)"); re-runs re-find the row by it.
func placeholderName(oldIdentifier string) string {
	return oldIdentifier + " (imported)"
}

// user resolves one old owner identifier. Humans are keyed by email,
// machines by actor (the --user-map domain); resolution order: the
// --user-map entry (must exist), then an existing placeholder from an
// earlier run (deterministic display_name), then — for machine actors
// only — an existing machine user with the same display_name (new-world
// registration continuity), then a freshly created placeholder machine
// user. Identity fields are NEVER blanked (B03).
func (r *resolver) user(oldID string, human bool) (UserMapping, error) {
	key := oldID + "||" + map[bool]string{true: "human", false: "machine"}[human]
	if m, ok := r.userDone[key]; ok {
		return m, nil
	}
	if newID, ok := r.userMapFile[oldID]; ok {
		if u := r.findUser(func(v userView) bool { return v.ID == newID }); u != nil {
			m := UserMapping{Old: oldID, New: u.ID, DisplayName: u.DisplayName}
			r.userDone[key] = m
			return m, nil
		}
		return UserMapping{}, resolveErrf("user map: %q → %q: no such user in the target system", oldID, newID)
	}
	phName := placeholderName(oldID)
	if u := r.findUser(func(v userView) bool { return v.DisplayName == phName }); u != nil {
		m := UserMapping{Old: oldID, New: u.ID, DisplayName: u.DisplayName}
		r.userDone[key] = m
		return m, nil
	}
	if !human {
		if u := r.findUser(func(v userView) bool { return v.Kind == "machine" && v.DisplayName == oldID }); u != nil {
			m := UserMapping{Old: oldID, New: u.ID, DisplayName: u.DisplayName}
			r.userDone[key] = m
			return m, nil
		}
	}
	created, err := r.api.createUser(phName, "machine", "writer")
	if err != nil {
		return UserMapping{}, resolveErrf("create placeholder user for %q: %v", oldID, err)
	}
	r.users = append(r.users, created)
	m := UserMapping{Old: oldID, New: created.ID, DisplayName: created.DisplayName, Created: true}
	r.userDone[key] = m
	return m, nil
}

func (r *resolver) findUser(match func(userView) bool) *userView {
	for i := range r.users {
		if match(r.users[i]) {
			return &r.users[i]
		}
	}
	return nil
}

// sampleIDs picks the content-sha/retrieval sample at the FIXED ratio of
// §10.1 ② (10%, at least one entry when anything was migrated). The RNG
// is deterministically seeded so a re-run verifies the same sample.
func sampleIDs(placed []placedMem, delta bool) []string {
	if len(placed) == 0 {
		return nil
	}
	n := (len(placed)*sampleRatioN + sampleRatioD - 1) / sampleRatioD // ceil
	if n < 1 {
		n = 1
	}
	idx := make([]int, len(placed))
	for i := range idx {
		idx[i] = i
	}
	rnd := seededRand()
	rnd.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	if n > len(idx) {
		n = len(idx)
	}
	out := make([]string, 0, n)
	for _, i := range idx[:n] {
		out = append(out, placed[i].rec.ID)
	}
	sort.Strings(out)
	return out
}

// archiveMappings writes the mapping tables (B03: 留档) as JSON under
// <data-dir>/migration/: project-map.json and user-map.json.
func archiveMappings(dataDir string, rep *Report) error {
	dir := filepath.Join(dataDir, "migration")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "project-map.json"), rep.ProjectMap); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "user-map.json"), rep.UserMap)
}

func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o640)
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// removeStalePlacement deletes the OPPOSITE-tree placement of one mem_id
// before (re)placing the current one: a redacted import removes the
// active-path directory, an active import removes a _redacted one. A
// no-op on the ordinary idempotent re-run (same status both times).
func removeStalePlacement(dataDir, projectID, memID string, redacted bool) error {
	other := authorityRelPath(projectID, memID, !redacted)
	if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(other))); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(filepath.Join(dataDir, filepath.FromSlash(other)))
}

// Render prints the migration report: stats, the placeholder-identity
// lists, the blanked work_item_id and related lists, and the four-step
// verification outcomes (the §10.1 report a human merges identities and
// re-attaches WI references from).
func (rep *Report) Render(w io.Writer) {
	fmt.Fprintf(w, "migrated %d memory record(s) (active %d, redacted %d", rep.Migrated, rep.ActiveCount, rep.RedactedCount)
	if rep.FilteredBySince > 0 {
		fmt.Fprintf(w, ", %d dropped by --since", rep.FilteredBySince)
	}
	fmt.Fprintf(w, "), rebuilt %d active entr(y/ies)\n", rep.Rebuilt)

	fmt.Fprintf(w, "\nproject mapping (%d):\n", len(rep.ProjectMap))
	for _, m := range rep.ProjectMap {
		mark := ""
		if m.Created {
			mark = "  [placeholder created, visible=false]"
		}
		fmt.Fprintf(w, "  %s -> %s%s\n", m.Old, m.New, mark)
	}
	fmt.Fprintf(w, "\nuser mapping (%d):\n", len(rep.UserMap))
	for _, m := range rep.UserMap {
		mark := ""
		if m.Created {
			mark = "  [placeholder machine user]"
		}
		fmt.Fprintf(w, "  %s -> %s (%s)%s\n", m.Old, m.New, m.DisplayName, mark)
	}

	fmt.Fprintf(w, "\nblanked work_item_id list (%d):\n", len(rep.BlankedWorkItems))
	for _, b := range rep.BlankedWorkItems {
		fmt.Fprintf(w, "  %s: %s\n", b.MemID, b.OldValue)
	}
	fmt.Fprintf(w, "\nblanked related list (%d):\n", len(rep.BlankedRelated))
	for _, b := range rep.BlankedRelated {
		fmt.Fprintf(w, "  %s: %s\n", b.MemID, b.DroppedID)
	}

	fmt.Fprintf(w, "\nverification:\n")
	for _, s := range rep.Verification {
		status := "PASS"
		if !s.OK {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %s: %s\n", status, s.Step, s.Detail)
	}
}
