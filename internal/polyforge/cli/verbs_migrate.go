package cli

// verbs_migrate.go: the two migration verbs.
//
//	pf migrate-memories  --from-dir DIR | --from-jsonl FILE --project P
//	                     [--owner U] [--user-map FILE] [--since TS] [--dry-run]
//	pf migrate-work-items --from FILE --project P [--reporter U]
//	                     [--user-map FILE] [--dry-run]
//
// Both need the admin key: identity resolution may create placeholder
// users and the memories path triggers the explicit reconcile.
// migrate-memories additionally needs --data-dir: it writes the
// authority FILES directly (CLI直写, the v0 A12 ruling) and lets the
// server's file-wins reconcile be the only row/FTS backfill — the first
// real use of the v1 self-healing path.

import (
	"encoding/json"
	"flag"
	"fmt"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

var cmdMigrateMemories = &Command{
	Name:    "migrate-memories",
	Summary: "One-time memory import (authority files or JSONL → target aihub)",
	Usage: "pf migrate-memories --from-dir DIR | --from-jsonl FILE --data-dir DIR --project P\n" +
		"                        [--owner U] [--user-map FILE] [--since RFC3339] [--dry-run]\n" +
		"\n" +
		"--from-dir: a v0 authority-file tree (memory/<pj>/<mem_id>/memory.md). Every\n" +
		"file's frontmatter is validated against the v1 format field-by-field; the\n" +
		"id/project/owner identity remap and every structural difference are listed\n" +
		"in the report.\n" +
		"--from-jsonl: the JSONL export (one object per line: id, type, title, tags,\n" +
		"visibility, content, project, owner, owner_kind, work_item_id, related,\n" +
		"created_at, updated_at, status) — the shape tools/v0-export emits.\n" +
		"\n" +
		"Placement is direct under the target data dir's memory/ tree; the live\n" +
		"server's rebuild-index (file-wins reconcile) is the ONLY backfill path.\n" +
		"work_item_id is preserved when the target already carries that work item\n" +
		"(import work items first); unresolvable references are blanked and logged.\n" +
		"Idempotent: re-runs re-place byte-identical files and verify with zero delta.",
	Setup: func(fs *flag.FlagSet) {
		fs.String("from-dir", "", "v0 authority-file tree (memory root or one project dir)")
		fs.String("from-jsonl", "", "memories JSONL export (v0/v1 record shape)")
		fs.String("data-dir", "", "TARGET aihub data dir (the authority memory/ tree root; required)")
		fs.String("project", "", "TARGET project name or id (required; must exist)")
		fs.String("owner", "", "map every old owner to this target user id")
		fs.String("user-map", "", "JSON file {\"old identifier\": \"new user id\"}")
		fs.String("since", "", "JSONL only: delta mode, records with updated_at >= this RFC3339")
		fs.Bool("dry-run", false, "parse, resolve and report; place nothing, verify nothing")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("migrate-memories: unexpected arguments %v", args)
		}
		userMap, err := readMapFile(stringFlag(fs, "user-map"))
		if err != nil {
			return fmt.Errorf("--user-map: %v", err)
		}
		opts := MigMemoriesOptions{
			DataDir:   stringFlag(fs, "data-dir"),
			FromDir:   stringFlag(fs, "from-dir"),
			FromJSONL: stringFlag(fs, "from-jsonl"),
			Project:   stringFlag(fs, "project"),
			Owner:     stringFlag(fs, "owner"),
			UserMap:   userMap,
			Since:     stringFlag(fs, "since"),
			DryRun:    boolFlag(fs, "dry-run"),
		}
		if opts.AihubURL == "" {
			opts.AihubURL = ctx.Cfg.Server
		}
		opts.APIKey = ctx.Cfg.APIKey
		rep, err := RunMigrateMemories(opts, aihubclient.New(opts.AihubURL, opts.APIKey))
		if rep != nil {
			if ctx.JSON {
				raw, merr := json.Marshal(rep)
				if merr != nil {
					return merr
				}
				ctx.emit(raw)
			} else {
				rep.Render(ctx.Stdout)
			}
		}
		return err
	},
}

var cmdMigrateWorkItems = &Command{
	Name:    "migrate-work-items",
	Summary: "Terminal work-item record import (JSONL → target aihub, admin)",
	Usage: "pf migrate-work-items --from FILE --project P [--reporter U] [--user-map FILE]\n" +
		"                        [--accept-changed] [--dry-run]\n" +
		"\n" +
		"One JSON object per line: id, seq, project (old, informational), goal,\n" +
		"content, status (wrapped|failed|canceled|cancelled|blocked), priority,\n" +
		"milestone, labels, attrs, reporter (old user id), assignee, waiting_on,\n" +
		"bundle_name, bundle_digest, workflow_budget_baseline, created_at,\n" +
		"updated_at, closed_at. Terminal records only: execution history never\n" +
		"migrates (no events, no sessions, no capsules). Ids and seqs are kept\n" +
		"verbatim (the old slug number survives as targetproj#seq). Idempotent:\n" +
		"a same-id re-run skips and re-verifies each row. A same-id record whose\n" +
		"fields diverge hard-fails with a per-field diff (a changed record is a\n" +
		"cutover conflict that needs eyes, never silently applied or skipped);\n" +
		"--accept-changed applies the new version in place (overwrite, admin\n" +
		"migration face only) and counts the row as changed in the report.",
	Setup: func(fs *flag.FlagSet) {
		fs.String("from", "", "work-items JSONL export (required)")
		fs.String("project", "", "TARGET project name or id (required; must exist)")
		fs.String("reporter", "", "map every old reporter to this target user id")
		fs.String("user-map", "", "JSON file {\"old reporter id\": \"new user id\"}")
		fs.Bool("accept-changed", false, "apply changed same-id records in place (new version wins; counted as changed)")
		fs.Bool("dry-run", false, "parse, resolve and report; import nothing")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("migrate-work-items: unexpected arguments %v", args)
		}
		userMap, err := readMapFile(stringFlag(fs, "user-map"))
		if err != nil {
			return fmt.Errorf("--user-map: %v", err)
		}
		opts := MigWorkItemsOptions{
			FromJSONL:     stringFlag(fs, "from"),
			Project:       stringFlag(fs, "project"),
			Reporter:      stringFlag(fs, "reporter"),
			UserMap:       userMap,
			AcceptChanged: boolFlag(fs, "accept-changed"),
			DryRun:        boolFlag(fs, "dry-run"),
		}
		opts.AihubURL = ctx.Cfg.Server
		opts.APIKey = ctx.Cfg.APIKey
		rep, err := RunMigrateWorkItems(opts, aihubclient.New(opts.AihubURL, opts.APIKey))
		if rep != nil {
			if ctx.JSON {
				raw, merr := json.Marshal(rep)
				if merr != nil {
					return merr
				}
				ctx.emit(raw)
			} else {
				rep.Render(ctx.Stdout)
			}
		}
		return err
	},
}
