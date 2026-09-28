# batch-confirm — confirm and submit a Batch wave

Execution-plane verb: routed to the local daemon (00 §5.1 / §3.4b; 04 §5.1
`mode(get_recommendation)` + `batch_submit`). A recommendation is never a
confirmation: no Job exists until the human says so.

## Triggers

- EN: run a batch / what's the recommended wave / show me the next batch /
  confirm and run all of them / run exactly these
- 中文: 跑一批 / 看看推荐哪批 / 确认这一波 / 把这些一起跑了 / 推荐下一批

## Command

```
pf batch-confirm                 # show the recommendation only — creates NO job
pf batch-confirm --all           # confirm and submit the whole recommendation
pf batch-confirm <id> [<id>...]  # confirm and submit exactly this set
```

The recommendation is always fetched first and shown to the user; the
confirmed set may be edited (add/remove ids) before submitting. The submitted
set is passed through exactly as confirmed — never widened or narrowed.

## Success

- Read-only call (`pf batch-confirm` bare): human mode renders the flat
  ready queue (same view as `pf queue --ready`) plus the hint
  `confirm with: pf batch-confirm --all  or  pf batch-confirm <id> ...`;
  `--json` emits the raw recommendation.
- Confirmed submit: `wave submitted: N work item(s)` plus the raw daemon
  answer in human mode; `--json` emits the raw answer alone.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| (no code) `the recommendation is empty; nothing to submit` | Nothing is executable right now | No wave to run — check the queue later or file new work. |
| `CORE-*` passthrough | A claim inside the wave was refused by Core (e.g. `CORE-409 ALREADY_CLAIMED` — a concurrent claimant won a race; `CORE-409 DEPENDENCY_BLOCKED`; `CORE-409 NOT_CLAIMABLE`) | Read the per-item verdict in the response; the raced item is simply not yours — the rest of the wave stands as submitted. |
| `DAEMON-507` (`PREFLIGHT_FAILED`) | The daemon's local precheck failed for the wave | The machine is not ready — read `detail`, fix, re-confirm. |
| `DAEMON-409` | Drain mode rejects an external batch submit (the drain loop is the only job-creation entrance in that mode) | Leave drain first (`pf ops mode abort-drain`) or wait for the drain loop. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Graceful shutdown in progress | Wait for the daemon to restart, then re-confirm the wave. |
| `DAEMON-503` | A WI Agent in the wave could not be spawned | Infrastructure failure — read which item failed, retry. |
| `dial unix …` | Daemon not running | Start the daemon or check `--socket`. |
