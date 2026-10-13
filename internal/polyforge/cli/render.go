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
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
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

// WIView is one rendered WI record: the wire work item plus the v1.5
// session audit fields, decoded ADDITIVELY from the raw server body
// (api.WorkItem does not carry them until Lane A's server lands; the
// additive decode means the CLI renders them the moment they appear,
// and "-" before that). Rendering uses the session fingerprint — the
// PUBLIC handle — never the secret or its hash.
type WIView struct {
	api.WorkItem
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
	SessionMachine     string `json:"session_machine,omitempty"`
	SessionOwner       string `json:"session_owner,omitempty"`
	SessionStartedAt   string `json:"session_started_at,omitempty"`
	CleanPause         bool   `json:"clean_pause,omitempty"`
}

// wiRow renders the shared per-WI cells (no segment). The PROJECT
// column is the #113 merged-presentation requirement: without it a
// multi-project list could not tell a row's project apart. The SESSION
// column is the v1.5 public fingerprint of the current holder ("-"
// when unheld).
func wiRow(it WIView) []string {
	slug := it.Slug
	if slug == "" {
		slug = it.ID
	}
	sess := "-"
	if it.SessionFingerprint != "" {
		sess = it.SessionFingerprint
	}
	return []string{
		slug, orDash(it.ProjectName), it.Status, sess, it.Priority, nullable(it.BundleName), truncate(it.Goal, 60),
	}
}

// renderWITable renders a WI list table.
func renderWITable(w io.Writer, items []WIView) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, wiRow(it))
	}
	printTable(w, []string{"SLUG", "PROJECT", "STATUS", "SESSION", "PRIORITY", "BUNDLE", "GOAL"}, rows)
}

// renderWIDetail renders the detail record. The v1.5 session block
// (fingerprint, machine, owner, started-at, clean_pause — the m4 public
// audit fields) renders whenever the server body carries them.
func renderWIDetail(w io.Writer, wi *WIView) {
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
	if wi.SessionFingerprint != "" {
		fmt.Fprintf(w, "  session:  %s", wi.SessionFingerprint)
		if wi.SessionMachine != "" || wi.SessionOwner != "" {
			fmt.Fprintf(w, " (machine %s, owner %s)", orDash(wi.SessionMachine), orDash(wi.SessionOwner))
		}
		if wi.SessionStartedAt != "" {
			fmt.Fprintf(w, " since %s", wi.SessionStartedAt)
		}
		fmt.Fprintln(w)
	}
	if wi.CleanPause {
		fmt.Fprintf(w, "  clean-pause: yes (operator pause; restart will not auto-resume)\n")
	}
	fmt.Fprintf(w, "  created:  %s\n", wi.CreatedAt)
	if wi.ClosedAt != nil {
		fmt.Fprintf(w, "  closed:   %s\n", *wi.ClosedAt)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

// renderStatus renders the daemon status() bookkeeping list. The
// WORKSPACE line is the #103 owner face: the active capsule base the
// daemon was launched with (-workspace; capsules live at
// <workspace>/capsules/<wi_id>) — exactly one line, no annotation. The
// QUESTION column is the D6 owner face: any job the daemon reports with a
// pending question shows its question id ("-" otherwise), so an agent
// blocked on a human is impossible to miss at a glance. The SPAWN column
// is the v1.5 B2 derivation counter and SESSION the public fingerprint of
// the job's session ("-" when unheld). STEP-AGE and LAST-WORK are the
// owner liveness face: mm:ss since the current dispatch began, and since
// its harness last did REAL work ("-" when no dispatch is in flight or
// no work signal exists yet). UI-noise records never refresh LAST-WORK,
// so a wedged-but-chattering job reads as stale — the pilot's 2.7-hour
// double-blind wedge had no such surface.
func renderStatus(w io.Writer, s *StatusResponse) {
	fmt.Fprintf(w, "daemon: %s", s.MachineID)
	if s.DisplayName != "" {
		fmt.Fprintf(w, " (%s)", s.DisplayName)
	}
	fmt.Fprintf(w, "  auto: %s  max: %d", onOff(s.Auto), s.Queue.MaxConcurrent)
	if s.ShuttingDown {
		fmt.Fprint(w, "  (shutting down)")
	}
	fmt.Fprintf(w, "  queue: %d pending / %d running\n",
		s.Queue.Pending, s.Queue.Running)
	// polyforge#113: the workspace SET, sorted for stable display — a
	// multi-workspace daemon must not present one workspace as if it
	// were the only one.
	wsSet := append([]string(nil), s.Workspaces...)
	sort.Strings(wsSet)
	fmt.Fprintf(w, "workspaces: %s\n\n", strings.Join(wsSet, ", "))
	showOrigin := false
	for _, j := range s.Jobs {
		showOrigin = showOrigin || j.Origin != nil
	}
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
		sess := "-"
		if j.SessionFingerprint != "" {
			sess = j.SessionFingerprint
		}
		spawn := "-"
		if j.SpawnN > 0 {
			spawn = "#" + strconv.Itoa(j.SpawnN)
		}
		stepAge, lastWork := "-", "-"
		if j.StepAgeSec != nil {
			stepAge = mmssAgo(*j.StepAgeSec)
		}
		if j.LastWorkAgoSec != nil {
			lastWork = mmssAgo(*j.LastWorkAgoSec)
		}
		origin := "-"
		if j.Origin != nil {
			origin = j.Origin.Kind
			if j.Origin.ChatID != "" {
				origin += ":" + j.Origin.ChatID
			}
		}
		row := []string{j.WorkItemID, j.Status, sess, spawn, pid, question, stepAge, lastWork}
		if showOrigin {
			row = append(row, origin)
		}
		rows = append(rows, row)
	}
	head := []string{"WORK ITEM", "STATUS", "SESSION", "SPAWN", "AGENT PID", "QUESTION", "STEP-AGE", "LAST-WORK"}
	if showOrigin { // the column appears only when some job has one
		head = append(head, "ORIGIN")
	}
	printTable(w, head, rows)
}

// mmssAgo renders whole seconds as the mm:ss figure the STEP-AGE and
// LAST-WORK columns carry (minutes grow unbounded past an hour —
// 97:30 reads faster than 1h37m on a scan).
func mmssAgo(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%02d:%02d", sec/60, sec%60)
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

// renderMachines renders the daemon liveness list (M3, admin read
// face): RUNTIME is the daemon's reported go version ("-" when it has
// none), LIVE WI the bare count of live work item ids, and STATE the
// server-computed stale projection (STALE vs alive). The aihub-side
// registration keeps its machine vocabulary; the user-facing wording
// names daemons.
func renderMachines(w io.Writer, list *aihubclient.MachinesList) {
	fmt.Fprintf(w, "daemons: %d (stale threshold %ds)\n", list.Count, list.StaleAfterSeconds)
	rows := make([][]string, 0, len(list.Machines))
	for _, m := range list.Machines {
		age := "?"
		if m.HeartbeatAgeSeconds != nil {
			age = fmt.Sprintf("%ds", *m.HeartbeatAgeSeconds)
		}
		rows = append(rows, []string{
			m.ID, m.Mode, orDash(m.RuntimeVersion), strconv.Itoa(len(m.LiveWIIDs)),
			age, m.LastHeartbeatAt,
			map[bool]string{true: "STALE", false: "alive"}[m.Stale],
		})
	}
	printTable(w, []string{"ID", "MODE", "RUNTIME", "LIVE WI", "HEARTBEAT AGE", "LAST HEARTBEAT", "STATE"}, rows)
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
