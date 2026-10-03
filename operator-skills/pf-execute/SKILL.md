---
name: pf-execute
description: >
  Use when a claimed wi is ready to be carried out step by step, e.g. the user says
  execute, run it, or go.
---

# pf-execute - Wi Agent Main Loop

> **Stub.** The real body of this step is injected at call time by the `PreToolUse(Skill)`
> router (`hooks/pf-skill-router`): with superpowers enabled it tells you to delegate each
> step's implementation to `superpowers:subagent-driven-development` and **stop before finishing-a-development-branch**; without it, it injects this folder's
> `engine.native.md` (the native entry). In both cases it also injects
> `../_common/{memory,storage,lifecycle}.md` (step reporting; commit/PR/wrap/CI owned by
> polyforge; `.pf_*` hygiene; wrap + cleanup).
>
> **Fallback:** if you are reading this line and did NOT receive an injected step body
> (the router did not fire; Codex / OpenCode / pi load this file directly), read this folder's `engine.native.md` plus
> `../_common/{memory,storage,lifecycle}.md` and follow those, or run `/pf-doctor`. When an
> injected body is present, it takes precedence over this stub.

## The only execution path (aihub#788): check the wi's own graph FIRST

The legacy scenario step graph is **retired** (aihub#788 C6): no wi is driven through
the scenario-graph loop any more. A wi's execution path is its OWN graph, and
`pf_get_workflow(work_item_id=<current>)` says which one it has:

```
wf.steps_version > 0   -> pinned DB flow: the ONLY executable path. Read
                          `../using-polyforge/fragments/workflow-v2.md`, then run
                          `polyforge engine workflow --work-item='<current>' --continue`.
                          Follow its prepare/submit/approval instructions exactly; do not
                          call the low-level workflow tools by hand or synthesize approval.
wf.steps_version == 0  -> DIAGNOSTIC BRANCH - this wi is legacy/uncomposed (pre-aihub#708
                          stock). Do NOT run any scenario-graph loop (retired, aihub#788):
                          report "this wi is a legacy/uncomposed work item - pin a DB
                          workflow first (pf_update_workflow) or apply the owner's
                          disposition (migrate / freeze / close per the aihub#788
                          disposition artifact), then execute the pinned flow", offer to
                          compose+pin, and STOP.
```

New wis always carry a flow - the server composes and pins one at create (aihub#731), and
`workflow_mode=legacy` is refused at create (400 LEGACY_RETIRED, aihub#788). A
`steps_version == 0` wi is pre-aihub#708 stock whose disposition is pending: the correct
move is migration to a pinned flow, never the retired loop.

## Usage

**Purpose**: Execute the wi's own graph (a pinned DB workflow generation) to completion.

**Pattern**: `/pf-execute`

**Required**: a currently-claimed wi (state file at `<workspace>/.polyforge/state/<wi_id>.json`).

**Flags**: Mode is derived from the wi's `requires_human_session` (`false` -> auto dispatch per step; `true` -> interactive step-by-step).

## NL Triggers

- "execute" / "run it" / "start executing" / "go"
