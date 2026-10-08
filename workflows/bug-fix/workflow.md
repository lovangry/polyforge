---
name: bug-fix
description: Root-cause lane - reproduce the failure first, prove the cause before fixing, then run the fix/verification/review/release tail.
rewind:
  max_total: 4
  max_per_step: 2
budget: { requests: 140, tokens: 350000, wall_minutes: 180 }
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
steps:
  - { id: repro, context_policy: fresh, handoff: true, kind: code, budget: { wall_minutes: 30 } }
  # root-cause is a gate: it must run FRESH (the judge never inherits the
  # judged conversation — driver rule, separation of powers) and takes the repro record (artifacts/repro.md — repro's declared output) through a DECLARED input instead: it reads the artifact,
  # repro evidence through a DECLARED input instead: it reads the artifact,
  # not the chat. (The inherit form violated the driver's gate rule and
  # made the whole lane unclaimable — caught at #108's first claim.)
  - { id: root-cause, context_policy: fresh, handoff: true, kind: qa, gate: true, budget: { wall_minutes: 20 }, inputs: [artifacts/repro.md], model_candidates: [sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra, sub2api-glm/glm-5.3], on_fail: { action: rewind, target: repro } }
  - { id: fix, context_policy: inherit, handoff: true, kind: code, budget: { wall_minutes: 30 }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, budget: { wall_minutes: 20 }, on_fail: { action: rewind, target: fix } }
  - { id: review, context_policy: fresh, handoff: false, kind: review, gate: true, budget: { wall_minutes: 20 }, on_fail: { action: rewind, target: fix } }
  - { id: release, context_policy: fresh, handoff: false, kind: code, gate: true, budget: { wall_minutes: 20 }, model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5], on_fail: { action: rewind, target: review } }
---

# bug-fix — root-cause lane

This is the flow-level narrative the WI Agent loads at step 0. It explains how the six
steps compose and what flows between them; it is not itself a structural schema — the
frontmatter `steps` array above is the *only* execution order (`01-workflow-skills.md`
§4.1 rule V3). Nothing here overrides it.

## Shape of the flow

```
repro --> root-cause --> fix --> verification --> review --> release
   ^         |                     |                |            |
   |   (FAIL: rewind to repro)  (FAIL: rewind to fix)  (FAIL: rewind to fix)  (FAIL: rewind to review)
```

The lane exists for bugs whose cause is NOT already known. Its contract with the rest of
the system is the evidence chain: a failure is captured as a reproducing artifact BEFORE
anyone theorizes about it, the theory is proven against that artifact BEFORE anyone
writes a fix, and the fix is validated by the same artifact that originally failed. A bug
whose root cause is already obvious from the WI text belongs on `quick-fix` instead; a
bug that turns out to need design decisions belongs on `feature-full`.

## Why `root-cause` is a gate (`kind: qa`, `gate: true`)

The schema allows any step to carry `gate: true` — `01-workflow-skills.md` §4.1 defines
the flag as "this step produces a business verdict" and lists review/verification/
grill-review as examples, not as an exclusive set. Root-cause analysis produces exactly
such a verdict, stated in the executor's gate convention (`01-workflow-skills.md` §6.2):
`pass` (the cause is identified and supported by evidence that explains the observed
repro; formerly worded `confirmed`) or `fail` (it is not, and proceeding to `fix` would
mean guessing; formerly `inconclusive`). The step's output schema uses
`enum: [pass, fail]` so no verdict-vocabulary mapping is ever needed — a reading agent
sees the same two words the gate machinery acts on. Making it a gate is also
structurally load-bearing rather than cosmetic:

- A `fail` verdict (inconclusive diagnosis) FAILs the gate and rewinds to `repro` — the
  correct bounded
  response is better evidence, not a blind fix attempt.
- Rule V10 ties non-gate `kind: qa` steps to the existence of a later `kind: spec` step.
  This lane has no spec step (bugs spec their corrected behavior inside the fix memo/
  commits, not via a dedicated authoring step), so `kind: qa` is only expressible here in
  its gate form — which V10 explicitly exempts from the position rule, same as
  feature-full's `grill-review`.

## Artifact flow between steps

Nothing here is wired through frontmatter. Every step's output is a path recorded in
`execution.json`'s `steps.<id>.output`, pointing at a file under `artifacts/`
(`03-execution-capsule.md` §4.2). Downstream steps read their inputs by following that
convention, not by any declared edge:

| step | primary artifact it writes | who reads it |
|---|---|---|
| `repro` | `artifacts/repro.md` (commands, environment, observed vs expected), plus a failing test under `repos/<name>/**` when one is authored | `root-cause`, `verification` |
| `root-cause` | `artifacts/root-cause.md` (verdict, cause, evidence) | `fix`, `review` |
| `fix` | changes under `repos/<name>/**`, `artifacts/fix-summary.md`, and the repro pinned as a regression test (required output `regression_test`; waived only with a review-visible reason) | `verification`, `review`, `release` |
| `verification` | `artifacts/verification-report.md` (verdict) | `review` |
| `review` | `artifacts/review.md` (verdict) | `release` |
| `release` | `artifacts/release-notes.md`, tag/version metadata | (terminal) |

The repro artifact is the spine of the lane: `root-cause` cites it as evidence, and
`verification` re-runs it — the originally failing case passing on the fixed tree is the
lane's core acceptance signal.

## Rewind boundaries

All rewind declarations live in the frontmatter `on_fail` of their gates; the mechanism
is `01-workflow-skills.md` §6.2 (rule V5: targets must exist and sit strictly earlier
than the failing step). This section explains the choices:

- **`root-cause` FAILs (fail verdict = inconclusive diagnosis) -> rewind to `repro`.**
  The analysis could not
  determine the cause from the evidence at hand. `repro` re-runs with the `fail_reason`
  (what was missing: intermittent behavior, missing logs, wrong environment) attached as
  its `pending_rewind_input`, and sharpens the reproduction accordingly.
- **`verification` FAILs -> rewind to `fix`.** Build/lint/tests — including the original
  repro case — did not pass; only `fix` can change the tree.
- **`review` FAILs -> rewind to `fix`.** Review found the fix wanting (wrong cause
  addressed, incomplete fix, or a fix that breaks something adjacent). `fix` re-runs
  with the blocking findings attached; `root-cause` is NOT a rewind target — if the
  review verdict says the diagnosis itself was wrong, `fix`'s re-run is expected to say
  so in its output and stop honestly rather than re-litigate the cause mid-flow (the
  composite-rewind ergonomics are a known open item, `01-workflow-skills.md` §12 item 2).
- **`release` FAILs -> rewind to `review`** (feature-full's default convention): a
  release failure is normally a release-blocking review concern resurfacing at ship time
  or a readiness judgment, not automatically evidence the code is wrong.

Workflow-level rewind bounds: `max_total: 4`, `max_per_step: 2`. A diagnosis-fix loop
that cannot converge in four bounded rewinds is telling you the WI is mis-scoped —
surface that honestly instead of burning the budget on repeat attempts.

## Budget guidance

The frontmatter workflow-level budget (`requests: 140, tokens: 350000,
wall_minutes: 180`) is sized between quick-fix and feature-full: six steps plus the
realistic rewinds of the two analysis-flavored lanes (`repro` re-runs are expected to be
cheap; `fix` re-runs are the expensive ones). Repos with slow CI should raise the
workflow-level ceiling or raise the per-step `budget.wall_minutes` on `fix` rather than
shrinking the verification set.

Every step declares its own `budget.wall_minutes`: the code-writing steps `repro`/`fix`
get 30, and `root-cause`, `verification`, `review` and `release` 20 each — summing to
140 minutes, under the 180-minute workflow ceiling so one in-budget pass never busts
it. The per-step wall is what the daemon watchdog's absolute backstop over a single
agent instance is computed from: `max(3× the step's declared wall, 15 min)`. A step
declaring nothing falls back to a 5-minute assumed wall, collapsing the backstop to its
15-minute floor and bounded-terminating agents that are still legitimately working
(measured attempts run 2–5 minutes healthy, 15–52 minutes in the slow tail). Declared
walls stay within `[5, 60]` by convention (enforced by
`internal/workflow/schema/bundles_test.go`): at least 5 so the backstop is genuinely
`3× wall` rather than clamped to the floor, at most 60 so one attempt never books more
than an hour.

## The release step (per-project convention)

`release` is an ordinary step slot reusing every existing step primitive
(`01-workflow-skills.md` §4.2). `agents/release.md` ships as a template skeleton —
version source, build, tag, publish target, channel promotion are per-project TODOs
each consuming project fills in, keeping the frontmatter contract (`name`,
`description`, `tools`, `output`) intact.
