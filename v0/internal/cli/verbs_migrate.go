package cli

import (
	"encoding/json"
	"flag"
	"fmt"

	"github.com/silgrid/polyforge/internal/migrate"
)

// migrate-memories — the one-time old→new memory import of
// 06-memory-search.md §10.1 (A12: the CLI writes the authority files
// directly under the aihub data dir, never via the Memory API; the live
// server's rebuild-index backfills Core + FTS from them).
//
// The tool NEVER connects to PostgreSQL (07 §2.1: no PostgreSQL anywhere
// in the vNext runtime dependency set — the exported JSONL is the only
// old-world input, and go.mod gains nothing). Input schema and the psql
// export command below are also documented in internal/migrate/doc.go.

// migrateExportHelp is the documented psql export: ONE query, one JSON
// object per line, ALL fields (adapt the two join column names to the
// gen-1 schema if it drifted; the key set is the contract).
const migrateExportHelp = `
Input: JSONL exported from the old aihub (PostgreSQL) by a single psql
query emitting every memory with ALL fields, one JSON object per line:

  psql "postgresql://user:pass@host/gen1_aihub" -At -c "
  SELECT json_build_object(
           'id',           m.id,
           'type',         m.type,
           'title',        m.title,
           'tags',         COALESCE(m.tags, '[]'::json),
           'visibility',   m.visibility,
           'content',      m.content,
           'project',      p.name,
           'owner',        CASE WHEN u.user_type = 'machine' THEN u.actor_name ELSE u.email END,
           'owner_kind',   u.user_type,
           'work_item_id', m.work_item_id,
           'related',      COALESCE(m.related, '[]'::json),
           'created_at',   to_char(m.created_at, 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),
           'updated_at',   to_char(m.updated_at, 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),
           'status',       m.status
         )::text
  FROM memories m
  JOIN projects p ON p.id = m.project_id
  JOIN users u    ON u.id = m.owner_user_id
  ORDER BY m.id;" > memories.jsonl

Required keys per line: id (mem_…), type, visibility (private|project|
team|public|admin), content, project (old project NAME), owner (email for
humans, actor for machines), created_at/updated_at (RFC3339), status
(active|redacted). Optional: title, tags, owner_kind (human|machine),
work_item_id (always blanked on import — old WI ids never migrate),
related (ids that resolve in neither this batch nor the already-placed
file tree are blanked and logged).

Idempotent: re-runs rewrite the same authority files and the rebuild
deduplicates by mem_id, so nothing is ever imported twice. The
--project-map/--user-map files (JSON objects: old identifier -> new id)
bind old identities to existing new rows; anything unmapped gets a
placeholder (project visible=false; machine user "<old> (imported)") that
the migration report lists for a human to merge later. Re-runs are
verified by the same four steps (count reconciliation, content_sha
sampling at a fixed 10% ratio, retrieval smoke over the live HTTP search,
blanked-list reconciliation); any failure exits non-zero.`

var cmdMigrateMemories = &Command{
	Name:    "migrate-memories",
	Summary: "One-time memory import from an old-aihub JSONL export (06 §10.1)",
	Usage: "pf migrate-memories --data-dir DIR --aihub-url URL " +
		"[--since RFC3339] [--project-map FILE] [--user-map FILE] <input.jsonl>" +
		migrateExportHelp,
	Setup: func(fs *flag.FlagSet) {
		fs.String("data-dir", "", "aihub data dir (the authority memory/ tree root; required)")
		fs.String("aihub-url", "", "live aihub base URL (default: the config's server)")
		fs.String("since", "", "delta mode: only records with updated_at >= this RFC3339 timestamp")
		fs.String("project-map", "", "JSON file {\"old project name\": \"new pj_ id\"}")
		fs.String("user-map", "", "JSON file {\"old owner identifier\": \"new usr_/mac_ id\"}")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		opts := migrate.Options{
			DataDir:        stringFlag(fs, "data-dir"),
			AihubURL:       stringFlag(fs, "aihub-url"),
			APIKey:         ctx.Cfg.APIKey,
			Since:          stringFlag(fs, "since"),
			ProjectMapPath: stringFlag(fs, "project-map"),
			UserMapPath:    stringFlag(fs, "user-map"),
		}
		if opts.AihubURL == "" {
			opts.AihubURL = ctx.Cfg.Server
		}
		if len(args) != 1 {
			return fmt.Errorf("migrate-memories: exactly one input JSONL path is required")
		}
		opts.InputPath = args[0]

		rep, err := migrate.Run(opts)
		// The report is printed even on a verification failure: the
		// placeholder/blanked lists and the failing steps are exactly what
		// a human needs to fix the input and re-run.
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
