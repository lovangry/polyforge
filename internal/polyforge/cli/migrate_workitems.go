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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
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
	// AcceptChanged: apply changed same-id records in place
	// (overwrite:true on the admin import; new version wins). Without
	// it a divergent record hard-fails the run with a per-field diff.
	AcceptChanged bool
	DryRun        bool
}

// MigWIFailure is one record the import face refused: the run continues
// (the whole file is accounted for), the report carries every failure
// and the run exits non-zero.
type MigWIFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// MigWIReport is the import report the verb prints.
type MigWIReport struct {
	Source            string          `json:"source"`
	InputLines        int             `json:"input_lines"`
	Imported          int             `json:"imported"`
	Skipped           int             `json:"skipped"`     // same-id re-import, record matches
	Overwritten       int             `json:"overwritten"` // same-id re-import, --accept-changed applied (counted as changed)
	Failed            int             `json:"failed"`      // refused by the import face (counted, run continues)
	Failures          []MigWIFailure  `json:"failures,omitempty"`
	StatusCounts      map[string]int  `json:"status_counts"`
	TargetProject     string          `json:"target_project"`
	ReporterOverride  string          `json:"reporter_override,omitempty"`
	ResolvedReporters []MigConversion `json:"resolved_reporters"`
	Verification      []StepResult    `json:"verification"`
	SampledIDs        []string        `json:"sampled_ids"`
	DryRun            bool            `json:"dry_run"`
}

// migFailuresError reports counted per-record import failures: the file
// is fully accounted for, but some records did not land. The report (with
// the closing summary) is printed first; the error sets the non-zero exit
// (client class).
type migFailuresError struct{ failures []MigWIFailure }

func (e *migFailuresError) Error() string {
	n := len(e.failures)
	first := e.failures[0]
	more := ""
	if n > 1 {
		more = fmt.Sprintf(" (and %d more — see the report)", n-1)
	}
	return fmt.Sprintf("%d record(s) failed to import: %s: %s%s", n, first.ID, first.Error, more)
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
			Overwrite: opts.AcceptChanged,
		}
		// The record as the import face will store it (the truncation is
		// deterministic, so pass-2 comparisons must mirror it — see
		// migCompareWIRecord).
		expGoal, expAttrs := migExpectedGoal(&r)
		if opts.DryRun {
			continue
		}
		res, ierr := client.ImportWorkItem(req)
		if ierr != nil {
			// Counted, never run-fatal: one refused record must not kill a
			// 3000-record import — the file is accounted for in the report
			// and the summary carries the failure count.
			rep.Failed++
			rep.Failures = append(rep.Failures, MigWIFailure{ID: r.ID, Error: ierr.Error()})
			continue
		}
		switch res.Outcome {
		case string(coreImportDone):
			importedIDs = append(importedIDs, r.ID)
			rep.Imported++
		case string(coreImportOverwritten):
			// --accept-changed applied a changed record: the row now
			// carries the record; count it as changed.
			importedIDs = append(importedIDs, r.ID)
			rep.Overwritten++
		case string(coreImportSkipped):
			// Idempotent re-run: verify the existing row carries the same
			// record (ALL fields) before counting the skip.
			existing, gerr := client.GetWorkItem(r.ID)
			if gerr != nil {
				rep.Failed++
				rep.Failures = append(rep.Failures, MigWIFailure{ID: r.ID, Error: fmt.Sprintf("skip-check: %v", gerr)})
				continue
			}
			if werr := migCompareWIRecord(existing, expGoal, expAttrs, status, reporter, assignee, &r); werr != nil {
				rep.Failed++
				rep.Failures = append(rep.Failures, MigWIFailure{
					ID: r.ID, Error: fmt.Sprintf("%v (re-run with --accept-changed to apply the new version)", werr)})
				continue
			}
			rep.Skipped++
		default:
			rep.Failed++
			rep.Failures = append(rep.Failures, MigWIFailure{ID: r.ID, Error: fmt.Sprintf("unknown outcome %q", res.Outcome)})
		}
	}
	if !opts.DryRun && rep.Imported+rep.Overwritten+rep.Skipped+rep.Failed != len(records) {
		return nil, fmt.Errorf("bookkeeping: imported %d + overwritten %d + skipped %d + failed %d != records %d",
			rep.Imported, rep.Overwritten, rep.Skipped, rep.Failed, len(records))
	}

	// Verification: count + sampled field comparison over the ordinary
	// business GET face.
	if !opts.DryRun {
		rep.SampledIDs = migSampleIDs(importedIDs)
		if verr := migVerifyWorkItems(rep, records, client); verr != nil {
			return rep, verr
		}
	}
	if rep.Failed > 0 {
		return rep, &migFailuresError{failures: rep.Failures}
	}
	return rep, nil
}

// coreImportDone/Skipped/Overwritten mirror core's outcome strings without
// importing the server-side package into the CLI.
const (
	coreImportDone        = "imported"
	coreImportSkipped     = "skipped-existing"
	coreImportOverwritten = "overwritten"
)

// migExpectedGoal mirrors the import face's over-long-goal handling
// (seq-fix lane fix 2): a goal over api.MaxGoalBytes is truncated at a
// rune boundary and the FULL original preserved under attrs.legacy_goal.
// The truncation is deterministic, so pass-2 field comparisons and the
// sampled verification compare against THIS expected shape, never the
// raw record.
func migExpectedGoal(r *migWIRecord) (string, map[string]any) {
	goal := strings.TrimSpace(r.Goal)
	attrs := r.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	if len(goal) > api.MaxGoalBytes {
		goal = api.TruncateUTF8(goal, api.MaxGoalBytes)
		merged := make(map[string]any, len(r.Attrs)+1)
		for k, v := range r.Attrs {
			merged[k] = v
		}
		merged["legacy_goal"] = strings.TrimSpace(r.Goal)
		attrs = merged
	}
	return goal, attrs
}

// migCompareWIRecord checks that an existing target row carries the same
// record as the JSONL line across ALL record fields — the idempotency
// contract: a skip is only a skip when the row already IS this record.
// expGoal/expAttrs are the expected post-import goal and attrs (the
// import face may have truncated an over-long goal — see
// migExpectedGoal); reporter/assignee are the RESOLVED target identities
// (the import remaps them); priority falls back to normal like the
// import does; an absent seq means "auto" and is not compared. Every
// divergent field is listed — a per-field diff, never a silent skip.
// The goal is MIRRORED through the import's own normalization first
// (TrimSpace → byte-cap truncate): a re-run against the raw export
// would otherwise flag every trimmed-or-truncated record (the legacy
// library's long CJK goals measured exactly this).
func migCompareWIRecord(wi *api.WorkItem, expGoal string, expAttrs map[string]any,
	status, reporter string, assignee *string, r *migWIRecord) error {
	var diffs []string
	priority := r.Priority
	if priority == "" {
		priority = api.PriorityNormal
	}
	// Trim AFTER truncating too: a rune-boundary cut can land right after a
	// space, re-introducing a trailing space the import-side store kept but
	// the target-side compare trims away (measured live at cutover: 496B != 499B).
	expGoal = strings.TrimSpace(api.TruncateUTF8(strings.TrimSpace(expGoal), api.MaxGoalBytes))
	if wi.Status != status {
		diffs = append(diffs, fmt.Sprintf("status: target %q != record %q", wi.Status, status))
	}
	if strings.TrimSpace(wi.Goal) != expGoal {
		diffs = append(diffs, fmt.Sprintf("goal differs: target %q != record %q",
			strings.TrimSpace(wi.Goal), expGoal))
	}
	if !migPtrEq(wi.Content, r.Content) {
		diffs = append(diffs, fmt.Sprintf("content: target %s != record %s",
			migPtrDesc(wi.Content), migPtrDesc(r.Content)))
	}
	if r.Seq != nil && wi.Seq != *r.Seq {
		diffs = append(diffs, fmt.Sprintf("seq: target %d != record %d", wi.Seq, *r.Seq))
	}
	if wi.Priority != priority {
		diffs = append(diffs, fmt.Sprintf("priority: target %q != record %q", wi.Priority, priority))
	}
	if !migPtrEq(wi.Milestone, r.Milestone) {
		diffs = append(diffs, fmt.Sprintf("milestone: target %s != record %s",
			migPtrDesc(wi.Milestone), migPtrDesc(r.Milestone)))
	}
	if !migSliceEq(wi.Labels, orEmpty(r.Labels)) {
		diffs = append(diffs, fmt.Sprintf("labels: target %v != record %v", wi.Labels, orEmpty(r.Labels)))
	}
	attrs := expAttrs
	if !reflect.DeepEqual(wi.Attrs, attrs) {
		diffs = append(diffs, fmt.Sprintf("attrs: target %v != record %v", wi.Attrs, attrs))
	}
	if wi.Reporter != reporter {
		diffs = append(diffs, fmt.Sprintf("reporter: target %q != record %q", wi.Reporter, reporter))
	}
	if !migPtrEq(wi.Assignee, assignee) {
		diffs = append(diffs, fmt.Sprintf("assignee: target %s != record %s",
			migPtrDesc(wi.Assignee), migPtrDesc(assignee)))
	}
	if !migPtrEq(wi.WaitingOn, r.WaitingOn) {
		diffs = append(diffs, fmt.Sprintf("waiting_on: target %s != record %s",
			migPtrDesc(wi.WaitingOn), migPtrDesc(r.WaitingOn)))
	}
	if !migPtrEq(wi.BundleName, r.BundleName) {
		diffs = append(diffs, fmt.Sprintf("bundle_name: target %s != record %s",
			migPtrDesc(wi.BundleName), migPtrDesc(r.BundleName)))
	}
	// The import face derives a stand-in bundle digest when the record
	// carries a bundle_name without one (core's §4.5 stand-in rule) — the
	// row therefore carries it even though the record does not.
	expBundleDigest := r.BundleDigest
	if expBundleDigest == nil && r.BundleName != nil && *r.BundleName != "" {
		sum := sha256.Sum256([]byte("bundle-name:" + *r.BundleName))
		d := hex.EncodeToString(sum[:])
		expBundleDigest = &d
	}
	if !migPtrEq(wi.BundleDigest, expBundleDigest) {
		diffs = append(diffs, fmt.Sprintf("bundle_digest: target %s != record %s",
			migPtrDesc(wi.BundleDigest), migPtrDesc(expBundleDigest)))
	}
	if !migRawJSONEq(wi.BudgetBaseline, r.BudgetBaseline) {
		diffs = append(diffs, fmt.Sprintf("workflow_budget_baseline: target %s != record %s",
			string(wi.BudgetBaseline), string(r.BudgetBaseline)))
	}
	if wi.CreatedAt != r.CreatedAt {
		diffs = append(diffs, fmt.Sprintf("created_at: target %q != record %q", wi.CreatedAt, r.CreatedAt))
	}
	if wi.UpdatedAt != r.UpdatedAt {
		diffs = append(diffs, fmt.Sprintf("updated_at: target %q != record %q", wi.UpdatedAt, r.UpdatedAt))
	}
	if !migPtrEq(wi.ClosedAt, r.ClosedAt) {
		diffs = append(diffs, fmt.Sprintf("closed_at: target %s != record %s",
			migPtrDesc(wi.ClosedAt), migPtrDesc(r.ClosedAt)))
	}
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("%d record field(s) diverge: %s", len(diffs), strings.Join(diffs, "; "))
}

func migPtrDesc(p *string) string {
	if p == nil {
		return "(none)"
	}
	return fmt.Sprintf("%q", *p)
}

// migRawJSONEq compares two raw JSON payloads. Absent is nil, empty
// or the JSON null (the wire marshals a nil RawMessage as null, which
// the import stores verbatim) — all three mean "no baseline".
func migRawJSONEq(a, b json.RawMessage) bool {
	norm := func(r json.RawMessage) string {
		s := strings.TrimSpace(string(r))
		if s == "null" {
			return ""
		}
		return s
	}
	return norm(a) == norm(b)
}

// migVerifyWorkItems is the work-item verification: ① every record's row
// exists in the target with the record's status (over the list face, so
// the whole check rides the ordinary business surface), ② the sampled
// imported rows field-compare against the record (goal, content, status,
// closed_at, priority, labels, milestone, seq).
func migVerifyWorkItems(rep *MigWIReport, records []migWIRecord, client *aihubclient.Client) error {
	// ① count: list each status the records the run PLACED or verified
	// carry. Records the import face refused are excluded — they are
	// accounted as failed in the report's summary, and demanding a row
	// for them here would just double-report the same failure.
	problems := []string{}
	failed := map[string]bool{}
	for _, f := range rep.Failures {
		failed[f.ID] = true
	}
	byStatus := map[string]int{}
	for _, r := range records {
		if failed[r.ID] {
			continue
		}
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
		Detail: fmt.Sprintf("%d record(s) across %d status(es)", len(records)-len(failed), len(byStatus))}
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
		expGoal, _ := migExpectedGoal(&r)
		if strings.TrimSpace(wi.Goal) != expGoal {
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
	// Whole-project count: the list endpoint's default page (500) would
	// silently undercount big legacy projects (aihub carried 678 wrapped
	// — the rehearsal measured the false "500 < 678" on exactly this).
	raw, err := client.ListWorkItemsRawLimit(project, status, 50000)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, err
	}
	return len(resp.Items), nil
}

// Render prints the human report.
func (rep *MigWIReport) Render(w io.Writer) {
	mode := "jsonl"
	if rep.DryRun {
		mode += " (dry-run)"
	}
	fmt.Fprintf(w, "migrate-work-items [%s]: %d record(s) from %s\n", mode, rep.InputLines, rep.Source)
	fmt.Fprintf(w, "  target project %s; imported %d, skipped %d, overwritten %d (changed); statuses: %s\n",
		rep.TargetProject, rep.Imported, rep.Skipped, rep.Overwritten, migStatusLine(rep.StatusCounts))
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
	// Every refused record is listed, then the summary — ALWAYS the last
	// line (seq-fix lane fix 4): imported / already-exists / failed
	// accounting for the whole file. A pass-2 idempotent re-run answers
	// "0 imported / N already-exists / 0 failed" with exit 0 — a success,
	// not an error.
	for _, f := range rep.Failures {
		fmt.Fprintf(w, "  failed %s: %s\n", f.ID, f.Error)
	}
	verdict := "PASS"
	if rep.Failed > 0 {
		verdict = "FAIL"
	}
	summary := fmt.Sprintf("  [%s] %d imported / %d already-exists / %d failed",
		verdict, rep.Imported, rep.Skipped, rep.Failed)
	if rep.Overwritten > 0 {
		summary += fmt.Sprintf(" / %d changed", rep.Overwritten)
	}
	if rep.DryRun {
		summary += " (dry-run: nothing imported)"
	}
	fmt.Fprintln(w, summary)
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
