## Memory: unified to polyforge (local .md deprecated)

In a polyforge workspace, **all memory lives in AIHub** - write via `pf remember`,
recall via `pf recall`. The harness's local Claude memory (`~/.claude/projects/.../memory/*.md`
+ `MEMORY.md`, `[[wiki-link]]` syntax) is **deprecated here: do NOT create or maintain
local `.md` memory files in a polyforge workspace.** (Non-polyforge projects may still
use local memory; this override is scoped to polyforge workspaces.)

Conventions:

- **Memory type** -> a type STRING with a prefix from the v1 domain - `experience.*` /
  `fact.*` / `rule.*` - not the local `user/feedback/project/reference` vocabulary.
  `--type` is required on `pf remember`; `pf recall --type` filters on it. Pick the
  type by who will recall the memory, not by what it is about: `experience.*` entries
  are typically filed by the retro verb, so a hand-written memory prefers `rule.*`
  (operating rules) or `fact.*` (domain facts).
- **Metadata over prose** -> use the flags, not the body: `--work-item` attributes the
  memory to its owning work item (do NOT hand-write "belongs to wi X" in the body);
  `--visibility` (default `project`; one of `admin|private|project|team|public`);
  `--title` and `--tags` aid later filtering (`pf recall --tags` requires every
  listed tag; `--limit` caps results).
- **Cross-memory links** -> aihub-native `related` metadata, never `[[name]]`
  wiki-links in memory content.
- **Recallable content** -> retrieval is text search over the content plus type/tags
  filters, so write the lesson in the words a future query would use; put the essence
  in the first line and keep one memory per lesson.
- **Cross-system link discipline** (keep separate): memories link via aihub `related`
  + owning work item; repo docs/PRs link via GitHub relative paths. Never put an
  aihub `mem_…` ref in a repo doc, nor a repo path inside an aihub memory.

This takes precedence over the harness's default local-memory instruction inside a
polyforge workspace. Caveat: the harness may still auto-recall a pre-existing local
`MEMORY.md` until those files are retired - retiring them and any global-config
change are a separate follow-up (the data already lives in AIHub).
