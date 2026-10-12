package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// ---------------------------------------------------------------------------
// file — create/edit/show work items (direct to aihub).
// ---------------------------------------------------------------------------

var cmdFile = &Command{
	Name:    "file",
	Group:   GroupData,
	Summary: "Create, edit or show a work item",
	Usage: "pf file create --project P --goal G [flags] | --stdin\n" +
		"pf file edit <id> [flags] | --stdin\n" +
		"pf file show <id>",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("file: expected create|edit|show (see `pf help file`)")
		}
		sub, rest := args[0], args[1:]
		switch sub {
		case "create":
			return fileCreate(ctx, rest)
		case "edit":
			return fileEdit(ctx, rest)
		case "show":
			return fileShow(ctx, rest)
		default:
			return fmt.Errorf("file: unknown action %q", sub)
		}
	},
}

// osGetwd is the file-create inference's cwd seam: the production value
// is the process working directory; tests pin it to a fixture path.
var osGetwd = os.Getwd

// requiredProjectFlagError is the polyforge#113 required-flag error,
// byte-identical wherever inference cannot answer (no containing
// workspace, no yaml). It never mutates.
const requiredProjectFlagError = "file create: --project is required — there is no default project (polyforge#113); workspaces and their projects are declared in [workspace.\"<path>\"] sections of cli.toml (`pf doctor` checks the mapping)"

// inferProjectFromCwd is the polyforge#79 file-create default: the
// process cwd resolved against the expanded [workspace] mapping by
// containment (cwd equal to or inside the workspace root). Exactly one
// containing workspace whose yaml lists exactly ONE project answers it —
// the create proceeds as if --project had been passed. Every other shape
// asks for --project or surfaces the typed yaml error; inference never
// silently degrades and never ambient-picks (the #113 principle).
func inferProjectFromCwd(ctx *Context) (string, error) {
	cwd, err := osGetwd()
	if err != nil || ctx.Cfg.Mapping == nil || ctx.Cfg.Mapping.Empty() {
		return "", fmt.Errorf(requiredProjectFlagError)
	}
	var containing []string
	for _, ws := range ctx.Cfg.Mapping.Workspaces() {
		if cwd == ws || strings.HasPrefix(cwd, ws+string(os.PathSeparator)) {
			containing = append(containing, ws)
		}
	}
	switch {
	case len(containing) == 0:
		return "", fmt.Errorf(requiredProjectFlagError)
	case len(containing) > 1:
		return "", fmt.Errorf("file create: --project is required — the working directory is inside more than one mapped workspace (%s); pass --project explicitly (placement stays unambiguous, polyforge#113)", strings.Join(containing, ", "))
	}
	wcfg, err := workspace.Load(containing[0])
	if err != nil {
		return "", fmt.Errorf("file create: %v", err)
	}
	if wcfg == nil || len(wcfg.Projects) == 0 {
		// No yaml (absent-equivalent) or a workflow-only yaml: no project
		// inventory to infer from — today's required-flag error stands.
		return "", fmt.Errorf(requiredProjectFlagError)
	}
	if len(wcfg.Projects) > 1 {
		names := make([]string, 0, len(wcfg.Projects))
		for name := range wcfg.Projects {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", fmt.Errorf("file create: --project is required — %s lists %d projects (%s); pass --project explicitly (no default is invented)", workspace.PathFor(containing[0]), len(names), strings.Join(names, ", "))
	}
	for name := range wcfg.Projects {
		return name, nil
	}
	return "", fmt.Errorf(requiredProjectFlagError) // unreachable
}

// requireMappedProject validates a placement-adjacent verb's --project
// against the loaded mapping (polyforge#113): placement needs a home on
// this machine. An unmapped (or unvalidatable — no mapping configured)
// project is a loud naming error pointing at the [workspace] shape and
// `pf doctor`; it never silently proceeds.
func requireMappedProject(ctx *Context, verb, project string) error {
	if ctx.Cfg.Mapping.Empty() {
		return fmt.Errorf("%s: project %q cannot be placed: no [workspace] mapping is configured in cli.toml — add a [workspace.\"<path>\"] section (`pf doctor` checks the mapping)", verb, project)
	}
	if _, err := ctx.Cfg.Mapping.WorkspaceFor(project); err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	return nil
}

// fileCreate builds the create body from flags or from full stdin JSON.
func fileCreate(ctx *Context, args []string) error {
	f := subFlags(ctx, "file create")
	var (
		project   = f.String("project", "", "project name (required; validated against the [workspace] mapping)")
		goal      = f.String("goal", "", "one-line goal (<=500 chars)")
		content   optString
		labels    stringList
		priority  = f.String("priority", "", "low|normal|high|urgent")
		milestone optString
		bundle    = f.String("bundle", "", "bundle_name: non-empty makes an execution ticket")
		parent    = f.String("parent", "", "parent work item id")
		attrs     kvList
		useStdin  = f.Bool("stdin", false, "read the full create-body JSON from stdin")
	)
	f.Var(&content, "content", "markdown background; '-' reads stdin, '@file' reads a file")
	f.Var(&labels, "label", "label (repeatable)")
	f.Var(&milestone, "milestone", "milestone name")
	f.Var(&attrs, "attr", "business attr key=json-value (repeatable)")
	rest, err := parseCommandArgs(f, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("file create: unexpected arguments %v (file create takes flags only)", rest)
	}

	var req aihubclient.CreateWorkItemRequest
	if *useStdin {
		raw, err := io.ReadAll(ctx.Stdin)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("stdin JSON: %w", err)
		}
	}
	if *project != "" {
		req.Project = *project // flag fills a gap in the stdin body
	}
	if *goal != "" {
		req.Goal = *goal
	}
	if req.Project == "" {
		// polyforge#79 inference: the cwd → workspace → yaml default. It
		// answers a project, the #113 required-flag error (verbatim), or
		// the typed yaml/ambiguity error — never a silent skip.
		proj, err := inferProjectFromCwd(ctx)
		if err != nil {
			return err
		}
		req.Project = proj
	}
	if err := requireMappedProject(ctx, "file create", req.Project); err != nil {
		return err
	}
	if req.Goal == "" {
		return fmt.Errorf("file create: --goal is required (or use --stdin with a full JSON body)")
	}
	if content.IsSet() {
		s, err := readValueOrStdin(ctx, content.Val())
		if err != nil {
			return err
		}
		req.Content = &s
	}
	if len(labels) > 0 {
		req.Labels = labels
	}
	if *priority != "" {
		req.Priority = *priority
	}
	if milestone.IsSet() {
		req.Milestone = milestone.Ptr()
	}
	if *bundle != "" {
		req.BundleName = strPtr(*bundle)
	}
	if *parent != "" {
		req.ParentID = strPtr(*parent)
	}
	if len(attrs) > 0 {
		req.Attrs = map[string]any{}
		for _, kv := range attrs {
			v, err := parseAttrValue(kv.Value)
			if err != nil {
				return fmt.Errorf("--attr %s: %w", kv.Key, err)
			}
			req.Attrs[kv.Key] = v
		}
	}

	raw, err := ctx.aihub().CreateWorkItem(req)
	if err != nil {
		return err
	}
	ctx.finish(raw, func() {
		var wi WIView
		if err := json.Unmarshal(raw, &wi); err == nil {
			fmt.Fprintln(ctx.Stdout)
			renderWIDetail(ctx.Stdout, &wi)
		}
	})
	return nil
}

// fileEdit issues a PATCH with only the flags actually provided.
func fileEdit(ctx *Context, args []string) error {
	f := subFlags(ctx, "file edit")
	var (
		goal      optString
		content   optString
		labels    stringList
		priority  optString
		milestone optString
		waitingOn optString
		attrs     kvList
		useStdin  = f.Bool("stdin", false, "read the full PATCH-body JSON from stdin")
	)
	f.Var(&goal, "goal", "new goal")
	f.Var(&content, "content", "new markdown background; '-' reads stdin, '@file' reads a file")
	f.Var(&labels, "label", "labels (repeatable; replaces the whole set)")
	f.Var(&priority, "priority", "low|normal|high|urgent")
	f.Var(&milestone, "milestone", "milestone (empty string clears it)")
	f.Var(&waitingOn, "waiting-on", "waiting_on value")
	f.Var(&attrs, "attr", "business attr key=json-value (repeatable)")
	rest, err := parseCommandArgs(f, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 && !*useStdin {
		return fmt.Errorf("file edit: expected exactly one work item id")
	}
	var id string
	if len(rest) == 1 {
		id = rest[0]
	}

	body := map[string]any{}
	if *useStdin {
		raw, err := io.ReadAll(ctx.Stdin)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("stdin JSON: %w", err)
		}
		if v, ok := body["id"].(string); ok && id == "" {
			id = v
		}
		delete(body, "id")
	}
	if id == "" {
		return fmt.Errorf("file edit: work item id required")
	}
	if goal.IsSet() {
		body["goal"] = goal.Val()
	}
	if content.IsSet() {
		s, err := readValueOrStdin(ctx, content.Val())
		if err != nil {
			return err
		}
		body["content"] = s
	}
	if len(labels) > 0 {
		body["labels"] = []string(labels)
	}
	if priority.IsSet() {
		body["priority"] = priority.Val()
	}
	if milestone.IsSet() {
		body["milestone"] = milestone.Ptr()
	}
	if waitingOn.IsSet() {
		body["waiting_on"] = waitingOn.Ptr()
	}
	if len(attrs) > 0 {
		m := map[string]any{}
		for _, kv := range attrs {
			v, err := parseAttrValue(kv.Value)
			if err != nil {
				return fmt.Errorf("--attr %s: %w", kv.Key, err)
			}
			m[kv.Key] = v
		}
		body["attrs"] = m
	}

	raw, err := ctx.aihub().PatchWorkItem(id, body)
	if err != nil {
		return err
	}
	ctx.finish(raw, func() {
		var wi WIView
		if err := json.Unmarshal(raw, &wi); err == nil {
			fmt.Fprintln(ctx.Stdout)
			renderWIDetail(ctx.Stdout, &wi)
		}
	})
	return nil
}

// fileShow reads the detail record.
func fileShow(ctx *Context, args []string) error {
	f := subFlags(ctx, "file show")
	rest, err := parseCommandArgs(f, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("file show: expected exactly one work item id")
	}
	raw, err := ctx.aihub().GetWorkItemRaw(rest[0])
	if err != nil {
		return err
	}
	ctx.finish(raw, func() {
		var wi WIView
		if err := json.Unmarshal(raw, &wi); err == nil {
			fmt.Fprintln(ctx.Stdout)
			renderWIDetail(ctx.Stdout, &wi)
		}
	})
	return nil
}

// readValueOrStdin resolves a value flag: "-" reads all stdin, "@path"
// reads a file, anything else is the literal value.
func readValueOrStdin(ctx *Context, v string) (string, error) {
	switch {
	case v == "-":
		b, err := io.ReadAll(ctx.Stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case strings.HasPrefix(v, "@"):
		b, err := os.ReadFile(strings.TrimPrefix(v, "@"))
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return v, nil
	}
}

// parseAttrValue decodes an --attr flag value: JSON when it parses, plain
// string otherwise.
func parseAttrValue(v string) (any, error) {
	var out any
	if err := json.Unmarshal([]byte(v), &out); err == nil {
		return out, nil
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// queue — WI list + ready-queue (direct to aihub).
// ---------------------------------------------------------------------------

var cmdQueue = &Command{
	Name:    "queue",
	Group:   GroupData,
	Summary: "List work items / show the ready queue",
	Usage: "pf queue [--project P] [--status S] [--limit N]\n" +
		"pf queue --ready [--project P]\n\n" +
		"Without --project: one merged list over ALL mapped projects (the\n" +
		"[workspace] mapping in cli.toml); --project P is passed to aihub\n" +
		"unvalidated (read-only: the mapping governs placement, not knowledge).",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name (optional: absent = all mapped projects, merged; present = passed to aihub unvalidated)")
		fs.Bool("ready", false, "show the ready queue instead of the list")
		fs.String("status", "", "filter: queued|running|paused|blocked|wrapped|failed|canceled")
		fs.Int("limit", 0, "cap the list at N items (1..200; default list mode only — "+
			"cannot be combined with --ready; omitted = the server's default cap)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("queue: unexpected arguments %v", args)
		}
		// Local flag validation, ahead of any config resolution or wire
		// call (same discipline as recall's --limit): a typo fails loudly
		// here and no request is sent. Unlike recall there is no CLI-side
		// default — an omitted --limit stays 0, so ?limit= is left off
		// the wire and the server's own default cap applies. An explicit
		// out-of-range value (including 0) is still refused loudly: Visit
		// tells "user typed it" apart from "flag not passed".
		limit := intFlag(fs, "limit")
		var limitSet bool
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "limit" {
				limitSet = true
			}
		})
		if limitSet {
			// The combination restriction is structural, so it is reported
			// ahead of the value range.
			if boolFlag(fs, "ready") {
				return fmt.Errorf("queue: --limit cannot be combined with --ready " +
					"(it caps the default list mode only)")
			}
			if limit < 1 || limit > 200 {
				return fmt.Errorf("queue: --limit must be 1..200")
			}
		}
		proj := stringFlag(fs, "project")
		if proj == "" {
			// polyforge#113: no ambient default project exists. Without
			// --project the verb covers ALL mapped projects in one merged
			// presentation; with no mapping at all it fails loudly with
			// guidance — it never silently lists nothing.
			if ctx.Cfg.Mapping.Empty() {
				return fmt.Errorf("queue: --project is required — no [workspace] mapping is configured in cli.toml; add a [workspace.\"<path>\"] section (`pf doctor` checks the mapping) or pass --project P")
			}
			return queueMerged(ctx, boolFlag(fs, "ready"), stringFlag(fs, "status"), limit)
		}
		// An explicit --project passes through to aihub UNVALIDATED
		// (mapped or not — exactly one query; the mapping governs
		// placement, not knowledge).
		var (
			raw json.RawMessage
			err error
		)
		if boolFlag(fs, "ready") {
			raw, err = ctx.aihub().ReadyQueueRaw(proj)
		} else {
			raw, err = ctx.aihub().ListWorkItemsRawLimit(proj, stringFlag(fs, "status"), limit)
		}
		if err != nil {
			return err
		}
		if boolFlag(fs, "ready") {
			// Ready queue: the takeable set is items + unclassified (the same
			// union aihubclient.Queue() implements). --json stays raw
			// passthrough of the full server body (all 7 sections).
			ctx.finish(raw, func() {
				var l struct {
					Items        []WIView `json:"items"`
					Unclassified []WIView `json:"unclassified"`
				}
				if err := json.Unmarshal(raw, &l); err == nil {
					fmt.Fprintf(ctx.Stdout, "%d work item(s)\n", len(l.Items)+len(l.Unclassified))
					renderReadyWITable(ctx.Stdout, l.Items, l.Unclassified)
				}
			})
			return nil
		}
		ctx.finish(raw, func() {
			var l struct {
				Items []WIView `json:"items"`
			}
			if err := json.Unmarshal(raw, &l); err == nil {
				fmt.Fprintf(ctx.Stdout, "%d work item(s)\n", len(l.Items))
				renderWITable(ctx.Stdout, l.Items)
			}
		})
		return nil
	},
}

// queueMerged is queue without --project over a non-empty mapping: one
// merged presentation of every mapped project's list (or ready queue),
// each row's project identifiable (PROJECT column; --json emits ONE
// merged items array — records already carry their project server-side).
// --limit applies per-project (each query is one server call); a
// per-project fetch failure fails the verb naming the project — fake
// coverage is worse than none. Determinism: mapping declaration order,
// then the server's own order within each project.
func queueMerged(ctx *Context, ready bool, status string, limit int) error {
	projects := ctx.Cfg.Mapping.Projects()
	var items, unclassified []WIView
	total := 0
	for _, p := range projects {
		raw, err := ctx.aihub().ListWorkItemsRawLimit(p, status, limit)
		if ready {
			raw, err = ctx.aihub().ReadyQueueRaw(p)
		}
		if err != nil {
			return fmt.Errorf("queue: project %s: %w", p, err)
		}
		var l struct {
			Items        []WIView `json:"items"`
			Unclassified []WIView `json:"unclassified"`
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			return fmt.Errorf("queue: project %s: %w", p, err)
		}
		items = append(items, l.Items...)
		unclassified = append(unclassified, l.Unclassified...)
		total += len(l.Items) + len(l.Unclassified)
	}
	if ctx.JSON {
		// ONE merged document: items (plus unclassified in ready mode).
		doc := struct {
			Items        []WIView `json:"items"`
			Unclassified []WIView `json:"unclassified,omitempty"`
			Total        int      `json:"total"`
		}{Items: items, Total: total}
		if ready {
			doc.Unclassified = unclassified
		}
		if b, err := json.Marshal(doc); err == nil {
			ctx.Out(b)
		}
		return nil
	}
	fmt.Fprintf(ctx.Stdout, "%d work item(s)\n", total)
	if ready {
		renderReadyWITable(ctx.Stdout, items, unclassified)
		return nil
	}
	renderWITable(ctx.Stdout, items)
	return nil
}

// ---------------------------------------------------------------------------
// cancel — terminate a non-active WI (direct to aihub).
// ---------------------------------------------------------------------------

var cmdCancel = &Command{
	Name:    "cancel",
	Group:   GroupData,
	Summary: "Cancel (abandon) a queued/paused/blocked work item (no live session)",
	Usage:   "pf cancel <id>",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("cancel: expected exactly one work item id")
		}
		raw, err := ctx.aihub().CancelWorkItem(args[0])
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var wi api.WorkItem
			if err := json.Unmarshal(raw, &wi); err == nil {
				fmt.Fprintf(ctx.Stdout, "cancelled %s (status %s)\n", wi.Slug, wi.Status)
			}
		})
		return nil
	},
}

// ---------------------------------------------------------------------------
// recall — memory retrieval (direct to the aihub memory face).
// ---------------------------------------------------------------------------

var cmdRecall = &Command{
	Name:    "recall",
	Group:   GroupData,
	Summary: "Search project memories",
	Usage:   "pf recall --project P <query> [--type T] [--tags a,b] [--limit N]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name (required; validated against the [workspace] mapping)")
		fs.String("q", "", "query text (alternative to the positional argument)")
		fs.String("type", "", "memory type filter: exact (experience.debug), prefix (experience.*), or * (all types)")
		fs.String("tags", "", "comma-separated tags")
		fs.Int("limit", 20, "max results (1..200; default 20)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		q := stringFlag(fs, "q")
		if q == "" {
			q = strings.Join(args, " ")
		}
		if strings.TrimSpace(q) == "" {
			return fmt.Errorf("recall: a query is required (positional or --q)")
		}
		// Local flag validation, ahead of any config resolution or wire
		// call: a typo fails loudly here and no request is sent. The
		// default-20 limit is the CLI's product choice; the endpoint's
		// own 50 default stays a wire backstop for other callers.
		limit := intFlag(fs, "limit")
		if limit < 1 || limit > 200 {
			return fmt.Errorf("recall: --limit must be 1..200")
		}
		memType, err := recallTypeFilter(stringFlag(fs, "type"))
		if err != nil {
			return err
		}
		proj := stringFlag(fs, "project")
		if proj == "" {
			return fmt.Errorf("recall: --project is required — there is no default project (polyforge#113); the routing skill derives it from the WI under discussion, and [workspace.\"<path>\"] sections of cli.toml declare the mapping (`pf doctor` checks it)")
		}
		if err := requireMappedProject(ctx, "recall", proj); err != nil {
			return err
		}
		var tagList []string
		if tags := stringFlag(fs, "tags"); tags != "" {
			tagList = strings.Split(tags, ",")
		}
		raw, err := ctx.aihub().SearchMemories(proj, q, memType, tagList, limit)
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var l struct {
				Items []api.Memory `json:"items"`
			}
			if err := json.Unmarshal(raw, &l); err == nil {
				fmt.Fprintf(ctx.Stdout, "%d hit(s)\n", len(l.Items))
				renderMemories(ctx.Stdout, l.Items)
			}
		})
		return nil
	},
}

// recallTypeFilter validates the --type form and returns the value to
// send on the wire. Exactly three forms are legal: a plain value
// (exact match), a trailing ".*" (prefix match — experience.* is
// interpreted by the store as the prefix "experience."), and a lone
// "*" (all types, normalized to no filter at all, exactly as if --type
// had been omitted). Any other "*" placement (*.debug, exp*rience,
// experience*, an empty prefix in .*) is a typo we refuse loudly
// instead of sending.
func recallTypeFilter(memType string) (string, error) {
	bad := fmt.Errorf("recall: --type must be a plain value (e.g. experience.debug), "+
		"a trailing .* prefix (e.g. experience.*), or * for all types; got %q", memType)
	switch {
	case memType == "" || memType == "*":
		return "", nil
	case strings.HasSuffix(memType, ".*"):
		if prefix := strings.TrimSuffix(memType, ".*"); prefix != "" && !strings.Contains(prefix, "*") {
			return memType, nil
		}
		return "", bad
	case strings.Contains(memType, "*"):
		return "", bad
	default:
		return memType, nil
	}
}

// ---------------------------------------------------------------------------
// remember — memory write face (the v1 CLI's read verb is `recall`; this
// is its missing write counterpart — the hole the F6 lane closes on the
// human side too).
// ---------------------------------------------------------------------------

var cmdRemember = &Command{
	Name:    "remember",
	Group:   GroupData,
	Summary: "Store a project memory (the write face of `recall`)",
	Usage: "pf remember --project P --type T [--title S] [--visibility V] [--tags a,b] " +
		"[--work-item ID] <content | - (stdin) | @file>",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name (required; validated against the [workspace] mapping)")
		fs.String("type", "", "memory type (required, e.g. experience.debug, fact.note)")
		fs.String("title", "", "memory title")
		fs.String("visibility", "project", "admin|private|project|team|public")
		fs.String("tags", "", "comma-separated tags")
		fs.String("work-item", "", "attributing work item id (optional)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		memType := stringFlag(fs, "type")
		if strings.TrimSpace(memType) == "" {
			return fmt.Errorf("remember: --type is required (e.g. experience.debug)")
		}
		proj := stringFlag(fs, "project")
		if proj == "" {
			return fmt.Errorf("remember: --project is required — there is no default project (polyforge#113); [workspace.\"<path>\"] sections of cli.toml declare the mapping (`pf doctor` checks it)")
		}
		if err := requireMappedProject(ctx, "remember", proj); err != nil {
			return err
		}
		// The content: positional text, '-' for stdin, '@file' for a file.
		var content string
		switch {
		case len(args) == 1 && args[0] == "-":
			raw, err := io.ReadAll(ctx.Stdin)
			if err != nil {
				return err
			}
			content = string(raw)
		case len(args) == 1 && strings.HasPrefix(args[0], "@"):
			raw, err := os.ReadFile(strings.TrimPrefix(args[0], "@"))
			if err != nil {
				return err
			}
			content = string(raw)
		case len(args) >= 1:
			content = strings.Join(args, " ")
		default:
			return fmt.Errorf("remember: content is required (positional, '-' for stdin or '@file')")
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("remember: content is required")
		}
		body := map[string]any{
			"project":    proj,
			"type":       memType,
			"content":    content,
			"visibility": stringFlag(fs, "visibility"),
		}
		if t := stringFlag(fs, "title"); t != "" {
			body["title"] = t
		}
		if tags := stringFlag(fs, "tags"); tags != "" {
			body["tags"] = strings.Split(tags, ",")
		}
		if wi := stringFlag(fs, "work-item"); wi != "" {
			body["work_item_id"] = wi
		}
		var raw json.RawMessage
		if err := ctx.aihub().Do("POST", "/api/v1/memories", body, &raw); err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var m api.Memory
			if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
				fmt.Fprintf(ctx.Stdout, "stored %s (type %s, visibility %s)\n", m.ID, m.Type, m.Visibility)
			}
		})
		return nil
	},
}

// ---------------------------------------------------------------------------
// v1.5 session-model faces.
// ---------------------------------------------------------------------------

// wiSessionView reads the work item's v1.5 PUBLIC session audit fields
// from aihub (additive decode: api.WorkItem carries them only once Lane
// A's server lands; the raw body already can). nil when the read fails —
// the renderers skip the session line rather than fabricate one.
func wiSessionView(ctx *Context, wiID string) *WIView {
	raw, err := ctx.aihub().GetWorkItemRaw(wiID)
	if err != nil {
		return nil
	}
	var v WIView
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return &v
}

// mintSessionSecret mints the v1.5 session secret: 32 crypto/rand
// bytes, hex (contract §0). It is presented once, never logged, never
// echoed by the server.
func mintSessionSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// takeoverIdempotencyKey mints a fresh takeover idempotency key so a
// network retry of the same invocation cannot double-swap (the server
// keys the cached answer on it, D4).
func takeoverIdempotencyKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "tk_" + hex.EncodeToString(b)
}

// daemonStatusOf reads the local daemon's GET /status — "the daemon on
// this box". The takeover verb uses it as the machine-identity source
// of truth (#109): the executor half's own view of itself, never a
// CLI-side guess like the old cli-<hostname> spelling.
func daemonStatusOf(ctx *Context) (*StatusResponse, error) {
	raw, err := ctx.daemon().Status()
	if err != nil {
		return nil, err
	}
	var st StatusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// cmdTakeover is the v1.5 §1.4 force-takeover verb: atomically swap the
// work item's session to a freshly minted secret — minted by THIS
// invocation and handed to the LOCAL daemon for durable custody (#109),
// because a session whose secret dies with the CLI process is an orphan
// no process can ever attach, verify, or fence (the #97 wedge). The
// machine identity is the local daemon's own (GET /status): the session
// this verb produces is one this box's daemon holds. The old holder
// learns at its next fenced use (push or the M4 revalidation). The
// response carries the PUBLIC fingerprint only — never the secret.
var cmdTakeover = &Command{
	Name:    "takeover",
	Group:   GroupData,
	Summary: "Force-takeover a running/paused WI to a fresh session (v1.5 §1.4; fences the old holder)",
	Usage:   "pf takeover <id> --reason <text> [--machine ID] [--owner ID] [--json]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("reason", "", "why the takeover is justified (required; recorded in the session_superseded event)")
		fs.String("machine", "", "machine identity override (must equal the local daemon's own id; default: the daemon's id from GET /status)")
		fs.String("owner", "", "owner identity reported to the hub (default the configured user_id)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("takeover: expected exactly one work item id")
		}
		reason := strings.TrimSpace(stringFlag(fs, "reason"))
		if reason == "" {
			return fmt.Errorf("takeover: --reason is required (recorded in the takeover audit event)")
		}
		// #109: the executor half must already be on this box. The local
		// daemon is the only process that can hold the minted session
		// secret and later run the work item, so a takeover without one
		// would mint an unusable orphan — refuse instead of minting.
		st, err := daemonStatusOf(ctx)
		if err != nil {
			return fmt.Errorf("takeover: no local daemon reachable (%v); a takeover minted without a daemon to hold the session orphans the work item (#109) — start the daemon and retry, or run the takeover on the box whose daemon should own the work item", err)
		}
		if m := stringFlag(fs, "machine"); m != "" && m != st.MachineID {
			return fmt.Errorf("takeover: --machine %q does not match this box's daemon (%q); the minted session would be unusable by any daemon that holds it (#109) — run the takeover on the box that should own the work item", m, st.MachineID)
		}
		secret, err := mintSessionSecret()
		if err != nil {
			return fmt.Errorf("takeover: minting the session secret: %w", err)
		}
		owner := stringFlag(fs, "owner")
		if owner == "" {
			owner = ctx.Cfg.UserID
		}
		if owner == "" {
			owner = "cli"
		}
		req := api.SessionClaimRequest{
			WorkItemID:     args[0],
			SessionID:      secret,
			MachineID:      st.MachineID,
			Owner:          owner,
			Reason:         reason,
			IdempotencyKey: takeoverIdempotencyKey(),
		}
		resp, err := ctx.aihub().ForceTakeover(args[0], req)
		if err != nil {
			return err
		}
		// Custody (#109): hand the minted secret to the local daemon over
		// the UDS. The daemon verify-backs the handoff against the hub
		// (only the live holder's secret is adopted) and journals it
		// durably — the session stays usable after this process exits.
		if _, aerr := ctx.daemon().AdoptSession(args[0], AdoptSessionRequest{
			SessionID: secret,
			Owner:     owner,
			Reason:    reason,
		}); aerr != nil {
			return fmt.Errorf("takeover: the hub session swapped, but handing the secret to the local daemon failed (%v) — the session is an orphan until a daemon-side takeover fences it: pf submit %s --takeover", aerr, args[0])
		}
		if ctx.JSON {
			// --json: the response envelope as-is.
			raw, merr := json.Marshal(resp)
			if merr != nil {
				return merr
			}
			ctx.Out(raw)
			return nil
		}
		fmt.Fprintf(ctx.Stdout, "took over %s\n", resp.Claim.WorkItemID)
		fmt.Fprintf(ctx.Stdout, "  session: %s (machine %s, owner %s)\n",
			resp.Claim.SessionFingerprint, resp.Claim.MachineID, resp.Claim.Owner)
		fmt.Fprintf(ctx.Stdout, "  bundle:  %s\n", resp.Claim.BundleName)
		fmt.Fprintln(ctx.Stdout, "  the previous holder is fenced at its next push or session revalidation")
		// #109: the mint is in the local daemon's custody — no orphan.
		fmt.Fprintf(ctx.Stdout, "  the session secret is in the local daemon's custody (%s); `pf submit %s --takeover` runs the work item on it\n", st.MachineID, resp.Claim.WorkItemID)
		return nil
	},
}

// cmdVerifySession is the v1.5 §1.7 public holder read: who holds
// this WI right now — the operator's takeover-decision input (held-
// running / held-paused / unheld are three ordinary answers; exit 0 in
// all three). It carries the PUBLIC audit fields only — no session
// secret ever travels through this face, and never the hash.
var cmdVerifySession = &Command{
	Name:    "verify-session",
	Group:   GroupData,
	Summary: "Show who holds a WI right now (v1.5 §1.7 public holder read; takeover-decision input)",
	Usage:   "pf verify-session <id> [--json]",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("verify-session: expected exactly one work item id")
		}
		resp, err := ctx.aihub().SessionHolder(args[0])
		if err != nil {
			return err
		}
		if ctx.JSON {
			// --json: the endpoint body (same wire type the server wrote,
			// so the marshal is byte-identical to the §1.7 response).
			raw, merr := json.Marshal(resp)
			if merr != nil {
				return merr
			}
			ctx.Out(raw)
			return nil
		}
		if resp.SessionFingerprint == "" {
			fmt.Fprintf(ctx.Stdout, "verify-session %s: nobody holds it (status %s)\n", resp.WorkItemID, resp.Status)
			return nil
		}
		fmt.Fprintf(ctx.Stdout, "verify-session %s: held (status %s)\n", resp.WorkItemID, resp.Status)
		fmt.Fprintf(ctx.Stdout, "  session: %s (machine %s, owner %s)\n",
			resp.SessionFingerprint, resp.MachineID, resp.SessionOwner)
		fmt.Fprintf(ctx.Stdout, "  started: %s\n", resp.SessionStartedAt)
		fmt.Fprintf(ctx.Stdout, "  clean_pause: %t\n", resp.CleanPause)
		return nil
	},
}
