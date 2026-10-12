# ops — daemon operations: status, mode, graceful shutdown

Execution-plane verb: routed to the local daemon (status / mode / shutdown
routes). Machine-local operations only: what this daemon is managing, how
it accepts work, and how it stops. Graceful shutdown is a
process-lifecycle action and is **not** drain — the shutdown sequence
terminates every live agent tree, persists its bookkeeping and exits; drain is a resident mode that keeps claiming queued work.

## Triggers

- EN: daemon status / what's this machine running / how many jobs / switch
  to batch mode / enter drain / leave drain / raise the concurrency / shut
  the daemon down gracefully / pre-upgrade check
- 中文: 升级前自检 / 启停 / 守护进程状态 / 本机在跑什么 / 切换模式 / 进入排水 /
  退出排水 / 优雅停机 / 调大并发

## Command

```
pf ops status                                        # local jobs + mode + queue counters
pf ops mode                                          # query the current mode (reads status)
pf ops mode set --max-concurrent-jobs N [--mode interactive|batch|drain]
pf ops mode drain                                    # enter autonomous drain mode
pf ops mode abort-drain                              # leave drain mode
pf ops shutdown                                      # graceful shutdown
```

## Success

- `status`: `machine: <id>  mode: <mode>` (plus `(shutting down)` when set),
  the queue counters (`pending / running / max`), one line naming the
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
- `mode` bare: `mode: <current>`; every mode transition answers with the
  new mode (and the effective concurrency on `set`).
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
| `DAEMON-409` | Mode conflict — e.g. a `set` that is not a plain mode switch while drain is active; in drain mode single submits and external `batch_submit` are rejected because the drain loop is the only job-creation entrance | Expected mode semantics, not a breakage — leave drain (`abort-drain`) to submit externally, or wait for the drain loop. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Any external submit while a graceful shutdown is in flight | Wait for the shutdown to complete and the daemon to restart. |
| `DAEMON-400` | Malformed mode request (unknown action, bad mode value, `max-concurrent-jobs < 1`) — the CLI's own flag validation usually catches this first with a usage error | Fix the invocation; read the detail. |
| `dial unix …` | The daemon is not running on this machine | Nothing to operate on — start it first (`pf daemon`); check `--socket` / `PF_DAEMON_SOCKET` if you expect it elsewhere. |

Drain vs shutdown: drain keeps the machine autonomously claiming and
running ready-queue work (internal loop; idle-polls when the queue is
empty, never exits); shutdown stops accepting anything new, closes what
remains and exits. They are independent concepts and never substitute for
each other.
