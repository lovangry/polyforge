# submit — submit a work item for execution on this machine

Execution-plane verb: the `pf` CLI talks to the local daemon over its unix
socket (claim / resume / takeover route family). Interactive submit of
exactly one work item; batch waves are the `batch-confirm` verb.

## Triggers

- EN: run it / submit it / start this work item / execute / take this one /
  force takeover / take it over from that machine
- 中文: 跑起来 / 执行这个 WI / 提交执行 / 认领开跑 / 接管 / 强制接管

## Command

```
pf submit <id> [--takeover] [--priority low|normal|high|urgent]
```

`--takeover` asks the daemon for the admin force-takeover claim instead of a
plain claim. `--priority` defaults to `normal`. Before the daemon is
involved, the CLI prechecks the work item against Core with the caller's own
api key, so obviously doomed submissions fail fast with a visible verdict.

## Success

`submitted <id> (accepted by the local daemon)` — acceptance means the daemon
queued the job; its dispatcher derives the Go driver for it within a
moment, priority-desc against the machine's concurrency cap. `--json`: the
raw answer `{accepted, work_item_id}`.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `CORE-404` | Unknown or invisible id (CLI precheck) | No such work item — check the `wi_` id. |
| (no code) `submit: <id> is terminal (<status>)` | The work item is already wrapped/failed/cancelled (CLI precheck) | It is finished and can never be claimed again — file a new work item instead. |
| `DAEMON-409` | The work item is already active or queued on this daemon, or drain mode rejects an external submit (the drain loop is the only job-creation entrance there) | Wait for the running attempt, or leave drain first (`pf ops mode abort-drain`). |
| `DAEMON-410` (`SHUTTING_DOWN`) | The daemon is in graceful shutdown | Wait for the shutdown to finish and the daemon to restart, then submit again. |
| `DAEMON-400` | Malformed request body | Fix the invocation. |
| `DAEMON-412` | The work item's project is not in the `[workspace]` mapping of this machine (refused before any capsule or claim state exists) | Add the project to a `[workspace."<path>"]` section; `pf doctor` checks the mapping. |
| `DAEMON-516` | (surfaces as a `submit_rejected` event) re-claim clone refusal: the capsule clone failed for a work item with published progress, so a local init that would restart at step 1 is refused | Fix aihub reachability / credentials, then `pf resume <id>`. |
| `dial unix …` | The daemon is not running on this machine | Start it (`pf daemon`) or check `--socket` / `PF_DAEMON_SOCKET`. |

The claim itself happens asynchronously in the daemon's dispatcher: a
server-side refusal (e.g. `CORE-409`, someone else holds the active
generation) surfaces in the daemon log and the job never appears in
`pf ops status` — observe/status is the follow-up, not the submit answer.
For a takeover refused as non-admin, the same path reports `CORE-403`.

## Takeover and holder check

`pf takeover <id> --reason <text> [--machine ID] [--owner ID]` moves a work
item whose holder is gone or superseded: it swaps the session under it (the
old holder is fenced at its next push) and hands the minted secret to the
local daemon's custody. `pf verify-session <id>` is the read-only check of who
holds the work item (public fingerprint, machine, owner; never the secret).
Both are data-plane CLI verbs with their own typed errors; they belong to this
fragment's intent family ("take it over").

## Watching what you submitted

`pf submit` subscribes the submitting chat (`PF_CHAT_ID`) to the work item in
the daemon — there is nothing to arm. Make sure the chat's resident watcher is
running (Claude Code: the `pf watch --chat "$PF_CHAT_ID" --wait-decision`
background task; pi: the extension does it) so a decision wakes you. Until the
daemon registers the work item with a monitor it shows as `queued/preparing`
— not an error. Full procedure: the `pf watch` section of `SKILL.md`.
