---
name: pf-sync
description: >
  Use when the user wants to sync work items between polyforge and an external tracker
  such as Jira or GitHub Issues, pulling issues in or pushing a wrapped wi back out.
---

# pf-sync - External System Sync (Phase 2 stub)

## Usage

**Purpose**: Sync work items between polyforge and external trackers (Jira / GitHub Issues). Currently a Phase 2 stub - emits a note event only.

**Pattern**: `/pf-sync { pull | push } [<external_id>]`

**Required**: a sub-mode (see below)

**Flags**: none (v1 is a stub; both directions emit a note event and surface a not-yet-implemented message)

## When to use

When the user wants to sync work items between polyforge and an external tracker
(Jira, GitHub Issues). Currently a Phase 2 feature - v1 emits a diagnostic note
and tracks the sync request as a follow-up work item.

## Mechanic

### `/pf-sync pull`

**Intent**: Import open Jira issues or GitHub Issues as polyforge wi's.

**v1 behavior** (stub):

1. Emit informational note - **only if the current wi has a live claimed attempt** (a resolvable state file). `pf_emit_event` authenticates from the claiming attempt's state file; a freshly created-but-unclaimed tracking wi has none, so never promise a note on one:
   ```
   pf_emit_event(   # only with a live claimed attempt; otherwise skip this step
     work_item_id=<current wi, if running/paused>,
     event_type="note",
     payload={text: "pf-sync pull requested - external sync not yet implemented in v1. Tracked in Phase 2 backlog."}
   )
   ```
   The unsupported-sync response itself is unconditional - do not make it depend on the note landing.

2. Output three-segment format:
   ```
   ## Result
   pf-sync pull is not yet implemented in v1.
   
   ## Status
   | feature | pf-sync pull         |
   | status  | Phase 2 backlog      |
   | tracker | External sync (Jira / GitHub Issues) |
   
   ## Next steps
   - Manually create wi's with `/pf-work --goal "..."` for urgent items
   - Track this feature request: create wi with `/pf-work --goal "implement pf-sync pull"`
   ```

---

### `/pf-sync push`

**Intent**: Publish a locally-wrapped wi back to Jira/GitHub (close issue, update status).

**v1 behavior** (stub):

1. Emit note - **only when the wi still has a live claimed attempt**. A wrapped wi's state file is deleted by wrap, so a post-wrap note on the wrapped wi cannot be emitted at all; emit before wrapping if a note is wanted, otherwise skip:
   ```
   pf_emit_event(   # only with a live claimed attempt; never on a wrapped/terminal wi
     work_item_id=<current>,
     event_type="note",
     payload={text: "pf-sync push requested - external push not yet implemented in v1."}
   )
   ```

2. Output three-segment format (same pattern as pull).

---

## Phase 2 Design (reference)

When implemented, `pf-sync pull` will:
1. Authenticate with Jira/GitHub via stored credentials
2. `pf_sync_pull(project, source="jira"|"github", filter="open")` -> list of imported wi's
3. For each imported wi: `pf_create_work_item(source="sync_jira"|"sync_github", ...)`
   - `source` is a CLOSED vocabulary (`human`, `auto_execute`, `auto_debug`, `auto_review`,
   `sync_jira`, `sync_github`, `admin`), published as a JSON-Schema enum since aihub#396.
   This line said `jira_import`/`github_import`, which are in no vocabulary: before the enum
   existed the value crossed all four hops and died on the `work_items_source_check`
   constraint as a 500, so phase 2 would have shipped a broken importer and found out in
   production. Do not invent a source value; use one of the seven.
4. Show summary of imported wi's

`pf-sync push` will:
1. `pf_sync_push(work_item_id, target_ref="jira:PROJ-123"|"github:owner/repo#42")`
2. Closes/transitions the external ticket
3. Emits `synced_to_external` event

## NL Triggers

- "sync" / "sync" / "sync to jira" / "sync to github"
- "import from jira" / "import from jira"
- "push to github" / "push to github"
- `/pf-sync pull` / `/pf-sync push`
