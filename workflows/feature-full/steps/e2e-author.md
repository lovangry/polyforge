---
name: e2e-author
description: Author end-to-end acceptance scenarios against the spec's acceptance criteria before code runs - executable against the pre-implementation tree, independent of the implementation details in code.
tools: [read, edit, bash]
output:
  type: object
  required: [scenarios_path, scenario_titles]
  properties:
    scenarios_path:
      type: string
      description: Capsule-relative path to the authored e2e scenario document/spec (artifacts/e2e-scenarios.md), plus any generated test files under repos/<name>/** if the project runs its e2e suite from the tree.
    scenario_titles:
      type: array
      minItems: 1
      uniqueItems: true
      items: { type: string }
      description: Titles of every e2e scenario authored, in document order. At least one is required (minItems 1) - zero scenarios means the acceptance criteria were not covered. Titles must be UNIQUE (uniqueItems) - `e2e-run` echoes this list verbatim as its authored_titles cross-check under its own uniqueItems constraint, so a duplicated title would leave the echo step with no valid response; a duplicate title also cannot key the 1:1 scenario-to-result correspondence.
    test_files:
      type: array
      items: { type: string }
      description: repo-relative paths of any generated test files, if the project's e2e tests live in the tree rather than purely as a scenario document.
---

# e2e-author

Write end-to-end acceptance scenarios against `artifacts/spec.md`'s `## Acceptance
criteria` and `#### Scenario` blocks — not against `code`'s implementation. This step
runs BEFORE `code` in the flow (step 5 of the reference graph, before `e2e-baseline`
at 6 and `code` at 7): authoring happens pre-code, against the spec alone, so the
scenarios never silently encode whatever `code` later happens to build; they verify
intent, not implementation.

## What to do

1. Read `artifacts/spec.md` in full, focusing on `## Acceptance criteria` and every
   `#### Scenario`.
2. For each acceptance criterion, author a corresponding end-to-end scenario: either as a
   structured document in `artifacts/e2e-scenarios.md` (GIVEN/WHEN/THEN form, mirroring
   the spec's own scenario grammar) and/or as runnable test files under
   `repos/<name>/**`, following whatever the project's existing e2e test convention is —
   inspect the repo for an existing e2e test directory/framework before inventing a new
   one.
3. Do not read or depend on `code`'s output for this step; if a scenario requires
   information only `code` would know (an internal API shape that was never part of the
   spec), that is itself a signal the spec under-specifies something — flag it in
   `artifacts/e2e-scenarios.md` as a spec under-specification rather than quietly
   reverse-engineering it from the as-yet-unwritten implementation.

## Executability against the pre-implementation tree

Generated test files MUST compile against the tree as it stands at authoring time — the
pre-implementation tree: drive the spec'd behavior through interfaces that already
exist (CLI, API, config — the project's e2e convention) and never reference symbols
that do not exist yet. `e2e-baseline` (the next step) executes this suite before `code`
runs, so its failures must be assertion-level, not build-level. A scenario that
genuinely cannot be expressed without not-yet-existing surface is either flagged in
`artifacts/e2e-scenarios.md` as a spec under-specification (do not fake it) or authored
as a document-only scenario `e2e-baseline` can drive manually through the project's
actual interfaces. Never reverse-engineer a scenario from an imagined implementation,
and never write an unrunnable file.

## Non-goals

- Do not run the scenarios here — `e2e-baseline` (the pre-code bar-laying run) and
  `e2e-run` (the post-code rerun gate) execute them, not this step.
- Do not write unit tests for implementation internals — those belong inside `code`'s own
  plan-step commits if the project expects them; this step is end-to-end/acceptance level
  only.

## Output

Emit the final JSON object per the schema above: the scenario document path, the ordered
scenario titles (at least one - the schema rejects an empty list), and any generated test
file paths.
