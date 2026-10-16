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
pf remember --project P --type T [--title S] [--visibility V] [--tags a,b] [--work-item ID] <content | - (stdin) | @file>
```

`--type` is required (e.g. `experience.debug`, `fact.note`). The content
is the positional text; `-` reads it from stdin, `@file` from a file.
`--project` is REQUIRED (polyforge#113 — no default project exists) and
must be a project of this machine's cli.toml `[workspace]` mapping (owner
ruling 2026-10-16, superseding the 2026-10-13 C15 ruling): an unmapped
project — or no mapping at all — is refused locally, naming the mapped
projects, and nothing is sent. aihub then gates memory by project membership on its own: a search
answers only memories of projects the caller can read; `project=` naming an
unknown project or one the caller cannot read answers the same non-leaking
`CORE-404`; reading a memory needs read access to its project (else `MEM-404`);
writing needs the writer (or maintainer) role (else `MEM-403`); admins pass. `--visibility` defaults
to `project` (`admin|private|project|team|public`); `--work-item`
optionally attributes the memory to a work item (id or `<project>#<seq>`
slug; an unknown one is a typed CORE-404). The work item must belong to
`--project` — a memory belongs to one project; another project's work
item is refused with CORE-400.

## Shared documents go here, not into files (polyforge#177)

A design, review, decision sheet or audit meant for others is a memory
(or, for a design, its work item's content while the work item is open —
`pf file edit <wi> --content @f`), never a file in the workspace root or
`.polyforge/`:

| Content | `--type` |
|---|---|
| a design of a closed (or no) work item | `doc.design` (with `--work-item` when it belongs to one) |
| a review report | `doc.review` |
| a decision sheet | `doc.decision` |
| an audit | `doc.audit` |
| a lesson / a fact | `experience.*` / `fact.*` |

Pass the body as `@file` (≤1 MiB). A memory cannot be edited — store a
new one (or redact). Local disposable output belongs in the workspace's
`scratch/` only (`pf doctor` reports entries older than 14 days).

## Step agents write memories too

Every workflow step's prompt carries a memory duty: at the end of the
step the agent writes at most ONE `experience.<kind>` memory, judged
against the workspace's `memory_policy` (`.polyforge.yaml`; `verbs/init.md`),
after a `pf recall --any` near-duplicate check; a lesson outside the
policy's `record` list carries the `policy-proposal` tag and a
"建议新增条目：…" line. The workflow's last step writes the work item's one
`experience.wrap` summary (title `<wi slug>: <what was done>`). Such
memories are attributed with `--work-item`; filter them with
`pf recall --project P --any '<words>' --type experience.wrap` or
`--tags policy-proposal`.

## Success

`--json`: the server body raw. Human mode: `stored <id> (type T,
visibility V)`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `AUTH-401` | api key missing or invalid | Credential rejected — check `--api-key` / `PF_API_KEY` / the config's `api_key`. |
| `MEM-400` | Blank content, missing `--type`, or an invalid type/visibility | Give real content and a valid type/visibility — read `detail` for the exact field. |
| `CORE-404` | Unknown project (the server's project lookup) | The project does not exist — check `--project` (there is no default project; it must also be listed in a cli.toml `[workspace]` section). |
| `MEM-403` | You lack the writer (or maintainer) role on the project | Ask a project maintainer for the writer role; nothing was saved. |
| `MEM-413` | Content (or the whole authority document) exceeds 1 MiB | Split the memory or trim the content. |
| `MEM-500` | The memory store failed to persist the authority file | Nothing was saved — retry; if it persists, the server's data dir is unhealthy. |

A local `pf: remember: ...` line (exit 1, no typed code) means a local
refusal before any wire call — missing `--type`, missing content, a
missing `--project`, or a `--project` that is not listed under a
`[workspace."<path>"]` section of cli.toml (the message names the mapped
projects; `pf doctor` checks the mapping).
