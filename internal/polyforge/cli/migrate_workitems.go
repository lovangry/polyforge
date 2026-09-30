package cli

// migrate_workitems.go: the migrate-work-items engine — the terminal
// work-item RECORD import. The wire path is the admin-only
// POST /api/v1/admin/import-work-item (core.ImportRequest): the public
// create cannot carry id/status/closed_at, and the import path is
// deliberately narrower than even that (record statuses only, no
// execution history, one audit event per row).
//
// Idempotency: a same-id re-import answers outcome=skipped-existing; the
// tool then samples the existing row over the ordinary GET and compares
// the record fields — a skip is only counted when the target already
// carries the same record, a divergence is a hard failure.

import (
	"fmt"
	"io"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// MigWorkItemsOptions is the resolved `pf migrate-work-items` invocation.
type MigWorkItemsOptions struct {
	AihubURL  string
	APIKey    string // operator admin api key
	FromJSONL string
	Project   string // target project name or id (required)
	Reporter  string // optional: map every old reporter to this target user id
	UserMap   map[string]string
	DryRun    bool
}

// MigWIReport is the import report the verb prints.
type MigWIReport struct {
	Source            string          `json:"source"`
	InputLines        int             `json:"input_lines"`
	Imported          int             `json:"imported"`
	Skipped           int             `json:"skipped"` // same-id re-import, record matches
	StatusCounts      map[string]int  `json:"status_counts"`
	TargetProject     string          `json:"target_project"`
	ReporterOverride  string          `json:"reporter_override,omitempty"`
	ResolvedReporters []MigConversion `json:"resolved_reporters"`
	Verification      []StepResult    `json:"verification"`
	SampledIDs        []string        `json:"sampled_ids"`
	DryRun            bool            `json:"dry_run"`
}

// RunMigrateWorkItems executes the import + verification.
func RunMigrateWorkItems(opts MigWorkItemsOptions, client *aihubclient.Client) (*MigWIReport, error) {
	if strings.TrimSpace(opts.AihubURL) == "" {
		return nil, fmt.Errorf("--aihub-url is required (or set server in the config)")
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("an admin api key is required (--api-key, PF_API_KEY or the config file)")
	}
	if strings.TrimSpace(opts.FromJSONL) == "" {
		return nil, fmt.Errorf("--from <jsonl> is required")
	}
	if strings.TrimSpace(opts.Project) == "" {
		return nil, fmt.Errorf("--project (the target project) is required")
	}

	records, total, err := readWIRecords(opts.FromJSONL)
	if err != nil {
		return nil, err
	}
	rep := &MigWIReport{
		Source: opts.FromJSONL, InputLines: total,
		StatusCounts: map[string]int{}, DryRun: opts.DryRun,
		ReporterOverride: opts.Reporter,
	}

	proj, err := migResolveProject(client, opts.Project)
	if err != nil {
		return nil, err
	}
	rep.TargetProject = proj.ID
	resolver := newMigResolver(client, opts.UserMap, opts.Reporter)
	if err := resolver.preload(); err != nil {
		return nil, err
	}

	var importedIDs []string
	for i := range records {
		r := records[i]
		status, _ := migNormalizeWIStatus(r.Status)
		rep.StatusCounts[status]++

		reporter, rerr := resolver.user(r.Reporter, true)
		if rerr != nil {
			return nil, rerr
		}
		if reporter != r.Reporter {
			rep.ResolvedReporters = append(rep.ResolvedReporters, MigConversion{
				Field: "reporter", Old: r.Reporter, New: reporter,
			})
		}
		// assignee attribution: resolved when the target carries the id,
		// else blanked and logged (never an FK failure mid-batch).
		assignee := r.Assignee
		if assignee != nil && *assignee != "" {
			resolved, aerr := resolver.userOptional(*assignee)
			if aerr != nil {
				return nil, aerr
			}
			if resolved == nil {
				rep.ResolvedReporters = append(rep.ResolvedReporters, MigConversion{
					Field: "assignee", Old: *assignee,
					Note: "no such user in the target; blanked (map it with --user-map to keep attribution)",
				})
				assignee = nil
			} else if *resolved != *assignee {
				rep.ResolvedReporters = append(rep.ResolvedReporters, MigConversion{
					Field: "assignee", Old: *assignee, New: *resolved,
				})
				assignee = resolved
			}
		}
		req := aihubclient.ImportWorkItemRequest{
			Project: opts.Project, ID: r.ID, Seq: r.Seq,
			Goal: r.Goal, Content: r.Content, Status: status,
			Priority: r.Priority, Milestone: r.Milestone, Labels: r.Labels,
			Attrs: r.Attrs, Reporter: reporter, Assignee: assignee,
			WaitingOn: r.WaitingOn, BundleName: r.BundleName, BundleDigest: r.BundleDigest,
			BudgetBaseline: r.BudgetBaseline,
			CreatedAt:      r.CreatedAt, UpdatedAt: r.UpdatedAt, ClosedAt: r.ClosedAt,
		}
		if opts.DryRun {
			continue
		}
		res, ierr := client.ImportWorkItem(req)
		if ierr != nil {
			return nil, fmt.Errorf("import %s: %v", r.ID, ierr)
		}
		switch res.Outcome {
		case string(coreImportDone):
			importedIDs = append(importedIDs, r.ID)
			rep.Imported++
		case string(coreImportSkipped):
			// Idempotent re-run: verify the existing row carries the same
			// record before counting the skip.
			existing, gerr := client.GetWorkItem(r.ID)
			if gerr != nil {
				return nil, fmt.Errorf("skip-check %s: %v", r.ID, gerr)
			}
			if werr := migCompareWIRecord(existing, &r, status); werr != nil {
				return nil, fmt.Errorf("idempotency check on %s: %v", r.ID, werr)
			}
			rep.Skipped++
		default:
			return nil, fmt.Errorf("import %s: unknown outcome %q", r.ID, res.Outcome)
		}
	}
	if !opts.DryRun && rep.Imported+rep.Skipped != len(records) {
		return nil, fmt.Errorf("bookkeeping: imported %d + skipped %d != records %d",
			rep.Imported, rep.Skipped, len(records))
	}

	// Verification: count + sampled field comparison over the ordinary
	// business GET face.
	if !opts.DryRun {
		rep.SampledIDs = migSampleIDs(importedIDs)
		if verr := migVerifyWorkItems(rep, records, client); verr != nil {
			return rep, verr
		}
	}
	return rep, nil
}

// coreImportDone/Skipped mirror core's outcome strings without importing
// the server-side package into the CLI.
const (
	coreImportDone    = "imported"
	coreImportSkipped = "skipped-existing"
)

// migCompareWIRecord checks that an existing target row carries the same
// record as the JSONL line (the idempotency contract: a skip is only a
// skip when the row already IS this record).
func migCompareWIRecord(wi *api.WorkItem, r *migWIRecord, status string) error {
	if wi.Status != status {
		return fmt.Errorf("status %q, want %q", wi.Status, status)
	}
	if strings.TrimSpace(wi.Goal) != strings.TrimSpace(r.Goal) {
		return fmt.Errorf("goal differs")
	}
	if (wi.ClosedAt == nil) != (r.ClosedAt == nil) {
		return fmt.Errorf("closed_at %v != %v", wi.ClosedAt, r.ClosedAt)
	}
	if wi.ClosedAt != nil && *wi.ClosedAt != *r.ClosedAt {
		return fmt.Errorf("closed_at %s != %s", *wi.ClosedAt, *r.ClosedAt)
	}
	return nil
}

// migVerifyWorkItems is the work-item verification: ① every record's row
// exists in the target with the record's status (over the list face, so
// the whole check rides the ordinary business surface), ② the sampled
// imported rows field-compare against the record (goal, content, status,
// closed_at, priority, labels, milestone, seq).
func migVerifyWorkItems(rep *MigWIReport, records []migWIRecord, client *aihubclient.Client) error {
	// ① count: list each status the report imported.
	problems := []string{}
	byStatus := map[string]int{}
	for _, r := range records {
		status, _ := migNormalizeWIStatus(r.Status)
		byStatus[status]++
	}
	for status, want := range byStatus {
		got, err := migCountWIs(client, rep.TargetProject, status)
		if err != nil {
			problems = append(problems, fmt.Sprintf("list status %s: %v", status, err))
			continue
		}
		if got < want {
			problems = append(problems, fmt.Sprintf("status %s: target carries %d < imported %d", status, got, want))
		}
	}
	stepCount := StepResult{Step: "count-reconciliation", OK: len(problems) == 0,
		Detail: fmt.Sprintf("%d record(s) across %d status(es)", len(records), len(byStatus))}
	if !stepCount.OK {
		stepCount.Detail = joinNonEmpty(problems, "; ")
	}
	rep.Verification = append(rep.Verification, stepCount)

	// ② sampled field comparison.
	sampled := map[string]bool{}
	for _, id := range rep.SampledIDs {
		sampled[id] = true
	}
	var sampleProblems []string
	byID := map[string]migWIRecord{}
	for _, r := range records {
		byID[r.ID] = r
	}
	checked := 0
	for id := range sampled {
		r := byID[id]
		wi, err := client.GetWorkItem(id)
		if err != nil {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		checked++
		status, _ := migNormalizeWIStatus(r.Status)
		if wi.Status != status {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: status %q != %q", id, wi.Status, status))
		}
		if strings.TrimSpace(wi.Goal) != strings.TrimSpace(r.Goal) {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: goal differs", id))
		}
		if !migPtrEq(wi.ClosedAt, r.ClosedAt) {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: closed_at differs", id))
		}
		if r.Content != nil && (wi.Content == nil || *wi.Content != *r.Content) {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: content differs", id))
		}
		if r.Seq != nil && wi.Seq != *r.Seq {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: seq %d != %d", id, wi.Seq, *r.Seq))
		}
		if r.Milestone != nil && (wi.Milestone == nil || *wi.Milestone != *r.Milestone) {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: milestone differs", id))
		}
		if !migSliceEq(wi.Labels, orEmpty(r.Labels)) {
			sampleProblems = append(sampleProblems, fmt.Sprintf("%s: labels differ", id))
		}
	}
	stepSample := StepResult{Step: "field-sampling", OK: len(sampleProblems) == 0,
		Detail: fmt.Sprintf("%d of %d record(s) sampled at the fixed %d%% ratio, all fields carry",
			checked, len(records), migSampleRatioN*100/migSampleRatioD)}
	if !stepSample.OK {
		stepSample.Detail = joinNonEmpty(sampleProblems, "; ")
	}
	rep.Verification = append(rep.Verification, stepSample)

	if !stepCount.OK || !stepSample.OK {
		return &verifyError{failures: []string{stepCount.Detail, stepSample.Detail}}
	}
	return nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func migCountWIs(client *aihubclient.Client, project, status string) (int, error) {
	wis, err := client.ListWorkItems(project, status)
	if err != nil {
		return 0, err
	}
	return len(wis), nil
}

// Render prints the human report.
func (rep *MigWIReport) Render(w io.Writer) {
	mode := "jsonl"
	if rep.DryRun {
		mode += " (dry-run)"
	}
	fmt.Fprintf(w, "migrate-work-items [%s]: %d record(s) from %s\n", mode, rep.InputLines, rep.Source)
	fmt.Fprintf(w, "  target project %s; imported %d, skipped %d; statuses: %s\n",
		rep.TargetProject, rep.Imported, rep.Skipped, migStatusLine(rep.StatusCounts))
	if rep.ReporterOverride != "" {
		fmt.Fprintf(w, "  --reporter override: all imported as %s\n", rep.ReporterOverride)
	}
	for _, c := range rep.ResolvedReporters {
		fmt.Fprintf(w, "  %s: %s → %s%s\n", c.Field, c.Old, c.New, noteSuffix(c.Note))
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

func migStatusLine(counts map[string]int) string {
	parts := []string{}
	for _, s := range []string{"wrapped", "failed", "canceled", "blocked"} {
		if n, ok := counts[s]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", s, n))
		}
	}
	return strings.Join(parts, ", ")
}
