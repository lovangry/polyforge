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
      enum: [pass, defer, fail]
      description: pass means the release completed (version cut, built, tagged, published per this project's convention); defer means this capsule found no release procedure to execute and the release is explicitly left to the project's owner; fail means the release attempt did not complete.
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
    - description: A deferred release - no release procedure found in this capsule, so nothing was executed; owes neither a version nor published artifacts.
      properties:
        verdict: { type: string, enum: [defer] }
      required: [verdict]
---

# release

This is a normal step slot — it reuses every existing step primitive (inputs, this
`output` schema, worktree access, budget, rewind; `01-workflow-skills.md` §4.2). It is
**not** a special mechanism in the workflow engine.

This template is a **dispatcher, not a procedure**: it ships project-generic, and the
release procedure itself belongs to each project's repository — the platform never
ships a procedure here and never fills one in. The dispatcher's only job is to find
the one repo that owns the release procedure and either execute it, defer, or fail
loudly when ownership is ambiguous.

## Ownership resolution

Scan the capsule's `repos/` directory (one level: `repos/<name>/`). A repo is a
release-procedure owner when it carries `.polyforge/release.sh` or
`.polyforge/release.md` — exactly these two spellings, inside the repo's `.polyforge/`
directory. A repo carrying neither file is not an owner. Classify the scan:

- **Zero owners.** No repo in this capsule carries either file. There is no procedure
  to execute and none may be invented here: finish with `verdict: defer` and nothing
  else in the output — no `version`, no `artifacts_published`. Defer is an explicit,
  honest outcome (the release is left to the project's owner), not a failure and not a
  license to improvise a release.
- **One owner.** Exactly one repo carries a procedure file: that repo is the release
  owner, and its procedure is executed in this step. Read the owner's procedure file
  first, then follow it exactly with your `bash` tool from the capsule root, treating
  its exit contract as the release's: any nonzero command exit is a release failure —
  `verdict: fail` with a precise `fail_reason` — never something to work around. On
  success, report `verdict: pass` with the `version` and the `artifacts_published`
  list the procedure actually produced (never invented ones).
- **Two or more owners.** More than one repo carries a procedure file: stop — this is
  a loud failure, not a choice. Finish with `verdict: fail` and a `fail_reason` that
  begins with `PF-RELEASE-AMBIGUOUS_OWNER` and names EVERY candidate repo. The
  one-release-owner rule is the contract; the dispatcher must never silently pick one
  owner (or merge procedures) when ownership is ambiguous.

## Honesty

A release failure is a business judgment about readiness to ship, not merely "a
command exited nonzero" — but a nonzero exit from the owner's procedure should still
`fail` the gate; do not report `pass` on a release that did not actually produce and
publish something. The schema enforces the same line mechanically: a `pass` verdict
requires a non-empty `version` and at least one `artifacts_published` entry, a `fail`
verdict requires a non-empty `fail_reason` (this bundle's default `on_fail` target
carries it back as its pending_rewind_input), and a `defer` verdict requires none of
them. One boundary on "execute exactly": a procedure file that exists but is
unreadable or empty is a release failure (`fail_reason` says so), not a defer —
defer means the procedure file is absent, not that it is broken.

## Output

Emit the final JSON object per the schema above.
