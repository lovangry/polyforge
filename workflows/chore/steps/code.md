---
name: code
description: Execute the maintenance plan in the repo worktree, keeping the build green and committing one compilable unit at a time.
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
        required: [sha, message]
        properties:
          sha: { type: string }
          message: { type: string }
      description: One entry per plan step actually committed, in the order committed. At least one commit is required (minItems 1) - a completed step that changed nothing is not a valid completion (report infeasibility honestly instead, never an empty list).
    build_status:
      type: string
      enum: [green, red]
      description: Build/test status of the tree at the end of this step's execution. Must be green unless this attempt is being deliberately handed off incomplete.
    summary:
      type: string
      description: Short prose summary of what was done, for verification and rewind context.
---

# code

Execute `artifacts/plan.md` against the repo worktree(s) declared for this WI. Read the
plan in full before starting; if this is a rewind re-entry, also read
`execution.json`'s `pending_rewind_input` for this step (a `verification` FAIL verdict)
and address every point it raises before proceeding with the rest of the plan.

## Discipline

- **Work only inside the declared repo worktree(s)** (`repos/<name>/**`); never touch
  paths outside them.
- **Follow the plan's step order.** Each plan step is designed to leave the tree
  buildable; if the order genuinely does not work, that is a finding to report in
  `summary`, not a license to silently reorder.
- **Keep the build green between commits.** After each plan step, run the project's
  build and test commands (via `bash`) before moving to the next. Do not proceed on top
  of a red build.
- **Commit per compilable unit**, with a message identifying which plan step it
  satisfies. For a single-step chore this is one commit; for a sweep it is many.
- **Stay mechanical.** This lane's contract is no judgment calls: no drive-by
  "improvements" the plan did not declare, no reformatting beyond what the chore
  requires, no half-included adjacent work. An undeclared write is a lock-scope
  violation waiting to happen — record any genuinely necessary deviation in `summary`.

## If you cannot finish

If a plan step is infeasible as written, or the build cannot be made green within
reasonable effort, do not fabricate a passing `build_status: green`. Report honestly:
leave `build_status: red` and explain the blocker in `summary`. On this lane a red hand
off is doubly important — there is no review step behind you; `verification` is the
only thing between your tree and merge.

## Output

Emit the final JSON object per the schema above: every commit actually made, the final
build status, and a prose summary useful to the next reader.

## Handoff duty

This step declares `handoff: true`. Before writing your result envelope:
1. Write a handoff file at `artifacts/handoff-<step-id>.md` summarizing what
   the next inherit step needs to know (decisions made, constraints
   discovered, what was produced, where to find details).
2. Compute its sha256 and byte count.
3. Include it in your result envelope's `handoff_ref` field:
   `"handoff_ref": {"path": "artifacts/handoff-<step-id>.md", "sha256": "<hash>", "bytes": <N>}`
