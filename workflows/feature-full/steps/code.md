---
name: code
description: Implement the plan's steps in the repo worktree, keeping the build green and committing one compilable unit at a time.
tools: [read, edit, bash]
output:
  type: object
  required: [commits, build_status]
  properties:
    commits:
      type: array
      minItems: 1
      items:
        type: object
        required: [step_id, sha, message]
        properties:
          step_id: { type: string }
          sha: { type: string }
          message: { type: string }
      description: One entry per plan step actually committed, in the order committed. At least one commit is required (minItems 1) - a completed code step that changed nothing is not a valid completion (report infeasibility honestly instead, never an empty list).
    build_status:
      type: string
      enum: [green, red]
      description: Build/test status of the tree at the end of this step's execution. Must be green unless this attempt is being deliberately handed off incomplete (see incomplete handling below).
    summary:
      type: string
      description: Short prose summary of what was implemented, for review and rewind context.
---

# code

Implement `artifacts/plan.md` against the repo worktree(s) declared for this WI. Read the
plan in full before starting; if this is a rewind re-entry, also read
`execution.json`'s `pending_rewind_input` for this step (a `verification` or `review` FAIL
verdict) and address every point it raises before proceeding with the rest of the plan.

## Discipline

- **Work only inside the declared repo worktree(s)** (`repos/<name>/**`); never touch
  paths outside them.
- **Follow the plan's step order.** Each plan step is designed to leave the tree
  buildable; do not skip ahead or reorder for convenience — if the order genuinely does
  not work, that is itself a finding to report in `summary`, not a license to silently
  reorder.
- **Keep the build green between commits — the bar is the `verification` gate's bar,
  verbatim.** After implementing each plan step, run the project's build and test
  commands that the `verification` gate runs — the same component list, no paraphrase,
  no subset (via `bash`) — before moving to the next step. When `e2e-author` authored
  executable test files for this WI, intermediate units run that bar minus exactly the
  files named in `execution.json`'s `steps.e2e-author.output.test_files` — each
  excluded by its exact repo-relative path, never a directory-, pattern-, or
  category-level subset (those files are allowed, expected even, to sit red until the
  implementation satisfies them). The FULL gate bar — the named files included —
  applies at the plan's final unit; no exclusion survives to it, and the rerun gate
  and `verification` re-run the full bar regardless.
- **Commit per compilable unit**, one commit per plan step (or a tightly related group of
  plan steps if the project's convention groups small steps), with a message that
  identifies which plan step it satisfies. Do not accumulate the entire plan into one
  undifferentiated commit — `review` and any rewind re-entry depend on being able to see
  what changed at each unit's boundary.
- **Do not touch files the plan did not declare** for this step without a good reason
  recorded in `summary`; an undeclared write is a lock-scope violation waiting to happen.

## If you cannot finish

If a plan step turns out to be infeasible as written, or the build cannot be made green
within reasonable effort, do not fabricate a passing `build_status: green`. Report
honestly: leave `build_status: red` and explain the blocker in `summary`. A dishonest
green here corrupts every downstream gate (`verification`, `e2e-run`, `review`) that
trusts this step's word for it.

## Output

Emit the final JSON object per the schema above: every commit actually made (step id, sha,
message - at least one; the schema rejects an empty list), the final build status, and a
prose summary useful to the next reader (reviewer, or a future rewound instance of this
very step).

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
