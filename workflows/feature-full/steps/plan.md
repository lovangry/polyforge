---
name: plan
description: Write the implementation plan — ordered, compilable units of work with declared touched files — grounded in the confirmed spec.
tools: [read, bash]
output:
  type: object
  required: [plan_path, step_ids, touched_files]
  properties:
    plan_path:
      type: string
      description: Capsule-relative path to the written plan artifact (artifacts/plan.md).
    step_ids:
      type: array
      minItems: 1
      items: { type: string }
      description: Ordered ids of every implementation step declared in the plan. At least one is required (minItems 1) - a plan with zero steps is an unfilled template.
    touched_files:
      type: array
      minItems: 1
      items:
        type: object
        required: [path, intent]
        properties:
          path: { type: string, description: "repo-relative path" }
          intent: { type: string, enum: [read, write] }
      description: Deduplicated list of every file the plan declares as read or written, across all steps (write wins over read for the same path). At least one entry is required (minItems 1) - a plan that touches nothing leaves `code` nothing to commit.
---

# plan

Write the implementation plan for this work item, grounded in `artifacts/spec.md` (read
it in full — do not plan against your own recollection of the goal).

## Contract

The plan MUST be written in OpenSpec grammar mirroring the spec's own requirement
structure where it maps directly (do not re-derive requirements from scratch; reference
the spec's requirement titles), plus an ordered `## Steps` section. Every step MUST:

- be phrased as a **compilable/testable unit** — something that leaves the tree in a
  working, buildable state when done, not a mid-refactor fragment;
- declare each step's **per-unit green bar** as the repo's FAST-CHECK bar - the same
  bar `code` runs per unit: the project's own build, its lint/format check, and the
  unit tests of the packages/modules the unit touches, as the repo itself defines them
  (Makefile, package.json scripts, CI config); for a Go repo, `go build ./... &&
  go vet ./... && gofmt -l .` plus `go test` of the touched packages. Name the concrete
  commands. The full suite is NOT a per-unit bar, not even for the final unit - it
  belongs to `e2e-run` and `verification`, which also exercise `e2e-author`'s test
  files (those are not part of any per-unit bar);
- carry a `Touched files: <repo-relative path> (write|read), ...` line, or
  `Touched files: (no file edits)` for steps that only reason/verify without changing
  the tree.

Example:

```markdown
## Steps

1. **implement_pvc_guard** (m) - add the bound-PVC check to the scheduler predicate
   - Touched files: `pkg/scheduler/predicates.go` (write)
2. **write_tests** (s) - unit tests for the guard
   - Touched files: `pkg/scheduler/predicates_test.go` (write)
3. **wire_config** (xs) - expose the new guard behind a feature flag
   - Touched files: `pkg/scheduler/config.go` (write), `pkg/scheduler/predicates.go` (read)
```

## What to do

1. Read `artifacts/spec.md`.
2. Order steps so that each one, once done, leaves `code` (the next step) able to build
   and run tests on the tree so far — do not plan a sequence that only compiles once
   every step has landed. This is what lets `code` commit per unit rather than in one
   undifferentiated blob.
3. If this is a rewind re-entry (the `code` or `review` step's FAIL verdict routed here
   indirectly through a re-planned `code` pass — see `workflow.md`'s rewind-boundaries
   section for why `plan` itself is not a direct rewind target in this bundle), still
   re-derive the plan from the current spec rather than patching the old plan blindly.
4. Declare every file each step will touch, honestly distinguishing read-only inspection
   from actual writes — this list becomes the WI's `declared_resources` and, downstream,
   its file-scope locks; an under-declared write is a real lock gap, not a paperwork gap.

## Non-goals

- Do not write any code here — that is `code`'s job. The plan is instructions, not a diff.
- Do not write test scenarios — `e2e-author` covers end-to-end scenarios separately;
  ordinary unit tests belong inside the relevant `code` step of this plan if the project
  expects them alongside the implementation.

## Output

Emit the final JSON object per the schema above: the artifact path, the ordered step ids
(at least one), and the deduplicated touched-files list (at least one entry; each path
once, `write` intent winning over `read` for the same path).

## Handoff duty

This step declares `handoff: true`. Before writing your result envelope:
1. Write a handoff file at `artifacts/handoff-<step-id>.md` summarizing what
   the next inherit step needs to know (decisions made, constraints
   discovered, what was produced, where to find details).
2. That path IS the contract: the next inherit step reads the file by the
   path convention `artifacts/handoff-<step-id>.md`. There is no digest, no
   byte count and no envelope field to fill in — the result envelope is the
   minimal contract (outcome + output; needs_input adds question_id and
   question), so write the file and leave the envelope lean.
