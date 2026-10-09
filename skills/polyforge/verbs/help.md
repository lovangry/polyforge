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
| file | `verbs/file.md` | `pf file create` / `pf file edit <id>` / `pf file show <id>` — create, edit or show a work item |
| queue | `verbs/queue.md` | `pf queue` — the list; `pf queue -ready` — the ready queue (`-status`, `-limit`, `-project` filter) |
| cancel | `verbs/cancel.md` | `pf cancel <id>` — cancel a queued/paused/blocked work item |
| recall | `verbs/recall.md` | `pf recall <query>` — search project memories (`-type`, `-tags`, `-limit`, `-project`) |
| remember | `verbs/remember.md` | `pf remember -type <T> <content>` — store a memory (`-title`, `-tags`, `-visibility`, `-work-item`, `-project`) |

## Execution-plane verbs (local daemon unix socket)

| Verb | Fragment | Command |
|---|---|---|
| submit | `verbs/submit.md` | `pf submit <id>` — submit for execution (`-takeover` force-claim, `-priority`) |
| observe | `verbs/observe.md` | `pf observe <id>` — progress projection (`-after <cursor>` for incremental events) |
| answer | `verbs/answer.md` | `pf answer <id> -text "..."` — reply to a pending question (`-question`, `-answer-id`, `-by`) |
| batch-confirm | `verbs/batch-confirm.md` | `pf batch-confirm` — show the wave recommendation; `pf batch-confirm -all` — confirm and submit it |
| pause | `verbs/pause.md` | `pf pause <id>` — operator pause at the next safe boundary |
| resume | `verbs/resume.md` | `pf resume <id>` — clear the operator pause (`-priority`) |
| wrap | `verbs/wrap.md` | `pf wrap <id>` — terminal success; `pf wrap <id> -fail` — terminal failure |
| ops | `verbs/ops.md` | `pf ops status` — jobs, mode, queue counters; `pf ops shutdown` — graceful stop |

## Delivery bridge (wrap → git, local daemon unix socket)

| Verb | Fragment | Command |
|---|---|---|
| ship | `verbs/ship.md` | `pf ship <id>` — push a closed work item's sealed commits to git origin |

## CLI-meta verbs

| Verb | Fragment | Command |
|---|---|---|
| help | `verbs/help.md` | `pf help` — the registry face; this fragment is its routing-side index |
| init | this fragment (`init.md`) | install / upgrade / machine setup — routes to the repo's `tools/install.sh` |

## Non-goals

- Prints reference output only: it never dispatches the listed verbs
  itself and never performs an intent's action.
- Not a substitute for the fragments: each entry's fragment owns the real
  command shape, success rendering and error branches — read it before
  running the verb for the first time in a session.
