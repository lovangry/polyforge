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

// wiViewsOf wraps plain wire records as views with no session fields
// (sources that cannot carry them: the daemon's recommendation body).
func wiViewsOf(items []api.WorkItem) []WIView {
	out := make([]WIView, len(items))
	for i, it := range items {
		out[i] = WIView{WorkItem: it}
	}
	return out
}

// wiRow renders the shared per-WI cells (no segment). The SESSION
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
		slug, it.Status, sess, it.Priority, nullable(it.BundleName), truncate(it.Goal, 60),
	}
}

// renderWITable renders a WI list table.
func renderWITable(w io.Writer, items []WIView) {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, wiRow(it))
	}
	printTable(w, []string{"SLUG", "STATUS", "SESSION", "PRIORITY", "BUNDLE", "GOAL"}, rows)
}

// renderReadyWITable renders the ready-queue table: items first, then
// unclassified, with a SEGMENT column carrying the wire segment key
// ("items" / "unclassified"). Both segments are takeable.
func renderReadyWITable(w io.Writer, items, unclassified []WIView) {
	segments := []struct {
		key string
		wis []WIView
	}{{"items", items}, {"unclassified", unclassified}}
	rows := make([][]string, 0, len(items)+len(unclassified))
	for _, seg := range segments {
		for _, it := range seg.wis {
			rows = append(rows, append(wiRow(it), seg.key))
		}
	}
	printTable(w, []string{"SLUG", "STATUS", "SESSION", "PRIORITY", "BUNDLE", "GOAL", "SEGMENT"}, rows)
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

// renderObserve renders the observe projection (protocol.JobView): the
// two-layer view of current step, the step table, the budget block, the
// pending question and the recent events.
// latencyFromBody defensively parses an event body for the commit_ms /
// push_ms bookkeeping keys published by the daemon (#29). It returns the
// human-readable latency segment (e.g. "commit 1234ms · push 567ms") when
// at least one key is present, and "" when the body carries neither key,
// is empty/null, or is not parseable JSON — the caller then falls back to
// the raw truncated rendering, so a malformed body never fails the render.
func latencyFromBody(body json.RawMessage) string {
	if len(body) == 0 || string(body) == "null" {
		return ""
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return ""
	}
	labels := map[string]string{"commit_ms": "commit", "push_ms": "push"}
	var parts []string
	for _, key := range []string{"commit_ms", "push_ms"} {
		v, ok := fields[key]
		if !ok {
			continue
		}
		n, ok := v.(float64)
		if !ok {
			// Unexpected shape for a latency field: show the raw body
			// instead of a half-parsed figure.
			return ""
		}
		parts = append(parts, labels[key]+" "+strconv.FormatFloat(n, 'f', -1, 64)+"ms")
	}
	return strings.Join(parts, " · ")
}

// wallFromBudget extracts the cumulative wall_ms figure from the raw
// JobView.Budget JSON. Returns 0 when the budget is empty, null, invalid,
// or carries no wall_ms key.
func wallFromBudget(budget json.RawMessage) int64 {
	if len(budget) == 0 || string(budget) == "null" {
		return 0
	}
	var b struct {
		WallMs int64 `json:"wall_ms"`
	}
	if err := json.Unmarshal(budget, &b); err != nil {
		return 0
	}
	return b.WallMs
}

// humanWall renders a wall_ms figure for the JOB header ("3.2h", "45m",
// "12s", "980ms"). Returns "" for non-positive values so the header
// never grows a "0ms" artifact.
func humanWall(ms int64) string {
	if ms <= 0 {
		return ""
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	sec := float64(ms) / 1000
	switch {
	case sec < 60:
		return formatWallFigure(sec) + "s"
	case sec < 3600:
		return formatWallFigure(sec/60) + "m"
	default:
		return formatWallFigure(sec/3600) + "h"
	}
}

// formatWallFigure formats one decimal place, dropping a trailing ".0".
func formatWallFigure(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

func renderObserve(w io.Writer, jv *protocol.JobView, sess *WIView) {
	fmt.Fprintf(w, "%s (%s) — %s", jv.WorkItem, jv.Project, jv.Status)
	if wall := humanWall(wallFromBudget(jv.Budget)); wall != "" {
		fmt.Fprintf(w, " · wall %s", wall)
	}
	fmt.Fprintln(w)
	// The v1.5 ownership face renders the session fingerprint — the
	// PUBLIC handle of the current holder — instead of the deleted
	// lineage's attempt/generation fields. sess is the additive
	// aihub WI read (nil when the daemon layer answered but the hub
	// read was not available: the session line is simply skipped, never
	// fabricated); "-" means the hub answered and nobody holds it.
	if sess != nil {
		fp := sess.SessionFingerprint
		if fp == "" {
			fp = "-"
		}
		fmt.Fprintf(w, "session: %s", fp)
		if sess.SessionFingerprint != "" {
			if sess.SessionMachine != "" || sess.SessionOwner != "" {
				fmt.Fprintf(w, " (machine %s, owner %s)", orDash(sess.SessionMachine), orDash(sess.SessionOwner))
			}
			if sess.CleanPause {
				fmt.Fprint(w, "  clean-pause: yes")
			}
		}
		if jv.BundleName != "" {
			fmt.Fprintf(w, "  bundle: %s", jv.BundleName)
		}
		fmt.Fprintln(w)
	} else if jv.BundleName != "" {
		fmt.Fprintf(w, "bundle: %s\n", jv.BundleName)
	}
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
				if lat := latencyFromBody(e.Body); lat != "" {
					body = lat
				} else {
					body = truncate(string(e.Body), 80)
				}
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

// renderReadThrough renders the §5.2 read-through projection: the
// aihub-side answer of the WI's last published revision, visibly a
// DIFFERENT layer from the daemon's live JobView — labeled
// layer=read-through with the as-of-last-publish freshness note.
// Daemon-layer-only content (live events ring, --after cursor,
// pending-question interactivity) has no read-through equivalent and
// is never fabricated here.
func renderReadThrough(w io.Writer, p *api.ProgressResponse) {
	fmt.Fprintf(w, "%s — layer=read-through (read-only projection of the last published revision)\n", p.WorkItemID)
	fmt.Fprintf(w, "head: seq %d  generation %d  updated %s\n",
		p.HeadSeq, p.HeadGeneration, p.HeadUpdatedAt)
	fmt.Fprintf(w, "freshness: %s\n", p.FreshnessNote)
	fmt.Fprintln(w, "\nprogress:")
	printJSON(w, p.Progress)
}

// renderStatus renders the daemon status() bookkeeping list. The
// QUESTION column is the D6 owner face: any job the daemon reports with a
// pending question shows its question id ("-" otherwise), so an agent
// blocked on a human is impossible to miss at a glance. The SESSION
// column is the v1.5 public fingerprint of the job's session (the
// daemon populates it at merge; "-" until then or when unheld). The
// LAST-PROGRESS column is the F15c owner face: mm:ss since the job's
// stream last proved agent progress ("-" when no progress record
// exists). UI-noise records never refresh it, so a wedged-but-chattering
// job reads as stale — the pilot's 2.7-hour double-blind wedge had no
// such surface.
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
		sess := "-"
		if j.SessionFingerprint != "" {
			sess = j.SessionFingerprint
		}
		progress := "-"
		if j.LastProgressAgoSec != nil {
			progress = mmssAgo(*j.LastProgressAgoSec)
		}
		rows = append(rows, []string{j.WorkItemID, j.Status, sess, pid, question, progress})
	}
	printTable(w, []string{"WORK ITEM", "STATUS", "SESSION", "AGENT PID", "QUESTION", "LAST-PROGRESS"}, rows)
}

// mmssAgo renders whole seconds as the mm:ss staleness figure the
// LAST-PROGRESS column carries (minutes grow unbounded past an hour —
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
