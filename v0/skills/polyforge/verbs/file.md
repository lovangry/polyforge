# file — create, edit or show a work item

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub (00 §5.1; 02 §5.1
rows 1/3). The daemon is not involved.

## Triggers

- EN: file a new work item / create a ticket / new task / edit this work item
  / change the goal or content / update priority or labels / show me this
  work item / what are its events / dependencies / children
- 中文: 建票 / 新建任务 / 提个需求 / 改一下目标 / 编辑这个票 / 更新优先级 /
  看看这个票 / 详情 / 事件流 / 依赖 / 子票

## Command

```
pf file create --project P --goal G [flags]        # or --stdin for a full JSON body
pf file edit <id> [flags]                          # or --stdin
pf file show <id> [--include events,deps,children]
```

`create` flags: `--project` (required unless the config's `project` or the
stdin body carries it), `--goal` (required, one line ≤500 chars),
`--content` (markdown background; `-` reads stdin, `@file` reads a file),
`--label` (repeatable), `--priority low|normal|high|urgent`, `--milestone`,
`--bundle NAME` (non-empty makes an execution ticket; omit it for a registry
ticket), `--parent ID` (epic-lite), `--repo name=url` (repeatable, order =
position), `--repo-base name=sha` (repeatable), `--attr key=json-value`
(repeatable), `--stdin`.

`edit` flags (only the flags you pass are sent; `--label` replaces the whole
set): `--goal`, `--content`, `--label`, `--priority`, `--milestone` (an empty
string clears it), `--waiting-on owner|runtime:<id>|dependency:<id>|gate`,
`--attr`.

`show` flags: `--include` — comma-separated `events`, `deps`, `children`.

## Success

`--json`: the server's record raw. Human mode renders the work item detail
view: `slug (id)`, status, project, goal, milestone, labels, bundle, assignee,
waiting, created, repo inventory, and the requested include sections.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-401` | api key missing or invalid | The AIHub credential is rejected — check `--api-key` / `PF_API_KEY` / the config's `api_key`. |
| `CORE-400` | Invalid field or type; unknown `--include` value; a workflow-frontmatter rule failed at create (`detail` names the `rule_id`); an `--attr` key hit the execution-state blacklist (`reason=EXECUTION_STATE_REJECTED`); a PATCH tried to change an immutable field (`reason=IMMUTABLE`: the workflow columns and `parent_id` never change after create) | The request body is wrong — read `detail` for the exact field/rule and fix the flags. |
| `CORE-404` | Unknown or invisible work item id, unknown project | No such work item (or you cannot see it) — check the `wi_` id and the project name. |
| `CORE-409` | Create-time conflict (e.g. duplicate) | The state machine refuses this write — read `detail`; for creates usually an existing identical record. |
| `CORE-410` | PATCH of a terminal work item (wrapped/failed/cancelled) | This work item is closed and can no longer be edited — file a new one instead. |
| `HTTP-502/503`-style refusal (`CORE-503`) | AIHub busy | Retry shortly; nothing was written. |

Exit 2 with a `pf:` usage line (no typed code) means a local flag/argument
mistake — read the message, fix the invocation.
