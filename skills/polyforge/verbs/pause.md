# pause — pause a running work item on this machine

Execution-plane verb: routed to the local daemon (the pause route: a close
of the current run with reason `paused`). The daemon seals the
capsule read-only and closes the session as `paused`; the work item
becomes `paused` and the only way back is the `resume` verb. The session
is KEPT (it is not cleared like a wrap/fail/block): `pf resume` on a paused
work item clears `clean_pause` on the hub and tells the holder daemon, which
re-attaches to the kept session and continues in place (a holder that was
down attaches on its next start).

## Triggers

- EN: pause it / stop it for now / hold on / park this one / freeze it
- 中文: 暂停 / 先停一下 / 挂起 / 停一停 / 先放一放

## Command

```
pf pause <id>
```

No flags. The daemon holds the session and performs the close itself: the
final commit and the push of the capsule's revision history. The session is
NOT ended — a paused WI keeps it for the resume.

## Success

`pause requested for <slug>` (`--json`: the daemon's raw answer instead). The local run stops but the session is kept; the sealed capsule stays on
disk as the read-only scene a later resume continues from.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | No local job for this work item on this machine | It is not running here — pause it on the machine that runs it. |
| `DAEMON-409` | A close is already in progress for this job (a wrap or another pause), or the job is mid-recovery | Wait for the in-flight close to settle, then re-check with `pf watch <id> --once`. |
| `CORE-*` passthrough | The server-side pause was refused | Read the typed Core verdict — typically the session is no longer this daemon's; act per its detail. |
| `DAEMON-500` | The close failed on the daemon side | The job is left mid-seal, recorded as such; the next daemon start finishes the close. Check the daemon log before retrying. |
| `dial unix …` | Daemon not running | Nothing is executing here — run `pf daemon ensure`, then retry once (or check `--socket`). |

A paused work item is still visible through `pf file show`; its live
execution view is gone (the local job is sealed).
