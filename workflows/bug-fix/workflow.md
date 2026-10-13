---
name: bug-fix
description: Root-cause lane - reproduce the failure first, prove the cause before fixing, then run the fix/verification/review tail.
rewind:
  max_total: 4
  max_per_step: 2
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
steps:
  - { id: repro, context_policy: fresh, handoff: true, kind: code, budget: { wall_minutes: 30 } }
  # root-cause is a gate: it must run FRESH (the judge never inherits the
  # judged conversation — driver rule, separation of powers) and takes the
  # repro record (artifacts/repro.md) by path convention: it reads the
  # artifact, not the chat.
  - { id: root-cause, context_policy: fresh, handoff: true, kind: qa, gate: true, budget: { wall_minutes: 20 }, model_candidates: [sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra, sub2api-glm/glm-5.3], on_fail: { target: repro } }
  - { id: fix, context_policy: inherit, handoff: true, kind: code, budget: { wall_minutes: 30 }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, budget: { wall_minutes: 20 }, on_fail: { target: fix } }
  - { id: review, context_policy: fresh, handoff: false, kind: review, gate: true, budget: { wall_minutes: 20 }, on_fail: { target: fix } }
---

# bug-fix — root-cause lane

This is the flow-level narrative for the workflow. It explains how the five
steps compose and what flows between them; it is not itself a structural schema — the
frontmatter `steps` array above is the *only* execution order. Nothing here overrides it.

## Shape of the flow

```
repro --> root-cause --> fix --> verification --> review
   ^         |                     |                |
   |   (FAIL: rewind to repro)  (FAIL: rewind to fix)  (FAIL: rewind to fix)
```

The lane exists for bugs whose cause is NOT already known. Its contract with the rest of
the system is the evidence chain: a failure is captured as a reproducing artifact BEFORE
anyone theorizes about it, the theory is proven against that artifact BEFORE anyone
writes a fix, and the fix is validated by the same artifact that originally failed. A bug
whose root cause is already obvious from the WI text belongs on `quick-fix` instead; a
bug that turns out to need design decisions belongs on `feature-full`.

## Why `root-cause` is a gate (`kind: qa`, `gate: true`)

Any step may carry `gate: true`: the flag means "this step produces a business verdict"
(review, verification and grill-review are examples, not an exclusive set). Root-cause analysis
produces exactly such a verdict: `pass` (the cause is identified and supported by evidence that
explains the observed repro) or `fail` (it is not, and proceeding to `fix` would mean guessing). The step's output schema uses
`enum: [pass, fail]` so no verdict-vocabulary mapping is ever needed — a reading agent
sees the same two words the gate machinery acts on. Making it a gate is also
structurally load-bearing rather than cosmetic:

- A `fail` verdict (inconclusive diagnosis) FAILs the gate and rewinds to `repro` — the
  correct bounded
  response is better evidence, not a blind fix attempt.
- This lane has no spec step (bugs spec their corrected behavior inside the fix memo/
  commits, not via a dedicated authoring step), so its `kind: qa` step is declared in its
  gate form, like feature-full's `grill-review`.

## Artifact flow between steps

Every step's output is a path recorded in `execution.json`'s `steps.<id>.output`, pointing at a
file under `artifacts/`. Downstream steps read the artifacts they need by following that path
convention — the driver injects nothing and there is no frontmatter declaration for it
(`root-cause`, for instance, simply reads `artifacts/repro.md`).

| step | primary artifact it writes | who reads it |
|---|---|---|
| `repro` | `artifacts/repro.md` (commands, environment, observed vs expected), plus a failing test under `repos/<name>/**` when one is authored | `root-cause`, `verification` |
| `root-cause` | `artifacts/root-cause.md` (verdict, cause, evidence) | `fix`, `review` |
| `fix` | changes under `repos/<name>/**`, `artifacts/fix-summary.md`, and the repro pinned as a regression test (required output `regression_test`; waived only with a review-visible reason) | `verification`, `review` |
| `verification` | `artifacts/verification-report.md` (verdict) | `review` |
| `review` | `artifacts/review.md` (verdict) | (terminal) |

The repro artifact is the spine of the lane: `root-cause` cites it as evidence, and
`verification` re-runs it — the originally failing case passing on the fixed tree is the
lane's core acceptance signal.

## Rewind boundaries

All rewind declarations live in the frontmatter `on_fail` of their gates; a target must
exist and sit strictly earlier than the failing step. This section explains the choices:

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
  so in its output and stop honestly rather than re-litigate the cause mid-flow.

Workflow-level rewind bounds: `max_total: 4`, `max_per_step: 2`. A diagnosis-fix loop
that cannot converge in four bounded rewinds is telling you the WI is mis-scoped —
surface that honestly instead of burning the budget on repeat attempts.

## Budget guidance

There is no workflow-level budget; the bounds are the rewind limits above and each step's wall estimate. Repos
with slow CI should raise the per-step `budget.wall_minutes` on `fix` rather than shrinking the verification set.

Every step declares its own `budget.wall_minutes`: the code-writing steps `repro`/`fix` get 30, and
`root-cause`, `verification` and `review` 20 each — 120 minutes in total.

The declared wall is an estimate, not a limit: the daemon bounds a step's total runtime across its
dispatches at T = max(3 × its `budget.wall_minutes`, the daemon's `--step-timeout`, default 90 minutes).
A step that declares nothing gets the default, so declare a wall only for a step that legitimately
needs more than 90 minutes in total.
