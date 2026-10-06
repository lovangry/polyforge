# status — from pf-status

# pf-status - Work Item Status & Ready Queue

## Usage

**Purpose**: Inspect current wi progress or the project-wide ready queue (LCRS seven segments).

**Pattern**: `/pf-status [--all]`

**Required**: none (no-arg inside a claimed wi -> single-wi view; no-arg outside -> global view; `--all` forces global)

**Flags**:
- `--all` - force global LCRS view even when a wi is currently claimed

## When to use

Any time the user wants to know what is happening - current wi progress, team-wide
ready queue, or which items are stalled/blocked.

## Mechanic

### Single wi view (inside a claimed wi)

1. ```
   pf_list_work_items(
     ids=[<current_wi_id>],
     include_step_state=true
   )
   ```

   > Since aihub#280 `ids` is a real filter and `project` is correctly omitted -
   > an id already names one wi. Before that, `ids` reached no forwarding table
   > and no `project` was sent, so this call was a hard 400 and never ran.

2. **Pinned DB workflow check (aihub#708)**: call `pf_get_workflow(work_item_id=<current>)`.
   If `steps_version > 0`, render the step row(s) from the STORED flow's progress (per-step
   status, review verdict, approval state, open invocation). Legacy `step_state` rows MAY
   exist for a workflow wi (claim upserts them unconditionally), but they are NOT the
   authoritative record of pinned-workflow progress - the workflow tables are - so do not
   report "no steps" for them and never take step progress from `step_state` when
   `steps_version > 0`. If `steps_version == 0`, use the `step_state` from call 1 as before.

3. ```
   pf_read_events(
     work_item_id=<current>,
     limit=5,
     pinned_first=true
   )
   ```

4. Render three-segment output with full wi fields + up to five events, pinned first
   (pinned events can predate the recent tail; this is the server's pinned-first ordering,
   not a strict recency list).

---

### Global view (`/pf-status` without an active wi, or `/pf-status --all`)

1. ```
   pf_get_ready_queue(project=<from .polyforge.yaml>)
   ```
   Returns all seven segments in one call.

2. Render LCRS (Layer-3 Concurrent Ready State) view:

```
LCRS - project: <name>
──────────────────────────────────────────────────
running       (2): wi_xxx "fix login bug" [Alice, last active 4min ago]
                   wi_yyy "add rate limiting" [Bot-1, last active 2h ago]
items         (5): wi_aaa (urgent) "critical DB migration" [queued]
                   wi_bbb (high)   "auth refactor" [queued]
                   ... +3 more
stalled       (1): wi_ccc [stalled 2 days ago: "waiting on human triage since
                   2026-09-20" - last actor: dahe.p]
paused        (0):
needs you     (1): wi_eee "design new billing API" [requires_human_session]
unclassified  (2): wi_fff (requires_human_session unresolved - the first claim
                   resolves it to true; claim it normally)
                   wi_ggg
stale running (1): wi_hhh "migrate search index" [running, held by Alice, last active 3h ago]
```

3. Highlight segments needing attention:
   - `needs you` -> bold, shown first
   - `stalled` -> show the stall_reason, stalled_since and last_actor_display the segment
     actually carries (these are human-triage stalls recorded by escalation, NOT dependency
     blocks: a separate pf_list_dependencies call is the only way to name blockers)
   - Idle `running` detection is server-side: an attempt with no server-side activity for
     2 hours (the server's `DefaultStaleRunningAfter` window) arrives as its own `stale_running`
     segment (ownership has no lease, so there is nothing to expire client-side)

4. Output three-segment format. "Next steps" section suggests the most important next action
   (e.g., "2 items need human-led sessions - run `/pf-work <slug>` to start").

## Output format notes

- `running`: show attempt owner (display name) and last-active age - the segment carries
  `id/slug/goal/owner_display/last_active_at` and nothing else (no attempt number, no expiry:
  the lease family was deleted in v1.21)
- `items` (ready queue): show priority, truncated goal (60 chars), status
- `stalled`: blocked wis carrying a `wi_stalled` escalation event - the segment carries
  `id`/`slug` plus the three diagnostic fields `stall_reason`, `stalled_since`,
  `last_actor_display` (no goal, no blocker id); render the diagnostics as given and never
  invent them - fetch the wi separately if more detail is wanted
- `needs you`: these are `requires_human_session=true` wi's awaiting a human session
- `stale_running`: running with no server-side activity for 2 hours (the server's stale
  window) - an ALERT, not an instruction: no ownership change is implied. Surface the holder
  and last-seen age to the user; taking over is an explicit human judgment (`/pf-work --force`),
  never the status skill's suggestion
- `unclassified`: QUEUED, dependency-unblocked wis whose `requires_human_session` is NULL
  (the classification was never explicitly set at filing). They ARE claimable - the first
  claim resolves NULL to `true` from a server default, which makes them human-session wis.
  (Missing `wi_type` is a separate, later claim refusal surfaced by `/pf-work` itself.)

## NL Triggers

- "status" / "progress" / "what's going on"
- "what work is there today"
- "what needs me" / "needs my attention"
- "ready queue" / "show queue" / "show all"
- `/pf-status` (direct command)
