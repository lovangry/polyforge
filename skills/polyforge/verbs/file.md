# file — create, edit or show a work item

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub. The daemon is not
involved.

## Triggers

- EN: file a new work item / create a ticket / new task / edit this work item
  / change the goal or content / update priority or labels / show me this
  work item
- 中文: 建票 / 新建任务 / 提个需求 / 改一下目标 / 编辑这个票 / 更新优先级 /
  看看这个票 / 详情

## Command

```
pf file create --project P --goal G [flags]        # or --stdin for a full JSON body
pf file edit <id> [flags]                           # or --stdin
pf file show <id>
```

`create` flags: `--project` (REQUIRED — there is no default project
anymore, polyforge#113; filing places work on THIS machine, so the value
is validated against the `[workspace]` mapping in cli.toml and a project
with no mapping fails loudly naming it — unlike `pf recall` / `pf
remember`, which are hub-wide and need no mapping),
`--goal` (required, one line ≤500 chars),
`--content` (markdown background; `-` reads stdin, `@file` reads a file),
`--label` (repeatable), `--priority low|normal|high|urgent`, `--milestone`,
`--bundle NAME` (non-empty makes an execution ticket; omit it for a
registry ticket), `--parent ID`, `--attr key=json-value` (repeatable),
`--repo NAME` (repeatable), `--stdin`.

`--repo NAME` declares the repos this work item works in: a name of one
of the project's repos (`pf projects get <project>` lists them). The
URL always comes from the project's record, looked up when a daemon
claims the work item. When a work item declares repos, the daemon clones
only those at claim (and at every re-claim); without `--repo` it clones
every repo of the project. The list is frozen at create — it cannot be
edited; cancel and re-create to change it. 1–8 names, no duplicates. A
`--stdin` body may carry the same list as `"repo_inventory": [{"name":
"..."}]` (not together with `--repo`). Only names are accepted: an entry
with `url`, `subdir` or `base` is refused.

`edit` flags (only the flags you pass are sent; `--label` replaces the whole
set): `--goal`, `--content`, `--label`, `--priority`, `--milestone` (an empty
string clears it), `--waiting-on`, `--attr`, `--bundle` (switch the work
item's workflow bundle, e.g. `--bundle release`; an empty string unbinds it).
aihub accepts a bundle change only while the work item is queued and has
never been claimed — once a daemon has claimed it, the bundle is fixed.

`show` takes exactly one id (or slug) and renders the detail record.

`--project` inference: when the flag is omitted, the CLI resolves the
working directory against the cli.toml `[workspace."<path>"]` mapping — a
cwd inside exactly one mapped workspace that lists exactly ONE project
gets that project as the default and the create proceeds as if the flag
had been passed. A workspace listing two or more projects errors asking
for `--project` (never a silent pick); a cwd matching no mapped workspace
keeps the #113 required-flag error. An explicit `--project` always
bypasses inference byte-for-byte.

## Success

`--json`: the server's record raw. Human mode renders the work item detail
view: `slug (id)`, status, project, goal, milestone, labels, bundle,
assignee, waiting, created (and closed when terminal).

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `AUTH-401` | api key missing or invalid | The AIHub credential is rejected — check `--api-key` / `PF_API_KEY` / the config's `api_key`. |
| `CORE-400` | Invalid field or type; a workflow-frontmatter rule failed at create (`detail` names the rule); an `--attr` key hit the execution-state blacklist | The request body is wrong — read `detail` for the exact field/rule and fix the flags. |
| `CORE-400` `REPO_NOT_IN_PROJECT` | A `--repo` name is not one of the project's repos | Name a repo the project has (`pf projects get <project>` lists them); a maintainer adds new repos to the project. |
| `CORE-400` `INVENTORY_DUPLICATE_NAME` / `INVENTORY_URL_NOT_ALLOWED` / `INVENTORY_SUBDIR_UNSUPPORTED` / `INVENTORY_BASE_UNSUPPORTED` | The same repo named twice; a `--stdin` entry carrying `url`, `subdir` or `base` | Drop the duplicate; give names only. |
| `CORE-404` | Unknown or invisible work item id, unknown project | No such work item (or you cannot see it) — check the `wi_` id and the project name. |
| `CORE-409` | Create-time conflict (e.g. duplicate) | The state machine refuses this write — read `detail`. |
| `CORE-410` | PATCH of a terminal work item (wrapped/failed/cancelled) | This work item is closed and can no longer be edited — file a new one instead. |

Exit 2 with a `pf:` usage line (no typed code) means a local flag/argument
mistake — read the message, fix the invocation.
