---
name: review
description: Human-judgment gate over the completed implementation — the code diff, the e2e results, and the plan/spec together — deciding whether the change is ready for the final verification gate and release.
tools: [read, bash]
output:
  type: object
  required: [verdict]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means the change is ready to release as-is; fail means it needs another code pass before release.
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

Judge whether this change is ready to release. Unlike `verification` and `e2e-run`, this
step is a genuine business/quality judgment call, not a mechanical check — it exists to
catch what automated checks cannot: design fit, scope creep or scope gaps against the
spec, code quality, and whether the e2e results actually demonstrate the spec's intent
rather than merely passing on a technicality.

## What to do

1. Read, in this order: `artifacts/spec.md` (what was supposed to happen),
   `artifacts/plan.md` (how it was supposed to be built), the actual diff produced by
   `code` (what was really built — read the repo worktree's history for the commits
   `code` reported), and `artifacts/e2e-results.md` (the rerun gate's verdict and
   per-scenario results; there is no verification report yet — `verification` runs
   after this step). Judge whether `e2e-run`'s skip justifications, if any, are
   acceptable — skips do not fail that gate; you do.
2. Check the implementation against the spec's requirements and acceptance criteria one
   by one — do not rubber-stamp because the e2e rerun reported pass; the mechanical
   gates check different things than "is this the right change."
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

## Position in the flow

This step is declared `kind: review` in the workflow frontmatter, which the schema grants
positional freedom (it does not have to be last) — in this reference bundle it sits
between `e2e-run` and `verification`: review is the human judgment on the rerun
evidence and the diff, and the machine gate follows it, re-running the full green bar
as the final mechanical word before `release`. A project adapting this bundle may
reposition `review` if its process genuinely needs review earlier, but should not do so
without also reconsidering what evidence would then be available to it.

## Output

Emit the final JSON object per the schema above.
