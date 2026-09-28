package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Rendering rules:
//   - --json: emit the server's response body as-is (raw passthrough).
//   - human (default): per-verb renderers below; anything without a
//     dedicated renderer falls back to pretty-printed JSON.
//   - errors: "pf: CORE-409 ALREADY_CLAIMED: detail" with the code
//     prominent; in --json mode the raw wire envelope is emitted instead.

// emit prints the command payload: raw passthrough in --json mode ONLY.
// Human mode renders instead (every caller's !ctx.JSON branch) — emitting
// the raw JSON there duplicated the server body above the rendered view.
func (ctx *Context) emit(raw []byte) {
	if !ctx.JSON {
		return
	}
	ctx.Out(raw)
}

// Out writes to stdout (indirection keeps verb code readable and tests
// hermetic).
func (ctx *Context) Out(data []byte) {
	ctx.Stdout.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(ctx.Stdout)
	}
}

// printJSON pretty-prints raw JSON for human-readable fallback rendering.
func printJSON(w io.Writer, raw []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not JSON (octet-stream payloads etc.): print verbatim.
		fmt.Fprintln(w, string(raw))
		return
	}
	buf.WriteTo(w)
	fmt.Fprintln(w)
}

// decodeJSON unmarshals raw into v.
func decodeJSON(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

// printTable renders a simple aligned text table.
func printTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	head := make([]string, len(headers))
	for i, h := range headers {
		head[i] = pad(h, widths[i])
	}
	fmt.Fprintln(w, strings.Join(head, "  "))
	for i := range headers {
		fmt.Fprint(w, strings.Repeat("-", widths[i]))
		if i < len(headers)-1 {
			fmt.Fprint(w, "  ")
		}
	}
	fmt.Fprintln(w)
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = pad(cell, widths[i])
		}
		fmt.Fprintln(w, strings.Join(cells, "  "))
	}
}

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// truncate shortens s for table cells.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// nullable renders a *string-ish JSON value for tables.
func nullable(v *string) string {
	if v == nil {
		return "-"
	}
	return *v
}

// ---------------------------------------------------------------------------
// Wire view structs for human rendering (subset of api/openapi.yaml).
// ---------------------------------------------------------------------------

// wiView is the decoded base record for table rendering.
type wiView struct {
	ID        string   `json:"id"`
	Slug      string   `json:"slug"`
	Project   string   `json:"project"`
	Goal      string   `json:"goal"`
	Status    string   `json:"status"`
	Priority  string   `json:"priority"`
	Labels    []string `json:"labels"`
	Milestone *string  `json:"milestone"`
	Assignee  *string  `json:"assignee"`
	WaitingOn *string  `json:"waiting_on"`
	Bundle    *string  `json:"bundle_name"`
	CreatedAt string   `json:"created_at"`
	ClosedAt  *string  `json:"closed_at"`
}

// detailView is wiView plus the row-3 detail sections.
type detailView struct {
	wiView
	RepoInventory []repoEntryView `json:"repo_inventory"`
	Events        []wiEventView   `json:"events,omitempty"`
}

type repoEntryView struct {
	Position int64   `json:"position"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Base     string  `json:"base"`
	Subdir   *string `json:"subdir"`
}

type wiEventView struct {
	ID        int64           `json:"id"`
	CreatedAt string          `json:"created_at"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

// readyQueueView is the flat two-array response (02 §5.1b).
type readyQueueView struct {
	Project    string           `json:"project"`
	Executable []readyEntryView `json:"executable"`
	Blocked    []readyEntryView `json:"blocked"`
}

type readyEntryView struct {
	ID        string   `json:"id"`
	Slug      string   `json:"slug"`
	Goal      string   `json:"goal"`
	Priority  string   `json:"priority"`
	Status    string   `json:"status"`
	Bundle    *string  `json:"bundle_name"`
	WaitingOn *string  `json:"waiting_on"`
	BlockedBy []string `json:"blocked_by"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
}

// wiListView is the {items:[...]} wrapper.
type wiListView struct {
	Items []wiView `json:"items"`
}

// memoryEntryView is the decoded memory row for search rendering.
type memoryEntryView struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Title     *string  `json:"title"`
	Project   string   `json:"project_id"`
	UpdatedAt string   `json:"updated_at"`
	Tags      []string `json:"tags"`
}

// memoryListView is the {items:[...]} wrapper.
type memoryListView struct {
	Items []memoryEntryView `json:"items"`
}

// userView is the M1 record.
type userView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Role        string `json:"role"`
}

type userListView struct {
	Items []userView `json:"items"`
}

// apiKeyMetaView is one M2 list row (never material).
type apiKeyMetaView struct {
	ID        string  `json:"id"`
	UserID    string  `json:"user_id"`
	ProjectID *string `json:"project_id"`
	RevokedAt *string `json:"revoked_at"`
}

type apiKeyListView struct {
	Items []apiKeyMetaView `json:"items"`
}

// projectView is the M3 record.
type projectView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Visible bool   `json:"visible"`
}

type projectListView struct {
	Items []projectView `json:"items"`
}

// depsView is the row-26 two-list answer.
type depsView struct {
	Blocking  []depEdgeView `json:"blocking"`
	BlockedBy []depEdgeView `json:"blocked_by"`
}

type depEdgeView struct {
	WiID string `json:"wi_id"`
	Kind string `json:"kind"`
}

// progressView is the row-6 read-through response.
type progressView struct {
	WorkItemID     string          `json:"work_item_id"`
	HeadSeq        *int64          `json:"head_seq"`
	HeadGeneration *int64          `json:"head_generation"`
	Progress       json.RawMessage `json:"progress"`
	HeadUpdatedAt  *string         `json:"head_updated_at"`
	FreshnessNote  string          `json:"freshness_note"`
}

// budgetView is the row-12 response.
type budgetView struct {
	Baseline  json.RawMessage `json:"baseline"`
	Effective json.RawMessage `json:"effective"`
	MaxSeq    int64           `json:"max_seq"`
}

// ---------------------------------------------------------------------------
// Human renderers.
// ---------------------------------------------------------------------------

// renderWITable renders a WI list table.
func renderWITable(w io.Writer, items []wiView) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{
			it.Slug, it.Status, it.Priority, nullable(it.Bundle), truncate(it.Goal, 60),
		})
	}
	printTable(w, []string{"SLUG", "STATUS", "PRIORITY", "BUNDLE", "GOAL"}, rows)
}

// renderReadyQueue renders the flat two-array queue.
func renderReadyQueue(w io.Writer, q *readyQueueView) {
	fmt.Fprintf(w, "project: %s\n\nexecutable (claimable now):\n", q.Project)
	rows := make([][]string, 0, len(q.Executable)+1)
	for _, it := range q.Executable {
		rows = append(rows, []string{it.ID, it.Slug, it.Priority, nullable(it.Bundle), truncate(it.Goal, 60)})
	}
	printTable(w, []string{"ID", "SLUG", "PRIORITY", "BUNDLE", "GOAL"}, rows)
	if len(q.Blocked) > 0 {
		fmt.Fprintf(w, "\nblocked (waiting):\n")
		rows = rows[:0]
		for _, it := range q.Blocked {
			wait := "-"
			if it.WaitingOn != nil {
				wait = *it.WaitingOn
			}
			by := "-"
			if len(it.BlockedBy) > 0 {
				by = strings.Join(it.BlockedBy, ",")
			}
			rows = append(rows, []string{it.ID, it.Slug, it.Status, it.Priority, wait, by, truncate(it.Goal, 40)})
		}
		printTable(w, []string{"ID", "SLUG", "STATUS", "PRIORITY", "WAITING_ON", "BLOCKED_BY", "GOAL"}, rows)
	}
}

// renderWIDetail renders the detail record (and include sections).
func renderWIDetail(w io.Writer, d *detailView) {
	fmt.Fprintf(w, "%s (%s)\n", d.Slug, d.ID)
	fmt.Fprintf(w, "  status:     %s (priority %s)\n", d.Status, d.Priority)
	fmt.Fprintf(w, "  project:   %s\n", d.Project)
	fmt.Fprintf(w, "  goal:      %s\n", d.Goal)
	if d.Milestone != nil {
		fmt.Fprintf(w, "  milestone: %s\n", *d.Milestone)
	}
	if len(d.Labels) > 0 {
		fmt.Fprintf(w, "  labels:    %s\n", strings.Join(d.Labels, ","))
	}
	fmt.Fprintf(w, "  bundle:    %s\n", nullable(d.Bundle))
	if d.Assignee != nil {
		fmt.Fprintf(w, "  assignee:  %s\n", *d.Assignee)
	}
	if d.WaitingOn != nil {
		fmt.Fprintf(w, "  waiting:   %s\n", *d.WaitingOn)
	}
	fmt.Fprintf(w, "  created:   %s\n", d.CreatedAt)
	if len(d.RepoInventory) > 0 {
		fmt.Fprintf(w, "  repo inventory:\n")
		for _, r := range d.RepoInventory {
			fmt.Fprintf(w, "    %d. %s  base=%s  %s\n", r.Position, r.Name, r.Base, r.URL)
		}
	}
	if len(d.Events) > 0 {
		fmt.Fprintf(w, "  events:\n")
		for _, e := range d.Events {
			payload := ""
			if len(e.Payload) > 0 {
				payload = " " + string(e.Payload)
			}
			fmt.Fprintf(w, "    %s %s%s\n", e.CreatedAt, e.EventType, payload)
		}
	}
}

// renderMemories renders a search/list result.
func renderMemories(w io.Writer, m *memoryListView) {
	rows := make([][]string, 0, len(m.Items))
	for _, it := range m.Items {
		rows = append(rows, []string{it.ID, it.Type, nullable(it.Title), it.UpdatedAt})
	}
	printTable(w, []string{"ID", "TYPE", "TITLE", "UPDATED"}, rows)
}

// renderProgress renders the read-through observe fallback; the progress
// payload is the Capsule execution.json passed through verbatim (02 §5.2),
// so it is pretty-printed as JSON rather than re-interpreted.
func renderProgress(w io.Writer, p *progressView) {
	fmt.Fprintf(w, "%s — %s\n", p.WorkItemID, p.FreshnessNote)
	if p.HeadSeq != nil {
		fmt.Fprintf(w, "head: seq=%s", strconv.FormatInt(*p.HeadSeq, 10))
		if p.HeadGeneration != nil {
			fmt.Fprintf(w, " generation=%s", strconv.FormatInt(*p.HeadGeneration, 10))
		}
		if p.HeadUpdatedAt != nil {
			fmt.Fprintf(w, " at=%s", *p.HeadUpdatedAt)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "head: (never claimed)")
	}
	if len(p.Progress) > 0 && string(p.Progress) != "null" {
		printJSON(w, p.Progress)
	}
}

// renderDeps renders the row-26 two-list answer.
func renderDeps(w io.Writer, d *depsView) {
	fmt.Fprintln(w, "blocking (this WI blocks):")
	rows := make([][]string, 0, len(d.Blocking))
	for _, e := range d.Blocking {
		rows = append(rows, []string{e.WiID, e.Kind})
	}
	printTable(w, []string{"WI", "KIND"}, rows)
	fmt.Fprintln(w, "\nblocked_by (blocking this WI):")
	rows = rows[:0]
	for _, e := range d.BlockedBy {
		rows = append(rows, []string{e.WiID, e.Kind})
	}
	printTable(w, []string{"WI", "KIND"}, rows)
}

// renderStatus renders the daemon status() bookkeeping list.
func renderStatus(w io.Writer, s *StatusResponse) {
	fmt.Fprintf(w, "machine: %s  mode: %s", s.MachineID, s.Mode)
	if s.ShuttingDown {
		fmt.Fprint(w, "  (shutting down)")
	}
	fmt.Fprintf(w, "  queue: %d pending / %d running / max %d\n\n",
		s.Queue.Pending, s.Queue.Running, s.Queue.MaxConcurrent)
	rows := make([][]string, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		rows = append(rows, []string{
			j.WorkItemID, j.HarnessAdapter, j.HarnessSessionID, strconv.FormatInt(j.RingLatest, 10),
		})
	}
	printTable(w, []string{"WORK ITEM", "ADAPTER", "SESSION", "RING"}, rows)
}

// renderObserve renders the layer-one event stream + snapshot, or the
// daemon-side read-through answer when the WI is not local.
func renderObserve(w io.Writer, resp *ObserveResponse) {
	if resp.ReadThrough {
		// The daemon performed the layer-two Core read-through itself.
		fmt.Fprintf(w, "%s\n", resp.FreshnessNote)
		if resp.HeadSeq != nil {
			fmt.Fprintf(w, "head: seq=%s\n", strconv.FormatInt(*resp.HeadSeq, 10))
		}
		if len(resp.StateSnapshot) > 0 && string(resp.StateSnapshot) != "null" {
			printJSON(w, resp.StateSnapshot)
		}
		return
	}
	if resp.Compacted {
		fmt.Fprintln(w, "(compacted: cursor fell out of the ring buffer; rebuild from the snapshot)")
	}
	if len(resp.Events) > 0 {
		rows := make([][]string, 0, len(resp.Events))
		for _, e := range resp.Events {
			payload := ""
			if len(e.Payload) > 0 && string(e.Payload) != "null" {
				payload = truncate(string(e.Payload), 80)
			}
			rows = append(rows, []string{strconv.FormatInt(e.Seq, 10), e.Type, payload})
		}
		printTable(w, []string{"SEQ", "EVENT", "PAYLOAD"}, rows)
	}
	if len(resp.StateSnapshot) > 0 && string(resp.StateSnapshot) != "null" {
		fmt.Fprintln(w, "state:")
		printJSON(w, resp.StateSnapshot)
	}
	fmt.Fprintf(w, "next_cursor: %s\n", strconv.FormatInt(resp.NextCursor, 10))
}

// renderUsers renders the M1 list.
func renderUsers(w io.Writer, u *userListView) {
	rows := make([][]string, 0, len(u.Items))
	for _, it := range u.Items {
		rows = append(rows, []string{it.ID, it.Kind, it.Role, it.DisplayName})
	}
	printTable(w, []string{"ID", "KIND", "ROLE", "DISPLAY NAME"}, rows)
}

// renderApiKeys renders the M2 list.
func renderApiKeys(w io.Writer, k *apiKeyListView) {
	rows := make([][]string, 0, len(k.Items))
	for _, it := range k.Items {
		revoked := "-"
		if it.RevokedAt != nil {
			revoked = *it.RevokedAt
		}
		rows = append(rows, []string{it.ID, it.UserID, nullable(it.ProjectID), revoked})
	}
	printTable(w, []string{"ID", "USER", "SCOPE", "REVOKED"}, rows)
}

// renderProjects renders the M3 list.
func renderProjects(w io.Writer, p *projectListView) {
	rows := make([][]string, 0, len(p.Items))
	for _, it := range p.Items {
		vis := "private"
		if it.Visible {
			vis = "visible"
		}
		rows = append(rows, []string{it.ID, it.Name, vis})
	}
	printTable(w, []string{"ID", "NAME", "DIRECTORY"}, rows)
}
