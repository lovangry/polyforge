---
name: review
description: Human-judgment gate over the completed change - the lane's own conformance judgment (from the workflow.md step entry's vars) plus the fixed owner-rules checklist - after the machine gate, before delivery. Shared by every delivery bundle with a review.
output:
  type: object
  required: [verdict]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means the change is ready to deliver as-is; fail means it needs another pass of the step this gate rewinds to.
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
      description: Required when verdict is fail - a synthesis of the blocking findings, to be carried back to the step this gate rewinds to as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
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

{{intro}}

## What to do

{{judge}}

Then, on every lane:

- Run the owner-rules checklist below — any hit is a blocking finding.
- Record every finding in `findings`, tagged `blocking` or `non-blocking`.
  Non-blocking findings (style nits, minor improvement suggestions) do not fail the
  gate but are still useful history, even when the overall verdict is pass.
- `verdict: pass` only if there are zero blocking findings — the schema enforces
  this: under a pass verdict every recorded finding must be `non-blocking`.
- `verdict: fail` if there is at least one blocking finding (the schema's fail
  branch requires the findings list to contain one); synthesize them into
  `fail_reason` — concrete and actionable enough that `{{build_step}}`'s next attempt
  (which receives this text as its `pending_rewind_input`) does not have to guess what
  to change. The schema requires `fail_reason` on any fail verdict.

**Do not run tests or builds.** This step reads and judges; it never runs the build, the
test suite (`go test ./...`, `make test`, the full `./test`, `npm test`) or an e2e suite.
The full bar ran exactly once, in `verification` (before this step; `deliver`, after you,
re-runs that same bar only after merging a moved default branch changed the tree): its
report (`artifacts/verification-report.md`) is this step's test evidence. Reading a test
file's source is part of the review; running it is not.

## Principles

Every principle is a check (a violation is blocking); the checklist spells them out.

{{principles}}

## Owner-rules checklist (any hit is a blocking finding)

On top of the lane judgment above, check the diff against the owner's
fixed rules. ANY hit is a blocking finding - there is no pass verdict with one of
these present:

- **No faked concurrency semantics.** No timing-based fakes (`time.Sleep` as a
  synchronization stand-in, backoff tuned to "usually long enough") and no
  in-process locks/mutexes standing in for the cross-process or distributed mutual
  exclusion the semantics actually require.
- **No unrequested concepts.** No new states, fields, error codes, config items, or
  concepts {{scope_source}} did not request. List each one you find as its own
  must-fix finding.
- **No manual-gate bypass.** No code path that bypasses a manual checkpoint
  (`needs_input` / gate) - the flow's interactive checkpoints must stay
  load-bearing.
- **Spec-forced inventions.** If any of the change's own requirements
  ({{scope_source}}) forces an invented mechanism - e.g. it demands "the loser
  must be refused" where both serializations are valid, which is what drove a sleep+latch hack - do not pass
  the diff for faithfully implementing it: flag it as a blocking finding and
  propose the spec change in that finding instead.
- **Unruled behavior change.** Existing behavior changed without an owner ruling.
- **I/O under a lock.** Network or disk I/O while holding a lock.
- **Weak tests.** A new test that passes on the old code, or an assertion relaxed
  without a stated reason.
- **Stale docs.** Docs not updated with the code they describe.
- **Leaked credentials** in an agent's env, a command line, a log or an artifact.
- **Guess as fact.** An unverified claim stated as fact.

## Position in the flow

In every delivery bundle that has a review this step sits between `verification`
and `deliver`: the mechanical gate runs first, so a mechanical failure never costs a
review, and the judgment reads its results.

## Output

Write `artifacts/review.md` (this step's declared output): the verdict, every
finding with its severity, and the `fail_reason` on a fail. Then emit the final JSON
object per the schema above.
