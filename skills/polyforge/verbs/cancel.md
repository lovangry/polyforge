# cancel — terminate a non-active work item

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub. The daemon is not
involved. Cancel only ends a work item that has **no active generation**;
it never interrupts a running one (that is `pause` or `wrap --fail`).

## Triggers

- EN: cancel it / drop it / don't want it anymore / scrap this ticket /
  abandon it
- 中文: 不要了 / 取消这个票 / 作废 / 删掉这个任务

## Command

```
pf cancel <id>
```

The argument is the work item id (`wi_` + random chars) or its slug.

## Success

`cancelled <slug> (status canceled)`; `--json`: the server record raw.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id | No such work item — check the id. |
| `CORE-409` | The work item has an active generation (running) | It is being executed — use `pause`/`wrap` on the machine that runs it, not cancel. |
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |

A canceled work item is terminal; only a brand-new work item can carry the
work forward.
