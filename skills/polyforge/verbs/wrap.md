# wrap — close a running work item as terminal

Execution-plane verb: routed to the local daemon (the wrap route). `wrap`
is a terminal outcome: the session ends for good, the capsule is
sealed read-only and pushed, and no re-claim happens automatically.

**Closes arbitrate.** The human path is this verb (or `pause`); the
workflow's own terminal exit is closed by the daemon's recovery engine.
Exactly one close wins: closes of one work item queue behind each other
on the daemon; a later one that finds the first close still retrying
answers `DAEMON-409`, and a close that finds the work item already
terminal answers gracefully
(`already_terminal`), never a hard error. The winner performs the final
commit+push and ends the session exactly once.

## Triggers

- EN: wrap it up / we're done / call it finished / mark it failed / close it
  out / end it
- 中文: 收尾 / 完事了 / 结束 / 判完成 / 收工 / 判失败 / 结束这个任务

## Command

```
pf wrap <id>          # close with reason=wrap  → terminal success
pf wrap <id> --fail   # close with reason=fail  → terminal failure
```

No other outcomes exist on this verb. `--fail` is the owner's failed
verdict on a RUNNING job. It is not a door into a parked workflow: a
work item parked blocked is continued with `pf resume <id>` or abandoned
with `pf cancel <id>`, and `pf wrap --fail` on one is refused (`DAEMON-409`).

**Wrap while the work item is still being claimed** (`pf ops status` shows
`claiming`): the daemon cancels the clone, quarantines the partial
capsule and ends the session `wrapped` (or `failed`) with no seal push —
no agent ran, so the capsule chain is unchanged. If the hub refuses that
close, the daemon falls back to a blocked close (retried until it lands).

## Success

Human mode: `wrap requested for <slug>` (or `fail requested for <slug>`);
`--json` prints the daemon's raw answer instead — `sealed`, or
`already_terminal` with the server-side status when the work item had
already closed. After the close lands, the work item is terminal
(`wrapped` / `failed`) and disappears from the local jobs list. A second
wrap answers the graceful `already_terminal` shape, not an error.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | No local job and the work item is not blocked or terminal on the server | It is not running here — check `pf watch <id> --once`; if it runs elsewhere, wrap it there. |
| `DAEMON-409` `the work item is parked blocked …` | A close of a work item parked blocked on an incomplete result (polyforge#116) or by a pause during its claim: a parked workflow never completed, so there is nothing to seal — a `wrap` (success), a `--fail`, or a `pause` of a parked workflow is all refused; there is no hub verdict door for a parked `--fail` (the terminate door is deleted, polyforge#149) | Use `pf resume <id>` to continue it, or `pf cancel <id>` to abandon it — those are the only two exits of a parked workflow. |
| `DAEMON-404` with `<id> has no running job on this daemon; if it is parked blocked …` | `--fail` on a work item with no local job (most often one parked blocked on an incomplete result, polyforge#116). The CLI makes NO hub call, terminates nothing and exits non-zero (the wrap did not happen): a parked workflow has exactly two owner moves, and neither of them is this verb | Continue it with `pf resume <id>`, or abandon it with `pf cancel <id>`. The work item stays blocked until one of those runs. |
| `DAEMON-409` | The job's close is already in progress (sealing, or retrying the session end until aihub takes it) | Wait for the in-flight close to settle, then check `pf watch <id> --once`; the work item is being closed exactly once either way. |
| `CORE-*` passthrough (e.g. `CORE-403`, `CORE-409`) | The server-side session end was refused | The typed Core verdict arrives verbatim — typically the session is no longer this daemon's to end; act per its detail. |
| `DAEMON-500` | The close failed on the daemon side | The job is left mid-seal, recorded as such; the next daemon start finishes the close. Check the daemon log before retrying. |
| `dial unix …` | Daemon not running | Nothing is executing here — run `pf daemon ensure`, then retry once (or check `--socket`). |

A `--fail` close is permanent for this work item: the CLI refuses any later
submit of it up front (`terminal (failed)`), and only a brand-new work item
can carry the work forward. Delivery is not a wrap concern (`verbs/ship.md`).
