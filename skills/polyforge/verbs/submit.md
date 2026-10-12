# submit — submit a work item for execution on this machine

Execution-plane verb: the `pf` CLI talks to the local daemon over its unix
socket (claim / resume route family). Explicit submit of
exactly one work item; always legal, also while the daemon's auto-pull
(`pf ops auto on`) is on. To see what is ready, use `pf queue --ready`.

## Triggers

- EN: run it / submit it / start this work item / execute / take this one
- 中文: 跑起来 / 执行这个 WI / 提交执行 / 认领开跑

"Take it over from that (dead/stuck) machine" is NOT a submit flag: end the
stuck session first with `pf ops end-session` (see `verbs/ops.md`), then
submit as usual.

## Command

```
pf submit <id> [--priority low|normal|high|urgent] [--models a,b,c]
```

`--priority` defaults to `normal`. `--models` is a manual model-candidate
list for this work item: it is the candidate chain of every step (advancing
on model-class failures like any list), kept across daemon restarts and
`pf resume`, local to this machine. Non-empty, comma-separated, no empty
or duplicate ids, else a local bad-input refusal. Candidate precedence,
highest first: `--models` > yaml `models.<project>.<kind>` > yaml
`models.<project>.default` > bundle step `model_candidates` > bundle
`defaults.model_candidates`. Before the daemon is
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
| `DAEMON-409` | The work item is already active or queued on this daemon | Wait for the running attempt. |
| `DAEMON-410` (`SHUTTING_DOWN`) | The daemon is in graceful shutdown | Wait for the shutdown to finish and the daemon to restart, then submit again. |
| `DAEMON-400` | Malformed request body | Fix the invocation. |
| `DAEMON-412` | The work item's project is not in the `[workspace]` mapping of this machine (refused before any capsule or claim state exists) | Add the project to a `[workspace."<path>"]` section; `pf doctor` checks the mapping. |
| `DAEMON-516` | (surfaces as a `submit_rejected` event) re-claim clone refusal: the capsule clone failed for a work item with published progress, so a local init that would restart at step 1 is refused | Fix aihub reachability / credentials, then `pf resume <id>`. |
| `dial unix …` | The daemon is not running on this machine | Start it (`pf daemon`) or check `--socket` / `PF_DAEMON_SOCKET`. |

The claim itself happens asynchronously in the daemon's dispatcher: a
server-side refusal (e.g. `CORE-409`, someone else holds the active
generation) surfaces in the daemon log and the job never appears in
`pf ops status` — `pf watch`/`pf ops status` is the follow-up, not the submit answer.

## Recovering a work item held by a dead or stuck daemon

An admin or project maintainer ends a dead or stuck holder's stale session (`pf ops end-session <id> --reason <text>`, see `verbs/ops.md`):
the work item goes back to `queued` and the old holder is fenced. Then
`pf submit <id>` on any daemon is the ordinary claim, which restores progress
from the capsule revision chain and continues from the recorded current step.
A `CORE-409 NOT_CLAIMABLE`/`ALREADY_HELD` answer on submit means the session
has not been ended yet.

## Watching what you submitted

`pf submit` subscribes the submitting chat (`PF_CHAT_ID`) to the work item in
the daemon — there is nothing to arm. Make sure the chat's resident watcher is
running (Claude Code: the `pf watch --chat "$PF_CHAT_ID" --wait-decision`
background task; pi: the extension does it) so a decision wakes you. Until the
daemon registers the work item with a monitor it shows as `queued/preparing`
— not an error. Full procedure: the `pf watch` section of `SKILL.md`.
