# Polyforge v1

Work-item orchestration platform: an aihub server (WI store, memory, git
revision chain) and a pf work-machine binary (Runtime + Observe + CLI).

Architecture: `docs/design/polyforge-vnext/v1-architecture-spec.md`
(full design record: `v1-spec-full.md`; v0 pilot code: `/v0`, frozen).

## Layout

```
internal/
  common/       shared contracts (api types, capsule, workflow, protocol)
  aihub/        server: core (WI store) / memory / git / server (HTTP)
  polyforge/    work machine: cli / runtime (lifecycle) / observe (monitors)
cmd/aihub       server binary
cmd/pf          work-machine binary (13 verbs + `pf daemon`)
```

## Build & test

    make check     # build + vet + test, all packages
    go build ./...
    go test ./...

## `pf daemon` flags

`pf daemon` runs the local work machine (Runtime + Observe over the UDS
face). Every setting is a flag; where an environment variable is listed
the flag wins over it, and the shared CLI config file
(`~/.config/polyforge/cli.toml`, or `--config`) is the last fallback for
the `PF_*` entries.

| Flag | Env | Default | Controls |
| --- | --- | --- | --- |
| `--config PATH` | — | `~/.config/polyforge/cli.toml` | Shared CLI config file read for the `PF_*` settings. |
| `--socket PATH` | `PF_DAEMON_SOCKET` | `/run/polyforge.sock` | Unix socket the daemon's UDS HTTP face listens on. |
| `--server URL` | `PF_SERVER` | — | aihub base URL the daemon talks to. |
| `--api-key KEY` | `PF_API_KEY` | — | Machine aihub credential (never reaches an agent env). |
| `--project NAME` | `PF_PROJECT` | — | Ready-queue project scope for batch/drain. |
| `--machine-id ID` | `POLYFORGE_MACHINE_ID` | `m-<hostname>` | Machine identity claims and leases are recorded under. |
| `--data-dir DIR` | `POLYFORGE_DATA_DIR` | `~/.local/share/polyforge` | Daemon state, events log and derived data. |
| `--workspace-root DIR` | `POLYFORGE_WORKSPACE_ROOT` | `<data-dir base>/workspace` | Root the per-WI capsules are materialized under. |
| `--workflows-dir DIR` | — | `./workflows` | Workflow-bundle checkout the executor reads. |
| `--executor-prompt FILE` | — | `./skills/pf-execute/SKILL.md` | Executor skill file injected into every agent. |
| `--pi-bin PATH` | — | `pi` | Harness binary the agent processes are derived from. |
| `--harness-sessions-root DIR` | `POLYFORGE_HARNESS_SESSIONS_ROOT` | `~/.pi/agent/sessions` | Harness session store for the live-transcript sync. |
| `--cgroup-root DIR` | `POLYFORGE_CGROUP_ROOT` | — | cgroup v2 root used for layer-0 agent cleanup. |
| `--git-remote-base URL` | — | `<server>/git` | aihub bare-repo base URL for the capsule revision chain. |
| `--max-concurrent-jobs N` | — | `4` | Cross-mode cap on concurrently running jobs. |
| `--recall-topk N` | `POLYFORGE_RECALL_TOPK` | `10` (max 50) | Claim-time memory snapshot hit count. |
| `--mode M` | — | `interactive` | Initial mode (`interactive\|batch\|drain`); an explicit flag overrides the persisted mode. |

Observe supervision windows — Go duration strings (e.g. `700ms`, `5m`),
flags only (no environment fallbacks). Omitting a flag, or passing `0`,
keeps the observe default listed here, so a daemon started without them
behaves exactly as before:

| Flag | Env | Default | Controls |
| --- | --- | --- | --- |
| `--hang-window D` | — | `10m` | Stream silence while a turn is in flight before the liveness probe arms. |
| `--idle-window D` | — | `10m` | No progress record and no turn in flight before the idle watchdog probes. |
| `--probe-timeout D` | — | `2m` | Ack bound of one liveness probe. |
| `--answer-ack-window D` | — | `2m` | Bound on the agent's `pf/answer` consumption ack. |

Before starting ANY implementation or review lane: recall the v0 lessons
ledger (`docs/design/polyforge-vnext/v0-lessons-ledger.md`, now including
the v1 pilot findings F1–F4) and declare, item by item, how your change
covers the entries it touches — the ledger only prevents re-hits if it is
actually read first.

Minimal external dependencies: pure-Go SQLite (modernc.org/sqlite), yaml.v3, goldmark, jsonschema, x/sys.
