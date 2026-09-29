# queue — list work items / show the ready queue

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub. The daemon is not
involved.

## Triggers

- EN: what's ready today / what can I claim / show the queue / what's in the
  backlog / list work items / filter by status
- 中文: 今天有什么活 / 就绪队列 / 有什么可干的 / 列一下所有票 / 按状态筛

## Command

```
pf queue [--project P] [--status S]
pf queue --ready [--project P]
```

`--project` defaults to the config's `project` (required for the ready
queue when unset). The plain form is the list filter (`status`:
queued|running|paused|blocked|wrapped|failed|canceled); `--ready` switches
to the project's ready queue — the claimable-now projection, flat
`{items:[...]}`, ordered priority-first.

## Success

`--json`: the server body raw. Human mode: `N work item(s)` + a table
(SLUG, STATUS, PRIORITY, BUNDLE, GOAL).

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |
| `CORE-400` | Bad filter value | A filter flag is malformed — read `detail` and fix the flag. |
| `CORE-404` | Unknown project | No such project (or no access to it) — check the project name. |
| (no code) `queue: --project is required` | No project resolvable | Pass `--project` or set `project` in the config. |

A `connection refused` one-liner (no typed code) means the AIHub server is
not reachable — check `--server` / `PF_SERVER` and whether AIHub is running.
