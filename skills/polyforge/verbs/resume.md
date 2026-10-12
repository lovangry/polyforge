# resume — continue a paused work item, or a blocked one

Execution-plane verb with two doors; the hub status of the work item picks
the door. This is the human "continue" (the "abandon" of a blocked or paused
work item is `cancel`; the separate `unblock` verb no longer exists). The
blocked door is open to the task's reporter, a project maintainer or an
admin.

## Door 1 — a paused work item (hub status `paused`)

The operator-pause move: a `clean_pause=false` PATCH on the hub. Nothing is
claimed locally and no daemon route is contacted — the paused work item
stays with its HOLDER daemon (the session's machine attaches on its next
daemon start; a work item whose holder is gone moves with
`pf takeover <id> --reason ...`; a live holder daemon does not pick this up
until its next start — to continue immediately use `pf takeover`).

Success: `resumed <id>: clean_pause cleared; the paused work item stays
with its holder daemon`. `--json`: the raw answer.

## Door 2 — a blocked work item (hub status `blocked`)

Two steps: the CLI first lands aihub's resume-blocked door with the HUMAN's
own api key (data plane, like `cancel`; the check is reporter / project
maintainer / admin against the person typing, never the daemon's machine
key), then asks the local daemon to claim and start the WI. A refused door
(`AUTH-403`) stops there and the daemon is never contacted.

The §6.4 owner move (polyforge#116): the workflow parked on an honest
`incomplete` step result — `workflow_status=blocked` in the capsule, the
current step frozen `in_progress`, the owner holding the next move. The
daemon re-establishes execution over the parked capsule: the hub's
resume-blocked door returns the WI to claimable, the daemon claims it back,
and the claim/start path's owner move unblocks the workflow — the frozen
current step re-dispatches IN PLACE as a fresh derivation (the spawn
journal and the step's prior output survive; the resumed lane continues
the parked capsule, never a pre-blocked re-derivation).

Success: `resumed <id>: the workflow was parked blocked (incomplete
result) — the §6.4 owner move re-established execution over the parked
capsule`, then `(the frozen current step re-dispatches in place as a fresh
derivation; watch it with pf observe <id>)`. `--json`: the raw answer.

Cross-daemon safety: the default is that the daemon which sealed the block
continues it. When a DIFFERENT daemon picks it up, the start is refused
(`DAEMON-518`, the claim is abandoned back to blocked) unless the aihub
capsule head equals the head the sealing daemon recorded at block time —
the sealing daemon's last push did not land, so continue it on that
machine, or abandon with `pf cancel <id>`.

A blocked workflow must never answer the pause copy, and a plain claim
(`pf submit`) of a parked work item still refuses (`CORE-409
NOT_CLAIMABLE`) — the owner's moves go through this door, not through the
claim gate.

## Door 3 — a work item parked `repair_pending` on THIS daemon

After a daemon restart a work item whose capsule directory is missing, or
whose project is no longer in the `[workspace]` mapping, parks
`repair_pending`: the session is kept, the daemon starts normally, the other
work items recover, and a `capsule_missing` / `project_unmapped` event names
the path or project and the fix. Restore the directory (or re-add the
project), then `pf resume <id>`: the daemon re-checks and continues the SAME
session in place — never a re-claim, nothing is rebuilt from the remote. A
`paused` work item has its `clean_pause` cleared first. Still broken answers
`DAEMON-517` (path in the detail) or `DAEMON-412` (project unmapped) and the
work item stays parked. To give up instead: `pf cancel <id>` or
`pf wrap <id> --fail` (see their fragments).

## Triggers

- EN: resume it / continue it / continue the paused one / it parked blocked,
  continue it / bring it back / pick it up again
- 中文: 恢复执行 / 继续 / 把暂停的那个恢复 / 阻塞了继续跑 / 重新跑起来

"Unblock it" and "give up on it" are not this verb's second spelling:
continue is `resume`; abandon is `cancel`.

## Command

```
pf resume <id>
```

Same CLI precheck as `submit`: an unknown id or a terminal work item is
refused before anything else happens.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id (CLI precheck) | No such work item — check the `wi_` id. |
| (no code) `submit: <id> is terminal (<status>)` | The work item is wrapped/failed/cancelled | A finished work item cannot be resumed — file a new one. |
| `DAEMON-409` | The work item is already active or queued on this daemon, or drain mode rejects the resume | Observe to see the running attempt; in drain mode leave drain first. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Graceful shutdown in progress | Wait for restart, then resume. |
| `AUTH-403` (from the resume door) | The caller is not the work item's reporter, a project maintainer or an admin | Ask the task owner or a maintainer to continue it. |
| `DAEMON-517` | The work item is parked `repair_pending` on this daemon (restart found its capsule missing) and the capsule is still missing | Restore the capsule directory named in the detail (the session is kept), then `pf resume` again; or give up with `pf cancel <id>`. |
| `DAEMON-412` | Parked `repair_pending` because its project is not in the `[workspace]` mapping, or a submit/claim for an unmapped project | Re-add the project to a `[workspace."<path>"]` section (`pf doctor` checks it), then `pf resume`. |
| `DAEMON-516` | The claim behind the resume could not clone the capsule for a work item with published progress; a local init would restart the workflow at step 1, so it is refused and the claim returns to blocked | Fix aihub reachability / credentials, then `pf resume` again. |
| `DAEMON-518` | A different daemon than the sealing one claimed the continue and the aihub capsule head is not the sealed head | Continue it on the machine that sealed it, or abandon with `pf cancel <id>`. |
| `CORE-409 NOT_BLOCKED` | The resume door on a work item that is no longer blocked | The state moved — check `pf queue`/`pf observe`; a paused WI unpauses with this same verb. |
| `CORE-409 NOT_CLAIMABLE` (from the resume door) | The blocked WI still carries a session (a half-settled close) | Nobody may resume it until the holder settles — check the holder daemon or take over as admin. |
| `CORE-404` (from the resume door, on claim) | Another machine claimed the work item between the door and the claim | It resumed elsewhere — observe there. |
| `dial unix …` | Daemon not running | The blocked-door resume re-establishes execution, so a daemon must be involved — start the daemon or check `--socket`. |

Do not route "continue / go on" of a **running** work item here — that is
`observe`. The §6.4 sibling moves of Door 2 (rewind to a step, terminate
the WI) are not exposed on this verb: `pf cancel <id>` abandons a blocked
work item, and `pf wrap --fail <id>` records the owner's failed verdict.

## Watching after a resume

`pf resume` subscribes the resuming chat to the work item like `pf submit`
does; nothing to re-arm. If the chat's watcher is not running, start it (see
the `pf watch` section of `SKILL.md`).
