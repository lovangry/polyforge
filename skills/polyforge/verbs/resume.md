# resume — resume a paused work item

Execution-plane verb: routed to the local daemon (00 §5.1; 04 §5.1 resume
row). Resume is **always a fresh claim**: a new generation, a new session, a
newly derived WI Agent. It is not "unpause the old run" — the previous
generation ended when the pause landed.

## Triggers

- EN: resume it / continue the paused one / bring it back / pick it up again
- 中文: 继续 / 恢复执行 / 接着跑 / 重新跑起来 / 把暂停的那个恢复

## Command

```
pf resume <id> [--priority low|normal|high|urgent]
```

`--priority` defaults to `normal`. Same CLI precheck as `submit`: an unknown
id or a terminal work item is refused before the daemon is involved.

## Success

Human mode: `resumed <id> (fresh claim accepted by the local daemon)`.
`--json`: the raw answer. A new generation starts from the execution record
the previous generation published — the workflow continues from where it
paused, in a brand-new session.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id (CLI precheck) | No such work item — check the `wi_` id. |
| (no code) `submit: <id> is terminal (<status>)` | The work item is wrapped/failed/cancelled | A finished work item cannot be resumed — file a new one. |
| `DAEMON-507` (`PREFLIGHT_FAILED`) | Local precheck failed; no claim started | The machine is not ready — read `detail`, fix, retry. |
| `CORE-409` (`ALREADY_CLAIMED`) | The work item is not actually paused — an active generation still exists | Someone (or another machine) is already running it — observe to see who, or take over as admin if it is stale. |
| `DAEMON-409` | The daemon's own precheck refuses (e.g. a non-paused or terminal work item), or drain mode rejects the external resume | Read the status the detail names; in drain mode leave drain first. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Graceful shutdown in progress | Wait for restart, then resume. |
| `DAEMON-503` | The new WI Agent could not be spawned | Infrastructure failure — retry; if persistent, check the daemon log. |
| `DAEMON-403` (`stale_generation` / `secret_rolled_back`) | The local scene was taken over or its credential rolled back | Recovery is a fresh claim or an admin force-takeover — never a plain retry. |
| `dial unix …` | Daemon not running | Start the daemon or check `--socket`. |

Do not route "continue / go on" of a **running** work item here — that is
`observe`. `resume` applies to a paused work item only.
