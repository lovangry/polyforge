// Package migrate implements the `pf migrate-memories` verb: the one-time
// memory migration of 06-memory-search.md §10.1 (A12 + B03 rulings).
//
// Input contract (B03 / the ticket): a JSONL file exported from the OLD
// aihub (gen-1, PostgreSQL) by a single psql query — one JSON object per
// line carrying ALL old-record fields. The tool NEVER connects to
// PostgreSQL: the JSONL file is the only input, so no new dependency ever
// enters go.mod for the old world (07-deployment-pilot.md §2.1: the vNext
// AIHub host depends on Go + SQLite + the local filesystem only).
//
// JSONL schema (one object per line; json_build_object emits exactly this
// key set):
//
//	{
//	  "id":           "mem_ab12cd34ef56",      // required, kept verbatim (B03: mem_id 不变)
//	  "type":         "experience.debug",       // required (frontmatter mirror)
//	  "title":        "…",                      // optional, nullable
//	  "tags":         ["a","b"],                // optional, nullable, defaults []
//	  "visibility":   "project",                // required: private|project|team|public|admin
//	  "content":      "…markdown body…",         // required, non-blank
//	  "project":      "old-project-name",        // required: OLD project NAME
//	  "owner":        "alice@example.com",       // required: old owner identifier —
//	                                             //   email for humans, actor for machines
//	  "owner_kind":   "human"|"machine",         // optional hint; "" → inferred (email contains '@')
//	  "work_item_id": "wi_old123",               // optional, nullable — ALWAYS blanked on import
//	  "related":      ["mem_xx","mem_yy"],       // optional, nullable
//	  "created_at":   "2026-01-01T00:00:00Z",    // required RFC3339
//	  "updated_at":   "2026-01-02T00:00:00Z",    // required RFC3339 (--since filters on this)
//	  "status":       "active"|"redacted"        // required; redacted → _redacted/, stays soft-deleted
//	}
//
// psql export command (adapt the two join column names to the gen-1
// schema if it drifted; everything else is the contract):
//
//	psql "postgresql://user:pass@host/gen1_aihub" -At -c "
//	SELECT json_build_object(
//	         'id',           m.id,
//	         'type',         m.type,
//	         'title',        m.title,
//	         'tags',         COALESCE(m.tags, '[]'::json),
//	         'visibility',   m.visibility,
//	         'content',      m.content,
//	         'project',      p.name,
//	         'owner',        CASE WHEN u.user_type = 'machine' THEN u.actor_name ELSE u.email END,
//	         'owner_kind',   u.user_type,
//	         'work_item_id', m.work_item_id,
//	         'related',      COALESCE(m.related, '[]'::json),
//	         'created_at',   to_char(m.created_at, 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),
//	         'updated_at',   to_char(m.updated_at, 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),
//	         'status',       m.status
//	       )::text
//	FROM memories m
//	JOIN projects p ON p.id = m.project_id
//	JOIN users u    ON u.id = m.owner_user_id
//	ORDER BY m.id;" > memories.jsonl
//
// Import order (B03): ① resolve/create projects (placeholder visible=false
// when absent) and users (humans keyed by email, machines by actor;
// unresolvable → placeholder machine user display_name "<old> (imported)")
// so every id mapping exists; ② place each memory file directly under
// <data-dir>/memory/<new_pid>/<mem_id>/memory.md (CLI直写, no Memory API,
// no write-order transients — A12) with redacted entries under
// _redacted/<mem_id>/; ③ POST /api/v1/memories/rebuild-index on the live
// server; ④ the four-step verification (count reconciliation,
// content_sha sampling, retrieval smoke over the real HTTP search,
// blanked-list reconciliation) — any failure fails the whole run with a
// non-zero exit.
//
// Re-runs are idempotent: the file tree is the authority and the rebuild
// dedupes by mem_id, and placeholder identity resolution re-finds the
// rows a previous run created (project by name, user by the deterministic
// "<old> (imported)" display_name), so no duplicate projects/users/memories
// are ever created.
package migrate
