---
name: review
description: Human-judgment gate over the completed bug fix - does the change remove the confirmed root cause, without breaking adjacent behavior, and is the evidence chain (repro -> cause -> fix -> verified) coherent?
tools: [read, bash]
output:
  type: object
  required: [verdict]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means the fix is ready to release as-is; fail means it needs another fix pass before release.
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
      description: Required when verdict is fail - a synthesis of the blocking findings, carried back to fix as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
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

Judge whether this bug fix is ready to release. Unlike `verification`, this step is a
genuine quality judgment, not a mechanical check — and in this lane it has a specific
question to answer above all: **did the fix remove the actual cause?** The lane's whole
reason to exist is preventing symptom-patches, and this is the last step positioned to
catch one.

## What to do

1. Read, in this order: `artifacts/root-cause.md` (the proven cause),
   `artifacts/repro.md` (the observed failure), the full diff (in each task repo
   `git diff $(git merge-base HEAD origin/HEAD)..HEAD`, falling back to `origin/main` or
   `origin/master` when `origin/HEAD` is unset — this carries `repro`'s test commit,
   `fix`'s commits and any commit `fix` did not report), and
   `artifacts/verification-report.md` (the mechanical results, repro re-run included).
2. Judge the causal fit: does the diff remove the *diagnosed condition*, or does it
   merely suppress the observed symptom at one call site? Would the failure still occur
   through a path the diff did not touch? If `fix` reported
   `root_cause_addressed: false`, that is an automatic blocking finding — the lane
   requires the cause itself to be gone.
3. Judge the blast radius: the diff's effect on adjacent behavior the diagnosis did not
   implicate; scope creep beyond what the cause demanded; missing test coverage of the
   corrected behavior if the project expects tests alongside fixes.
   Judge the tests in the diff too: a repro or regression test that does not actually
   reproduce the diagnosed failure (vacuous, or passing whatever the code does) is a
   blocking finding — `fix` may not change it on its own (the driver's authored-tests
   gate) and must ask a human through `needs_input`, so name the test and why. A
   redundant or oversized test is a `non-blocking` finding.
4. Record every finding in `findings`, tagged `blocking` or `non-blocking`. Non-blocking
   findings do not fail the gate but are still useful history.
5. `verdict: pass` only if there are zero blocking findings - the schema enforces this:
   under a pass verdict every recorded finding must be `non-blocking`.
6. `verdict: fail` if there is at least one blocking finding (the schema's fail branch
   requires the findings list to contain one); synthesize them into `fail_reason` -
   concrete enough that `fix`'s next attempt (which receives this text
   as its `pending_rewind_input`) does not have to guess what to change, and required
   by the schema on any fail verdict. If your
   blocking finding is that the *diagnosis* was wrong, say exactly that in
   `fail_reason` — `fix` is expected to report it honestly rather than re-litigate the
   cause mid-flow (there is no direct rewind to `root-cause` in this lane; see
   `workflow.md`'s rewind-boundaries section).

**Do not run tests or builds.** This step reads and judges; it never runs the build or
the test suite (`go test ./...`, `make test`, the full `./test`, `npm test`). The full
bar ran exactly once, in `verification` (`deliver`, after you, re-runs that same bar only
after merging a moved default branch changed the tree): its report (`artifacts/verification-report.md`,
read above) is this step's test evidence. Reading a test file's source is part of the
review; running it is not.

## Owner-rules checklist (any hit is a blocking finding)

On top of the spec-conformance judgment above, check the diff against the owner's
fixed rules. ANY hit is a blocking finding - there is no pass verdict with one of
these present:

- **No faked concurrency semantics.** No timing-based fakes (`time.Sleep` as a
  synchronization stand-in, backoff tuned to "usually long enough") and no
  in-process locks/mutexes standing in for the cross-process or distributed mutual
  exclusion the semantics actually require.
- **No unrequested concepts.** No new states, fields, error codes, config items, or
  concepts the WI and the root-cause scope guidance did not request. List each one
  you find as its own must-fix finding.
- **No manual-gate bypass.** No code path that bypasses a manual checkpoint
  (`needs_input` / gate) - the flow's interactive checkpoints must stay
  load-bearing.
- **No backward-compatibility code.** No compat shims, dual paths, or
  legacy-behavior fallbacks; when behavior changes, the old shape is deleted, not
  carried alongside.
- **Spec-forced inventions.** If the WI or the root-cause scope guidance itself forces an invented
  mechanism - e.g. it demands "the loser must be refused" where both
  serializations are valid, which is what drove a sleep+latch hack - do not pass
  the diff for faithfully implementing it: flag it as a blocking finding and
  propose the spec change in that finding instead.
- **Agent reachability for new entry points.** If the diff adds a new daemon or
  aihub entry point, the diff must state whether the step agent can reach it; an
  unstated new entry point is itself a finding.

## Position in the flow

This step is declared `kind: review` in the workflow frontmatter, which the schema
grants positional freedom — in every delivery bundle that has a review it sits between `verification` and
`deliver` because ship-readiness judgment needs both the mechanical results and the
evidence chain in hand.

## Output

Write `artifacts/review.md` (this step's declared output): the verdict, every
finding with its severity, and the `fail_reason` on a fail. Then emit the final JSON
object per the schema above.
