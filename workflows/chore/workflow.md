---
name: chore
description: Maintenance lane - plan, code, verify. No review step, no release slot; chores do not ship.
rewind:
  max_total: 2
  max_per_step: 2
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
steps:
  - { id: plan, context_policy: fresh, handoff: true, kind: author, budget: { wall_minutes: 15 } }
  - { id: code, context_policy: inherit, handoff: true, kind: code, budget: { wall_minutes: 30 }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, budget: { wall_minutes: 20 }, on_fail: { target: code } }
---

# chore — maintenance flow

This is the flow-level narrative for the workflow. It explains how the three
steps compose; it is not itself a structural schema — the frontmatter `steps` array above
is the *only* execution order. Nothing here overrides it.

## Shape of the flow

```
plan --> code --> verification
                |
        (FAIL: rewind to code)
```

The lane covers maintenance work with no product behavior to judge: dependency bumps,
tooling and lint config updates, mechanical refactors, doc sweeps, test hygiene. Its two
deliberate omissions define it:

- **No review step.** There is no human-quality judgment to make — a chore is judged
  mechanically (does the tree still build, lint, and pass its tests after the change).
  If a "chore" turns out to need design or quality judgment, it was misrouted: cancel
  and re-file on `feature-full` or `bug-fix`.
- **No release slot.** Chores do not ship. They merge as ordinary commits and ride the
  project's next release train; the release hook belongs to whichever WI carries that
  release (release is an ordinary step slot, and this bundle simply does not include one).

## Artifact flow between steps

Every step's output is a path recorded in `execution.json`'s `steps.<id>.output`, pointing at a
file under `artifacts/`; no step declares frontmatter `inputs:`, so downstream steps read upstream artifacts by that convention:

| step | primary artifact it writes | who reads it |
|---|---|---|
| `plan` | `artifacts/plan.md` (may legitimately be a single trivial step for a small chore) | `code` |
| `code` | changes under `repos/<name>/**`, `artifacts/code-summary.md` | `verification` |
| `verification` | `artifacts/verification-report.md` (verdict) | (terminal) |

## `plan` is a normal step, not an optional one

The schema has no optional-step semantics, and this bundle does not invent any: `plan`
is a full step that always runs. Its contract is *proportionality* instead — for a
one-line dependency bump, a plan listing exactly one trivial step is the correct,
complete plan; for a sweep across many files, the plan enumerates the units so `code`
can keep the tree green between commits. Skipping the thinking because the chore is
small is not an option; writing the small thought down is cheap.

## Rewind boundaries

- **`verification` FAILs -> rewind to `code`.** The only gate's only sensible target:
  build/lint/tests failed on the changed tree, and `code` is the only step that can
  change it.

Workflow-level rewind bounds are the tightest of the reference bundles:
`max_total: 2`, `max_per_step: 2`. A chore that needs more than two bounded attempts to
keep the tree green is either not mechanical or is fighting a real problem — surface it
honestly instead of burning the budget.

## Budget guidance

There is no workflow-level budget; the bounds are the rewind limits above and each step's wall estimate. Slow-CI
repos should raise the `code` and `verification` walls rather than thin the verification set.

Every step declares its own `budget.wall_minutes` — `plan: 15`, `code: 30`, `verification: 20`, 65 minutes
in total, matching the lane's contract: mechanical work, known shape, no judgment passes.

The declared wall is an estimate, not a limit: the daemon bounds a step's total runtime across its
dispatches at T = max(3 × its `budget.wall_minutes`, the daemon's `--step-timeout`, default 90 minutes).
A step that declares nothing gets the default, so declare a wall only for a step that legitimately
needs more than 90 minutes in total.
