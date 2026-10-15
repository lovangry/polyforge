---
name: review
description: Human-judgment gate over the completed implementation — the full diff including the authored tests, the verification report, and the plan/spec together — deciding whether the change is ready to deliver.
tools: [read, bash]
output:
  type: object
  required: [verdict]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means the change is ready to deliver as-is; fail means it needs another code pass.
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
      description: Required when verdict is fail — a synthesis of the blocking findings, to be carried back to code as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
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

Judge whether this change is ready to deliver. Unlike `verification`, this
step is a genuine business/quality judgment call, not a mechanical check — it exists to
catch what automated checks cannot: design fit, scope creep or scope gaps against the
spec, code quality, and whether the authored scenarios actually demonstrate the spec's intent
rather than merely passing on a technicality.

## What to do

1. Read, in this order: `artifacts/spec.md` (what was supposed to happen),
   `artifacts/plan.md` (how it was supposed to be built), the full diff (what was
   really built, tests included — see below), and `artifacts/verification-report.md`
   (the machine gate's results, with the per-scenario results of the authored
   acceptance suite). Judge whether `verification`'s scenario skips, if any, are
   acceptable — skips do not fail that gate; you do.
2. Check the implementation against the spec's requirements and acceptance criteria one
   by one — do not rubber-stamp because verification passed; the mechanical gate
   checks different things than "is this the right change."
3. Record every finding in `findings`, tagged `blocking` or `non-blocking`. Non-blocking
   findings (style nits, minor improvement suggestions) do not fail the gate but should
   still be recorded — they are useful history even when the overall verdict is pass.
4. `verdict: pass` only if there are zero blocking findings — the schema enforces this:
   under a pass verdict every recorded finding must be `non-blocking`.
5. `verdict: fail` if there is at least one blocking finding (the schema's fail branch
   requires the findings list to contain one). Synthesize the blocking
   findings into `fail_reason` — concrete and actionable enough that `code`'s next attempt
   (which receives this text as its `pending_rewind_input`) does not have to guess what to
   change. The schema requires `fail_reason` on any fail verdict.

**Do not run tests or builds.** This step reads and judges; it never runs the build, the
test suite (`go test ./...`, `make test`, the full `./test`, `npm test`) or the e2e
suite. The full bar ran exactly once, in `verification` (before this step; `deliver`
re-runs that same bar only after merging a moved default branch changed the tree), and
its report is this step's test evidence. Reading a test file's source is part of the
review; running it is not.

## The full diff, including the tests

Read the whole change, not only `code`'s commits: in each task repo,
`git diff $(git merge-base HEAD origin/HEAD)..HEAD` (fall back to `origin/main` or
`origin/master` when `origin/HEAD` is unset — the same base `verification` uses for
pre-existing failures). That diff carries `e2e-author`'s test commit and any commit
`code` did not report. Judge the tests as part of the change:

- **Wrong or vacuous tests are a blocking finding.** An assertion that does not
  actually test the spec's behaviour, or a test that would pass whatever the
  implementation does. `code` may not change authored tests on its own (the driver's
  authored-tests gate); after your FAIL it asks a human through `needs_input`, and the
  approved change becomes the new baseline — say precisely which test and why.
- **Scenarios deleted as already satisfied.** Every title in
  `steps.e2e-author.output.already_satisfied` must really be existing behaviour; a
  new-behaviour scenario deleted because it was written wrong is a blocking finding.
- **New-behaviour coverage.** Every new-behaviour acceptance criterion of the spec has
  a scenario; a missing one is a blocking finding.
- **Redundant or oversized tests are non-blocking.** A scenario that re-tests
  existing behaviour or duplicates an existing test, or a suite above the 400-line
  soft cap (`added_test_lines`) whose `size_justification` does not hold up: record it
  as a `non-blocking` finding — removing a test would itself need a human round trip
  under the authored-tests gate, which is not worth it for size alone.

## Owner-rules checklist (any hit is a blocking finding)

On top of the spec-conformance judgment above, check the diff against the owner's
fixed rules. ANY hit is a blocking finding - there is no pass verdict with one of
these present:

- **No faked concurrency semantics.** No timing-based fakes (`time.Sleep` as a
  synchronization stand-in, backoff tuned to "usually long enough") and no
  in-process locks/mutexes standing in for the cross-process or distributed mutual
  exclusion the semantics actually require.
- **No unrequested concepts.** No new states, fields, error codes, config items, or
  concepts the spec did not request. List each one you find as its own must-fix
  finding.
- **No manual-gate bypass.** No code path that bypasses a manual checkpoint
  (`needs_input` / gate) - the flow's interactive checkpoints must stay
  load-bearing.
- **No backward-compatibility code.** No compat shims, dual paths, or
  legacy-behavior fallbacks; when behavior changes, the old shape is deleted, not
  carried alongside.
- **Spec-forced inventions.** If the spec itself forces an invented
  mechanism - e.g. it demands "the loser must be refused" where both
  serializations are valid, which is what drove a sleep+latch hack - do not pass
  the diff for faithfully implementing it: flag it as a blocking finding and
  propose the spec change in that finding instead.
- **Agent reachability for new entry points.** If the diff adds a new daemon or
  aihub entry point, the diff must state whether the step agent can reach it; an
  unstated new entry point is itself a finding.

## Position in the flow

In every delivery bundle that has a review this step sits between `verification` and
`deliver`: the mechanical gate runs first, so a mechanical failure never costs a review,
and the judgment reads its results.

## Output

Write `artifacts/review.md` (this step's declared output): the verdict, every
finding with its severity, and the `fail_reason` on a fail. Then emit the final JSON
object per the schema above.
