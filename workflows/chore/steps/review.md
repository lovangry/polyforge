---
name: review
description: Human-judgment gate over the completed maintenance/deletion change - plan conformance plus the fixed owner-rules checklist - after the machine gate, before delivery.
tools: [read, bash]
output:
  type: object
  required: [verdict]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means the change is ready to ship as-is; fail means it needs another code pass.
    findings:
      type: array
      items:
        type: object
        required: [severity, detail]
        properties:
          severity: { type: string, enum: [blocking, non-blocking] }
          detail: { type: string }
      description: Individual review findings, whether or not they are severe enough to fail the gate. A pass verdict with a blocking finding is self-contradictory - the schema's oneOf forbids it.
    fail_reason:
      type: string
      description: Required when verdict is fail - a synthesis of the blocking findings, to be carried back to code as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
  oneOf:
    - description: A pass verdict - zero blocking findings; any recorded findings must be non-blocking.
      properties:
        verdict: { type: string, enum: [pass] }
        findings:
          type: array
          items:
            type: object
            properties:
              severity: { type: string, enum: [non-blocking] }
      required: [verdict]
    - description: A fail verdict - at least one blocking finding exists; requires a non-empty fail_reason synthesizing them.
      properties:
        verdict: { type: string, enum: [fail] }
        fail_reason: { type: string, minLength: 1 }
        findings:
          type: array
          contains:
            properties:
              severity: { type: string, enum: [blocking] }
      required: [verdict, fail_reason]
---

# review

Judge whether this maintenance/deletion change is ready to ship. Unlike
`verification`, this step is a judgment call, not a mechanical check — and on this
lane it carries the same fixed owner-rules checklist the other delivery lanes'
reviews carry, so a deletion-type change cannot dodge the rules by riding the light
lane.

## What to do

1. Read, in this order: `artifacts/plan.md` (the declared units and touched files),
   the full diff (in each task repo `git diff $(git merge-base HEAD origin/HEAD)..HEAD`,
   falling back to `origin/main` or `origin/master` when `origin/HEAD` is unset — this
   includes any commit `code` did not report), `artifacts/code-summary.md` (the code
   step's own account of what it did and why) and `artifacts/verification-report.md`
   (the machine gate's results).
2. Judge plan conformance: the diff covers the plan's declared steps, touches no
   files the plan did not declare (an undeclared write is a lock-scope gap, not a
   paperwork gap), and a declared deletion is actually complete — no half-removed
   feature left compiling against ghosts of itself.
3. Judge the deletion-type acceptance bar: the existing test suite stays meaningful
   after the deletion, and the few targeted tests for behavior the deletion actually
   changes exist where the project expects tests. A full acceptance suite or an e2e
   lane is NOT this lane's bar — demanding one here is scope creep, not rigor. A test
   that asserts nothing real (vacuous) is a blocking finding; a redundant one is
   non-blocking.
4. Run the owner-rules checklist below — any hit is a blocking finding.
5. Record every finding in `findings`, tagged `blocking` or `non-blocking`.
   Non-blocking findings do not fail the gate but are still useful history.
6. `verdict: pass` only if there are zero blocking findings — the schema enforces
   this: under a pass verdict every recorded finding must be `non-blocking`.
7. `verdict: fail` if there is at least one blocking finding (the schema's fail
   branch requires the findings list to contain one); synthesize them into
   `fail_reason` — concrete enough that `code`'s next attempt (which receives this
   text as its `pending_rewind_input`) does not have to guess what to change.

**Do not run tests or builds.** This step reads and judges; it never runs the build or
the test suite (`go test ./...`, `make test`, the full `./test`, `npm test`). The full
bar ran exactly once, in `verification` (before this step;
`deliver` re-runs that same bar only after merging a moved default branch changed the tree): its report
is this step's test evidence. Reading a test file's source is part of the review;
running it is not.

## Owner-rules checklist (any hit is a blocking finding)

On top of the plan-conformance judgment above, check the diff against the owner's
fixed rules. ANY hit is a blocking finding - there is no pass verdict with one of
these present:

- **No faked concurrency semantics.** No timing-based fakes (`time.Sleep` as a
  synchronization stand-in, backoff tuned to "usually long enough") and no
  in-process locks/mutexes standing in for the cross-process or distributed mutual
  exclusion the semantics actually require.
- **No unrequested concepts.** No new states, fields, error codes, config items, or
  concepts the WI and the plan did not request. List each one you find as its own
  must-fix finding.
- **No manual-gate bypass.** No code path that bypasses a manual checkpoint
  (`needs_input` / gate) - the flow's interactive checkpoints must stay
  load-bearing.
- **No backward-compatibility code.** No compat shims, dual paths, or
  legacy-behavior fallbacks; when behavior is removed, the old shape is deleted,
  not carried alongside.
- **Spec-forced inventions.** If the WI or the plan itself forces an invented
  mechanism - e.g. it demands "the loser must be refused" where both
  serializations are valid, which is what drove a sleep+latch hack - do not pass
  the diff for faithfully implementing it: flag it as a blocking finding and
  propose the spec change in that finding instead.
- **Agent reachability for new entry points.** If the diff adds a new daemon or
  aihub entry point, the diff must state whether the step agent can reach it; an
  unstated new entry point is itself a finding.

## Position in the flow

This step is declared `kind: review` in the workflow frontmatter, which the schema
grants positional freedom — in every delivery bundle that has a review it sits between `verification`
and `deliver`: the mechanical gate runs first, so a mechanical failure never costs a
review, and the judgment reads its results.

## Output

Write `artifacts/review.md` (this step's declared output): the verdict, every
finding with its severity, and the `fail_reason` on a fail. Then emit the final JSON
object per the schema above.
