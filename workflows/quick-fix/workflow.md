---
name: quick-fix
description: Light remediation flow - one interactive checkpoint, a single fix pass and a machine gate, sized for small well-understood changes.
rewind:
  max_total: 2
  max_per_step: 2
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
steps:
  - { id: grill, context_policy: fresh, handoff: true, kind: spec, requires_human_answer: if_asked }
  - { id: fix, context_policy: inherit, handoff: true, kind: code }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, on_fail: { target: fix }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: deliver, context_policy: fresh, handoff: false, kind: code, gate: true, requires_command_evidence: true, on_fail: { target: fix } }
---

# quick-fix — light remediation flow

This is the flow-level narrative for the workflow. It explains how the four
steps compose and what flows between them; it is not itself a structural schema — the
frontmatter `steps` array above is the *only* execution order. Nothing here overrides it.

## Shape of the flow

```
grill --> fix --> verification --> deliver
           ^          |               |
           +----------+---------------+
                (FAIL: rewind to fix)
```

The flow collapses the feature-full clarification trio (`grill` -> `spec` ->
`grill-review`) into ONE front-loaded interactive checkpoint (`grill`), then runs a
single fix pass and one machine-checkable gate, then delivers (`deliver`, below). It is for work whose
scope is already small and well understood: a bounded bug fix, a small config or
dependency change, a tiny feature. If a WI routed here turns out to need a real spec or a
separate review pass, that is a signal it was misrouted — cancel and re-file against
`feature-full` or `bug-fix` rather than stretching this lane.

## The human checkpoint (`requires_human_answer: if_asked`)

`grill` is the lane's only human checkpoint. Its questions are a human's to answer: the
daemon refuses an answer that comes from a step process. Zero questions is a legitimate
outcome for a clear, small ask, so `grill` may complete without asking — an unattended
claim runs on — but that completion is recorded (`human_gate_unasked` event) and the
owner is notified.

## Why `grill` is `kind: spec` (not `kind: qa`)

In `feature-full`, the pre-spec clarification step is `kind: qa` and a separate `spec` step
follows it. This bundle deliberately has no separate spec authoring step: the `grill` step does not
stop at gathering answers, it commits them to a micro-spec (`artifacts/fix-memo.md`) that `fix`
implements directly. That is spec work, so it is declared `kind: spec`.

## Artifact flow between steps

Every step's output is a path recorded in `execution.json`'s `steps.<id>.output`, pointing at a
file under `artifacts/`; no step declares frontmatter `inputs:`, so downstream steps read upstream artifacts by that convention:

| step | primary artifact it writes | who reads it |
|---|---|---|
| `grill` | `artifacts/grill-qa.md` (question/answer log), `artifacts/fix-memo.md` (micro-spec) | `fix` |
| `fix` | changes under `repos/<name>/**`, `artifacts/fix-summary.md` | `verification`, `deliver` |
| `verification` | `artifacts/verification-report.md` (verdict) | `deliver` |
| `deliver` | the pushed feature branch, PR and merge outcome per repo, `artifacts/delivery.md` | (terminal) |

## Rewind boundaries

Both gates (`verification`, `deliver`) declare `on_fail: {target: fix}` — this section explains the choice. A rewind target must exist in the `steps` array and sit strictly
earlier than the failing step:

- **`verification` FAILs -> rewind to `fix`.** Verification is the machine gate
  (build/lint/tests); a failure means the tree is wrong, and `fix` is the only step that
  can change it.
- **`deliver` FAILs ("back to code") -> rewind to `fix`.** The human sent the delivery
  back, or the merge rule was not met and a re-work is due; `fix` is again the only
  step that can change the tree.

Workflow-level rewind bounds for this bundle are declared once in the frontmatter:
`max_total: 2`, `max_per_step: 2`. A quick fix that cannot land within two bounded
rewinds is not a quick fix; past those bounds the workflow ends in
`failed_rewind_exhausted` and the honest response is a re-filed WI on a heavier lane,
not more loops.

## Budget guidance

There is no workflow-level budget; the bounds are the rewind limits above and the daemon's
per-step runtime bound. No step declares `budget.wall_minutes` (polyforge#164): the daemon
bounds a step's total runtime across its dispatches at T = max(3 × `budget.wall_minutes`,
the daemon's `--step-timeout`, default 90 minutes), so any estimate of 30 minutes or less is
inert and T is 90 minutes for every step here. Declare a wall only for a step that
legitimately needs more than 90 minutes in total (31 or more).

## No release slot

This bundle has no release step. Releases and deploys never ride a delivery lane: they
happen only through the dedicated `release` workflow bundle (a separate WI), so shipping
is always an explicit, separately-gated act on a verified origin head.

## Delivery: the `deliver` step

`deliver` runs last, after every gate passed (a failing gate never reaches it). Per declared
repo it fetches and merges the latest origin default branch (a conflict is resolved in the
capsule, then the full test bar is re-run and a fresh-context reviewer checks the
resolution), pushes ONLY the feature branch `polyforge/<project>-<seq>`, opens or reuses
the PR, and merges it with `gh pr merge --merge` iff the independent review passed with no
blocking finding, the tests passed, no conflict was LLM-resolved and its mechanical
pre-flight (nothing uncommitted, no release residue) found nothing. Otherwise it asks a
human-only question (merge / don't merge / back to code). This workflow has no independent `review` step, so verification's pass stands in for it: a quick-fix delivery auto-merges when its tests are covered on the pushed head, no conflict was hand-resolved and the pre-flight is clean, and asks otherwise. "Back to code" is a
`gate_failed` verdict that rewinds to `fix`; a re-run reuses the same branch and PR. The
result records branch, PR URL and merge commit (or why not merged) per repo, plus the
delivery note (`artifacts/delivery.md`). The driver refuses a completed `deliver` whose
recorded commands push anything but the feature branch, merge other than `--merge`, or
merge/leave unmerged outside the rule without a human answer. Step definition:
`steps/deliver.md`. Releases stay in the separate `release` bundle, cut from the merged
origin head.
