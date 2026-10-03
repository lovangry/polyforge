## Post-claim Next steps Routing

When a `requires_human_session=true` wi is being viewed or operated on - any three-segment
output that lists "Next steps" (post-claim via `/pf-work`, status view via `/pf-status`,
mid-flow output from `/pf-spec`, `/pf-plan`, `/pf-retro`, `/pf-stop`, etc.) - the
"Next steps" section is **mechanically populated** from the table below. No LLM judgment,
no session-context inference. This rule applies to **all skills** emitting three-segment
output when the user is operating inside a claimed wi.

### Source of `wi_type` (CRITICAL)

`wi_type` MUST be read from one of:

- the `wi_type` field in the `pf_claim_work_item` response (preferred - just returned)
- the `wi_type` field in `pf_get_work_item` (canonical, always current)
- the `wi_type` field shown in the wi detail page

**FORBIDDEN**: inferring `wi_type` from the wi's `goal` text, slug name, labels, parent
type, or recent session context. A wi with `goal` starting `"feat:"`, `"fix:"`, or
`"release"` may still have a completely different `wi_type` - for instance, a `chore`
that edits a release scenario file has `wi_type=chore` even though its goal mentions
"release". Treat the API field as the only authority.

### Routing table

| wi_type | primary next step | alternates |
|---|---|---|
| `chore` | `/pf-execute` - iterate step graph directly | `/pf-stop --pause` |
| `fix_bug` | `/pf-execute` - start from prepare_context | `/pf-spec --debug` (if root cause unclear); `/pf-stop --pause` |
| `feature` | `/pf-execute` - start from prepare_context | `/pf-spec` (if scope unclear); `/pf-plan` (if approach contested); `/pf-stop --pause` |
| `critical_bug` | `/pf-spec --debug` - root cause first | `/pf-execute`; `/pf-stop --pause` |
| `release` | `/pf-release` (or `/pf-execute` against release graph) | `/pf-stop --pause` |
| _(no match - default)_ | `/pf-spec` - define scope before acting | `/pf-stop --pause` |

### Mandatory output rules for "Next steps"

1. The **first** item under "Next steps" MUST be the table's `primary` cell for the matched
   `wi_type`, copied verbatim (slash-command + its description).
2. **No suggestion may appear before the primary** - not `/pf-spec`, not `/pf-plan`,
   not any LLM-improvised option. The primary is always row 1.
3. Alternates from the table follow the primary as rows 2..N, in the order listed in
   the table cell, one per row.
4. Suggestions outside the table (e.g., `wi.content` explicitly invites a different
   skill, or memory recall surfaces a relevant procedure) may be appended **AFTER** all
   table-derived rows, each marked `_(from wi.content)_` or `_(from memory <mem_id>)_`.
   This is the **only** legitimate way to add to the list.
5. If the `wi_type` does not match any row, use the `_(no match - default)_` row.

### Worked example

A wi has `wi_type=chore`, `requires_human_session=true`, goal `"feat: add CI hook for
descriptions"`. Despite the misleading "feat:" prefix in the goal, the API field says
`chore`. The "Next steps" output - from `/pf-work` post-claim, from `/pf-status`, or from
any other three-segment skill - is:

```
## Next steps
- `/pf-execute` - iterate step graph directly
- `/pf-stop --pause`
```

That is the entire "Next steps" - primary on row 1, the sole alternate on row 2, nothing
inserted before. If `wi.content` happened to invite `/pf-spec`, it would appear on
row 3 marked `_(from wi.content)_`, never on row 1.

### Revision after review

When a human reviewer has added section annotations to a spec or plan artifact in the /ui
viewer, the "Next steps" for the reviewing session should include `/pf-revise`. This applies
in any skill output (three-segment) after a spec/plan is saved and sent for review:

```
## Next steps
- ...  (primary from routing table above)
- `/pf-revise` - if reviewer has annotated the spec/plan, run this to apply the feedback
                  and resolve all open annotations in one round
```

`/pf-revise` is an **alternate** suggestion - never the primary - unless `wi.content`
explicitly invites it. After `/pf-revise` completes, the reviewer may annotate the NEW
head version for another round.

### Why this table exists

`/pf-execute` is itself data-driven over the wi's own graph (pinned DB flow; aihub#788),
so it is the universal correct entry for any wi that has a graph. The table only encodes
exceptions (`release`, `critical_bug`, `default`). `/pf-spec` and `/pf-plan` are escape valves - listed
as alternates, never as primary for `chore`/`fix_bug`/`feature`, because their graphs start
with code-side steps, not a spec discussion.

The `rhs=false` path emits no three-segment output, so this table does not apply to it.
`fragments/post-claim-dispatch.md` (resident) is authoritative for that branch.

### Claiming always walks the wi's actual graph - `rhs` decides who drives, not whether (aihub#685, aihub#788)

A claim on any wi means walking the graph the wi actually owns: a pinned DB workflow
generation (`pf_get_workflow` steps_version > 0) is the only execution path (aihub#788).
`requires_human_session` (`rhs`) picks the driver, nothing else:

- `rhs=false` - claim auto-dispatches `/pf-execute` (`fragments/post-claim-dispatch.md`,
  resident - that fragment carries the short, actionable form of this whole rule; this
  section is the full argument behind it).
- `rhs=true` - a human paces `/pf-execute` step by step, in this session. Same graph,
  same steps, same recorded `artifact_summary` per step - only who advances it differs.

A steps_version==0 wi is legacy/uncomposed stock: `/pf-execute`'s diagnostic branch reports
it for migration - pin a DB flow via `pf_update_workflow`, or apply the owner's
migrate/freeze/close disposition per the aihub#788 artifact; the retired scenario loop
never runs. Measured 2026-09-15 on the 25 most recently wrapped wi's: 16 walked their
graph (`wi_step_state.version` 6-18); the other 9 (`version=0`) were, every one,
dispatched with a hand-written path instead of an outcome - see the next section.
Skipping the graph is not a smaller version of doing the work: it means no `spec`/`plan`,
no mandatory clean-context reviewer, and no traceable `artifact_summary` per step.
`aihub#673`'s `code_review` step returned a BLOCKER that exact way - a reviewer with no
memory of writing the fix caught it where the author would not have.

### Dispatching a wi is not claiming it - say WHAT you want, never HOW to get there

The rule above binds the claimant. It has a mirror for whoever writes the prompt that hands
a wi out, and that half had no home anywhere before this section: state the desired outcome
and the acceptance criteria, and stop there. Do not write the execution path - not even a
"helpful" sketch of one.

Same dispatcher, same day, two phrasings, two different outcomes, measured:

- *"Claim it, get it done, open a PR."* -> the receiver claims, then does what claiming a wi
  means: `/pf-execute` runs the full step graph.
- *"Implement per the design section, then `pf_commit`, push, open a PR."* -> the receiver
  follows that literally. Zero step records. The instruction was not wrong, it was simply
  more specific than "walk the graph" and a more specific instruction wins.

The fix is not "remember to mention `/pf-execute`" - it is to never author a path in the
first place. A dispatcher who only ever states outcome + acceptance criteria cannot produce
this failure, because there is no path in the prompt to follow instead of the graph. This is
also not conditional on the wi looking simple: see the paragraph above - no `wi_type` in this
scenario repo lacks a graph, so there is no size or kind of change for which prescribing the
path is the cheaper-but-still-correct option.
