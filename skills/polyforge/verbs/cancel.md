# cancel — abandon a non-active work item

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub. Cancel only ends a
work item that has **no active generation**; it never interrupts a running
one (that is `pause` or `wrap --fail`). This is also the "abandon" of a
**blocked** work item (the counterpart of `resume` = continue; there is no
separate `unblock` verb). Allowed to the task's reporter, a project
maintainer or an admin.

One exception touches the local daemon: a work item the daemon parked
`repair_pending` after a restart (capsule missing / project unmapped; see
`pf ops status`) is still `running` at AIHub under the daemon's kept session,
so AIHub's cancel refuses it (`CORE-409`). When this machine's daemon holds
the work item in that state, `pf cancel` first checks with YOUR api key that
you are the reporter, a maintainer or an admin (a read-only probe), then asks
the daemon to end the session itself (verdict `canceled`), drop the parked
record and write a `repair_abandoned` event. A session end AIHub does not
acknowledge is retried by the daemon until it lands.

## Triggers

- EN: cancel it / drop it / don't want it anymore / scrap this ticket /
  abandon it / give up on the blocked one
- 中文: 不要了 / 取消这个票 / 作废 / 删掉这个任务 / 放弃

## Command

```
pf cancel <id>
```

The argument is the work item id (`wi_` + random chars) or its slug.

## Success

`cancelled <slug> (status canceled)`; `--json`: the server record raw. For
the repair_pending abandon: `canceled <id>: the repair_pending work item was
abandoned — the daemon ended its session and dropped the parked record`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id | No such work item — check the id. |
| `CORE-409` | The work item has an active generation (running) and is not parked repair_pending on this daemon | It is being executed — use `pause`/`wrap` on the machine that runs it, not cancel. |
| `AUTH-403` | The caller is not the reporter, a project maintainer or an admin (also from the repair_pending abandon probe) | Ask the task owner or a maintainer. |
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |

A canceled work item is terminal; only a brand-new work item can carry the
work forward.
