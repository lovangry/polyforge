# ops — daemon operations: status, auto-pull, max jobs, graceful shutdown

Execution-plane verb: routed to the local daemon (status / ops-config /
shutdown routes). Machine-local operations only: what this daemon is
managing, whether it also pulls work by itself, and how it stops. The
daemon has ONE switch and ONE number: `auto` (off = only explicit
`pf submit`; on = the daemon additionally pulls the aihub ready queue by
itself) and `max` (concurrent jobs). Manual `pf submit` stays legal while
auto is on. Graceful shutdown is a process-lifecycle action and is **not**
the auto switch — the shutdown sequence terminates every live agent tree,
persists its bookkeeping and exits; auto keeps claiming queued work while
the daemon lives.

## Triggers

- EN: daemon status / what's this machine running / how many jobs / turn
  auto-pull on / turn auto-pull off / let the machine pick up ready work
  itself / raise the concurrency / shut the daemon down gracefully /
  pre-upgrade check
- 中文: 升级前自检 / 启停 / 守护进程状态 / 本机在跑什么 / 开启自动领取 /
  关闭自动领取 / 优雅停机 / 调大并发

## Command

```
pf ops status                                        # local jobs + auto/max + queue counters
pf ops auto [on|off]                                 # bare: show; on: pull the ready queue itself; off: explicit pf submit only
pf ops max [N]                                       # bare: show; N >= 1: max concurrent jobs
pf ops shutdown                                      # graceful shutdown
```

To see what is ready before turning auto on (or to submit by hand), use
`pf queue --ready`.

## Success

- `status`: `daemon: <id>  auto: on|off  max: N` (plus `(shutting down)` when set),
  the queue counters (`pending / running`), one line naming the
  mapped workspace SET (`workspaces: <path>, <path>` — sorted; every
  `[workspace]` section the daemon routes capsules into, polyforge#113;
  per-WI capsules live at `<workspace>/capsules/<wi_id>`), and a
  jobs table (WORK
  ITEM, STATUS, ATTEMPT, AGENT PID, QUESTION, LAST-PROGRESS). This is
  bookkeeping about what this machine manages — not workflow execution
  progress (observe is the progress verb) — with one deliberately
  progress-shaped exception: LAST-PROGRESS is mm:ss since the job's
  stream last proved **agent progress** (`-` when no progress record
  exists). UI noise never refreshes it, so a job that is chattering but
  wedged reads as STALE, not fresh. A LAST-PROGRESS over ~10 minutes on
  a non-`needs_input` job is a stalled job — report it to the owner
  before anything else (see SKILL.md's turn-start obligation).
  A job with STATUS `repair_pending` is a work item the daemon parked at
  restart because its capsule directory is missing (`capsule_missing`) or
  its project is no longer mapped (`project_unmapped`); its session is
  kept. The owner's moves: fix the cause and `pf resume <id>`, or give up
  with `pf cancel <id>` / `pf wrap <id> --fail`.
- `auto` / `max` bare: `auto: on|off` / `max: N`; a change answers with
  the effective `{"auto":..,"max":..}`.
- `shutdown`: `graceful shutdown requested; the daemon quiesces and exits`
  — the sequence terminates the live agent trees, persists the
  bookkeeping and exits within its five-minute budget (there is no
  per-agent flush wait: uncommitted state is picked up by the recovery
  engine at the next start).
  Interrupted work items stay running server-side; the next daemon start
  recovers them through the transcript-resume engine.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-410` (`SHUTTING_DOWN`) | Any external submit while a graceful shutdown is in flight | Wait for the shutdown to complete and the daemon to restart. |
| `DAEMON-400` | Malformed ops-config request (`max < 1`, empty body) — the CLI's own argument validation usually catches this first with a usage error | Fix the invocation; read the detail. |
| `dial unix …` | The daemon is not running on this machine | Nothing to operate on — start it first (`pf daemon`); check `--socket` / `PF_DAEMON_SOCKET` if you expect it elsewhere. |

Auto vs shutdown: with auto on the machine also claims and runs
ready-queue work by itself (an internal loop; idle-polls when the queue is
empty, never exits); shutdown stops accepting anything new, closes what
remains and exits. They are independent concepts and never substitute for
each other.

Upgrade note: the pre-v2 `pf ops mode ...`, `abort-drain`, batch mode and
`pf batch-confirm` are gone. An old daemon-state.json `mode` migrates on
load (drain becomes auto on, anything else auto off), and the daemon flag
`-mode interactive|drain` is accepted for one release as a deprecated
alias of `-auto`.
