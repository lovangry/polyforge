# observe — watch a work item's progress

Execution-plane verb: routed to the local daemon (the observe projection:
the execution record direct-read layer plus the event layer). Short pull,
no streaming, no push, no long connection.

## Triggers

- EN: how's it going / show progress / what step is it on / any events /
  is it asking anything / what's the latest
- 中文: 看看进度 / 跑到哪了 / 观察一下 / 到哪一步了 / 它在问我什么吗 / 最新状态

## Command

```
pf observe <id> [--after N]
```

`--after N` is the last event cursor you already received (first call: `0`).
The answer is this machine's LIVE view of the work item — it exists only
while the daemon runs the work item here.

## Success

Human mode: the header line (`wi, project, status`), the attempt/generation/
bundle line, the current step, the step table (STEP, STATUS, ATTEMPTS,
MODEL, STARTED), the budget block when present, the pending question when
one is open, and the recent events table (AT, KIND, BODY). `--json`: the raw
projection (`work_item`, `status`, `attempt_id`, `generation`, `steps`,
`budget`, `pending_question`, `events`).

A `needs_input` status means a question is waiting — route the user to the
`answer` verb, quoting its `question_id`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | The work item is not running on this daemon | It is not executed on this machine (or not at all) — observe from the machine that runs it. |
| `DAEMON-500` | The monitor did not answer in time | Transient — retry; if it persists, check the daemon log. |
| `dial unix …` | Daemon not running | Nothing is executing here — start the daemon or check `--socket`. |

Progress is never a second authority: the step/budget fields are a live
direct read of the capsule's execution record; the events are news, not
state.
