# help — the pf quick-reference

Read-only contract: this fragment prints reference output and stops — it
dispatches no verb, performs no wi-lifecycle action and edits no file. It
is the one-to-one index of the routing skill's verb fragments and the
`pf` subcommand each fragment drives. Flags are spelled exactly as the
binary's help faces print them; `pf help` lists the full registry and
`pf <verb> -help` prints one verb's face.

## Data-plane verbs (pf CLI talks HTTP direct to AIHub)

| Verb | Fragment | Command |
|---|---|---|
| file | `verbs/file.md` | `pf file create` / `pf file edit <id>` / `pf file show <id>` / `pf file dep add` / `rm` / `list` — create, edit or show a work item; its `blocks` dependencies |
| queue | `verbs/queue.md` | `pf queue` — the merged list over all mapped projects; `pf queue -project P` — one project; `pf queue -ready` — the ready queue (`-status`, `-limit` filters) |
| cancel | `verbs/cancel.md` | `pf cancel <id>` — cancel (abandon) a queued/paused/blocked work item |
| recall | `verbs/recall.md` | `pf recall -project P <query>` — search project memories (`-type`, `-tags`, `-limit`) |
| remember | `verbs/remember.md` | `pf remember -project P -type <T> <content>` — store a memory (`-title`, `-tags`, `-visibility`, `-work-item`) |
| memory | `verbs/memory.md` | `pf memory show <id>` — one memory's metadata header + full body (`-max-bytes N`, `0` = unlimited; `--json` = metadata verbatim + CLI-added `content`) |

## Execution-plane verbs (local daemon unix socket)

| Verb | Fragment | Command |
|---|---|---|
| submit | `verbs/submit.md` | `pf submit <id>` — submit for execution (`-priority`) |
| watch | `verbs/watch.md` | `pf watch <id> --once` — one snapshot (status, step, pending question, events); `pf watch --chat "$PF_CHAT_ID" --wait-decision` — the resident watcher; `pf watch subscribe\|unsubscribe\|list` |
| answer | `verbs/answer.md` | `pf answer <id> -text "..."` — reply to a pending question (`-question`, `-by`) |
| pause | `verbs/pause.md` | `pf pause <id>` — operator pause at the next safe boundary |
| resume | `verbs/resume.md` | `pf resume <id>` — continue a paused or blocked work item |
| wrap | `verbs/wrap.md` | `pf wrap <id>` — terminal success; `pf wrap <id> --fail` — the owner's failed verdict |
| ops | `verbs/ops.md` | `pf ops status` — jobs, auto/max, queue counters, the workspace set; `pf ops auto on|off`; `pf ops max N`; `pf ops shutdown` — graceful stop (the restart loop ends too); `pf daemon ensure` — start the daemon under its restart loop; `pf ops end-session <wi> --reason ...` — force-end a stuck holder's session (admin/maintainer), then `pf submit` |

## Delivery (no verb)

There is no `pf ship`. The delivery lanes end with a `deliver` workflow step
(push the feature branch, open/reuse the PR, auto-merge or ask a human) —
see `verbs/ship.md`.

## CLI-meta verbs

| Verb | Fragment | Command |
|---|---|---|
| doctor | `verbs/doctor.md` | `pf doctor` — the machine's one health face: config, `[workspace]` mapping, connectivity (`--json` for machines) |
| chat | (`SKILL.md`, Chat identity) | `pf chat init --harness claude\|pi --harness-session ID` — the chat id a harness adapter exports as `PF_CHAT_ID` |
| help | `verbs/help.md` | `pf help` — the registry face; this fragment is its routing-side index |
| init | the init fragment (`init.md`) | install / upgrade / machine setup — routes to the repo's `tools/install.sh` |

## Non-goals

- Prints reference output only: it never dispatches the listed verbs
  itself and never performs an intent's action.
- Not a substitute for the fragments: each entry's fragment owns the real
  command shape, success rendering and error branches — read it before
  running the verb for the first time in a session.
