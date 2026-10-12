# observe — watch a work item's progress (`pf watch`)

Execution-plane verb: routed to the local daemon. The `pf watch` lane replaces
the old observe verb and the old watcher script: one command for a snapshot,
one resident watcher per chat for progress and human-decision wake-ups.

## Triggers

- EN: how's it going / show progress / what step is it on / any events /
  is it asking anything / what's the latest / keep an eye on it
- 中文: 看看进度 / 跑到哪了 / 观察一下 / 到哪一步了 / 它在问我什么吗 / 最新状态 / 盯着

## Command

```
pf watch <id> --once [--json] [--after N] [--events N]    # one snapshot
pf watch subscribe <id> | unsubscribe <id> | list          # this chat's subscriptions
pf watch --chat <chat> [--wait-decision]                   # resident watcher (default chat: $PF_CHAT_ID)
pf watch --chat <chat> --statusline                        # print the progress file
```

`--once` is the pull: status (`running`, `needs input`, `completed`, `failed`,
`blocked`, `repair pending`, or `queued/preparing` for a work item the daemon
has not registered yet), the current step and how long it has run, the age of
the last work signal when known, the pending question's id and text, and the
latest events. `--json` is the daemon's raw watch snapshot. The state comes
from the capsule's `execution.json` (the authority), so it also answers for a
work item that finished while nobody was watching.

The resident watcher, the Claude Code status line / wake loop and the pi
extension are described in the `pf watch` section of `SKILL.md`. A decision
wake-up line is a fixed template and carries no question or error text:
present it to the human, never answer it yourself, and read the question text
here (`--once`) only when asked.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| (no code) `watch: no chat id` | Neither `--chat` nor `PF_CHAT_ID` is set | The session has no chat id — see the Chat identity section of `SKILL.md`. |
| `DAEMON-400` | Bad cursor or chat id | Fix the invocation. |
| `dial unix …` | Daemon not running | Nothing is executing here — start the daemon or check `--socket`. The resident watcher prints `daemon unreachable; progress paused` and keeps retrying. |

Progress is never a second authority: the step fields are a live direct read of
the capsule's execution record; the events are news, not state.
