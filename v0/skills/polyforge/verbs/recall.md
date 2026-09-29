# recall — search project memories

Data-plane verb: the `pf` CLI talks HTTP direct to AIHub (00 §5.1; 02 §5.1
row 23 — real-time trigram FTS retrieval, the only v1 search path; 06 §5.1
holds the signatures). The daemon is not involved. Write paths (creating or
updating memories) are not part of this verb — retrieval only.

## Triggers

- EN: how did we handle X before / any past experience with X / did we hit
  this before / search the project memory / any notes on X
- 中文: 之前怎么处理过 X / 有没有相关经验 / 查一下记忆 / 项目记忆里搜搜 /
  遇到过这个问题吗

## Command

```
pf recall [--project P] <query> [--type T] [--tags a,b] [--limit N]
pf recall [--project P] --q "<query>" [flags]
```

The query is the positional argument (or `--q`); `--project` defaults to the
config's `project` and is required in the end. `--type` filters one memory
type (e.g. `experience.debug`); `--tags` is comma-separated and an entry must
contain all of them; `--limit` caps results (server default 50, cap 200).
Filter order is fixed: project membership first, visibility inside the
candidate set.

## Success

`--json`: the server body raw (`{items:[...]}`). Human mode: `N hit(s)` + a
table of ID, TYPE, TITLE, UPDATED.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / config. |
| `MEM-400` | Blank query | Give the search words — an empty query is refused. |
| `MEM-404` | Unknown or missing project (wire truth, `api/openapi.yaml` row 23: the membership-first filter needs the project scope) | The project does not exist (or you cannot see it) — check `--project` / the config's `project`. |
| `MEM-503` | FTS backend failure | The search index is unavailable right now — retry shortly; the memories themselves are intact. |

Zero hits are **not** an error: the table simply says `0 hit(s)`. A semantic
miss is not evidence of absence — try different wording or drop a tag filter
before concluding nothing is stored.
