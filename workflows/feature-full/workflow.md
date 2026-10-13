---
name: feature-full
steps:
  - { id: grill, context_policy: fresh, handoff: true, kind: qa, budget: { wall_minutes: 30 } }
  - { id: spec, context_policy: inherit, handoff: true, kind: spec, budget: { wall_minutes: 15 } }
  - { id: grill-review, context_policy: fresh, handoff: true, kind: qa, requires_human_answer: true, gate: true, budget: { wall_minutes: 30 }, on_fail: { target: spec } }
  - { id: plan, context_policy: inherit, handoff: true, kind: author, budget: { wall_minutes: 15 } }
  - { id: e2e-author, context_policy: fresh, handoff: true, kind: author, budget: { wall_minutes: 15 } }
  - { id: e2e-baseline, context_policy: fresh, handoff: true, kind: code, gate: false, budget: { wall_minutes: 20 } }
  - { id: code, context_policy: inherit, handoff: true, kind: code, budget: { wall_minutes: 30 }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: e2e-run, context_policy: fresh, handoff: true, kind: code, gate: true, budget: { wall_minutes: 20 }, on_fail: { target: code } }
  - { id: review, context_policy: fresh, handoff: false, kind: review, gate: true, budget: { wall_minutes: 20 }, on_fail: { target: code } }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, budget: { wall_minutes: 20 }, on_fail: { target: code } }
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
---

# feature-full — reference flow

This is the flow-level narrative for the workflow. It explains how the
ten steps compose and what flows between them; it is not itself a structural
schema — the frontmatter `steps` array above is the *only* execution order. Nothing here overrides it.

## Shape of the flow

```
grill --> spec --> grill-review --> plan --> e2e-author --> e2e-baseline --> code --> e2e-run --> review --> verification
```

The four gate FAIL edges, each declared in the frontmatter as that gate's `on_fail`
rewind target: `grill-review` --> `spec`, `e2e-run` --> `code`, `review` --> `code`
and `verification` --> `code`.

The flow has two front-loaded interactive checkpoints (`grill`, `grill-review`) that
replace a heavy spec-review loop with a leaner two-pass confirmation, an acceptance
lane that runs BEFORE the build (`e2e-author` authors scenarios against the *spec*;
`e2e-baseline` executes them against the pre-implementation tree and lays the
demonstrably-red bar), a build lane that implements against that executable
acceptance (`code` continues `e2e-baseline`'s session via its inherit handoff, then
`e2e-run` re-runs the same authored suite as the hard rerun gate, rewinding to `code`
on red), a judgment pair (`review` first, then `verification` as the last machine
gate). There is no release slot: shipping is the separate `release` bundle.

## Artifact flow between steps

Every step's output is a path recorded in `execution.json`'s `steps.<id>.output`, pointing at a
file under `artifacts/`; no step declares frontmatter `inputs:`, so downstream steps read upstream artifacts by that convention:

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
| `review` | `artifacts/review.md` (verdict) | `verification` |
| `verification` | `artifacts/verification-report.md` (verdict) | (terminal) |

`grill` and `grill-review` share one running document, `artifacts/grill-qa.md`, sectioned
by step id: `grill` appends its question/answer pairs first, `spec` reads the whole file
for context, and `grill-review` appends its own confirmation pass underneath.

## Rewind boundaries

Every gate in this flow declares its rewind target in the frontmatter `on_fail` — four
gates, two distinct targets (`spec`, `code`). This section only explains
*why* each target was picked:

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
  this).
- **`verification` FAILs -> rewind to `code`.** Verification is the last machine gate:
  it re-runs the full green bar as the final mechanical check, after `review`; a
  failure here means the implementation itself needs another pass, not the plan.

Workflow-level rewind bounds: this bundle's frontmatter deliberately omits the
`rewind` block, so the driver defaults apply — `max_total: 4`, `max_per_step: 2`. A gate that keeps failing past those bounds ends
the workflow in `failed_rewind_exhausted`, not an infinite loop.

## Budget guidance

There is no workflow-level budget; the bounds are the rewind limits above and each step's wall estimate. Projects
with larger codebases or slower CI should raise the per-step `budget.wall_minutes` on `code`/`e2e-run`.

Every step declares its own `budget.wall_minutes`: the checkpoints `grill` and `grill-review` get 30 (the
user's own thinking time is inside the step), the authoring steps `spec`/`plan`/`e2e-author` 15, the e2e
executions `e2e-baseline`/`e2e-run` 20, `code` 30, and the judgment gates `verification`/`review`
20 — 215 minutes in total.

The declared wall is an estimate, not a limit: the daemon bounds a step's total runtime across its
dispatches at T = max(3 × its `budget.wall_minutes`, the daemon's `--step-timeout`, default 90 minutes).
A step that declares nothing gets the default, so declare a wall only for a step that legitimately
needs more than 90 minutes in total.

## No release slot

This bundle has no release step. Releases and deploys never ride a delivery lane: they
happen only through the dedicated `release` workflow bundle (a separate WI), so shipping
is always an explicit, separately-gated act on a verified origin head.
