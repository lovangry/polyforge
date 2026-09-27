package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// file — create/edit/show work items (00 §5.1 data-plane verb; aihub row
// 1/3). Flags or full stdin JSON for
// goal/content/labels/priority/milestone/bundle_name/parent_id/
// repo_inventory.
// ---------------------------------------------------------------------------

var cmdFile = &Command{
	Name:    "file",
	Summary: "Create, edit or show a work item",
	Usage: "pf file create --project P --goal G [flags] | --stdin\n" +
		"pf file edit <id> [flags] | --stdin\n" +
		"pf file show <id> [--include events,deps,children]",
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

// fileCreate builds the row-1 create body from flags or from full stdin
// JSON (CreateWorkItemRequest shape, api/openapi.yaml).
func fileCreate(ctx *Context, args []string) error {
	f := flag.NewFlagSet("pf file create", flag.ContinueOnError)
	f.SetOutput(ctx.Stderr)
	var (
		project   = f.String("project", "", "project name (required unless stdin JSON carries it)")
		goal      = f.String("goal", "", "one-line goal (<=500 chars)")
		content   = optString{}
		labels    stringList
		priority  = f.String("priority", "", "low|normal|high|urgent")
		milestone = optString{}
		bundle    = f.String("bundle", "", "bundle_name: non-empty makes an execution ticket")
		parent    = f.String("parent", "", "parent work item id (epic-lite)")
		repos     kvList
		repoBases kvList
		attrs     kvList
		useStdin  = f.Bool("stdin", false, "read the full create-body JSON from stdin")
	)
	f.Var(&content, "content", "markdown background; '-' reads stdin, '@file' reads a file")
	f.Var(&labels, "label", "label (repeatable)")
	f.Var(&milestone, "milestone", "milestone name")
	f.Var(&repos, "repo", "repo declaration name=url (repeatable, order = position)")
	f.Var(&repoBases, "repo-base", "frozen base commit name=sha (repeatable)")
	f.Var(&attrs, "attr", "business attr key=json-value (repeatable)")
	if _, err := parseCommandArgs(f, args); err != nil {
		return err
	}

	var req CreateWorkItemRequest
	if *useStdin {
		raw, err := io.ReadAll(ctx.Stdin)
		if err != nil {
			return err
		}
		if err := decodeJSON(raw, &req); err != nil {
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
		req.Project = ctx.Cfg.Project
	}
	if req.Project == "" {
		return fmt.Errorf("file create: --project is required")
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
	if len(repos) > 0 {
		for _, kv := range repos {
			req.RepoInventory = append(req.RepoInventory, RepoDecl{Name: kv.Key, URL: kv.Value})
		}
	}
	if len(repoBases) > 0 {
		for _, kv := range repoBases {
			req.RepoInventoryBase = append(req.RepoInventoryBase, RepoBaseDecl{Name: kv.Key, Base: kv.Value})
		}
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

	client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
	raw, err := client.CreateWorkItem(&req)
	if err != nil {
		return err
	}
	ctx.emit(raw)
	if !ctx.JSON {
		var d detailView
		if err := decodeJSON(raw, &d); err == nil {
			fmt.Fprintln(ctx.Stdout)
			renderWIDetail(ctx.Stdout, &d)
		}
	}
	return nil
}

// fileEdit issues a row-3 PATCH with only the flags actually provided.
func fileEdit(ctx *Context, args []string) error {
	f := flag.NewFlagSet("pf file edit", flag.ContinueOnError)
	f.SetOutput(ctx.Stderr)
	var (
		goal      = optString{}
		content   = optString{}
		labels    stringList
		priority  = optString{}
		milestone = optString{}
		waitingOn = optString{}
		attrs     kvList
		useStdin  = f.Bool("stdin", false, "read the full PATCH-body JSON from stdin")
	)
	f.Var(&goal, "goal", "new goal")
	f.Var(&content, "content", "new markdown background; '-' reads stdin, '@file' reads a file")
	f.Var(&labels, "label", "labels (repeatable; replaces the whole set)")
	f.Var(&priority, "priority", "low|normal|high|urgent")
	f.Var(&milestone, "milestone", "milestone (empty string clears it)")
	f.Var(&waitingOn, "waiting-on", "waiting_on value (owner | runtime:<id> | dependency:<id> | gate)")
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
		if err := decodeJSON(raw, &body); err != nil {
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

	client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
	raw, err := client.PatchWorkItem(id, body)
	if err != nil {
		return err
	}
	ctx.emit(raw)
	if !ctx.JSON {
		var d detailView
		if err := decodeJSON(raw, &d); err == nil {
			fmt.Fprintln(ctx.Stdout)
			renderWIDetail(ctx.Stdout, &d)
		}
	}
	return nil
}

// fileShow reads the row-3 detail (with optional include sections).
func fileShow(ctx *Context, args []string) error {
	f := flag.NewFlagSet("pf file show", flag.ContinueOnError)
	f.SetOutput(ctx.Stderr)
	include := f.String("include", "", "comma-separated: events,deps,children")
	rest, err := parseCommandArgs(f, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("file show: expected exactly one work item id")
	}
	var inc []string
	if *include != "" {
		inc = strings.Split(*include, ",")
	}
	client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
	raw, err := client.GetWorkItem(rest[0], inc)
	if err != nil {
		return err
	}
	ctx.emit(raw)
	if !ctx.JSON {
		var d detailView
		if err := decodeJSON(raw, &d); err == nil {
			fmt.Fprintln(ctx.Stdout)
			renderWIDetail(ctx.Stdout, &d)
			if len(d.Events) == 0 && contains(inc, "events") {
				fmt.Fprintln(ctx.Stdout, "  events: (none)")
			}
		}
	}
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// queue — WI list + ready-queue (00 §5.1 data-plane verb; aihub rows 2/4).
// ---------------------------------------------------------------------------

var cmdQueue = &Command{
	Name:    "queue",
	Summary: "List work items / show the flat ready queue",
	Usage: "pf queue [--project P] [--status S] [--assignee A] [--waiting-on W] [--parent ID] [--milestone M]\n" +
		"pf queue --ready [--project P]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name")
		fs.Bool("ready", false, "show the ready queue (flat two arrays) instead of the list")
		fs.String("status", "", "filter: queued|running|paused|blocked|wrapped|failed|cancelled")
		fs.String("assignee", "", "filter by assignee")
		fs.String("waiting-on", "", "filter by waiting_on value")
		fs.String("parent", "", "filter direct children of this epic")
		fs.String("milestone", "", "filter by milestone")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("queue: unexpected arguments %v", args)
		}
		proj := stringFlag(fs, "project")
		if proj == "" {
			proj = ctx.Cfg.Project
		}
		if proj == "" {
			return fmt.Errorf("queue: --project is required (or set project in the config)")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		if boolFlag(fs, "ready") {
			raw, err := client.ReadyQueue(proj)
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var q readyQueueView
				if err := decodeJSON(raw, &q); err == nil {
					fmt.Fprintln(ctx.Stdout)
					renderReadyQueue(ctx.Stdout, &q)
				}
			}
			return nil
		}
		raw, err := client.ListWorkItems(proj, map[string]string{
			"status":     stringFlag(fs, "status"),
			"assignee":   stringFlag(fs, "assignee"),
			"waiting-on": stringFlag(fs, "waiting-on"),
			"parent":     stringFlag(fs, "parent"),
			"milestone":  stringFlag(fs, "milestone"),
		})
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var l wiListView
			if err := decodeJSON(raw, &l); err == nil {
				fmt.Fprintf(ctx.Stdout, "%d work items\n", len(l.Items))
				renderWITable(ctx.Stdout, l.Items)
			}
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// cancel — terminate a non-active WI (00 §5.1 data-plane verb; row 5).
// ---------------------------------------------------------------------------

var cmdCancel = &Command{
	Name:    "cancel",
	Summary: "Cancel a queued/paused/blocked work item (no active generation)",
	Usage:   "pf cancel <id|slug>",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("cancel: expected exactly one work item id")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		raw, err := client.CancelWorkItem(args[0])
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var d detailView
			if err := decodeJSON(raw, &d); err == nil {
				fmt.Fprintf(ctx.Stdout, "cancelled %s (status %s)\n", d.Slug, d.Status)
			}
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// deps — dependency edges (row 26; `pf` CLI is the canonical caller).
// ---------------------------------------------------------------------------

var cmdDeps = &Command{
	Name:    "deps",
	Summary: "Dependency edges: add/list/remove (aihub row 26)",
	Usage: "pf deps add <blocked-id> --blocking <wi> [--kind blocks|supersedes|related]\n" +
		"pf deps list <id>\n" +
		"pf deps remove <blocked-id> <blocking-id> <kind>",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("deps: expected add|list|remove")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		sub, rest := args[0], args[1:]
		switch sub {
		case "add":
			f := flag.NewFlagSet("pf deps add", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			blocking := f.String("blocking", "", "blocking work item id (required)")
			kind := f.String("kind", "blocks", "blocks|supersedes|related")
			ids, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(ids) != 1 || *blocking == "" {
				return fmt.Errorf("deps add: expected <blocked-id> --blocking <wi>")
			}
			raw, err := client.AddDependency(ids[0], &DependencyCreateRequest{
				BlockingWiID: *blocking,
				Kind:         *kind,
			})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				fmt.Fprintf(ctx.Stdout, "edge created: %s --%s--> %s\n", ids[0], *kind, *blocking)
			}
			return nil
		case "list":
			if len(rest) != 1 {
				return fmt.Errorf("deps list: expected exactly one work item id")
			}
			raw, err := client.ListDependencies(rest[0])
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var d depsView
				if err := decodeJSON(raw, &d); err == nil {
					renderDeps(ctx.Stdout, &d)
				}
			}
			return nil
		case "remove", "rm":
			if len(rest) != 3 {
				return fmt.Errorf("deps remove: expected <blocked-id> <blocking-id> <kind>")
			}
			raw, err := client.DeleteDependency(rest[0], rest[1], rest[2])
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				fmt.Fprintf(ctx.Stdout, "edge removed: %s -/-%s-/-> %s\n", rest[0], rest[2], rest[1])
			}
			return nil
		default:
			return fmt.Errorf("deps: unknown action %q", sub)
		}
	},
}

// ---------------------------------------------------------------------------
// budget — human budget face (rows 11/12; role-based, no generation token).
// ---------------------------------------------------------------------------

var cmdBudget = &Command{
	Name:    "budget",
	Summary: "Workflow budget state / CAS increments (aihub rows 11/12)",
	Usage: "pf budget <id>\n" +
		"pf budget raise <id> --dimension requests|tokens|wall_minutes --amount N [--note text]",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("budget: expected <id> or raise <id>")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		if args[0] == "raise" {
			f := flag.NewFlagSet("pf budget raise", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			dimension := f.String("dimension", "", "requests|tokens|wall_minutes")
			amount := f.Int64("amount", 0, "positive increment amount")
			note := f.String("note", "", "free-text human annotation")
			rest, err := parseCommandArgs(f, args[1:])
			if err != nil {
				return err
			}
			if len(rest) != 1 || *dimension == "" || *amount <= 0 {
				return fmt.Errorf("budget raise: expected <id> --dimension D --amount N")
			}
			// CAS: read the current global max seq first (row 12 is the
			// based_on_seq read source, 02 §6.7).
			braw, err := client.GetBudget(rest[0])
			if err != nil {
				return err
			}
			var bv budgetView
			if err := decodeJSON(braw, &bv); err != nil {
				return fmt.Errorf("budget: cannot read current max seq: %w", err)
			}
			raw, err := client.AddBudgetIncrement(rest[0], &BudgetIncrementRequest{
				Dimension:  *dimension,
				Amount:     *amount,
				Note:       *note,
				BasedOnSeq: bv.MaxSeq,
			})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				printJSON(ctx.Stdout, raw)
			}
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("budget: expected exactly one work item id")
		}
		raw, err := client.GetBudget(args[0])
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			printJSON(ctx.Stdout, raw)
		}
		return nil
	},
}
