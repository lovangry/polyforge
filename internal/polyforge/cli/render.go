package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Rendering rules:
//   - --json: emit the server's response body as-is (raw passthrough).
//   - human (default): per-verb renderers below; anything without a
//     dedicated renderer falls back to pretty-printed JSON.
//   - errors: "pf: CORE-409: detail" with the code prominent; in --json
//     mode the typed envelope is re-emitted instead.

// emit prints the command payload: raw passthrough in --json mode ONLY.
func (ctx *Context) emit(raw []byte) {
	if !ctx.JSON {
		return
	}
	if len(raw) > 0 {
		ctx.Out(raw)
	}
}

// finish prints the command payload: raw passthrough in --json mode, the
// human renderer closure otherwise.
func (ctx *Context) finish(raw json.RawMessage, human func()) {
	ctx.emit(raw)
	if !ctx.JSON {
		human()
	}
}

// Out writes to stdout.
func (ctx *Context) Out(data []byte) {
	ctx.Stdout.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(ctx.Stdout)
	}
}

// printJSON pretty-prints raw JSON for the human fallback.
func printJSON(w io.Writer, raw []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		fmt.Fprintln(w, string(raw))
		return
	}
	buf.WriteTo(w)
	fmt.Fprintln(w)
}

// printTable renders a simple aligned text table. The last column is
// not padded (no trailing whitespace on output lines).
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
		if i == len(headers)-1 {
			head[i] = h // last column: no padding (no trailing whitespace)
		} else {
			head[i] = pad(h, widths[i])
		}
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
			if i == len(widths)-1 {
				cells[i] = cell
			} else {
				cells[i] = pad(cell, widths[i])
			}
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(cells, "  "), " "))
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

// nullable renders a *string for tables.
func nullable(v *string) string {
	if v == nil {
		return "-"
	}
	return *v
}

// ---------------------------------------------------------------------------
// Human renderers.
// ---------------------------------------------------------------------------

// renderWITable renders a WI list table.
func renderWITable(w io.Writer, items []api.WorkItem) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		slug := it.Slug
		if slug == "" {
			slug = it.ID
		}
		rows = append(rows, []string{
			slug, it.Status, it.Priority, nullable(it.BundleName), truncate(it.Goal, 60),
		})
	}
	printTable(w, []string{"SLUG", "STATUS", "PRIORITY", "BUNDLE", "GOAL"}, rows)
}

// renderWIDetail renders the detail record.
func renderWIDetail(w io.Writer, wi *api.WorkItem) {
	name := wi.Slug
	if name == "" {
		name = wi.ID
	}
	fmt.Fprintf(w, "%s (%s)\n", name, wi.ID)
	fmt.Fprintf(w, "  status:   %s (priority %s)\n", wi.Status, wi.Priority)
	fmt.Fprintf(w, "  project:  %s\n", wi.ProjectName)
	fmt.Fprintf(w, "  goal:     %s\n", wi.Goal)
	if wi.Milestone != nil {
		fmt.Fprintf(w, "  milestone: %s\n", *wi.Milestone)
	}
	if len(wi.Labels) > 0 {
		fmt.Fprintf(w, "  labels:   %s\n", strings.Join(wi.Labels, ","))
	}
	fmt.Fprintf(w, "  bundle:   %s\n", nullable(wi.BundleName))
	if wi.Assignee != nil {
		fmt.Fprintf(w, "  assignee: %s\n", *wi.Assignee)
	}
	if wi.WaitingOn != nil {
		fmt.Fprintf(w, "  waiting:  %s\n", *wi.WaitingOn)
	}
	fmt.Fprintf(w, "  created:  %s\n", wi.CreatedAt)
	if wi.ClosedAt != nil {
		fmt.Fprintf(w, "  closed:   %s\n", *wi.ClosedAt)
	}
}

// renderMemories renders a memory search result.
func renderMemories(w io.Writer, items []api.Memory) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		tags := "-"
		if len(it.Tags) > 0 {
			tags = strings.Join(it.Tags, ",")
		}
		rows = append(rows, []string{it.ID, it.Type, truncate(firstLine(it.Content), 50), it.UpdatedAt, tags})
	}
	printTable(w, []string{"ID", "TYPE", "CONTENT", "UPDATED", "TAGS"}, rows)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// renderObserve renders the observe projection (protocol.JobView): the
// two-layer view of current step, the step table, the budget block, the
// pending question and the recent events.
func renderObserve(w io.Writer, jv *protocol.JobView) {
	fmt.Fprintf(w, "%s (%s) — %s\n", jv.WorkItem, jv.Project, jv.Status)
	fmt.Fprintf(w, "attempt: %s  generation: %s", jv.AttemptID, strconv.FormatInt(jv.Generation, 10))
	if jv.BundleName != "" {
		fmt.Fprintf(w, "  bundle: %s", jv.BundleName)
	}
	fmt.Fprintln(w)
	if jv.CurrentStep != "" {
		fmt.Fprintf(w, "current step: %s\n", jv.CurrentStep)
	}
	if len(jv.Steps) > 0 {
		fmt.Fprintln(w, "\nsteps:")
		names := make([]string, 0, len(jv.Steps))
		for name := range jv.Steps {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([][]string, 0, len(names))
		for _, name := range names {
			s := jv.Steps[name]
			if s == nil {
				continue
			}
			rows = append(rows, []string{
				name, s.Status, strconv.Itoa(s.Attempts), s.Model, s.StartedAt,
			})
		}
		printTable(w, []string{"STEP", "STATUS", "ATTEMPTS", "MODEL", "STARTED"}, rows)
	}
	if len(jv.Budget) > 0 && string(jv.Budget) != "null" {
		fmt.Fprintln(w, "\nbudget:")
		printJSON(w, jv.Budget)
	}
	if jv.Pending != nil {
		fmt.Fprintf(w, "\npending question:\n  %s [%s] asked at %s\n  %s\n",
			jv.Pending.ID, jv.Pending.Step, jv.Pending.AskedAt, jv.Pending.Question)
	}
	if len(jv.Events) > 0 {
		fmt.Fprintln(w, "\nevents:")
		rows := make([][]string, 0, len(jv.Events))
		for _, e := range jv.Events {
			body := ""
			if len(e.Body) > 0 && string(e.Body) != "null" {
				body = truncate(string(e.Body), 80)
			}
			rows = append(rows, []string{e.At, e.Kind, body})
		}
		printTable(w, []string{"AT", "KIND", "BODY"}, rows)
		// The next-cursor hint: the last served event's seq is the cursor a
		// follow-up `pf observe --after` passes to see only what lands after
		// this view. Served entries always carry seq; the guard keeps the
		// renderer quiet against a hand-built view without one.
		if last := jv.Events[len(jv.Events)-1]; last.Seq > 0 {
			fmt.Fprintf(w, "next cursor: pf observe %s --after %d\n", jv.WorkItem, last.Seq)
		}
	}
}

// renderStatus renders the daemon status() bookkeeping list.
// renderStatus renders the daemon status() bookkeeping list. The
// QUESTION column is the D6 owner face: any job the daemon reports with a
// pending question shows its question id ("-" otherwise), so an agent
// blocked on a human is impossible to miss at a glance.
func renderStatus(w io.Writer, s *StatusResponse) {
	fmt.Fprintf(w, "machine: %s  mode: %s", s.MachineID, s.Mode)
	if s.ShuttingDown {
		fmt.Fprint(w, "  (shutting down)")
	}
	fmt.Fprintf(w, "  queue: %d pending / %d running / max %d\n\n",
		s.Queue.Pending, s.Queue.Running, s.Queue.MaxConcurrent)
	rows := make([][]string, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		pid := "-"
		if j.AgentPID > 0 {
			pid = strconv.Itoa(j.AgentPID)
		}
		question := "-"
		if j.QuestionID != "" {
			question = j.QuestionID
		}
		rows = append(rows, []string{j.WorkItemID, j.Status, j.AttemptID, pid, question})
	}
	printTable(w, []string{"WORK ITEM", "STATUS", "ATTEMPT", "AGENT PID", "QUESTION"}, rows)
}

// renderShip renders the ship answer: the fixed-name branch plus one
// line per inventory repo, each with the PR URL or the compare-URL
// fallback.
func renderShip(w io.Writer, s *ShipResponse) {
	fmt.Fprintf(w, "shipped %s → branch %s\n", s.WorkItemID, s.Branch)
	if len(s.Repos) == 0 {
		fmt.Fprintln(w, "  (no inventory repos in the sealed capsule: nothing to push)")
		return
	}
	for _, r := range s.Repos {
		verdict := "pushed"
		if r.Reused {
			verdict = "reused (remote branch already at the sealed commit)"
		}
		fmt.Fprintf(w, "  %-18s %s  %s\n", r.Name+":", r.Head, verdict)
		if r.PRURL != "" {
			fmt.Fprintf(w, "    pr:      %s\n", r.PRURL)
		}
		if r.CompareURL != "" {
			fmt.Fprintf(w, "    compare: %s\n", r.CompareURL)
		}
	}
}

// renderUsers renders the M1 list.
func renderUsers(w io.Writer, items []api.User) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{it.ID, it.UserType, it.Role, it.DisplayName})
	}
	printTable(w, []string{"ID", "TYPE", "ROLE", "DISPLAY NAME"}, rows)
}

// renderApiKeys renders the M2 list (metadata only, never material).
func renderApiKeys(w io.Writer, raw json.RawMessage) {
	var l struct {
		Items []struct {
			ID        string  `json:"id"`
			UserID    string  `json:"user_id"`
			ProjectID *string `json:"project_id"`
			RevokedAt *string `json:"revoked_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		printJSON(w, raw)
		return
	}
	rows := make([][]string, 0, len(l.Items))
	for _, it := range l.Items {
		rows = append(rows, []string{it.ID, it.UserID, nullable(it.ProjectID), nullable(it.RevokedAt)})
	}
	printTable(w, []string{"ID", "USER", "SCOPE", "REVOKED"}, rows)
}

// renderProjects renders the M3 list.
func renderProjects(w io.Writer, items []api.Project) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		vis := "private"
		if it.Visible {
			vis = "visible"
		}
		rows = append(rows, []string{it.ID, it.Name, vis})
	}
	printTable(w, []string{"ID", "NAME", "DIRECTORY"}, rows)
}
