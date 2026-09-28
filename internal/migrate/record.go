package migrate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Record is one line of the exported JSONL (see package doc for the
// schema and the psql export command that produces it).
type Record struct {
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

// memIDPattern is the 06 §4.2 id domain: "mem_" + identifier body. The
// migration keeps old ids verbatim (B03: mem_id 不变), so the check only
// pins the prefix and rejects path-hostile bodies — the exact length the
// new-world NewID produces is NOT required of old-world stock.
var memIDPattern = regexp.MustCompile(`^mem_[A-Za-z0-9_-]+$`)

// visibilities mirrors internal/memory's set (06 §4.2 required field).
var visibilities = map[string]bool{
	"private": true, "project": true, "team": true, "public": true, "admin": true,
}

// ParseError names the input line a validation failure came from: a bad
// record aborts the whole batch (B03: 不因单条解析失败中断整批迁移
// applies to OPTIONAL relations only — a malformed record is input
// corruption, never something to silently drop).
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("input line %d: %s", e.Line, e.Msg)
}

// readRecords parses the JSONL export and applies the --since delta
// filter (updated_at >= since; RFC3339). It returns the records to
// migrate, how many the filter dropped and how many non-blank input
// lines were read. Blank lines are skipped (psql -At can emit a trailing
// newline); anything else must parse as one JSON object per line.
func readRecords(path, since string) (recs []Record, dropped, total int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()

	var sinceT time.Time
	if since != "" {
		sinceT, err = parseRFC3339(since)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("--since: %w", err)
		}
	}

	var (
		out  []Record
		seen = map[string]bool{}
		line int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // memory bodies can be large
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var r Record
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, 0, 0, &ParseError{Line: line, Msg: err.Error()}
		}
		if err := validateRecord(&r); err != nil {
			return nil, 0, 0, &ParseError{Line: line, Msg: err.Error()}
		}
		if seen[r.ID] {
			return nil, 0, 0, &ParseError{Line: line, Msg: fmt.Sprintf("duplicate id %q in input", r.ID)}
		}
		seen[r.ID] = true
		total++
		if sinceT.IsZero() {
			out = append(out, r)
			continue
		}
		upd, err := parseRFC3339(r.UpdatedAt)
		if err != nil {
			return nil, 0, 0, &ParseError{Line: line, Msg: err.Error()}
		}
		if !upd.Before(sinceT) {
			out = append(out, r)
		} else {
			dropped++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, err
	}
	return out, dropped, total, nil
}

// validateRecord enforces the fields the authority frontmatter will need
// to pass internal/memory parseAuthorityDoc (06 §4.2): id, type,
// visibility domain, owner, project, timestamps, non-blank content and a
// known status. Identity fields are NEVER defaulted or blanked here — a
// missing identity is an input error (B03: identities are placeholder-
// created at RESOLUTION time, never blanked).
func validateRecord(r *Record) error {
	if !memIDPattern.MatchString(r.ID) {
		return fmt.Errorf("bad id %q: must match %s", r.ID, memIDPattern.String())
	}
	if strings.TrimSpace(r.Type) == "" {
		return fmt.Errorf("id %s: type required", r.ID)
	}
	if !visibilities[r.Visibility] {
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
	if _, err := parseRFC3339(r.CreatedAt); err != nil {
		return fmt.Errorf("id %s: created_at: %v", r.ID, err)
	}
	if _, err := parseRFC3339(r.UpdatedAt); err != nil {
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

// humanOwner reports whether an owner identifier names a human (keyed by
// email) or a machine/agent account (keyed by actor). An explicit
// owner_kind from the export wins; otherwise the email heuristic ("@").
func (r *Record) humanOwner() bool {
	if r.OwnerKind != "" {
		return r.OwnerKind == "human"
	}
	return strings.Contains(r.Owner, "@")
}

// parseRFC3339 accepts the RFC3339 timestamps the whole Core uses
// (02 §4 preamble); gen-1 exports may carry fractional seconds.
func parseRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("not RFC3339: %q (%v)", s, err)
	}
	return t, nil
}

// smokeSubstring picks the retrieval probe for the §10.1 verification
// smoke: the leading 3 runes of the longest alphanumeric word of the
// body (or the whole word when shorter). Alphanumeric words survive the
// §4.4 structural strip verbatim, so the chosen probe is guaranteed to
// appear in the FTS-indexed body; 3 runes is the trigram threshold the
// smoke test pins.
func smokeSubstring(content string) (string, error) {
	best, cur := "", ""
	flush := func() {
		if len(cur) > len(best) {
			best = cur
		}
		cur = ""
	}
	for _, rn := range content {
		if unicode.IsLetter(rn) || unicode.IsDigit(rn) {
			cur += string(rn)
		} else {
			flush()
		}
	}
	flush()
	if best == "" {
		return "", fmt.Errorf("no alphanumeric run in body to probe retrieval with")
	}
	runes := []rune(best)
	if len(runes) > 3 {
		runes = runes[:3]
	}
	return string(runes), nil
}
