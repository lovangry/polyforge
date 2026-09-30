package cli

// migrate_record.go: the JSONL input contracts of the two migration
// verbs. One JSON object per line; the key sets below are the frozen
// contract (unknown keys are refused — a drifted export must fail loudly,
// never silently drop fields).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// migMemRecord is one line of the memories JSONL export (the v0
// `pf migrate-memories` shape, produced by tools/v0-export from the
// pilot's table+authority-file dual source):
type migMemRecord struct {
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

var memIDPattern = regexp.MustCompile(`^mem_[A-Za-z0-9_-]+$`)

func (r *migMemRecord) humanOwner() bool {
	if r.OwnerKind != "" {
		return r.OwnerKind == "human"
	}
	return strings.Contains(r.Owner, "@")
}

// readMemRecords parses the memories JSONL. Blank lines are skipped;
// anything else must parse and validate. Duplicate ids are input
// corruption.
func readMemRecords(path, since string) (recs []migMemRecord, dropped, total int, err error) {
	var sinceT time.Time
	if since != "" {
		sinceT, err = migParseRFC3339(since)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("--since: %v", err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	var seen = map[string]bool{}
	var line int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // memory bodies can be large
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var r migMemRecord
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if derr := dec.Decode(&r); derr != nil {
			return nil, 0, 0, &migParseError{Line: line, Msg: derr.Error()}
		}
		if verr := validateMemRecord(&r); verr != nil {
			return nil, 0, 0, &migParseError{Line: line, Msg: verr.Error()}
		}
		if seen[r.ID] {
			return nil, 0, 0, &migParseError{Line: line, Msg: fmt.Sprintf("duplicate id %q in input", r.ID)}
		}
		seen[r.ID] = true
		total++
		if !sinceT.IsZero() {
			upd, perr := migParseRFC3339(r.UpdatedAt)
			if perr != nil {
				return nil, 0, 0, &migParseError{Line: line, Msg: perr.Error()}
			}
			if upd.Before(sinceT) {
				dropped++
				continue
			}
		}
		recs = append(recs, r)
	}
	if serr := sc.Err(); serr != nil {
		return nil, 0, 0, serr
	}
	return recs, dropped, total, nil
}

// validateMemRecord enforces the fields the v1 authority frontmatter will
// need to pass parseAuthorityDoc. Identity fields are never defaulted or
// blanked here — a missing identity is an input error (resolution-time
// placeholders are the resolver's business).
func validateMemRecord(r *migMemRecord) error {
	if !memIDPattern.MatchString(r.ID) {
		return fmt.Errorf("bad id %q: must match %s", r.ID, memIDPattern.String())
	}
	if strings.TrimSpace(r.Type) == "" {
		return fmt.Errorf("id %s: type required", r.ID)
	}
	if !migVisibilities[r.Visibility] {
		return fmt.Errorf("id %s: bad visibility %q", r.ID, r.Visibility)
	}
	if strings.TrimSpace(r.Project) == "" {
		return fmt.Errorf("id %s: project name required", r.ID)
	}
	if strings.TrimSpace(r.Owner) == "" {
		return fmt.Errorf("id %s: owner identifier required", r.ID)
	}
	if strings.TrimSpace(r.Content) == "" {
		return fmt.Errorf("id %s: content required", r.ID)
	}
	if _, err := migParseRFC3339(r.CreatedAt); err != nil {
		return fmt.Errorf("id %s: created_at: %v", r.ID, err)
	}
	if _, err := migParseRFC3339(r.UpdatedAt); err != nil {
		return fmt.Errorf("id %s: updated_at: %v", r.ID, err)
	}
	switch r.Status {
	case "active", "redacted":
	default:
		return fmt.Errorf("id %s: bad status %q (active|redacted)", r.ID, r.Status)
	}
	if r.OwnerKind != "" && r.OwnerKind != "human" && r.OwnerKind != "machine" {
		return fmt.Errorf("id %s: bad owner_kind %q (human|machine)", r.ID, r.OwnerKind)
	}
	for _, rel := range r.Related {
		if !memIDPattern.MatchString(rel) {
			return fmt.Errorf("id %s: bad related id %q", r.ID, rel)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Work-item JSONL records

// migWIRecord is one line of the work-items JSONL export. Fields map
// onto core.ImportRequest; the old-world reporter id is resolved to a
// TARGET user by the import. `status` accepts the v0 vocabulary
// (cancelled) and is normalized to v1's canceled on import.
type migWIRecord struct {
	ID             string          `json:"id"`
	Seq            *int64          `json:"seq"`
	Project        string          `json:"project"` // old project name, informational
	Goal           string          `json:"goal"`
	Content        *string         `json:"content"`
	Status         string          `json:"status"`
	Priority       string          `json:"priority"`
	Milestone      *string         `json:"milestone"`
	Labels         []string        `json:"labels"`
	Attrs          map[string]any  `json:"attrs"`
	Reporter       string          `json:"reporter"` // old-world user id
	Assignee       *string         `json:"assignee"`
	WaitingOn      *string         `json:"waiting_on"`
	BundleName     *string         `json:"bundle_name"`
	BundleDigest   *string         `json:"bundle_digest"`
	BudgetBaseline json.RawMessage `json:"workflow_budget_baseline"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	ClosedAt       *string         `json:"closed_at"`
}

// statusAlias maps the v0 vocabulary onto v1's.
func migNormalizeWIStatus(status string) (string, bool) {
	switch status {
	case "wrapped", "failed", "blocked":
		return status, true
	case "canceled", "cancelled":
		return "canceled", true
	}
	return "", false
}

var wiIDPattern = regexp.MustCompile(`^wi_[A-Za-z0-9_-]+$`)

// readWIRecords parses the work-items JSONL (same line discipline as the
// memory records).
func readWIRecords(path string) ([]migWIRecord, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []migWIRecord
	var seen = map[string]bool{}
	var line, total int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var r migWIRecord
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if derr := dec.Decode(&r); derr != nil {
			return nil, 0, &migParseError{Line: line, Msg: derr.Error()}
		}
		if verr := validateWIRecord(&r); verr != nil {
			return nil, 0, &migParseError{Line: line, Msg: verr.Error()}
		}
		if seen[r.ID] {
			return nil, 0, &migParseError{Line: line, Msg: fmt.Sprintf("duplicate id %q in input", r.ID)}
		}
		seen[r.ID] = true
		total++
		out = append(out, r)
	}
	if serr := sc.Err(); serr != nil {
		return nil, 0, serr
	}
	return out, total, nil
}

// validateWIRecord enforces the record contract up front: terminal
// records only (wrapped/failed/canceled[+v0 cancelled]/blocked — live
// state is never migration input), the id domain, non-blank goal,
// RFC3339 timestamps and a closed_at consistent with the status.
func validateWIRecord(r *migWIRecord) error {
	if !wiIDPattern.MatchString(r.ID) {
		return fmt.Errorf("bad id %q: must match %s", r.ID, wiIDPattern.String())
	}
	status, ok := migNormalizeWIStatus(r.Status)
	if !ok {
		return fmt.Errorf("id %s: status %q is not an importable record status (wrapped|failed|canceled|cancelled|blocked)", r.ID, r.Status)
	}
	if strings.TrimSpace(r.Goal) == "" {
		return fmt.Errorf("id %s: goal required", r.ID)
	}
	if len(r.Goal) > 500 {
		return fmt.Errorf("id %s: goal exceeds 500 characters", r.ID)
	}
	if strings.TrimSpace(r.Reporter) == "" {
		return fmt.Errorf("id %s: reporter required", r.ID)
	}
	if _, err := migParseRFC3339(r.CreatedAt); err != nil {
		return fmt.Errorf("id %s: created_at: %v", r.ID, err)
	}
	if _, err := migParseRFC3339(r.UpdatedAt); err != nil {
		return fmt.Errorf("id %s: updated_at: %v", r.ID, err)
	}
	switch status {
	case "wrapped", "failed", "canceled":
		if r.ClosedAt == nil || strings.TrimSpace(*r.ClosedAt) == "" {
			return fmt.Errorf("id %s: status %s requires closed_at", r.ID, status)
		}
		if _, err := migParseRFC3339(*r.ClosedAt); err != nil {
			return fmt.Errorf("id %s: closed_at: %v", r.ID, err)
		}
	case "blocked":
		// a non-terminal record carries no close time; a stale one is
		// dropped so the target stays consistent with its own state machine
		r.ClosedAt = nil
	}
	if len(r.Labels) > 20 {
		return fmt.Errorf("id %s: too many labels (max 20)", r.ID)
	}
	return nil
}
