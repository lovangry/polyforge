# submit — submit a work item for execution on this machine

Execution-plane verb: the `pf` CLI talks to the local daemon over its Unix
domain socket (00 §5.1; 04 §5.1 `claim`/`takeover` rows). Interactive submit
of exactly one work item; batch waves are the `batch-confirm` verb.

## Triggers

- EN: run it / submit it / start this work item / execute / take this one /
  force takeover / take it over from that machine
- 中文: 跑起来 / 执行这个 WI / 提交执行 / 认领开跑 / 接管 / 强制接管

## Command

```
pf submit <id> [--takeover] [--priority low|normal|high|urgent]
```

`--takeover` asks the daemon for the admin force-takeover path instead of a
plain claim. `--priority` defaults to `normal`. Before the daemon is
involved, the CLI prechecks the work item against Core with the caller's own
api key, so obviously doomed submissions fail fast with a visible verdict.

## Success

`--json`: the daemon's answer raw. Human mode:
`submitted <id> (accepted by the local daemon)` — acceptance means a local
Job exists and a persistent WI Agent is being derived for it.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id (CLI precheck) | No such work item — check the `wi_` id. |
| (no code) `submit: <id> is terminal (<status>)` | The work item is already wrapped/failed/cancelled (CLI precheck) | It is finished and can never be claimed again — file a new work item instead. |
| `CORE-409` (`ALREADY_CLAIMED` / `NOT_CLAIMABLE` / `DEPENDENCY_BLOCKED`) | Passthrough from Core during the daemon-side claim: someone else holds the active generation; the ticket is a registry ticket with no bundle (to execute its content, create a child ticket); an unfinished dependency blocks it | Read the reason: wait for the other owner, take over as admin, create an executable child ticket, or let the blocking dependency finish. |
| `CORE-403` | Takeover attempted by a non-admin | Only an admin can force-takeover. |
| `DAEMON-507` (`PREFLIGHT_FAILED`) | The daemon's local precheck failed; no claim was started | The machine is not ready to run this work item — read `detail` (mode, capacity, state) and retry after fixing it. |
| `DAEMON-410` (`SHUTTING_DOWN`) | The daemon is in graceful shutdown | Wait for the shutdown to finish and the daemon to restart, then submit again. |
| `DAEMON-409` | The daemon's own precheck refuses (e.g. a terminal or otherwise unclaimable work item), or drain mode rejects an external single submit | Read the status the detail names; in drain mode, external submits are refused by design — let the drain loop feed itself or leave drain first. |
| `DAEMON-403` (`stale_generation` / `secret_rolled_back`) | The local scene belongs to a generation that was taken over or had its credential rolled back | The local scene is sealed; recovery is a fresh claim, or for `secret_rolled_back` an admin force-takeover — never a plain retry. |
| `DAEMON-503` | The WI Agent could not be spawned | Infrastructure failure — retry; if it persists, check the daemon log. |
| `dial unix …` | The daemon is not running on this machine | Start the daemon or check `--socket` / `PF_DAEMON_SOCKET`. |
