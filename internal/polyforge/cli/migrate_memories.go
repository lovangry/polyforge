package cli

// migrate_memories.go: the migrate-memories engine.
//
// Import order (the v0 tool's ①–④, adapted):
//  ① resolve the target project and every old owner to a target user
//     (placeholders are created up front, never mid-batch);
//  ② place each authority file directly under the TARGET data dir's
//     memory/<project>/<mem_id>/ (CLI直写 — no memory API, no write-order
//     transients; redacted stock under _redacted/);
//  ③ the live server's rebuild-index runs the file-wins reconcile: this
//     is the first real use of the v1 self-healing path — the ONLY way
//     these files become rows + FTS entries;
//  ④ the four-step verification (count reconciliation, content_sha
//     sampling at the fixed 10% ratio, retrieval smoke over the real
//     HTTP search, conversion reconciliation against the placed files).
//
// work_item_id is preserved when the target already carries that work
// item (v1 migrates terminal WI records with their ids); unresolvable
// references are blanked and logged. Related ids that resolve in neither
// the batch nor the already-placed tree are blanked and logged.

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	_ "modernc.org/sqlite"
)

// MigMemoriesOptions is the resolved `pf migrate-memories` invocation.
type MigMemoriesOptions struct {
	DataDir   string // target aihub data dir (the authority memory/ tree root)
	AihubURL  string
	APIKey    string // operator admin api key
	FromDir   string // v0 authority-file tree (mutually exclusive with FromJSONL)
	FromJSONL string // v0/v1 JSONL export
	Project   string // target project name or id (required)
	Owner     string // optional: map every old owner to this target user id
	UserMap   map[string]string
	Since     string // JSONL only: delta filter on updated_at
	DryRun    bool
}

// MigConversion is one logged frontmatter conversion of the run.
type MigConversion struct {
	MemID   string `json:"mem_id"`
	Field   string `json:"field"`
	Old     string `json:"old,omitempty"`
	New     string `json:"new,omitempty"`
	Note    string `json:"note,omitempty"`
}

// MigMemoriesReport is the migration report the verb prints.
type MigMemoriesReport struct {
	Mode           string // "dir" | "jsonl"
	InputLines     int    // JSONL: non-blank input lines; dir: files found
	FilteredBySince int
	Migrated       int
	Unchanged       int // re-placed byte-identical (idempotent re-run evidence)
	ActiveCount     int
	RedactedCount    int
	Rebuilt         int
	Source          string
	TargetProject   string
	OwnerOverride   string
	FormatDiffs     []MigConversion // structural v0→v1 frontmatter findings
	Conversions     []MigConversion // identity/association conversions applied
	BlankedRelated  []MigConversion
	PreservedWIs    int
	OldProjects     []string // distinct old project identifiers seen
	Verification    []StepResult
	SampledIDs      []string
	DryRun          bool
}

// placedMem carries one written entry's placement plus the expected
// post-conversion frontmatter (kept independent from the file on disk so
// step ④ judges the file, not the writer's intent).
type placedMem struct {
	rec        migSourceMem
	relPath    string
	expectedFM migFrontmatter
	probe      string // retrieval-smoke probe ("" = unprobeable)
}

// RunMigrateMemories executes the full import. On success it returns the
// report; a verification failure returns the report AND a non-nil error
// (the CLI prints the report, then exits non-zero).
func RunMigrateMemories(opts MigMemoriesOptions, client *aihubclient.Client) (*MigMemoriesReport, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("--data-dir is required")
	}
	if strings.TrimSpace(opts.AihubURL) == "" {
		return nil, fmt.Errorf("--aihub-url is required (or set server in the config)")
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("an admin api key is required (--api-key, PF_API_KEY or the config file)")
	}
	if (opts.FromDir == "") == (opts.FromJSONL == "") {
		return nil, fmt.Errorf("exactly one of --from-dir / --from-jsonl is required")
	}
	if strings.TrimSpace(opts.Project) == "" {
		return nil, fmt.Errorf("--project (the target project) is required")
	}
	if opts.FromJSONL == "" && opts.Since != "" {
		return nil, fmt.Errorf("--since applies to --from-jsonl only")
	}

	rep := &MigMemoriesReport{DryRun: opts.DryRun, OwnerOverride: opts.Owner}

	// Parse the input into the uniform source shape.
	var sources []migSourceMem
	if opts.FromDir != "" {
		rep.Mode = "dir"
		var diffs []migFormatDiff
		var err error
		sources, diffs, err = migReadDirMemories(opts.FromDir)
		if err != nil {
			return nil, err
		}
		rep.InputLines = len(sources)
		rep.Source = opts.FromDir
		for _, d := range diffs {
			rep.FormatDiffs = append(rep.FormatDiffs, MigConversion{MemID: d.MemID, Field: d.Field, Note: d.Note})
		}
	} else {
		rep.Mode = "jsonl"
		recs, dropped, total, err := readMemRecords(opts.FromJSONL, opts.Since)
		if err != nil {
			return nil, err
		}
		rep.InputLines = total
		rep.FilteredBySince = dropped
		rep.Source = opts.FromJSONL
		for i := range recs {
			r := recs[i]
			sources = append(sources, migSourceMem{
				ID: r.ID, Type: r.Type, Title: r.Title, Tags: r.Tags,
				Visibility: r.Visibility, Content: r.Content,
				OldProject: r.Project, OldOwner: r.Owner, OwnerKind: r.OwnerKind,
				WorkItemID: r.WorkItemID, Related: r.Related,
				CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Status: r.Status,
			})
		}
	}
	rep.Migrated = len(sources)

	// Distinct old projects (informational: everything lands in --project).
	oldProjects := map[string]bool{}
	for i := range sources {
		oldProjects[sources[i].OldProject] = true
	}
	for p := range oldProjects {
		rep.OldProjects = append(rep.OldProjects, p)
	}
	sortStrings(rep.OldProjects)

	// The reference-resolution set: ids in this batch plus every mem_id
	// already placed under the target tree.
	batchIDs := map[string]bool{}
	for i := range sources {
		batchIDs[sources[i].ID] = true
	}
	placedIDs, err := migScanPlacedIDs(opts.DataDir)
	if err != nil {
		return nil, err
	}

	// ① identities.
	proj, err := migResolveProject(client, opts.Project)
	if err != nil {
		return nil, err
	}
	rep.TargetProject = proj.ID
	resolver := newMigResolver(client, opts.UserMap, opts.Owner)
	if err := resolver.preload(); err != nil {
		return nil, err
	}

	// The work-item existence check needs the target rows (read-only —
	// the live server is the only writer).
	var wiDB *sql.DB
	if !opts.DryRun {
		wiDB, err = migOpenCoreReadOnly(opts.DataDir)
		if err != nil {
			return nil, err
		}
		defer wiDB.Close()
	}

	// ② convert + place each entry.
	var placed []placedMem
	if opts.DryRun {
		// dry-run converts and validates without touching the tree
		for i := range sources {
			p, cerr := migConvertOne(&sources[i], resolver, proj.ID, wiDB, batchIDs, placedIDs, rep)
			if cerr != nil {
				return nil, cerr
			}
			placed = append(placed, p)
		}
		rep.ActiveCount, rep.RedactedCount = migStatusCounts(placed)
		return rep, nil
	}
	for i := range sources {
		p, cerr := migConvertOne(&sources[i], resolver, proj.ID, wiDB, batchIDs, placedIDs, rep)
		if cerr != nil {
			return nil, cerr
		}
		if rerr := migRemoveStalePlacement(opts.DataDir, proj.ID, p.rec.ID, p.rec.Status == "redacted"); rerr != nil {
			return nil, rerr
		}
		content, rerr := migRenderAuthority(&p.expectedFM, p.rec.Content)
		if rerr != nil {
			return nil, rerr
		}
		if existing, eerr := os.ReadFile(migAuthorityPath(opts.DataDir, p.relPath)); eerr == nil &&
			string(existing) == string(content) {
			rep.Unchanged++
		}
		if perr := migPlaceAuthority(opts.DataDir, p.relPath, content); perr != nil {
			return nil, fmt.Errorf("place %s: %v", p.relPath, perr)
		}
		placed = append(placed, p)
	}
	rep.ActiveCount, rep.RedactedCount = migStatusCounts(placed)

	// ③ the live server's reconcile backfills rows + FTS from the files.
	rep.Rebuilt, err = client.RebuildMemoryIndex()
	if err != nil {
		return nil, resolveErrf("rebuild-index: %v (start the server first — the file-wins reconcile is the only backfill path)", err)
	}

	// ④ the four-step verification.
	rep.SampledIDs = migSampleIDs(migPlacedIDs(placed))
	if verr := migVerifyMemories(opts, rep, placed, client); verr != nil {
		return rep, verr
	}
	return rep, nil
}

// migConvertOne resolves identities and associations for one source
// memory and returns the placed-entry shape (no file written here).
func migConvertOne(src *migSourceMem, resolver *migResolver, projectID string,
	wiDB *sql.DB, batchIDs, placedIDs map[string]bool, rep *MigMemoriesReport) (placedMem, error) {
	owner, err := resolver.user(src.OldOwner, src.OwnerKind != "machine")
	if err != nil {
		return placedMem{}, err
	}
	if owner != src.OldOwner {
		rep.Conversions = append(rep.Conversions, MigConversion{
			MemID: src.ID, Field: "owner_user_id", Old: src.OldOwner, New: owner,
		})
	}
	// work_item_id: preserved only when the target already carries the WI
	// (work items are imported BEFORE memories — see tools/seed-v1.sh).
	var workItemID *string
	if src.WorkItemID != nil && *src.WorkItemID != "" {
		resolvable := false
		if wiDB != nil {
			var one int
			if qerr := wiDB.QueryRow(`SELECT 1 FROM work_items WHERE id=?1`, *src.WorkItemID).Scan(&one); qerr == nil {
				resolvable = true
			} else if qerr != sql.ErrNoRows {
				return placedMem{}, qerr
			}
		}
		if resolvable {
			workItemID = src.WorkItemID
			rep.PreservedWIs++
		} else {
			rep.Conversions = append(rep.Conversions, MigConversion{
				MemID: src.ID, Field: "work_item_id", Old: *src.WorkItemID,
				Note: "no such work item in the target; blanked (import work items first)",
			})
		}
	}
	// related: keep what resolves in the batch or the placed tree.
	related := []string{}
	for _, rel := range src.Related {
		if batchIDs[rel] || placedIDs[rel] {
			related = append(related, rel)
		} else {
			rep.BlankedRelated = append(rep.BlankedRelated, MigConversion{
				MemID: src.ID, Field: "related", Old: rel,
				Note: "resolves in neither the batch nor the placed tree",
			})
		}
	}
	fm := migFrontmatter{
		ID: src.ID, Type: src.Type, OwnerUserID: owner, Title: src.Title,
		Tags: src.Tags, Visibility: src.Visibility, WorkItemID: workItemID,
		ProjectID: projectID, Related: related,
		CreatedAt: src.CreatedAt, UpdatedAt: src.UpdatedAt,
	}
	if fm.Tags == nil {
		fm.Tags = []string{}
	}
	if fm.Related == nil {
		fm.Related = []string{}
	}
	probe, _ := migSmokeSubstring(src.Content)
	return placedMem{
		rec:        *src,
		relPath:    migRelPath(projectID, src.ID, src.Status == "redacted"),
		expectedFM: fm,
		probe:      probe,
	}, nil
}

func migAuthorityPath(dataDir, rel string) string {
	return filepath.Join(dataDir, filepath.FromSlash(rel), "memory.md")
}

func migStatusCounts(placed []placedMem) (active, redacted int) {
	for _, p := range placed {
		if p.rec.Status == "redacted" {
			redacted++
		} else {
			active++
		}
	}
	return active, redacted
}

func migPlacedIDs(placed []placedMem) []string {
	out := make([]string, len(placed))
	for i := range placed {
		out[i] = placed[i].rec.ID
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// RenderMemoriesReport prints the human report.
func (rep *MigMemoriesReport) Render(w io.Writer) {
	mode := rep.Mode
	if rep.DryRun {
		mode += " (dry-run)"
	}
	fmt.Fprintf(w, "migrate-memories [%s]: %d source(s) from %s\n", mode, rep.Migrated, rep.Source)
	if rep.FilteredBySince > 0 {
		fmt.Fprintf(w, "  %d dropped by --since\n", rep.FilteredBySince)
	}
	fmt.Fprintf(w, "  target project %s; active %d, redacted %d", rep.TargetProject, rep.ActiveCount, rep.RedactedCount)
	if rep.Unchanged > 0 {
		fmt.Fprintf(w, ", %d re-placed byte-identical (idempotent re-run)", rep.Unchanged)
	}
	if rep.Rebuilt > 0 || !rep.DryRun {
		fmt.Fprintf(w, "; reconcile rebuilt %d active entr(y/ies)", rep.Rebuilt)
	}
	fmt.Fprintln(w)
	if rep.OwnerOverride != "" {
		fmt.Fprintf(w, "  --owner override: all imported as %s\n", rep.OwnerOverride)
	}
	if len(rep.OldProjects) > 1 {
		fmt.Fprintf(w, "  old projects merged into the target: %s\n", strings.Join(rep.OldProjects, ", "))
	}
	if n := len(rep.FormatDiffs); n > 0 {
		fmt.Fprintf(w, "\nfrontmatter format findings (%d):\n", n)
		for _, c := range rep.FormatDiffs {
			fmt.Fprintf(w, "  %s [%s]: %s\n", c.MemID, c.Field, c.Note)
		}
	} else {
		fmt.Fprintf(w, "\nfrontmatter format findings: none (source format matches v1 field-by-field)\n")
	}
	if n := len(rep.Conversions); n > 0 {
		fmt.Fprintf(w, "\nconversions applied (%d):\n", n)
		for _, c := range rep.Conversions {
			fmt.Fprintf(w, "  %s [%s]: %s → %s%s\n", c.MemID, c.Field, c.Old, c.New, noteSuffix(c.Note))
		}
	}
	if n := len(rep.BlankedRelated); n > 0 {
		fmt.Fprintf(w, "\nblanked related (%d):\n", n)
		for _, c := range rep.BlankedRelated {
			fmt.Fprintf(w, "  %s: %s (%s)\n", c.MemID, c.Old, c.Note)
		}
	}
	if rep.PreservedWIs > 0 {
		fmt.Fprintf(w, "preserved work_item_id references: %d\n", rep.PreservedWIs)
	}
	if !rep.DryRun {
		fmt.Fprintf(w, "\nverification:\n")
		for _, s := range rep.Verification {
			status := "PASS"
			if !s.OK {
				status = "FAIL"
			}
			fmt.Fprintf(w, "  [%s] %s: %s\n", status, s.Step, s.Detail)
		}
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}
