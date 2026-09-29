# cancel — terminate a non-active work item

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub (00 §5.1; 02 §5.1
row 5, cancel contract 02 §6.5). The daemon is not involved. Cancel only ends a
work item that has **no active generation**; it never interrupts a running
one.

## Triggers

- EN: cancel it / drop it / don't want it anymore / scrap this ticket /
  abandon it
- 中文: 不要了 / 取消这个票 / 作废 / 删掉这个任务

## Command

```
pf cancel <id>
```

The argument is the work item **id** (`wi_` + 12 chars) — this path has no
slug resolution. Allowed source states: `queued`, `paused`, `blocked`.
Authorized for the reporter, a project maintainer, or an admin.

## Success

`--json`: the final record raw. Human mode: `cancelled <slug> (status <status>)`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |
| `CORE-403` | Caller is not the reporter / a maintainer / an admin | You are not allowed to cancel this work item. |
| `CORE-404` | Unknown or invisible id | No such work item — check the id. |
| `CORE-409` | The work item is `running` (an active generation exists; the detail names the status) | A running work item cannot be cancelled — pause it first (or wrap it), then cancel. |
| `CORE-410` | The work item is already terminal (wrapped/failed/cancelled) | It is already closed — nothing to cancel. |

Exit 2 with a `pf:` usage line means a local argument mistake (e.g. more or
fewer than exactly one id).
