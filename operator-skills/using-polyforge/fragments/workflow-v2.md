## Workflow Execution Model (v1)

How a claimed work item actually runs, where progress lives, and what drives
the steps. (Filename is historical; the model below is v1 only - the v0
stored-flow machinery is deleted, there is no compatibility path.)

### A workflow is a pinned bundle

- A workflow = a bundle: `workflow.md` + one step skill per step
  (`agents/<step-id>.md`). At WI creation the server reads the local
  checkout, validates the frontmatter, and pins a `bundle_digest`; the WI
  stores only `bundle_name` + digest (+ budget baseline). The reference is
  immutable - changing a workflow means cancelling the WI and creating a
  new one, never re-pinning.
- The WI Agent materializes the bundle read-only into the capsule (step 0)
  and verifies it against the pinned digest; that snapshot never changes
  during execution.
- `workflow.md`'s frontmatter `steps` array IS the execution order (array
  order is the only ordering source). Steps carry
  `kind|interactive|gate|on_fail|budget|model_candidates`; the Markdown
  body is narrative only and is never parsed for control flow.

### Who drives, where state lives

- ONE persistent WI Agent process drives the loop claim-to-terminal: pick
  the earliest non-terminal step, dispatch it, record the result, repeat.
  If it dies, the daemon re-derives a fresh one; recovery re-derives
  position purely from the capsule's `runtime-state/execution.json`,
  never from process memory.
- `runtime-state/execution.json` is the ONLY execution authority (WI Agent
  single-writer): `current_step`, per-step status
  `pending|in_progress|done`, archived `attempts[]`, `pending_question`,
  `pending_rewind_input`, `budget_consumed`, `rewind_stats`, and
  `workflow_status` (`running|completed|failed_gate|
  failed_rewind_exhausted|blocked`). Step outputs land in capsule
  `artifacts/` and `steps.<id>.output`; downstream steps read them by path
  convention.

### Observing progress

- `pf observe <id>` and `pf ops status` read the published capsule state;
  the whole `execution.json` IS the progress surface, so published state
  answers "which step, which status, what verdict, what question".

### Failure semantics - two lanes, never mixed

- **Gates produce business verdicts.** Gate FAIL with no `on_fail` (or
  `action: terminate`) ends the workflow (`failed_gate`). `action: rewind`
  is bounded: `[target, gate)` steps reset to `pending` (prior outputs
  archived as attempts), the FAIL verdict text is delivered to the target
  step; exhausting `rewind.max_total` (default 4) or `max_per_step`
  (default 2) ends it (`failed_rewind_exhausted`).
- **Infrastructure failures never rewind.** Crash, upstream 5xx/429, or
  output failing the step's `output` schema produces NO business verdict:
  the step stays `in_progress` and resumes in place, advancing the model
  candidate one slot per re-dispatch (bounded by the candidate list; gate
  FAIL never switches candidates).
- **Interactive steps** (grill/grill-review) block on needs_input: the
  question is persisted as `steps.<id>.pending_question` and surfaced via
  the publish chain; the owner's answer returns via the daemon's answer
  relay and the SAME step continues in place.
- Terminal failure leaves the WI blocked for an owner decision; it is not
  auto-retried.

Authority: docs/design/polyforge-vnext/01-workflow-skills.md,
03-execution-capsule.md (§4.2).
