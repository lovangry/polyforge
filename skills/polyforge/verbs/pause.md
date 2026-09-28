# pause — pause a running work item on this machine

Execution-plane verb: routed to the local daemon (00 §5.1; 04 §5.1 pause row).
The pause lands at the running work item's next safe boundary; the
generation is closed server-side and the local capsule is sealed read-only.
The work item stays claimed — it does not return to the ready queue — and
the only way back is the `resume` verb (always a fresh claim).

## Triggers

- EN: pause it / stop it for now / hold on / park this one / freeze it
- 中文: 暂停 / 先停一下 / 挂起 / 停一停 / 先放一放

## Command

```
pf pause <id>
```

No flags. The request carries no credentials of its own: the daemon holds
the generation token and performs the close itself, including the final
publish it must produce for the close to be accepted.

## Success

Human mode: `pause requested for <id>` plus the daemon's raw answer.
`--json`: the raw answer alone. The request is accepted immediately; the
server-side `paused` status lands when the running step reaches its safe
boundary — observe to follow the transition.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | No local job for this work item on this machine | It is not running here — pause it on the machine that runs it. |
| `DAEMON-409` | A close is already in progress for this job | Wait for the in-flight close (pause or wrap) to settle, then re-check with observe. |
| `DAEMON-410` (`SHUTTING_DOWN`) | The daemon is shutting down gracefully | Nothing new is accepted; the shutdown itself closes remaining jobs as paused — observe instead. |
| `DAEMON-511` (`SHUTDOWN_FLUSH_FAILED`) | The final publish backing the pause did not converge | The pause cannot be confirmed yet — the daemon stays up for manual investigation; check the daemon log before retrying. |
| `DAEMON-403` (`stale_generation` / `secret_rolled_back`) | The local scene belongs to a taken-over generation or a rolled-back credential; the scene is quarantined | Do not retry — recovery is a fresh claim, or for `secret_rolled_back` an admin force-takeover. |
| `CORE-*` passthrough (e.g. `CORE-403 stale_generation`, `CORE-425 FLUSH_REQUIRED`, `CORE-409`) | The Core-side close was refused | Read the Core verdict verbatim — the typed code and reason come straight from Core; act per its detail (typically: the generation is no longer yours, or the closing publish evidence did not check out). |
| `dial unix …` | Daemon not running | Nothing is executing here — start the daemon or check `--socket`. |

A paused work item is still visible through observe — the local event-stream
layer is gone, so observation falls to the published-progress view.
