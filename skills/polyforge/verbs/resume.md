# resume — resume a paused work item

Execution-plane verb: routed to the local daemon (the resume route: a fresh
claim, generation+1). Resume is **always a new generation** with a newly
derived WI Agent — it is not "unpause the old run"; the previous generation
ended when the pause landed. The new generation continues from the
execution record the sealed capsule holds.

## Triggers

- EN: resume it / continue the paused one / bring it back / pick it up again
- 中文: 恢复执行 / 把暂停的那个恢复 / 重新跑起来（已暂停的）

## Command

```
pf resume <id> [--priority low|normal|high|urgent]
```

Same CLI precheck as `submit`: an unknown id or a terminal work item is
refused before the daemon is involved.

## Success

`resumed <id> (fresh claim accepted by the local daemon)`. `--json`: the
raw answer. The daemon queues the job; its dispatcher derives the new
agent, which recovers its position from the capsule's
`runtime-state/execution.json` and continues from where the pause landed.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id (CLI precheck) | No such work item — check the `wi_` id. |
| (no code) `submit: <id> is terminal (<status>)` | The work item is wrapped/failed/cancelled | A finished work item cannot be resumed — file a new one. |
| `DAEMON-409` | The work item is already active or queued on this daemon, or drain mode rejects the external resume | Observe to see the running attempt; in drain mode leave drain first. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Graceful shutdown in progress | Wait for restart, then resume. |
| `CORE-409` (async, in the daemon log) | The server-side claim was refused — an active generation still exists elsewhere | Someone (or another machine) is already running it — observe, or take over as admin if it is stale. |
| `dial unix …` | Daemon not running | Start the daemon or check `--socket`. |

Do not route "continue / go on" of a **running** work item here — that is
`observe`. `resume` applies to a paused work item only.
