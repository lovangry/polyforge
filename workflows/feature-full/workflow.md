---
name: feature-full
steps:
  - { id: grill, context_policy: fresh, handoff: true, kind: qa, interactive: true, requires_human_answer: true, budget: { wall_minutes: 30 } }
  - { id: spec, context_policy: inherit, handoff: true, kind: spec, budget: { wall_minutes: 15 } }
  - { id: grill-review, context_policy: fresh, handoff: true, kind: qa, interactive: true, requires_human_answer: true, gate: true, budget: { wall_minutes: 30 }, on_fail: { action: rewind, target: spec } }
  - { id: plan, context_policy: inherit, handoff: true, kind: author, budget: { wall_minutes: 15 } }
  - { id: e2e-author, context_policy: fresh, handoff: true, kind: author, budget: { wall_minutes: 15 } }
  - { id: e2e-baseline, context_policy: fresh, handoff: true, kind: code, gate: false, budget: { wall_minutes: 20 } }
  - { id: code, context_policy: inherit, handoff: true, kind: code, budget: { wall_minutes: 30 }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: e2e-run, context_policy: fresh, handoff: true, kind: code, gate: true, budget: { wall_minutes: 20 }, on_fail: { action: rewind, target: code } }
  - { id: review, context_policy: fresh, handoff: false, kind: review, gate: true, budget: { wall_minutes: 20 }, on_fail: { action: rewind, target: code } }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, budget: { wall_minutes: 20 }, on_fail: { action: rewind, target: code } }
  - { id: release, context_policy: fresh, handoff: false, kind: code, gate: true, budget: { wall_minutes: 15 }, model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5], on_fail: { action: rewind, target: review } }
budget: { requests: 200, tokens: 500000, wall_minutes: 240 }
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
---

# feature-full — reference flow

This is the flow-level narrative the WI Agent loads at step 0. It explains how the
eleven steps compose and what flows between them; it is not itself a structural
schema — the frontmatter `steps` array above is the *only* execution order
(`01-workflow-skills.md` §4.1 rule V3). Nothing here overrides it.

## Shape of the flow

```
grill --> spec --> grill-review --> plan --> e2e-author --> e2e-baseline --> code --> e2e-run --> review --> verification --> release
```

The five gate FAIL edges, each declared in the frontmatter as that gate's `on_fail`
rewind target: `grill-review` --> `spec`, `e2e-run` --> `code`, `review` --> `code`,
`verification` --> `code`, and `release` --> `review`.

The flow has two front-loaded interactive checkpoints (`grill`, `grill-review`) that
replace a heavy spec-review loop with a leaner two-pass confirmation, an acceptance
lane that runs BEFORE the build (`e2e-author` authors scenarios against the *spec*;
`e2e-baseline` executes them against the pre-implementation tree and lays the
demonstrably-red bar), a build lane that implements against that executable
acceptance (`code` continues `e2e-baseline`'s session via its inherit handoff, then
`e2e-run` re-runs the same authored suite as the hard rerun gate, rewinding to `code`
on red), a judgment pair (`review` first, then `verification` as the last machine
gate), and a release slot at the end that is a normal step, not a special mechanism.

## Artifact flow between steps

Nothing here is wired through frontmatter. Every step's output is a path recorded in
`execution.json`'s `steps.<id>.output`, pointing at a file under `artifacts/`
(`03-execution-capsule.md` §4.2). Downstream steps read their inputs by following that
convention, not by any declared edge:

| step | primary artifact it writes | who reads it |
|---|---|---|
| `grill` | `artifacts/grill-qa.md` (grill section) | `spec` |
| `spec` | `artifacts/spec.md` | `grill-review`, `plan`, `e2e-author` |
| `grill-review` | `artifacts/grill-qa.md` (grill-review section, appended) | `spec` (only on rewind, via `pending_rewind_input`) |
| `plan` | `artifacts/plan.md` | `code` |
| `e2e-author` | `artifacts/e2e-scenarios.md` (+ generated test files under `repos/<name>/**` if the project runs them from the tree) | `e2e-baseline`, `e2e-run` |
| `e2e-baseline` | `artifacts/e2e-baseline.md` (the laid bar: one fail entry per scenario with evidence) | `code` (primary, via its inherit handoff), `review` (consultative) |
| `code` | changes under `repos/<name>/**`, `artifacts/code-summary.md` | `e2e-run`, `verification`, `review` |
| `e2e-run` | `artifacts/e2e-results.md` (rerun verdict + per-scenario results) | `review` |
| `review` | `artifacts/review.md` (verdict) | `verification`, `release` |
| `verification` | `artifacts/verification-report.md` (verdict) | `release` |
| `release` | `artifacts/release-notes.md`, tag/version metadata | (terminal) |

`grill` and `grill-review` share one running document, `artifacts/grill-qa.md`, sectioned
by step id: `grill` appends its question/answer pairs first, `spec` reads the whole file
for context, and `grill-review` appends its own confirmation pass underneath.

## Rewind boundaries

Every gate in this flow declares its rewind target in the frontmatter `on_fail` — five
gates, three distinct targets (`spec`, `code`, `review`). This section only explains
*why* each target was picked, the mechanism is `01-workflow-skills.md` §6.2:

- **`grill-review` FAILs -> rewind to `spec`.** The user's confirmation pass found the
  spec itself wrong (not just the confirmation script). Rewinding to `spec` re-runs
  spec authoring with the FAIL verdict attached as input; `grill` is *not* re-run —
  the raw requirements gathered there are still valid, only the spec's interpretation
  of them needs correction.
- **`e2e-run` FAILs -> rewind to `code`.** The rerun gate found the implementation
  under-delivering the laid bar: a red scenario after `code` is an implementation
  failure, not review material — the bar was laid and visible before `code` ran.
  Rewinding to `code` re-runs the build step with the rerun verdict attached as
  `pending_rewind_input`; the authored scenarios and `artifacts/e2e-baseline.md` are
  still valid, so the acceptance lane (`e2e-author`/`e2e-baseline`) is not re-run.
- **`review` FAILs -> rewind to `code`.** Human/senior review found the implementation
  wanting after the e2e rerun passed; `code` is re-run with the review verdict
  attached. `plan` is not re-run unless the reviewer's verdict says the plan itself was
  wrong, in which case the plan step's own re-authoring happens as part of the `code`
  step consuming the verdict text (the flow does not need a third rewind target for
  this — see `01-workflow-skills.md` §12 item 2 on composite rewind ergonomics).
- **`verification` FAILs -> rewind to `code`.** Verification is the last machine gate:
  it re-runs the full green bar as the final mechanical check, after `review` and
  before `release`; a failure here means the implementation itself needs another pass,
  not the plan.
- **`release` FAILs -> rewind to `review`** (this bundle's default; a project may
  override `release`'s `on_fail.target` to `code` if its release gate is really a code
  gate). A release failure re-runs review because the failure is normally either a
  release-blocking review comment resurfacing at ship time, or a release-gate check that
  is itself business judgment about readiness — not automatically evidence the code is
  wrong.

Workflow-level rewind bounds: this bundle's frontmatter deliberately omits the
`rewind` block, so the schema defaults apply — `max_total: 4`, `max_per_step: 2`
(`01-workflow-skills.md` §4.1 rule V6). A gate that keeps failing past those bounds ends
the workflow in `failed_rewind_exhausted`, not an infinite loop.

## Budget guidance

The frontmatter workflow-level budget (`requests: 200, tokens: 500000, wall_minutes: 240`)
is sized for one full pass through all eleven steps plus one rewind of the most
expensive lane (`code` + `e2e-run` + `review` + `verification`). Projects with larger
codebases or slower CI should raise the workflow-level ceiling and/or raise the
per-step `budget.wall_minutes` on `code`/`e2e-run` rather than trying to fit a bigger
repo into the reference default. Do not treat hitting the budget as a quality signal —
it is purely a resource ceiling (`WF-103 OVER_BUDGET`), unrelated to gate verdicts, and
never triggers rewind.

Every step declares its own `budget.wall_minutes`: the interactive checkpoints `grill`
and `grill-review` get 30 (the user's own thinking time is inside the step), the
authoring steps `spec`/`plan`/`e2e-author` 15, the e2e executions
`e2e-baseline`/`e2e-run` 20, `code` 30, `release` 15, and the judgment gates
`verification`/`review` 20 — summing to 230 minutes, under the 240-minute workflow
ceiling so one in-budget pass never busts it. The per-step wall is what the daemon
watchdog's absolute backstop over a single agent instance is computed from:
`max(3× the step's declared wall, 15 min)`. A step declaring nothing falls back to a
5-minute assumed wall, collapsing the backstop to its 15-minute floor and bounded-
terminating agents that are still legitimately working (measured attempts run 2–5
minutes healthy, 15–52 minutes in the slow tail, and interactive steps routinely
exceed 15). Declared walls stay within `[5, 60]` by convention (enforced by
`internal/workflow/schema/bundles_test.go`): at least 5 so the backstop is genuinely
`3× wall` rather than clamped to the floor, at most 60 so one attempt never books more
than an hour.

## The release step (per-project convention)

`release` is an ordinary step slot, not a distinct mechanism — it reuses every existing
step primitive (inputs, `output` schema, worktree, budget, rewind;
`01-workflow-skills.md` §4.2). The version shipped in this reference bundle
(`agents/release.md`) is deliberately generic: it defines the shape of a release step
(what a release verdict/output looks like) without hard-wiring a specific version scheme,
build system, tagging convention, or publish target, because those are legitimately
per-project. Each project consuming this bundle as a template is expected to replace the
body of `agents/release.md` with its own concrete release procedure while keeping the
frontmatter contract (`name`, `description`, `tools`, `output`) intact. The platform's
own release of aihub and the work-machine binary (two build outputs) is just one
instance of this same hook, not a special case.
