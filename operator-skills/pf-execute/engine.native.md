# pf-execute - native engine (Wi Agent main loop)

> Injected when `superpowers` is absent; bracket / ship / wrap come from `_common/lifecycle.md`.
> **`Read @@PLUGIN_ROOT@@/skills/pf-execute/references/engine-native-details.md` before step 1**
> - §0h's `polyforge engine` verbs, the rhs=true loop, older-binary fallbacks.

## The ONLY execution path (aihub#788): the wi's own graph

The scenario-graph loop below is RETIRED (aihub#788 C6). One entry, checked first:

```
wf = pf_get_workflow(work_item_id=<current>)
wf.steps_version > 0  -> pinned DB flow: Read `../using-polyforge/fragments/workflow-v2.md`,
   run `polyforge engine workflow --work-item='<current>' --continue`; follow its
   prepare/submit instructions exactly.
wf.steps_version == 0  -> DIAGNOSTIC BRANCH (§4): legacy/uncomposed wi. STOP - the retired
   loop must not run. Report: "no pinned DB workflow - pin one first (pf_update_workflow
   or the owner's aihub#788 disposition), then execute the pinned flow." Compose+pin, or stop.
```

## Execute (rhs=false, auto mode) - RETIRED reference

> Spec of the still-shipping `polyforge engine startup`, `resolve-role`, `parse-review`,
> `bracket-plan` verbs; prescribes nothing. `polyforge engine startup` clones the
> scenario at `<workspace_root>/.repo/<owner>__<repo>/` (SHA pinned into `.pf_meta.json`).
> A review FAIL PAUSES via §0c, never terminal (aihub#788/#706).

```python
# STEP KIND -> ROLE: `polyforge engine resolve-role --step-id='<sid>'` decides it. Models live ONLY
# in the agent files. HARNESS (aihub#670): the cc spellings below; off cc your row is §0f:
# id `step-<role>` (pi, codex, opencode), call differs.
ROLE_AGENT = {"executor": "polyforge:step-executor", "operator": "polyforge:step-operator",
    "explorer": "polyforge:step-explorer", "reviewer": "polyforge:step-reviewer",
    "designer": "polyforge:step-designer"}

sa_id = new_ulid()
pf_update_step(work_item_id=<current>, step_id=steps[0].id, status="in_progress",
               step_attempt_id=sa_id)

for i, (step_id, expanded) in enumerate(steps):   # steps[] as `engine startup` printed them
    role = `polyforge engine resolve-role --step-id='<step_id>'`.role
    dispatch Agent(subagent_type=ROLE_AGENT[role], prompt=§0b)   # cc's row; §0f for yours
    # ^ copy §0b's template VERBATIM; the AGENT ID is the only channel that reaches a model.

    if a step called pf_pause_attempt (or a pf_* call is rejected "attempt is paused"):
        break   # stop the loop; no retry, and do NOT call pf_complete_attempt (§0e)

    if role == "reviewer":
        # parse_review_result = `polyforge engine parse-review`, subagent output on STDIN
        result = that verb's {"result": "PASS"|"WARN"|"FAIL"}
        if result == "FAIL":  do §0c (step failed + attempt PAUSED), then break
        if result == "WARN":  print the warning and continue

    # Next: mint next_sa (None on the last step), run `polyforge engine bracket-plan`
    # (§0h; QUOTE every value), make the calls it prints, then sa_id = next_sa.
    next_sa = new_ulid() if i + 1 < len(steps) else None

# all steps done -> wrap + cleanup (_common/lifecycle.md ## Once per wi)
```

**The model tier is keyed on the step's KIND, never on `level:`** - that is review DEPTH, a
different parameter sharing the key name. §0f has the contract, the mapping and its gate.

## Execute (rhs=true, interactive mode) - RETIRED reference

One layer (aihub#644), retired with the loop: dispatch as above, then STOP - wait for the human
before bracketing next. **`Read` §1** - the verbs, `skip` **completes** (§1b), §1c adjudicates
verdicts, §0d startup values.
