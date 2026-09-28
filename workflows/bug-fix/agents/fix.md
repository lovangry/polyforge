---
name: fix
description: Remove the confirmed root cause - implement the change that eliminates the diagnosed condition, keep the build green, and encode the corrected behavior where the project expects tests.
tools: [read, edit, bash]
output:
  type: object
  required: [commits, build_status, root_cause_addressed]
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
      description: Every commit actually made, in the order committed. At least one commit is required (minItems 1) - a completed fix step that changed nothing is not a valid completion (report infeasibility honestly instead, never an empty list).
    build_status:
      type: string
      enum: [green, red]
      description: Build/test status of the tree at the end of this step's execution. Must be green unless this attempt is being deliberately handed off incomplete.
    root_cause_addressed:
      type: boolean
      description: True only if the change removes the diagnosed condition itself. False means this attempt mitigates the symptom or addresses a different cause - say which in summary.
    summary:
      type: string
      description: "Short prose summary - what changed, why it removes the root cause, and any scope notes for review and rewind context."
---

# fix

Implement the change that removes the root cause `root-cause` confirmed. Your inputs are
`artifacts/root-cause.md` (the proven cause and its scope guidance) and
`artifacts/repro.md` (the observable failure). Read both in full before starting; if this
is a rewind re-entry, also read `execution.json`'s `pending_rewind_input` for this step
(a `verification` or `review` FAIL verdict) and address every point it raises first.

## Discipline

- **Fix the cause, not the symptom.** `root_cause_addressed` is the step's honesty field:
  the change must remove the diagnosed condition itself. If mid-implementation you
  discover the diagnosis was wrong, do NOT silently fix what you now believe is the real
  cause — report it: set `root_cause_addressed: false`, explain in `summary`, and let
  the review verdict route the WI honestly (there is no direct rewind to `root-cause`
  in this lane; see `workflow.md`'s rewind-boundaries section).
- **Work only inside the declared repo worktree(s)** (`repos/<name>/**`).
- **Keep the build green between commits.** Run the project's build and test commands
  (via `bash`) after each compilable unit; do not proceed on a red build.
- **Commit per compilable unit** with a message that references what it changes and why.
- **The repro test must now pass** — if `repro` encoded the failure as a failing test,
  that test passing on your tree is your own fastest feedback; run it before declaring
  done. Do not modify the test to make it pass (tightening an over-broad assertion you
  can *prove* was wrong is the only exception, and it belongs in `summary`).
- **No drive-by fixes.** Adjacent ugliness the diagnosis did not implicate stays
  untouched; note it in `summary` if it deserves its own WI.

## If you cannot finish

If the fix is infeasible as diagnosed, or the build cannot be made green within
reasonable effort, do not fabricate `build_status: green`. Leave it `red`, explain in
`summary`. A dishonest green corrupts `verification` and `review` downstream.

## Output

Emit the final JSON object per the schema above: every commit made (at least one - the
schema rejects an empty list), the final build
status, whether the root cause itself is addressed, and a prose summary for the next
reader.
