# observe — watch a work item's progress

Execution-plane verb: routed to the local daemon (00 §5.1; 04 §5.1 observe
row and §5.1a two-layer protocol). The CLI picks the layer automatically; no
SSE, no push, no long connection — every observation is a short pull.

## Triggers

- EN: how's it going / show progress / what step is it on / any events since
  I last checked / is it asking anything / what's the latest
- 中文: 看看进度 / 跑到哪了 / 观察一下 / 到哪一步了 / 它在问我什么吗 / 最新状态

## Command

```
pf observe <id> [--after N] [--read-through]
```

- Default: the daemon event stream. `--after N` is the last event cursor you
  already received (first call: `0`); pass back the printed `next_cursor` to
  pull incrementally.
- `--read-through`: skip the local layer and render the latest **published**
  progress directly from Core (row 6). Use it to observe a work item that
  runs on another machine or has already ended.

Layer selection without `--read-through`: if this machine has no local view
of the work item (the daemon answers `DAEMON-404`), or the daemon is not
reachable at all, the CLI automatically falls back to the Core read-through
(printing a note on stderr); when the daemon itself has the published
progress, its answer already carries the read-through marker.

## Success

Human mode, event stream: an event table (SEQ, EVENT, PAYLOAD — nine coarse
types like `step_started`, `needs_input`, `answered`, `published`), the
current `state:` snapshot, and `next_cursor`. A `(compacted …)` line means
your cursor fell out of the ring buffer — rebuild your view from the
snapshot and continue from the returned cursor. Read-through mode prints the
freshness note (`data as of the last publish`), the `head: seq=N` and the
snapshot. `--json`: the raw response either way.

A `needs_input` event means a question is waiting — route the user to the
`answer` verb, quoting its `question_id`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | Neither a local job nor any published progress exists (the CLI then retries via Core read-through, which produces one of the two rows below if it also fails) | Nothing observable anywhere yet — check the id, or the work item has never been submitted/published. |
| `CORE-404` | Unknown or invisible id (read-through layer) | No such work item — check the `wi_` id. |
| `CORE-504` | The work item was claimed but nothing was ever published yet (read-through layer) | No published progress exists so far — watch it on the machine that runs it, or wait for the first publish. |
| `dial unix …` | Daemon not running — the CLI notes it on stderr and falls back to read-through | Local-layer observation is unavailable; the published view still renders. |

Progress is never a second authority: the snapshot is a live view of the
execution record, and the read-through layer always shows the latest
**published** state, which can lag a live run by up to one publish.
