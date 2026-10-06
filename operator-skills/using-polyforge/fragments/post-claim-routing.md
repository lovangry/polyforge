## After Submit: Who Drives What (v1)

The operator's posture after `pf submit`: the full reference behind
output-format.md's resident short form. Workflow mechanics:
`fragments/workflow-v2.md`.

### What happens to a submitted wi

The daemon claims the job asynchronously, materializes the execution capsule
(repo worktrees cloned from the wi's frozen inventory, the bundle snapshot,
`runtime-state/execution.json`) and spawns ONE persistent WI Agent that
drives the workflow claim-to-terminal - every step runs inside that one
agent; it does not exit between steps. Your session never executes the
workflow. `pf observe <id>` is the follow-up.

### While it runs, the operator

- Watches progress: `pf observe <id>` (step, attempts, budget, pending
  question) and `pf ops status` (this machine's jobs).
- Answers an interactive step's needs_input question when the daemon relays
  it: the WI Agent persists the question, observe surfaces it, and the
  `answer` verb carries your reply back to the waiting step, which continues
  in place - no restart, no new step.
- Otherwise leaves the WI Agent to it.

### What the operator must NOT do

- Drive or edit the workflow's steps from the outside session. Execution
  state is the capsule's `runtime-state/execution.json`, written only by the
  WI Agent - an outside edit forks the truth the workflow runs on.
- Write into a repo outside a claimed work item (IR1). The capsule's
  worktrees are the WI Agent's execution site, not the operator's.
- Route around a failing gate by hand. A gate FAIL is workflow-internal
  business: `on_fail` rewind is the workflow's own bounded mechanism, and the
  FAIL verdict travels back into the target step's input. Hand-patching the
  artifact to make the gate pass defeats the review and leaves no record.

### Dispatcher half: outcome, not path

Hand an executor an OUTCOME plus acceptance criteria and stop - never an
execution path. A path in a prompt is followed literally and beats the
workflow's own step graph: write `commit -> push -> PR` into a prompt and
that is what runs - no step records, no gates. An outcome-only prompt cannot
produce this failure: there is no path in it to follow instead of the graph.
This is the full argument behind output-format.md's resident one-liner.

### Terminal outcomes

- **wrapped** - all steps done; final flush published; capsule sealed
  read-only.
- **failed** - a gate FAIL with no usable rewind (none declared, or the
  rewind limits already spent).
- **blocked** - waiting on the owner: over budget, an incomplete result, or
  an interactive step with nobody to answer it.

A blocked wi needs an owner decision - add budget, unblock, or cancel the wi -
never a session-side workaround that hides the block.
