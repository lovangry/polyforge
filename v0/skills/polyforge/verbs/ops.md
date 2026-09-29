# ops — daemon operations: status, mode, graceful shutdown

Execution-plane verb: routed to the local daemon (00 §5.1; 04 §5.1
`status()` / `mode(set/drain/abort-drain)` / `shutdown()`). Machine-local
operations only: what this daemon is managing, how it accepts work, and how
it stops. Graceful shutdown (quiesce) is a process-lifecycle action and is
**not** drain — during shutdown remaining jobs are closed as paused, live
agents are quiesced at safe boundaries.

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
pf ops shutdown                                      # graceful shutdown (quiesce)
```

## Success

- `status`: `machine: <id>  mode: <mode>` (plus `(shutting down)` when set),
  the queue counters (`pending / running / max`), and a jobs table (WORK
  ITEM, ADAPTER, SESSION, RING). This is bookkeeping about what this machine
  manages — not workflow execution progress (observe is the progress verb).
- `mode` bare: `mode: <current>`; every mode transition answers with the new
  mode (e.g. `"mode": "drain"`).
- `shutdown`: `graceful shutdown requested; the daemon quiesces and exits` —
  remaining jobs land as `paused` server-side before the process exits.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-409` | Mode conflict — e.g. a `set` that is not a plain mode switch while drain is active; in drain mode single submits (`claim`/`resume`/`takeover`) and external `batch_submit` are rejected because the drain loop is the only job-creation entrance | Expected mode semantics, not a breakage — leave drain (`abort-drain`) to submit externally, or wait for the drain loop. |
| `DAEMON-410` (`SHUTTING_DOWN`) | Any external submit while a graceful shutdown is in flight | Wait for the shutdown to complete and the daemon to restart. |
| `DAEMON-400` | Malformed mode request (unknown action, bad JSON, bad flag value) — the CLI's own flag validation usually catches this first with a usage error | Fix the invocation; read the detail. |
| `DAEMON-511` (`SHUTDOWN_FLUSH_FAILED`) | During graceful shutdown a job's closing publish did not converge after retries: the shutdown aborts and the daemon stays up | Manual intervention required — investigate the sync obstacle in the daemon log, then retry the shutdown. |
| `dial unix …` | The daemon is not running on this machine | Nothing to operate on — start the daemon first; check `--socket` / `PF_DAEMON_SOCKET` if you expect it elsewhere. |

Drain vs shutdown: drain keeps the machine autonomously taking and running
batches (internal loop); shutdown stops accepting anything new and closes
what remains as paused. They are independent concepts and never substitute
for each other.
