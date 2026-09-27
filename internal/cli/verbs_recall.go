package cli

import (
	"flag"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// recall — memory search (00 §5.1 data-plane verb; aihub row 23: the
// real-time trigram FTS retrieval, the ONLY v1 search path).
// ---------------------------------------------------------------------------

var cmdRecall = &Command{
	Name:    "recall",
	Summary: "Search project memories (FTS)",
	Usage:   "pf recall [--project P] <query> [--type T] [--tags a,b] [--limit N]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("project", "", "project name")
		fs.String("q", "", "query text (alternative to the positional argument)")
		fs.String("type", "", "memory type filter (e.g. experience.debug)")
		fs.String("tags", "", "comma-separated tags; an entry must contain all")
		fs.Int("limit", 0, "max results (server default 50, cap 200)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		q := stringFlag(fs, "q")
		if q == "" {
			q = strings.Join(args, " ")
		}
		if strings.TrimSpace(q) == "" {
			return fmt.Errorf("recall: a query is required (positional or --q)")
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
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		raw, err := client.SearchMemories(proj, q, stringFlag(fs, "type"), tagList, intFlag(fs, "limit"))
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var l memoryListView
			if err := decodeJSON(raw, &l); err == nil {
				fmt.Fprintf(ctx.Stdout, "%d hit(s)\n", len(l.Items))
				renderMemories(ctx.Stdout, &l)
			}
		}
		return nil
	},
}
