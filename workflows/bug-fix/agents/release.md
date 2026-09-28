---
name: release
description: Project-specific release hook - cut a version, build/tag/publish per this project's own convention, after review has passed. Template; each project fills in its own procedure.
tools: [read, bash]
output:
  type: object
  required: [verdict]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means the release completed (version cut, built, tagged, published per this project's convention); fail means the release attempt did not complete.
    version:
      type: string
      minLength: 1
      description: The version identifier produced by this release (e.g. a semver tag). Bug-fix lanes usually consume the patch segment of the project's existing scheme. Required non-empty when verdict is pass - an unfilled template must not validate as a completed release.
    artifacts_published:
      type: array
      items: { type: string }
      description: Identifiers or locations of anything actually published (tags, packages, images, channel promotions). Required with at least one entry when verdict is pass - "pass" means something was actually published.
    fail_reason:
      type: string
      description: Required when verdict is fail - what went wrong, to be carried back to review (this bundle's on_fail target) as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
  oneOf:
    - description: A completed release - requires a non-empty version and at least one published artifact; the unfilled template does not validate.
      properties:
        verdict: { type: string, enum: [pass] }
        version: { type: string, minLength: 1 }
        artifacts_published: { type: array, minItems: 1 }
      required: [verdict, version, artifacts_published]
    - description: A failed release attempt - requires a non-empty fail_reason.
      properties:
        verdict: { type: string, enum: [fail] }
        fail_reason: { type: string, minLength: 1 }
      required: [verdict, fail_reason]
---

# release

This is a normal step slot — it reuses every existing step primitive (inputs, this
`output` schema, worktree access, budget, rewind, sync boundaries;
`01-workflow-skills.md` §4.2). It is a template each project is expected to replace with
its own concrete release procedure while keeping this file's frontmatter contract
(`name`, `description`, `tools`, `output`) intact.

Before starting, read `artifacts/review.md` and `artifacts/verification-report.md`: a
release attempted on top of anything less than a clean review pass on this lane is a
self-inflicted failure — the whole point of proving the root cause was to ship a fix
worth releasing.

## Template body — replace this section per project

1. **Determine the version identifier.** Decide where the next version number comes from
   for this project — a tag-based scheme (`git describe`), a changelog-driven bump, a
   monotonic build counter, or something else. State the source explicitly; do not invent
   a version number ad hoc each run. For a bug-fix lane the bump is usually the patch
   segment, and the release notes should reference the fixed defect.
2. **Build the release artifact(s).** Run this project's actual build/package command(s)
   for a release build (which may differ from the dev-mode build `verification` ran —
   e.g. optimized builds, stripped binaries, container images).
3. **Tag.** Create the version control tag (or equivalent) recording this release point,
   tied to the exact commit `fix`'s last commit produced (read it from
   `execution.json`'s `steps.fix.output.commits` rather than assuming HEAD).
4. **Publish.** Upload/push the built artifact(s) to this project's actual distribution
   target. Record every location/identifier actually published to in
   `artifacts_published`.
5. **Channel promotion**, if this project has release channels, is a project-specific
   decision: state in this file once filled in whether this step promotes directly or a
   mechanism outside this workflow bundle handles promotion.

## Rewind contract

This bundle points `release`'s `on_fail` at `review` (feature-full's default
convention): a release failure is normally a release-blocking review concern resurfacing
at ship time, or a readiness judgment, not automatically evidence the code is wrong. In
practice that means your `fail_reason` must be written for a **re-reviewing** reader —
what to re-check before another release attempt — not as a code-change order.

## Honesty

A nonzero exit from any of the steps above should still `fail` the gate; do not report
`pass` on a release that did not actually publish something. The schema enforces the
same line mechanically: a `pass` verdict requires a non-empty `version` and at least
one `artifacts_published` entry, so a template left unfilled cannot validate as a
completed release.

## Output

Emit the final JSON object per the schema above.
