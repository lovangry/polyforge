# ops — daemon operations: status, auto-pull, max jobs, graceful shutdown; end-session recovery

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
- EN (recovery): take over a stuck work item / its daemon is dead / the
  machine holding it is gone / force-end the session
- 中文 (恢复): 接管卡住的 WI / 持有它的机器挂了 / 强制结束会话

## Command

```
pf ops status                                        # local jobs + auto/max + queue counters
pf ops auto [on|off]                                 # bare: show; on: pull the ready queue itself; off: explicit pf submit only
pf ops max [N]                                       # bare: show; N >= 1: max concurrent jobs
pf ops shutdown                                      # graceful shutdown
pf ops end-session <wi> --reason <text>              # force-end a stuck holder's session (aihub-direct)
```

`pf ops auto off` is more than the switch: the queue's not-yet-started
`origin=auto` entries are REMOVED at the same time (from memory and from
the persisted `submit-queue.json` a restart replays — auto off = only
explicit `pf submit`, v2-converge §5), each with a `submit_dropped` event
in `pf watch`; explicit `pf submit` / chat entries ride untouched, and so
do entries already being claimed. A restart with auto off replays the
same rule: the previous process's auto entries are dropped, never
re-admitted.

To see what is ready before turning auto on (or to submit by hand), use
`pf queue --ready`.

`end-session` is the one `ops` action that does not touch the local daemon:
it calls aihub with the CALLER's own key (admin or project maintainer,
checked server-side), ends the running/paused session without the holder's
secret, and returns the work item to `queued`; the old holder is fenced (its
secret no longer matches). Then any daemon claims it with a plain
`pf submit <wi>` — progress is restored from the aihub capsule revision chain
and the driver continues from the recorded current step. Failures: `AUTH-403`
(not admin/maintainer), `CORE-409` (`NOT_HELD`: not running/paused, nothing to
end), `CORE-410` (terminal).

## Success

- `status`: `daemon: <id>  auto: on|off  max: N` (plus `(shutting down)` when set),
  the queue counters (`pending / running`), one line naming the
  mapped workspace SET (`workspaces: <path>, <path>` — sorted; every
  `[workspace]` section the daemon routes capsules into, polyforge#113;
  per-WI capsules live at `<workspace>/capsules/<wi_id>`), and a
  jobs table (WORK
  ITEM, STATUS, SESSION, SPAWN, AGENT PID, QUESTION, STEP-AGE, LAST-WORK;
  plus ORIGIN when some job has one). This is
  bookkeeping about what this machine manages — not workflow execution
  progress (`pf watch` is the progress verb) — with two deliberately
  progress-shaped exceptions: STEP-AGE is mm:ss since the current dispatch
  began, and LAST-WORK is mm:ss since its harness last did real work (`-`
  when there is none). UI noise never refreshes LAST-WORK, so a job that is
  chattering but wedged reads as stale, not fresh. The daemon's own
  supervisor owns the stuck verdict (its silence/tool/step bounds, which
  a workflow step may override) and acts on it; a long LAST-WORK here is
  an observation to read in that light, not a rule to apply.
  A work item whose capsule directory is missing at restart
  (`capsule_missing`) or whose project is no longer mapped
  (`project_unmapped`) is not listed here: the daemon sets it `blocked` at
  the hub (reason note names the cause; the event shows in `pf watch`).
  The owner's moves: fix the cause if any and `pf resume <id>`, or give up
  with `pf cancel <id>` / `pf wrap <id> --fail`.
- `auto` / `max` bare: `auto: on|off` / `max: N`; a change answers with
  the effective `{"auto":..,"max":..}`.
- `shutdown`: `graceful shutdown requested; the daemon quiesces and exits`
  — the sequence terminates the live agent trees, persists the
  bookkeeping and exits within its five-minute budget. Interrupted work
  items stay running server-side; the next daemon start recovers them and
  commits any uncommitted working-tree state.

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
