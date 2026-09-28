# queue — list work items / show the flat ready queue

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub (00 §5.1; 02 §5.1
rows 2/4). The daemon is not involved.

## Triggers

- EN: what's ready today / what can I claim / show the queue / what's in the
  backlog / list work items / filter by status, assignee, milestone or epic /
  which ones are blocked and by what
- 中文: 今天有什么活 / 就绪队列 / 有什么可干的 / 列一下所有票 / 按状态筛 /
  里程碑过滤 / 哪些被阻塞了

## Command

```
pf queue [--project P] [--status S] [--assignee A] [--waiting-on W] [--parent ID] [--milestone M]
pf queue --ready [--project P]
```

`--project` defaults to the config's `project`. The plain form is the list
filter (02 row 2: `status`, `assignee`, `waiting_on`, `parent`, `milestone`);
`--ready` switches to the ready queue (02 row 4), whose response is flat two
arrays — `executable` (claimable right now; registry tickets without a bundle
are never listed) and `blocked` (waiting on a dependency or otherwise paused),
both ordered priority-first then oldest-first. v1 has no pagination.

## Success

`--json`: the server body raw. Human mode:

- list: `N work items` + a table (SLUG, STATUS, PRIORITY, BUNDLE, GOAL);
- ready: `project: …`, an `executable` table (ID, SLUG, PRIORITY, BUNDLE,
  GOAL) and, when non-empty, a `blocked` table adding WAITING_ON / BLOCKED_BY
  columns.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |
| `CORE-400` | Bad filter value | A filter flag is malformed — read `detail` and fix the flag. |
| `CORE-404` | Unknown project on `--ready` | No such project (or no access to it) — check the project name; a global admin is not membership-limited. |

A `connection refused` one-liner (no typed code) means the AIHub server is not
reachable — check `--server` / `PF_SERVER` and whether AIHub is running.
