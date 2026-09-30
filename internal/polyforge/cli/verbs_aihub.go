package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// ---------------------------------------------------------------------------
// file — create/edit/show work items (direct to aihub).
// ---------------------------------------------------------------------------

var cmdFile = &Command{
	Name:    "file",
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

// fileCreate builds the create body from flags or from full stdin JSON.
func fileCreate(ctx *Context, args []string) error {
	f := subFlags(ctx, "file create")
	var (
		project   = f.String("project", "", "project name (required unless stdin JSON carries it)")
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
		var wi api.WorkItem
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
		var wi api.WorkItem
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
		var wi api.WorkItem
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
	Summary: "List work items / show the ready queue",
	Usage: "pf queue [--project P] [--status S]\n" +
		"pf queue --ready [--project P]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name")
		fs.Bool("ready", false, "show the ready queue instead of the list")
		fs.String("status", "", "filter: queued|running|paused|blocked|wrapped|failed|canceled")
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
		var (
			raw json.RawMessage
			err error
		)
		if boolFlag(fs, "ready") {
			raw, err = ctx.aihub().ReadyQueueRaw(proj)
		} else {
			raw, err = ctx.aihub().ListWorkItemsRaw(proj, stringFlag(fs, "status"))
		}
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var l struct {
				Items []api.WorkItem `json:"items"`
			}
			if err := json.Unmarshal(raw, &l); err == nil {
				fmt.Fprintf(ctx.Stdout, "%d work item(s)\n", len(l.Items))
				renderWITable(ctx.Stdout, l.Items)
			}
		})
		return nil
	},
}

// ---------------------------------------------------------------------------
// cancel — terminate a non-active WI (direct to aihub).
// ---------------------------------------------------------------------------

var cmdCancel = &Command{
	Name:    "cancel",
	Summary: "Cancel a queued/paused/blocked work item (no active generation)",
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
	Summary: "Search project memories",
	Usage:   "pf recall [--project P] <query> [--type T] [--tags a,b] [--limit N]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name")
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
			proj = ctx.Cfg.Project
		}
		if proj == "" {
			return fmt.Errorf("recall: --project is required (or set project in the config)")
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
	Summary: "Store a project memory (the write face of `recall`)",
	Usage: "pf remember [--project P] --type T [--title S] [--visibility V] [--tags a,b] " +
		"[--work-item ID] <content | - (stdin) | @file>",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name (default: the configured project)")
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
			proj = ctx.Cfg.Project
		}
		if proj == "" {
			return fmt.Errorf("remember: --project is required (or set project in the config)")
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
