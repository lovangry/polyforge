# Polyforge v1

Work-item orchestration platform: an aihub server (WI store, memory, git
revision chain) and a pf work-machine binary (Runtime + Observe + CLI).

## Architecture

Design record: `docs/design/polyforge-vnext/v1-architecture-spec.md`
(full design record: `v1-spec-full.md`; v0 pilot code: `/v0`, frozen).
The subsections below describe the code as it actually exists in this tree —
the live code is the authority; design-doc references are for traceability.

### Modules

One row per sub-package, exactly as `internal/` exists today:

| Package | Role |
| --- | --- |
| `internal/common/api` | Shared business contract between aihub and pf: work items, projects, users, generations, memories, and the typed error-code registry (`CORE-*`, `DAEMON-*`, `GIT-*`). |
| `internal/common/capsule` | Execution-capsule contract shared by pf (runtime creates capsules, observe commits them) and aihub (serves the matching bare repos; git-dir lives outside the capsule). |
| `internal/common/protocol` | The three in-machine contracts: Runtime↔Observe messages, the agent-side control surface (stdio answers + the UDS `flush(reason)` proxy), and the pf CLI↔daemon UDS HTTP API surface. |
| `internal/common/workflow` | Workflow-bundle digest contract shared by pf (bundle materialization with digest verification) and aihub (serves bundle bytes, validates digests at claim time). |
| `internal/aihub/core` | aihub persistent layer: SQLite store (single writer, WAL), the work-item state machine, claim/generation arbitration with attempt-scoped push credentials, the ready queue and the admin face. |
| `internal/aihub/git` | aihub git surface: per-WI bare-repo lifecycle, the git-http-backend smart-protocol proxy, attempt-scoped credential auth (generation fencing) and the pre-receive hook enforcing the capsule-chain invariants. |
| `internal/aihub/memory` | Core-side memory store: FTS5-trigram full-text search, authoritative `memory.md` files and the reconcile procedure. |
| `internal/aihub/server` | The v1 AIHub HTTP face: business endpoints (v0 openapi surface minus every SYNC-*/revision-chain route), the admin face, and the `/git/{wi}.git/...` proxy mount; Bearer API key auth, typed `api.Error` envelope. |
| `internal/polyforge/aihubclient` | Shared HTTP client for the aihub server, used by runtime, observe and the CLI verbs; decodes the typed error envelope back into `*api.Error`. |
| `internal/polyforge/cli` | The `pf` client CLI: 13 user verbs + the M1–M5 management face, the daemon client over the UDS socket, config resolution, and the v0 migration paths. |
| `internal/polyforge/daemon` | Phase-3 assembly of the local work machine: the UDS HTTP face, Runtime + Observe wired over the five protocol messages, admission modes, the pending-job dispatcher, the drain loop, the ship delivery bridge and the graceful shutdown sequence. |
| `internal/polyforge/gitops` | Shared capsule-git operations layer used by Runtime (clone at start, dirty-commit at recover) and Observe (async commit+push); owns the per-WI git mutex and the `Polyforge-Seq` trailer derivation. |
| `internal/polyforge/observe` | Observe lane: the events log, per-WI monitor goroutines, live-transcript streaming, and the observe/answer/flush/recall/remember HTTP face over the monitors. |
| `internal/polyforge/runtime` | Runtime lane: the WI lifecycle manager (Start/Stop/Recover/Restart), the pi-harness agent spawner, the six-layer process cleanup and the recovery engine. |

Binaries: `cmd/aihub` (server binary) and `cmd/pf` (work-machine binary:
CLI verbs + `pf daemon`).

### Daemon UDS routes

The routes mounted by the daemon's local Unix-socket HTTP face
(`internal/polyforge/daemon/routes.go`). **Face** marks who calls the route:
`user` = the operator's CLI / observe tooling; `agent` = the in-capsule
agent's lifeline proxies.

| Method | Path | Role | Face |
| --- | --- | --- | --- |
| GET | `/jobs/{wi}/observe` | Live observe view of one job's transcript stream (mounted verbatim from the Observe handler). | user |
| POST | `/jobs/{wi}/answer` | Answer a pending agent question. | user |
| POST | `/jobs/{wi}/flush` | Agent lifeline: `flush(reason)` close/publish signal (reason=wrap passes the close-arbitration gate first). | agent |
| POST | `/jobs/{wi}/recall` | Lifeline proxy: stateless memory search against the aihub memory face. | agent |
| POST | `/jobs/{wi}/remember` | Lifeline proxy: stateless memory write-back to the aihub memory face. | agent |
| POST | `/jobs/{wi}/claim` | Submit family: enqueue a claim of a queued work item. | user |
| POST | `/jobs/{wi}/resume` | Submit family: enqueue a resume (fresh generation) of a paused item. | user |
| POST | `/jobs/{wi}/takeover` | Submit family: enqueue a takeover of an item running elsewhere. | user |
| POST | `/jobs/{wi}/pause` | CLI close path: seal the job with reason `paused`. | user |
| POST | `/jobs/{wi}/wrap` | CLI close path: seal the job with reason `wrap` or `fail`. | user |
| POST | `/jobs/{wi}/ship` | Delivery bridge: push the sealed capsule's task repos to their origins as branch `polyforge/<wi>` (+ PR when a credential exists). | user |
| GET | `/status` | Machine status: jobs (with the needs-input projection), queue stats, mode. | user |
| POST | `/mode` | Mode actions: `set` / `drain` / `abort-drain` / `get_recommendation`. | user |
| POST | `/batch_submit` | Submit a whole confirmed wave of work items at once. | user |
| POST | `/shutdown` | Trigger the bounded graceful shutdown sequence. | user |

Authoritative source for this table is the code itself — the
`mux.Handle`/`mux.HandleFunc` mount list in `internal/polyforge/daemon/routes.go`;
design-doc references (e.g. `04-local-runtime.md §5.1`) are for traceability only.

### Release flow

The platform ships with zero release-specific code: release logic lives in
the project bundle's release step (`agents/release.md`), and the platform's
own release is one instance of that same hook. `pf upgrade` is a deployment
pipeline, not a release system (`07-deployment-pilot.md §1.1/§6.3`,
`00-architecture.md §3.3`).

```
 RELEASE: bundle release-step hook (no platform release code)
 +-----------------------------------------------------------+
 | project bundle release step (`agents/release.md`);       |
 | the platform's own release = one instance of that hook   |
 +----------------------------+----------------------------+
                              |
                              v
               mono-repo git tag (single semver per tag)
                              |
                              v
                releases/<tag>/  --  aihub      (server binary)
                                   --  pf        (work-machine binary)
                                   --  SHA256SUMS (checksum manifest)
                              |
                              v
 DEPLOYMENT: `pf upgrade` (pipeline, not a release system)
 +-----------------------------------------------------------+
 |  download -> sha256 verify -> quiesce -> replace -> restart  |
 +-----------------------------------------------------------+
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
the v1 pilot findings F1–F7) and declare, item by item, how your change
covers the entries it touches — the ledger only prevents re-hits if it is
actually read first. After a fix lands, write its status back: append the
「已修」/Fixed annotation (with commit reference) to the ledger entry the
change fixes, so the ledger the next lane recalls is never stale.

Minimal external dependencies: pure-Go SQLite (modernc.org/sqlite), yaml.v3, goldmark, jsonschema, x/sys.

## `pf recall` — memory search

Searches a project's memories on the aihub memory face (trigram-indexed
full-text search):

    pf recall [--project P] <query> [--type T] [--tags a,b] [--limit N]

`<query>` is required (positional or `--q`); `--project` defaults to the
configured project. Flags:

- `--limit N` — max results. **Default 20** when the flag is omitted
  (the CLI's product choice, sent explicitly); values must satisfy
  `1 ≤ N ≤ 200` — anything else fails loudly before a request is sent
  (`pf: recall: --limit must be 1..200`). The endpoint's own default of
  50 is a wire backstop for other callers, not this verb's contract.
- `--type T` — memory type filter, exactly three legal forms:
  - exact — `--type experience.debug` matches only that type;
  - prefix — `--type experience.*` matches every type under the
    `experience.` prefix (`experience.debug`, `experience.approach`, …;
    `fact.*` does not match `experience.*` types);
  - all — `--type *` is equivalent to omitting the flag.

  Any other `*` placement (`*.debug`, `exp*rience`, `experience*`) is
  rejected locally with no request sent. Both the GET and POST faces of
  `/api/v1/memories/search` interpret a trailing `.*` the same way.
- `--tags a,b` — comma-separated tag filter (a hit carries all of them).

Examples:

    pf recall flaky build
    pf recall flaky build --type experience.* --limit 50
    pf recall "retry ladder" --type fact.note --tags postgres,backoff
