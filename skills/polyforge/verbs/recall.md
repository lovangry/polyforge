# recall — search project memories

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub (memory
retrieval). The daemon is not involved. Write paths (creating or updating
memories) are not part of this verb — retrieval only.

## Triggers

- EN: how did we handle X before / any past experience with X / did we hit
  this before / search the project memory / any notes on X
- 中文: 之前怎么处理过 X / 有没有相关经验 / 查一下记忆 / 项目记忆里搜搜

## Command

```
pf recall [--project P] <query> [--type T] [--tags a,b] [--limit N]
```

The query is the positional argument; `--project` is REQUIRED
(polyforge#113 — no default project exists) and validated against the
`[workspace]` mapping in cli.toml: an unmapped project fails loudly
naming it. The routing flow derives the value from the WI under
discussion. `--type` filters one memory type (e.g. `experience.debug`);
`--tags` is comma-separated and an entry must contain all of them;
`--limit` caps results.

## Success

`--json`: the server body raw (`{items:[...]}`). Human mode: `N hit(s)` + a
table of ID, TYPE, CONTENT, UPDATED, TAGS.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `AUTH-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |
| `MEM-400` | Blank query | Give the search words — an empty query is refused. |
| `CORE-404` | Unknown project, or you are not a member of it | The project does not exist (or you cannot see it) — check `--project`; `pf doctor` checks the `[workspace]` mapping. |

Zero hits are **not** an error: the table simply says `0 hit(s)`. Try
different wording or drop a tag filter before concluding nothing is stored.
