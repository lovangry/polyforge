# remember — store a project memory

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub (the memory
write face). The daemon is not involved. It is the write counterpart of
`recall` (retrieval): `pf remember` files a lesson, fact or note into
the project memory so a later `recall` finds it.

## Triggers

- EN: remember this / save this lesson / note it down / keep this for
  next time / store it in the project memory
- 中文: 记下来 / 存个经验 / 写入项目记忆 / 下次别再踩这个坑

## Command

```
pf remember [--project P] --type T [--title S] [--visibility V] [--tags a,b] [--work-item ID] <content | - (stdin) | @file>
```

`--type` is required (e.g. `experience.debug`, `fact.note`). The content
is the positional text; `-` reads it from stdin, `@file` from a file.
`--project` is REQUIRED (polyforge#113 — no default project exists) and
validated against the `[workspace]` mapping in cli.toml: an unmapped
project fails loudly naming it. `--visibility` defaults
to `project` (`admin|private|project|team|public`); `--work-item`
optionally attributes the memory to a work item id.

## Success

`--json`: the server body raw. Human mode: `stored <id> (type T,
visibility V)`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / the config's `api_key`. |
| `MEM-400` | Blank content, missing `--type`, or an invalid type/visibility | Give real content and a valid type/visibility — read `detail` for the exact field. |
| `MEM-404` | Unknown or missing project | The project does not exist (or you cannot see it) — check `--project` / the config's `project`. |
| `MEM-413` | Content (or the whole authority document) exceeds 1 MiB | Split the memory or trim the content. |
| `MEM-500` | The memory store failed to persist the authority file | Nothing was saved — retry; if it persists, the server's data dir is unhealthy. |

A local `pf: remember: ...` line (exit 1, no typed code) means a local
refusal before any wire call — missing `--type`, missing content, a
missing `--project`, or a `--project` that is not listed under a
`[workspace."<path>"]` section of cli.toml (`pf doctor` checks the
mapping).
