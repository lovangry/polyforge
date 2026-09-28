---
name: release
description: Project-specific release hook — cut a version, build/tag/publish per this project's own convention, after review has passed. Template; each project fills in its own procedure.
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
      description: The version identifier produced by this release (e.g. a semver tag). Required non-empty when verdict is pass - an unfilled template must not validate as a completed release.
    artifacts_published:
      type: array
      items: { type: string }
      description: Identifiers or locations of anything actually published (tags, packages, images, channel promotions). Required with at least one entry when verdict is pass - "pass" means something was actually published.
    fail_reason:
      type: string
      description: Required when verdict is fail — what went wrong, to be carried back to review (this bundle's default on_fail target) as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
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
`01-workflow-skills.md` §4.2). It is **not** a special "deploy mechanism" in the workflow
engine; it is a template each project is expected to replace with its own concrete release
procedure while keeping this file's frontmatter contract (`name`, `description`, `tools`,
`output`) intact.

## Template body — replace this section per project

This reference bundle ships this step as a filled-in skeleton rather than a bare TODO, so
a project can see the shape it needs to commit to. Concretely, a real `release` step
should:

1. **Determine the version identifier.** Decide where the next version number comes from
   for this project — a tag-based scheme (`git describe`), a changelog-driven bump, a
   monotonic build counter, or something else. State the source explicitly; do not invent
   a version number ad hoc each run.
2. **Build the release artifact(s).** Run this project's actual build/package command(s)
   for a release build (which may differ from the `code`/`verification` dev-mode build —
   e.g. optimized builds, stripped binaries, container images).
3. **Tag.** Create the version control tag (or equivalent) recording this release point,
   tied to the exact commit `code`'s last commit produced (read it from
   `execution.json`'s `steps.code.output.commits` rather than assuming HEAD, in case
   later steps also produced commits).
4. **Publish.** Upload/push the built artifact(s) to this project's actual distribution
   target — a package registry, a container registry, a release page, an internal
   artifact store. Record every location/identifier actually published to in
   `artifacts_published`.
5. **Channel promotion**, if this project has release channels (alpha/stable, canary/
   prod), is a project-specific decision: either this step promotes directly, or a
   separate mechanism outside this workflow bundle handles promotion — state which,
   explicitly, in this file once filled in.

Until a project fills this in with its own concrete procedure, treat every step above as
literally what must be replaced — do not run this generic template against a real release
target as if it were a working implementation for any specific project.

## Honesty

A release failure is a business judgment about readiness to ship, not merely "the deploy
command exited nonzero" — but a nonzero exit from any of the steps above should still
`fail` the gate; do not report `pass` on a release that did not actually publish
something. The schema enforces the same line mechanically: a `pass` verdict requires a
non-empty `version` and at least one `artifacts_published` entry, so a template left
unfilled cannot validate as a completed release. `fail_reason` becomes this bundle's rewind input to `review` (its declared
default `on_fail.target`) — and the schema requires it on any fail verdict — so describe
what went wrong precisely enough that a re-review knows what to check for before another
release attempt.

## Output

Emit the final JSON object per the schema above.
