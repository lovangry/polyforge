---
name: plan
description: Write the maintenance plan - ordered, compilable units of work with declared touched files - proportionate to the chore's actual size.
tools: [read, bash]
output:
  type: object
  required: [plan_path, step_ids, touched_files]
  properties:
    plan_path:
      type: string
      description: Capsule-relative path to the written plan artifact (artifacts/plan.md).
    step_ids:
      type: array
      minItems: 1
      items: { type: string }
      description: Ordered ids of every implementation step declared in the plan. One entry is the correct answer for a genuinely single-step chore. At least one is required (minItems 1) - a plan with zero steps is an unfilled template.
    touched_files:
      type: array
      minItems: 1
      items:
        type: object
        required: [path, intent]
        properties:
          path: { type: string, description: "repo-relative path" }
          intent: { type: string, enum: [read, write] }
      description: Deduplicated list of every file the plan declares as read or written, across all steps (write wins over read for the same path). At least one entry is required (minItems 1) - a plan that touches nothing leaves `code` nothing to commit.
---

# plan

Write the maintenance plan for this work item, grounded in `runtime-state/work-item.json`
(the daemon-materialized task definition — read it in full; do not plan against your own
recollection of the ask).

## Proportionality is the contract

This is a normal step that always runs — the schema has no optional steps, and this
lane does not invent any. What scales with the chore's size is the plan's *depth*, not
whether it exists:

- A dependency bump, a config flag flip, a one-file mechanical edit: a plan declaring
  exactly one trivial step is the *correct* plan, not a degenerate one. Do not pad it.
- A sweep (rename across many files, lint-rule rollout, dependency chain upgrade):
  enumerate the units so `code` can commit per compilable unit and keep the tree green
  between commits. A sweep planned as one undifferentiated blob is a plan failure.

## What to do

1. Read `runtime-state/work-item.json` in full.
2. Determine the unit structure: if the change cannot land in one compilable unit, order
   the steps so each leaves the tree buildable — same discipline as feature-full's
   `plan`, minus the spec input (chores carry no product spec).
3. Every step MUST carry a `Touched files: <repo-relative path> (write|read), ...` line,
   or `Touched files: (no file edits)` for steps that only verify. Declare honestly —
   this list becomes the WI's `declared_resources` and, downstream, its file-scope
   locks; an under-declared write is a real lock gap.
4. Write `artifacts/plan.md` with an ordered `## Steps` section, then emit the ordered
   step ids and the deduplicated touched-files list.
5. BEHAVIOR-FACE ESCALATION (change-coverage rules, discipline ① — hard rule): a
   chore graph authors no acceptance scenarios. If reading the WI reveals the work
   alters OBSERVABLE BEHAVIOR — new endpoint/verb, protocol/contract/semantics
   change, ownership or fence rule change — you cannot proceed silently. The
   chore graph has no interactive checkpoint, so the escalation routes through
   the WI agent's needs_input channel: emit the plan anyway BUT set `out_of_scope:
   "behavior-face: requires an e2e-carrying graph"` in your output summary, so
   the WI agent raises the owner question before running `code`. A behavior-facing
   change executed on the chore graph without an owner waiver on record is a
   contract violation.

## Non-goals

- Do not write code — that is `code`'s job.
- Do not add scope the WI did not ask for. The classic chore failure is the dependency
  bump that becomes a refactor; if adjacent work is genuinely needed, say so in the plan
  as a called-out exclusion and let a human decide, rather than silently including it.

## Output

Emit the final JSON object per the schema above: the artifact path, the ordered step
ids, and the deduplicated touched-files list (each path once, `write` intent winning
over `read` for the same path).
