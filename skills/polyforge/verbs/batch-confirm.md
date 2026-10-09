# batch-confirm — confirm and submit a Batch wave

Execution-plane verb: routed to the local daemon (`mode
get_recommendation` + `batch_submit`). A recommendation is never a
confirmation: no job exists until the human says so.

## Triggers

- EN: run a batch / what's the recommended wave / show me the next batch /
  confirm and run all of them / run exactly these
- 中文: 跑一批 / 看看推荐哪批 / 确认这一波 / 把这些一起跑了 / 推荐下一批

## Command

```
pf batch-confirm [--project P]   # show the recommendation only — creates NO job
pf batch-confirm --all           # confirm and submit the whole recommendation
pf batch-confirm <id> [<id>...]  # confirm and submit exactly this set
```

Without `--project` the recommendation covers ALL mapped projects'
ready queues, merged (the `[workspace]` mapping in cli.toml —
polyforge#113); `--project P` scopes the recommendation to P and is
validated against the mapping (unmapped → a naming error with guidance).
The recommendation is always fetched first and shown to the user; the
confirmed set may be edited (add/remove ids) before submitting. The
submitted set is passed through exactly as confirmed — never widened or
narrowed. Jobs beyond the machine's concurrency cap queue pending and
backfill as slots open; spawning is priority-desc, no preemption.

## Success

- Read-only call (`pf batch-confirm` bare): the recommended work item table
  plus the hint `confirm with: pf batch-confirm --all  or  pf batch-confirm
  <id> ...`; `--json` emits the raw recommendation `{items:[...]}`.
- Confirmed submit: `wave submitted: N work item(s)` plus the raw daemon
  answer; `--json` emits the raw answer alone.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| (no code) `the recommendation is empty; nothing to submit` | Nothing is executable right now | No wave to run — check the queue later or file new work. |
| `CORE-*` passthrough | The recommendation read (or a claim inside the wave) was refused by Core | Read the detail: the raced item is simply not yours — the rest of the wave stands as submitted. |
| `DAEMON-409` | Drain mode rejects an external batch submit (the drain loop is the only job-creation entrance in that mode) | Leave drain first (`pf ops mode abort-drain`) or wait for the drain loop. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Graceful shutdown in progress | Wait for the daemon to restart, then re-confirm the wave. |
| `DAEMON-400` | Empty `wi_ids` set or a malformed body | Give the confirmed ids explicitly. |
| `dial unix …` | Daemon not running | Start the daemon or check `--socket`. |
