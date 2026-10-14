---
name: chore
description: Deletion-type maintenance lane - plan, code, review, verify; deletions and cleanup ship through this lane. No release slot.
rewind:
  max_total: 2
  max_per_step: 2
defaults:
  model_candidates: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra]
steps:
  - { id: plan, context_policy: fresh, handoff: true, kind: author }
  - { id: code, context_policy: inherit, handoff: true, kind: code }
  - { id: review, context_policy: fresh, handoff: false, kind: review, gate: true, on_fail: { target: code } }
  - { id: verification, context_policy: fresh, handoff: false, kind: gate, gate: true, on_fail: { target: code }, model_candidates: [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
  - { id: deliver, context_policy: fresh, handoff: false, kind: code, gate: true, requires_command_evidence: true, on_fail: { target: code } }
---

# chore — deletion-type maintenance flow

This is the flow-level narrative for the workflow. It explains how the five
steps compose; it is not itself a structural schema — the frontmatter `steps` array above
is the *only* execution order. Nothing here overrides it.

## Shape of the flow

```
plan --> code --> review --> verification --> deliver
          ^          |             |             |
          +----------+-------------+-------------+
                     (FAIL: rewind to code)
```

The lane covers deletion-type and maintenance work: dead-code and feature removal,
cleanup sweeps, dependency bumps, tooling and lint config updates, mechanical
refactors, doc sweeps, test hygiene. The bundle chosen at filing IS the
classification — filing a WI on `chore` declares it a deletion-type change (no new
label or field is added later), and `feature-full` is NOT trimmed by change type, so
a pure-deletion ticket does not belong there (feature-full's `grill` probes that
mismatch and asks the owner). Deletion-type acceptance on this lane is deliberately
light: the existing test suite green plus a few targeted tests for any behavior the
deletion actually changes — no `e2e-author`, no full acceptance suite. The lane is
deliverable: its changes ship like any other delivery lane's, merging as ordinary
commits on a reviewed, verified tree.

Its two defining properties:

- **Review is the owner-rules gate.** `review` carries the same fixed owner-rules
  checklist as `bug-fix` and `feature-full` (no faked concurrency semantics, no
  unrequested concepts, no manual-gate bypass, no backward-compatibility code,
  agent-reachability statement for new daemon/aihub entry points) on top of plan
  conformance — a deletion-type change cannot dodge the rules by riding the light
  lane. If a "chore" turns out to need genuine design or spec work rather than
  removal judgment, it was misrouted: cancel and re-file on `feature-full`.
- **No release slot.** Deliverable does not mean self-releasing: the lane's changes
  merge as ordinary commits and ride the project's next release train; the release
  hook belongs to whichever WI carries that release (releases happen only via the
  dedicated `release` bundle, which this bundle never includes).

## Artifact flow between steps

Every step's output is a path recorded in `execution.json`'s `steps.<id>.output`, pointing at a
file under `artifacts/`; no step declares frontmatter `inputs:`, so downstream steps read upstream artifacts by that convention:

| step | primary artifact it writes | who reads it |
|---|---|---|
| `plan` | `artifacts/plan.md` (may legitimately be a single trivial step for a small chore) | `code` |
| `code` | changes under `repos/<name>/**`, `artifacts/code-summary.md` | `review`, `verification`, `deliver` |
| `review` | `artifacts/review.md` (verdict) | `verification`, `deliver` (the merge rule's independent review), (rewind context for `code` via `pending_rewind_input`) |
| `verification` | `artifacts/verification-report.md` (verdict) | `deliver` |
| `deliver` | the pushed feature branch, PR and merge outcome per repo, `artifacts/delivery.md` | (terminal) |

## `plan` is a normal step, not an optional one

The schema has no optional-step semantics, and this bundle does not invent any: `plan`
is a full step that always runs. Its contract is *proportionality* instead — for a
one-line dependency bump, a plan listing exactly one trivial step is the correct,
complete plan; for a sweep across many files, the plan enumerates the units so `code`
can keep the tree green between commits. Skipping the thinking because the chore is
small is not an option; writing the small thought down is cheap.

## Rewind boundaries

All rewind declarations live in the frontmatter `on_fail` of the gates; a target must
exist and sit strictly earlier than the failing step. This section explains the choices:

- **`review` FAILs -> rewind to `code`.** The owner-rules checklist or the
  plan-conformance judgment found the change wanting (undeclared writes, a
  half-finished deletion, a rule hit); `code` is the only step that can change the
  tree, so every gate on this lane shares it as its rewind target.
- **`verification` FAILs -> rewind to `code`.** Build/lint/tests failed on the changed
  tree, and `code` is the only step that can change it.
- **`deliver` FAILs ("back to code") -> rewind to `code`.** The human sent the delivery
  back; `code` re-runs and the re-run delivery reuses the same branch and PR.

Workflow-level rewind bounds are the tightest of the reference bundles:
`max_total: 2`, `max_per_step: 2`. A chore that needs more than two bounded attempts to
keep the tree green is either not mechanical or is fighting a real problem — surface it
honestly instead of burning the budget.

## Budget guidance

There is no workflow-level budget; the bounds are the rewind limits above and the daemon's
per-step runtime bound. No step declares `budget.wall_minutes` (polyforge#164): the daemon
bounds a step's total runtime across its dispatches at T = max(3 × `budget.wall_minutes`,
the daemon's `--step-timeout`, default 90 minutes), so any estimate of 30 minutes or less is
inert and T is 90 minutes for every step here. Declare a wall only for a step that
legitimately needs more than 90 minutes in total (31 or more).

## Delivery: the `deliver` step

`deliver` runs last, after every gate passed (a failing gate never reaches it). Per declared
repo it fetches and merges the latest origin default branch (a conflict is resolved in the
capsule, then the full test bar is re-run and a fresh-context reviewer checks the
resolution), pushes ONLY the feature branch `polyforge/<project>-<seq>`, opens or reuses
the PR, and merges it with `gh pr merge --merge` iff the independent review passed with no
blocking finding, the tests passed, no conflict was LLM-resolved and its mechanical
pre-flight (nothing uncommitted, no release residue) found nothing. Otherwise it asks a
human-only question (merge / don't merge / back to code). The independent review is this workflow's `review` gate (`steps.review.output`). "Back to code" is a
`gate_failed` verdict that rewinds to `code`; a re-run reuses the same branch and PR. The
result records branch, PR URL and merge commit (or why not merged) per repo, plus the
delivery note (`artifacts/delivery.md`). The driver refuses a completed `deliver` whose
recorded commands push anything but the feature branch, merge other than `--merge`, or
merge/leave unmerged outside the rule without a human answer. Step definition:
`steps/deliver.md`. Releases stay in the separate `release` bundle, cut from the merged
origin head.
