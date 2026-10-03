## Workflow v2 - stored steps & the pinned flow (aihub#708)

A claimed wi walks ONE step graph, and `pf_get_workflow(work_item_id=<wi>)` says
which:

- `steps_version == 0` + `steps: null` - read WITH `workflow_mode`: `legacy` = a
  RETIRED execution path (aihub#788 C6): the wi is pre-aihub#708 stock, `/pf-execute`'s
  DIAGNOSTIC branch reports it for migration (pin a DB flow via `pf_update_workflow`, or
  the owner's migrate/freeze/close disposition) - the scenario-graph loop never runs;
  `pending` = the same shape but UNCLAIMABLE (**409 `COMPOSE_PENDING`**).
- `steps_version > 0` - a PINNED DB workflow generation: the stored flow is the authority
  for what runs next, in what order, pinned to exact skill versions.

### Reading stored state (server-enforced facts)

- Progress arrives per step: status, review_verdict, step_attempt_id, open_invocation_id,
  approved, rhs. Results live in the workflow tables and intentionally create NO legacy
  `wi_step_state` rows - a workflow wi's `step_state` is absent by design, not "no steps".
- The next action is one of: `run` (a step may start) / `open_invocation` (record its
  result, or reconcile it away) / `repair_required` (a step failed or its review failed;
  only an authorized repair reopens it) / `revision_required` (a human rejected the
  step's latest RHS artifact: the SAME step re-records a superseding artifact for
  another decision; nothing later runs until it) / `approval_required` (an rhs step completed but
  no human decision applies to its artifact yet) / `complete` (every step done; wrap
  remains an explicit act).
- Step invocations are SERVER-minted (`pf_start_workflow_step`); a worker result must
  echo the invocation identity exactly (`pf_workflow_result`) and can never carry an
  approval - a result that tries is rejected outright.
- Approval (`pf_approve_workflow`) is HUMAN-ONLY: the server refuses a machine
  credential whatever role it holds, and there is no actor parameter - the authenticated
  principal is the actor. An approval is a distinct act from an artifact annotation
  (`/pf-revise`): resolving annotations never approves a step, and approving never
  resolves annotations.
- Recovery: `pf_repair_workflow` (kind=retry for an infrastructure-class failure: a
  provider_error, an invalid_result whose recorded envelope is the controller-synthesized
  no-output one, or a blocked whose recorded cause is the read-only toolset's missing command
  execution; kind=episode after a
  review FAIL - the role set is CONDITIONAL: a pre-code design-gate FAIL takes a
  repair producer + fresh independent review WITHOUT verification; a post-code gate
  FAIL adds fresh verification when the flow has one. Each role starts with the
  episode id; kind=supersede after a worker-reported BLOCKED or INCOMPLETE result with
  a non-FAIL verdict (aihub#755; incomplete since aihub#775) - the session
  driver's recorded judgment that the outcome is resolvable, binding one fresh
  invocation of the same step while the blocked/incomplete result
  stays in history) and `pf_reconcile_workflow` (fences a dead or paused
  attempt's open invocations so the CURRENT attempt can start replacements).
- A review FAIL pauses the attempt - recoverable, never terminal.

### Callable three-mode protocol

For a claimed pinned-flow wi, the main harness does not improvise the loop and does not
feed symbolic `{step_id,output}` descriptors to a model. Use the controller protocol:

```text
polyforge engine workflow --work-item='<wi>' --continue
```

Its statuses are actionable:

- `prepare_required` - run the printed `--prepare` command. The response opens ONE
  server-fenced invocation and returns the exact pinned skill entry/support files, params,
  access-checked concrete predecessor values, and `expected_output_schema`. Discuss those
  instructions in this main human session. Do not edit/rewrite the bundle; author only the
  submit object `{invocation:<unchanged preparation.invocation>, output:<schema value>}`.
- `prepared` - continue the human discussion (grill questions one at a time; spec revisions
  preserve the user's decisions), then write that submit object to a private temporary JSON
  file and call `polyforge engine workflow --work-item='<wi>' --submit='<file>'`. Submission
  validates the pinned schema, saves through the existing attempt-authorized methodology
  artifact API, and records the controller result with the exact returned id/version/hash.
- `approval_required` - show the named exact artifact/version/hash to the human. Do not call
  approval from model judgment. Only after the authenticated human explicitly says
  **approved** or **rejected** for that displayed tuple may the human-session caller invoke
  `pf_approve_workflow` with the response's exact `steps_version`, `step_id`, and `artifact`.
  Missing approval stays here. Rejection becomes `revision_required`; `--continue` then
  points back to `--prepare` for the SAME step and never starts a successor.
- `run` automatic steps reached by `--continue` execute through the same `controller.RunStep`
  used by drain. Repeat `--continue` after each result/gate. `complete` still requires the
  ordinary explicit wrap action; the protocol never wraps by implication.

### Step timeouts and worker liveness (aihub#732)

`--continue` runs an automatic step IN-LINE: the driving process waits on the worker, so the
CALLER's outer clock matters. Two rules and one probe:

- **The controller's step budget is the only clock that may stop a healthy worker.** The
  response's `step_budget_seconds` is the bound the controller enforced (default 2h;
  `--step-timeout=<Go duration>` overrides, e.g. `--step-timeout=90m`). NEVER wrap the
  `--continue` call in a shell `timeout` smaller than the printed budget: a 3600s shell
  timeout around a 2h-budget step is exactly how healthy workers were killed mid-work
  (measured 2026-09-20: 11 planned files on disk, worker dead, invocation stranded). If you
  must cap the call externally, cap it at budget + margin, or don't cap it at all and let
  the controller's confirmed-stop timeout fire.
- **A recoverable timeout names the worker.** The output's `timed_out_worker` carries
  `{step_attempt_id, worker_pid, detail}`: probe the pid (`kill -0 <pid>`) instead of
  guessing, and only reconcile/pause when the worker is actually gone.
- **The bare call is the status path**: `polyforge engine workflow --work-item='<wi>'` with
  no mode flag prints the classification plus every open invocation's
  `{step_id, step_attempt_id, worker_pid, worker_alive, advice}`. `worker_alive: true` means
  wait (the step is still running; do not reconcile). `worker_alive: false` means the invocation
  is stranded; reconcile (`--reconcile=<attempt>`) or pause. `worker_known: false` means no
  durable pointer (the worker predates this registry); judge from the timeline.

The three modes share server transitions: unattended (`requires_human_session=false`) is
owned by drain; automatic steps inside a human session are driven by `--continue`; manual
or effective-RHS steps use `--prepare`/`--submit`. Effective human gating remains
**wi RHS AND step RHS**. A workflow read/error never falls back to the scenario graph.

### Who drives (mode split)

- `requires_human_session=false`: `polyforge drain` (Layer 3) claims, drives and wraps;
  a session that claimed such a wi dispatches it rather than pacing it.
- `requires_human_session=true`: the human paces with the callable protocol above. The main
  harness runs `--continue`, uses `--prepare`/`--submit` for discussion-authored steps, and
  repeats `--continue` for automatic steps. Approval remains a separate authenticated human
  act bound to the exact artifact; the protocol never synthesizes it.
- Composing/revising a flow: `pf_update_workflow` (author-tier; CAS via
  `expected_steps_version`; `requires_human_session` explicit on every revision; refused
  while a worker is in progress). New flows pin EXACT skill versions; `skill_version: 0`
  ("latest accessible") is resolved and frozen at pin time - never a silent fallback a
  running flow silently re-resolves.
- Wrapping: explicit, as always (`/pf-stop --wrap`); for a pinned-flow wi, pass the
  no-steps reason naming the workflow result tables when the gate asks.

### Compatibility (what did NOT change)

The legacy scenario path is RETIRED as an execution path (aihub#788): `workflow_mode=legacy`
is refused at create (400 LEGACY_RETIRED), `/pf-execute` routes every wi through its own
graph, and surviving open legacy wis are dispositioned (migrate / freeze / close) per the
aihub#788 artifact before any execution. `/pf-spec` / `/pf-plan` / pf-crystallize operate
as before on the wi's own graph. "A claim always walks the graph" reads as: the graph the
wi actually has - and a steps_version==0 wi gets migration first, never the retired loop.
The seeded reference bundles behind
new flows (grill-me, spec, plan, design-review, code-change, review, verification, ship, ci) live in the
skill registry; see docs/workflow-v2/ in the aihub repo for the seed, its licenses and the
pinning rules.
