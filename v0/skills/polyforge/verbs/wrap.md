# wrap — close a running work item as terminal

Execution-plane verb: routed to the local daemon (00 §5.1; 04 §5.1 wrap row,
same final-publish requirement as pause). `wrap` is a terminal outcome: the
generation closes for good, the job is sealed, and no re-claim happens
automatically.

## Triggers

- EN: wrap it up / we're done / call it finished / mark it failed / close it
  out / end it
- 中文: 收尾 / 完事了 / 结束 / 判完成 / 收工 / 判失败 / 结束这个任务

## Command

```
pf wrap <id>          # close with reason=wrap  → terminal success
pf wrap <id> --fail   # close with reason=fail  → terminal failure
```

No other outcomes exist on this verb. The daemon performs the closing
publish itself and then ends the generation server-side.

## Success

Human mode: `wrap requested for <id>` (or `fail requested for <id>`) plus
the daemon's raw answer; `--json`: the raw answer alone. After the close
lands the work item is terminal (`wrapped` / `failed`) and disappears from
the local jobs list — a second wrap answers `DAEMON-404`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | No local job — the work item is not running here, or it was already wrapped | Check observe first: if it already closed, there is nothing to do; if it runs elsewhere, wrap it there. |
| `DAEMON-409` | A close is already in progress for this job | Wait for the in-flight close to settle, then observe. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Graceful shutdown in progress | The shutdown itself pauses remaining jobs; re-run the wrap after the daemon restarts. |
| `DAEMON-511` (`SHUTDOWN_FLUSH_FAILED`) | The closing publish did not converge | The close cannot be confirmed — manual investigation needed; check the daemon log before retrying. |
| `DAEMON-403` (`stale_generation` / `secret_rolled_back`) | The local scene was taken over or its credential rolled back; quarantined | Do not retry — the only arbitration path is an admin force-takeover. |
| `CORE-*` passthrough (e.g. `CORE-403`, `CORE-425 FLUSH_REQUIRED`, `CORE-409`, `CORE-410`) | The Core-side end-generation was refused | The typed Core verdict arrives verbatim — typically the generation is no longer yours to end, or the closing evidence did not check out; act per its detail. |
| `dial unix …` | Daemon not running | Nothing is executing here — start the daemon or check `--socket`. |

A `--fail` close is permanent for this work item: the CLI refuses any later
submit of it up front (`terminal (failed)`), and only a brand-new work item
can carry the work forward.
