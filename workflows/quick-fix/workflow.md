---
name: quick-fix
description: Light remediation flow - one interactive checkpoint, a single fix pass, a machine gate, and a release slot, sized for small well-understood changes.
rewind:
  max_total: 2
  max_per_step: 2
budget: { requests: 60, tokens: 150000, wall_minutes: 180 }
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
steps:
  - { id: grill, context_policy: fresh, handoff: true, kind: spec, interactive: true, requires_human_answer: true, budget: { wall_minutes: 30 } }
  - { id: fix, context_policy: inherit, handoff: true, kind: code, budget: { wall_minutes: 30 }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, budget: { wall_minutes: 20 }, on_fail: { action: rewind, target: fix } }
  - { id: release, context_policy: fresh, handoff: false, kind: code, gate: true, budget: { wall_minutes: 20 }, model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5], on_fail: { action: rewind, target: fix } }
---

# quick-fix — light remediation flow

This is the flow-level narrative for the workflow. It explains how the four
steps compose and what flows between them; it is not itself a structural schema — the
frontmatter `steps` array above is the *only* execution order (`01-workflow-skills.md`
§4.1 rule V3). Nothing here overrides it.

## Shape of the flow

```
grill --> fix --> verification --> release
                |                  |
          (FAIL: rewind to fix)  (FAIL: rewind to fix)
```

The flow collapses the feature-full clarification trio (`grill` -> `spec` ->
`grill-review`) into ONE front-loaded interactive checkpoint (`grill`), then runs a
single fix pass, one machine-checkable gate, and the release slot. It is for work whose
scope is already small and well understood: a bounded bug fix, a small config or
dependency change, a tiny feature. If a WI routed here turns out to need a real spec or a
separate review pass, that is a signal it was misrouted — cancel and re-file against
`feature-full` or `bug-fix` rather than stretching this lane.

## Why `grill` is `kind: spec` (not `kind: qa`)

In `feature-full`, the pre-spec clarification step is `kind: qa` and the schema
structurally guarantees it precedes the spec step (rule V10: any `kind: qa` non-gate step
requires a `kind: spec` step to exist later in the array). This bundle deliberately has
no separate spec authoring step, so a non-gate `kind: qa` checkpoint is not expressible
here — and it should not be: this step does not stop at gathering answers, it commits
them to a micro-spec (`artifacts/fix-memo.md`) that `fix` implements directly. That is
spec work, so it is declared `kind: spec`, `interactive: true`. Interactive steps have no
positional restriction (rule V4), so the checkpoint is legal in first position.

## Artifact flow between steps

Nothing here is wired through frontmatter. Every step's output is a path recorded in
`execution.json`'s `steps.<id>.output`, pointing at a file under `artifacts/`
(`03-execution-capsule.md` §4.2). Downstream steps read their inputs by following that
convention, not by any declared edge:

| step | primary artifact it writes | who reads it |
|---|---|---|
| `grill` | `artifacts/grill-qa.md` (question/answer log), `artifacts/fix-memo.md` (micro-spec) | `fix` |
| `fix` | changes under `repos/<name>/**`, `artifacts/fix-summary.md` | `verification`, `release` |
| `verification` | `artifacts/verification-report.md` (verdict) | `release` |
| `release` | `artifacts/release-notes.md`, tag/version metadata | (terminal) |

## Rewind boundaries

Both gates in this flow declare `on_fail: {action: rewind, target: fix}` — the mechanism
is `01-workflow-skills.md` §6.2, this section explains the choice. Rule V5 requires a
rewind target to exist in the `steps` array and sit strictly earlier than the failing
step; both candidates available to `release` (`verification`, `fix`) satisfy that
mechanically, and this bundle picks `fix` deliberately:

- **`verification` FAILs -> rewind to `fix`.** Verification is the machine gate
  (build/lint/tests); a failure means the tree is wrong, and `fix` is the only step that
  can change it.
- **`release` FAILs -> rewind to `fix`, not `verification`.** Rewinding to
  `verification` would be legal but wasteful: verification is a read-only observer — its
  agent contract forbids modifying files — so it cannot act on a release verdict; it
  would re-run the same checks, pass, and hand an unchanged tree back to `release` to
  fail identically until `rewind.max_per_step` exhausts. Pointing at `fix` instead resets
  the whole `[fix, release)` interval — `fix` AND `verification` both go back to
  `pending` — so the release verdict reaches the one step that can change the tree
  (e.g. a version constant, a changelog entry, a build-flag the release procedure needs)
  and the mechanical gate still re-validates afterwards as part of the interval re-run.

Workflow-level rewind bounds for this bundle are declared once in the frontmatter:
`max_total: 2`, `max_per_step: 2`. A quick fix that cannot land within two bounded
rewinds is not a quick fix; past those bounds the workflow ends in
`failed_rewind_exhausted` and the honest response is a re-filed WI on a heavier lane,
not more loops.

## Budget guidance

The frontmatter workflow-level budget (`requests: 60, tokens: 150000, wall_minutes: 180`)
is sized for one pass over four steps plus at most the declared two rewinds. This is the
tightest reference budget on purpose: the lane's contract is that the WI is small. Do
not raise the budget to make an oversized WI fit — re-route the WI instead. Hitting the
budget is a resource ceiling (`WF-103 OVER_BUDGET`), unrelated to gate verdicts, and
never triggers rewind.

Every step also declares its own `budget.wall_minutes` — `grill: 30` (interactive, the
user's own thinking time is inside the step), `fix: 30`, `verification: 20`,
`release: 20`, summing to 100 minutes, comfortably under the 180-minute workflow
ceiling so one in-budget pass never busts it. These are not decorative: the daemon
watchdog's absolute backstop over a single agent instance is `max(3× the step's
declared wall, 15 min)`, and a step that declares nothing falls back to a 5-minute
assumed wall, collapsing the backstop to its 15-minute floor. Measured attempts span
2–5 minutes healthy and 15–52 minutes in the slow tail, so an undeclared step gets
bounded-terminated while still legitimately working. Declared walls stay within
`[5, 60]` by convention (enforced by `internal/workflow/schema/bundles_test.go`): at
least 5 so the backstop is genuinely `3× wall` rather than clamped to the floor, at
most 60 so one attempt never books more than an hour.

## The release step (per-project convention)

`release` is an ordinary step slot, not a distinct mechanism (`01-workflow-skills.md`
§4.2). `agents/release.md` ships as a template skeleton: version source, build, tag,
publish target, channel promotion are all per-project TODOs each consuming project
fills in, keeping the frontmatter contract (`name`, `description`, `tools`, `output`)
intact.
