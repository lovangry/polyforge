# wrap — close a running work item as terminal

Execution-plane verb: routed to the local daemon (the wrap route). `wrap`
is a terminal outcome: the generation closes for good, the capsule is
sealed read-only and pushed, and no re-claim happens automatically.

**Two paths close a work item, and they arbitrate.** The human path is this
verb; the agent path is the executor's own `flush(wrap)` call (the agent
finishing its workflow on its own). Exactly one close wins: the daemon's
seal guard makes the late arrival answer `DAEMON-409` while the first close
is in flight, and a close that finds the work item already terminal answers
gracefully (`already_terminal`), never a hard error. The winner performs
the final commit+push and ends the generation exactly once.

## Triggers

- EN: wrap it up / we're done / call it finished / mark it failed / close it
  out / end it
- 中文: 收尾 / 完事了 / 结束 / 判完成 / 收工 / 判失败 / 结束这个任务

## Command

```
pf wrap <id>          # close with reason=wrap  → terminal success
pf wrap <id> --fail   # close with reason=fail  → terminal failure
```

No other outcomes exist on this verb.

## Success

Human mode: `wrap requested for <id>` (or `fail requested for <id>`) plus
the daemon's raw answer (`--json`: the raw answer alone — `sealed`, or
`already_terminal` with the server-side status when the work item had
already closed). After the close lands, the work item is terminal
(`wrapped` / `failed`) and disappears from the local jobs list. A second
wrap answers the graceful `already_terminal` shape, not an error.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | No local job and the work item is not terminal on the server | It is not running here — check observe; if it runs elsewhere, wrap it there. |
| `DAEMON-409` | A close is already in progress for this job — the agent's own wrap flush, another pause/wrap, or a mid-recovery state | Wait for the in-flight close to settle, then observe; the work item is being closed exactly once either way. |
| `CORE-*` passthrough (e.g. `CORE-403`, `CORE-409`) | The server-side end-generation was refused | The typed Core verdict arrives verbatim — typically the generation is no longer yours to end; act per its detail. |
| `DAEMON-500` | The close failed on the daemon side | The job is left mid-seal, recorded as such; the next daemon start finishes the close. Check the daemon log before retrying. |
| `dial unix …` | Daemon not running | Nothing is executing here — start the daemon or check `--socket`. |

A `--fail` close is permanent for this work item: the CLI refuses any later
submit of it up front (`terminal (failed)`), and only a brand-new work item
can carry the work forward. After a wrap, `ship` delivers the sealed
commits to git origin.
