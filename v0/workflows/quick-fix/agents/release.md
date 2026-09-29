---
name: release
description: Project-specific release hook - cut a version, build/tag/publish per this project's own convention, after verification has passed. Template; each project fills in its own procedure.
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
      description: Required when verdict is fail - what went wrong, to be carried back to fix (this bundle's on_fail target) as its pending_rewind_input. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
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

This is a normal step slot - it reuses every existing step primitive (inputs, this
`output` schema, worktree access, budget, rewind, sync boundaries;
`01-workflow-skills.md` §4.2). It is **not** a special "deploy mechanism". For this repo
the template is already filled in: the body below is polyforge's own concrete release
procedure - the platform bootstraps its releases through the same hook it ships to
projects - while this file's frontmatter contract (`name`, `description`, `tools`,
`output`) stays exactly as shipped.

In this lane there is no separate `review` step, so `release` is the last judgment point.
Read `artifacts/verification-report.md` before starting: a release attempted on top of a
verification attempt that was anything less than a clean pass is a self-inflicted
failure.

## The polyforge release procedure

Polyforge is a mono-repo with a single repo-level semver: one git tag is one version, and
the two deployment artifacts are built together under that same tag
(`00-architecture.md` §3.3). Distribution is the intranet `releases/<tag>/` directory
holding the two binaries plus a sha256 checksum manifest - that is what `pf upgrade`
downloads and verifies before running the `07-deployment-pilot.md` §6.3 upgrade
sequence, and what a §6.4 rollback restores from. `skills/` and `workflows/` are not
released separately; they travel with the repo itself. Concretely:

1. **Determine the version.** The version is the git tag and nothing else - no
   per-artifact versioning, no build or step numbers. Take the current floor from
   `git tag -l 'v*' | sort -V | tail -1` and bump one segment of it: patch for a fix,
   minor for new capability. Record the decided version and its notes in
   `artifacts/release-notes.md`; do not invent a version ad hoc each run.
2. **Build the two artifacts.** Build from the exact commit `fix`'s last commit produced
   (read it from `execution.json`'s `steps.fix.output.commits` rather than assuming
   HEAD):

   ```
   GOWORK=off go build -o artifacts/release-<tag>/ ./cmd/aihub ./cmd/pf
   ```

   Two binaries come out and nothing else is shipped: `aihub`, the AIHub service
   binary, and `pf`, the work-machine binary - one file carrying the `pf` CLI verbs plus
   the `daemon` subcommand (`00-architecture.md` §3.3).
3. **Checksum manifest.** In the same staging directory, produce `SHA256SUMS` covering
   the two binaries in the plain `sha256sum -c` format that `pf upgrade` verifies
   against:

   ```
   cd artifacts/release-<tag> && sha256sum aihub pf > SHA256SUMS
   ```

   The manifest is itself one of the published artifacts: it sits beside the binaries
   and is listed in `artifacts_published` with them.
4. **Tag.** Create a single annotated tag named exactly the version (`v<semver>`) on the
   commit from step 2. The tag is the version - there is no second numbering scheme
   anywhere in the release.
5. **Publish.** Place `aihub`, `pf` and `SHA256SUMS` into the intranet `releases/<tag>/`
   directory - one directory per tag holding the two binaries and their checksum
   manifest (`00-architecture.md` §3.3 placement). That directory is v1's only
   distribution channel: there are no alpha/stable channel promotions, so publishing
   into the per-tag directory is the whole promotion. `artifacts_published` is the
   published path list: `releases/<tag>/aihub`, `releases/<tag>/pf`,
   `releases/<tag>/SHA256SUMS`.

## Rewind contract

This bundle points `release`'s `on_fail` at `fix`, not at the immediately preceding
`verification` - see `workflow.md`'s rewind-boundaries section for the reasoning. In
practice that means your `fail_reason` must be written for a **code-fixing** reader:
name the concrete thing that failed (missing changelog entry, wrong version constant,
tag rejected because of uncommitted files), not just "release failed", so `fix`'s next
attempt can act on it. If the failure is purely procedural and no code change could
help, say that explicitly - the bounded rewind will exhaust itself rather than loop
forever, and the honest record is what the human closing out the WI needs.

## Honesty

A nonzero exit from any of the steps above should still `fail` the gate; do not report
`pass` on a release that did not actually publish something. The schema enforces the
same line mechanically: a `pass` verdict requires a non-empty `version` and at least
one `artifacts_published` entry, so a template left unfilled cannot validate as a
completed release.

## Output

Emit the final JSON object per the schema above.
