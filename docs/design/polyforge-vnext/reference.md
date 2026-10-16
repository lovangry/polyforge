# Polyforge reference — the current system

> What exists in this tree (v2.10.0 line), derived from the code; every claim names its source file.
> **The code is the authority**; where this file and the code disagree, fix this file. Decisions and
> their rationale live in [v2-converge.md](v2-converge.md) (decision authority, Chinese); the
> parent/child/bundle contract in [wi-kind-contract.md](wi-kind-contract.md); bundle authoring in
> [01-workflow-skills.md](01-workflow-skills.md); memory in [06-memory-search.md](06-memory-search.md);
> capsule base migration in [08-capsule-base-migration.md](08-capsule-base-migration.md). The v0/v1
> design set is in [archive/](archive/) — history only, do not build from it.
> Anchors use `<!-- §N -->`; `tools/check-xrefs.py` (run by `make check`) resolves every backticked
> name.md#§N reference.

## 1. System overview <!-- §1 -->

Core = **task** (aihub work items) + **memory** + **workflow** (bundles). Two binaries, one skill:

| Component | Code | Role |
|---|---|---|
| aihub | `cmd/aihub`, `internal/aihub/{server,core,memory,git}` | Single HTTP service on SQLite: work items, projects, users, API keys, sessions, memories, and a git smart-HTTP proxy holding one bare repo per WI (the capsule chain). |
| `pf` CLI | `cmd/pf`, `internal/polyforge/cli` | Data-plane verbs talk to aihub over HTTP (`internal/polyforge/aihubclient`); execution-plane verbs talk to the local daemon over a unix socket (`internal/polyforge/cli/daemon.go`). |
| daemon | `pf daemon` → `internal/polyforge/daemon` | One per machine. Claims WIs, owns capsules, hosts the driver and the supervisor in-process, serves the UDS face, heartbeats to aihub. |
| runtime | `internal/polyforge/runtime` | Job records, claim/attach/end, materialization, recovery engine, restart reconciliation, process reaping. |
| driver | `internal/polyforge/driver` | Runs one WI's workflow: dispatches one short-lived harness process per step, collects result envelopes, advances `runtime-state/execution.json`. |
| supervisor | `internal/polyforge/supervise` (rules), `internal/polyforge/observe` (monitor, events, push loop) | Judges a step dead / the driver stalled, records events, pushes the capsule chain. Never advances execution (`v2-converge.md#§2`). |
| capsule | `internal/common/capsule` | Per-WI working directory `<workspace>/capsules/<project>-<seq>/`, a git worktree whose history is the WI's chain on aihub (§8). |
| harness adapters | `internal/common/harness`, `internal/polyforge/runtime/spawn.go`, `runtime/claudespawn.go` | pi (`--mode rpc`) and Claude Code (`-p --bare`) step processes (§7.6). |
| watcher | `internal/polyforge/watcher` | `pf watch`: human progress + decision notifications (`v2-converge.md#§3`). |
| routing skill | `skills/polyforge/` (`SKILL.md` + `verbs/*.md`, `extensions/pf-chat.ts`, `hooks/claude-session-start.sh`) | The only install face in the user's harness: maps intent to a `pf` verb; never parses or runs a workflow (§11). |

Trust model: team-internal; harnesses are logged in by a human; the daemon never manages credentials;
no heavy isolation (#135 frozen). Guards exist against **accidents** (wrong push target, skipped human
gate, unreviewed release, agent driving the daemon) — `v2-converge.md#§0`.

### 1.1 Glossary <!-- §1.1 -->

- **WI** — work item (aihub row). Slug `<project>#<seq>`; `seq` is unique per project.
- **bundle** — a workflow: `workflows/<name>/workflow.md` + `steps/<id>.md` (§7.1). A WI with `bundle_name` is an *execution* ticket; without, a *registration* ticket; with children, an *epic* (derived, not stored — `wi-kind-contract.md#§1`).
- **session** — the claim lock: a daemon-minted secret whose hash aihub stores (§3). Not a chat.
- **chat** — `PF_CHAT_ID`, a notification label for an interactive harness session; never a credential (`v2-converge.md#§4`).
- **capsule** — the per-WI directory + git chain (§8). **task repo** — a project repo cloned under `repos/<name>/`.
- **spawn / spawn_n** — one step process dispatch and its per-WI monotonic counter.
- **step agent** — the harness process running one step. **operator** — the human.

## 2. Work items <!-- §2 -->

### 2.1 Statuses and transitions <!-- §2.1 -->

Statuses (`internal/common/api/types.go`): `queued running paused blocked wrapped failed canceled`
(single-l `canceled`). Terminal: `wrapped failed canceled` (`internal/aihub/core/workitem.go` `isTerminal`).
PATCH never sets status (`patchableFields`). Every transition is one guarded UPDATE in one transaction.

| Transition | Door (route → function) | Guard / effect |
|---|---|---|
| — → queued | `POST /work-items` → `WIManager.Create` | writer on the project; `repo_inventory` frozen at create (names only, must be project repos — `core/inventory.go`) |
| queued → running | `POST …/claim` → `core/session.go` `Claim` | §3.2 |
| running → paused | `POST …/session-end` `status=paused` → `Pause` | must be running (`NOT_RUNNING`); session kept; `clean_pause` set atomically; event `session_paused` |
| paused → running | `POST …/session-attach` → `Attach` | hash mismatch → `CORE-403 STALE_SESSION`; not paused or `clean_pause=true` → `CORE-409 NOT_ATTACHABLE`; event `session_attached`; operator unpause = PATCH `clean_pause:false` |
| running/paused → wrapped/failed/blocked | `POST …/session-end` → `End` | hash-fenced; clears the whole session row; wrapped/failed set `closed_at`; blocked sets `waiting_on='owner'` and, with `head_sha`, `attrs.blocked_seal` (§3.5); event `session_ended` |
| running/paused → queued | `POST …/force-end-session` → `ForceEnd` | admin or project maintainer; reason required; terminal → `CORE-410`, no session → `CORE-409 NOT_HELD`; clears session + seal; event `session_superseded` (old fingerprint). CLI: `pf ops end-session <wi> --reason …` |
| blocked → queued | `POST …/resume-blocked` → `ResumeBlocked` | reporter/maintainer/admin; terminal → `CORE-410`; status blocked (`NOT_BLOCKED`) ∧ no session (`SESSION_HELD`); event `wi_resumed` |
| queued/paused/blocked → canceled | `POST …/cancel` → `Cancel` | reporter/maintainer/admin; running → `CORE-409`; terminal → `CORE-410`; event `work_item_canceled` |

The blocked WI has exactly two exits: `pf resume` (continue = resume-blocked + a fresh claim) and
`pf cancel` (abandon) — `v2-converge.md#§6`. `end_pending`, `recovering`, `sealing`, `claiming` are
**daemon-local job states** (`internal/polyforge/runtime/state.go`), not hub statuses.

### 2.2 Dependencies, parents, ready queue <!-- §2.2 -->

- Edge kinds `blocks` (default), `supersedes`, `related` (`core/workitem.go` `AddDependency`); same project only; no self-edge; a `blocks` blocker must be an execution ticket (`CORE-400 BLOCKER_NOT_EXECUTABLE`); non-`related` edges refuse cycles (`CORE-409 DEPENDENCY_CYCLE`). Only a **wrapped** blocker releases a `blocks` edge (`core/owner.go`, #161).
- Parent/child and bundle binding rules: `wi-kind-contract.md#§2` (one nesting level, `PARENT_NOT_REGISTRY`, `HAS_CHILDREN_NO_BUNDLE`, bundle frozen at first claim).
- Ready queue `GET /projects/{p}/ready-queue` (`core/queue.go`): `{project, items, running, paused, pending_bundles}`; `items` = queued ∧ `waiting_on` empty ∧ no unfinished `blocks` edge ∧ has a bundle ∧ inventory repos still exist; ordered priority (`urgent high normal low`) then `created_at`. No `blocked` segment.

### 2.3 attrs: no execution state <!-- §2.3 -->

`attrs` is a free JSON object written at create; aihub never stores workflow execution state in it.
Create refuses these keys (at any nesting depth) with `CORE-400 EXECUTION_STATE_REJECTED` (`core/workitem.go`
`executionStateKeys`): `current_step step_id step_attempt_id attempt review_round review_rounds
review_verdict progress workflow_status steps rewind_stats budget_consumed command_count
step_start_refs pending_question`. Execution state lives only in the capsule's `execution.json`
(§7.3). The one daemon-written key is `blocked_seal` (§3.5).

## 3. Sessions and fencing <!-- §3 -->

### 3.1 Secret, hash, fingerprint <!-- §3.1 -->

- **secret** `session_id`: 32 random bytes hex, minted by the daemon (`runtime/runtime.go`), journaled before the claim call (§3.5). Lives only in the daemon's data dir — the claiming journal (`claiming.json`) and the job record (`runtime-state.json`) — and in a one-shot `GIT_ASKPASS` file; never in the capsule, argv or step env.
- aihub stores only `session_hash = hex(sha256(secret))`; public **fingerprint** = first 16 hex chars (`api.SessionHashHex`, `api.SessionFingerprint` in `internal/common/api/types.go`). WI reads expose `session_fingerprint`, `session_machine`, `session_started_at`, `clean_pause`.

### 3.2 Claim <!-- §3.2 -->

`POST /api/v1/work-items/{id}/claim` body `{work_item_id, session_id, machine_id, idempotency_key}`
(`api.SessionClaimRequest`); caller needs writer/maintainer membership or admin (key scope alone does
not authorize). One transaction (`core/session.go` `claimTx`), checked in order: not terminal
(`CORE-410 TERMINAL_STATE`), no session (`CORE-409 ALREADY_HELD`), queued (`NOT_CLAIMABLE`), no
unfinished blocker (`DEPENDENCY_BLOCKED`), bundle present (`NOT_CLAIMABLE`); then
`UPDATE … WHERE status='queued' AND session_hash IS NULL` (0 rows → `CORE-409 RACE_LOST`); a declared inventory repo that left the project → `CORE-409 INVENTORY_REPO_GONE`. Response `{claim, work_item}`
(`api.SessionClaimResponse`; `claim.repos` = the inventory-filtered project repos; `sealed_head` /
`sealed_machine` from the blocked seal). Idempotency (`claim_idempotency` table): same key replays the
same response; a different caller/WI → `CORE-409 IDEMPOTENCY_KEY_REUSED`; a replay whose session is no
longer live → `CORE-403 STALE_SESSION`. Idempotency rows are kept forever (`v2-converge.md#§13`).

### 3.3 Verify, attach, end <!-- §3.3 -->

- `session-verify` `{session_id}` → `{work_item_id, matches, status, current_fingerprint, clean_pause}`; compare-only, no state change. Used at restart, by the claiming-journal replay, and by the M4 revalidation (§3.5).
- `session-attach` `{work_item_id, session_id, machine_id}` → same shape as claim; same-session resume of a paused WI (restart reconciliation, `pf resume` of a paused WI via daemon `POST /jobs/{wi}/resume-paused`, `v2-converge.md#§7`).
- `session-end` `{session_id, status: wrapped|failed|blocked|paused, note, clean_pause, head_sha}` (`api.SessionEndRequest`). Hash mismatch (or a lost race on the guarded UPDATE) → `CORE-403 STALE_SESSION`.

### 3.4 Git push auth <!-- §3.4 -->

`/git/<wi>.git` (`internal/aihub/git/proxy.go`, `core/owner.go` `CheckSessionCredential`): Basic user
must be the WI id, password the live session secret. No session → `GIT-401 NO_SESSION`; mismatch →
`GIT-401 BAD_CREDENTIAL` (message names the current holder fingerprint); stored fingerprint not the
secret's → `GIT-401 BAD_FINGERPRINT`; status ≠ running → `GIT-403 PAUSED` (checked on every smart-HTTP
request, so fetch/clone are refused too). Backup layer: the push must carry `--push-option=session-fingerprint=<fp>` and the
pre-receive hook compares it with the proxy-derived fingerprint (`git/hook.go`). Fencing covers the
aihub proxy only — pushes a step makes to an external origin are not fenced.

### 3.5 Daemon-side session state <!-- §3.5 -->

- **Claiming journal** `<data-dir>/claiming.json` (`runtime/claiming.go`): `{work_item_id, session_id, idempotency_key, machine_id, at, models}` written durably **before** the claim call (write failure refuses the claim); cleared when the start settles. Restart replay (`resumeClaiming`): verify transport error → keep; no match → drop; already in the job table → drop; else re-claim with the same key; a refused re-claim ends the session blocked. Corrupt file → quarantined `claiming.json.corrupt.<ts>`.
- **Job record** `<data-dir>/runtime-state.json` (`runtime/state.go`): secret, fingerprint, job state, capsule/git dirs, remote, `spawn_n`, recovery-failure count, per-step candidate snapshot.
- **M4 revalidation**: the monitor re-verifies every running job's session (default every 5m, `--revalidate-every`, `observe/session.go`); `matches=false` → kill tree, quarantine the capsule (`<name>.quarantined.<ts>`), drop the record, event `session_superseded`; no hub transition.
- **end_pending lane** (`runtime/endpending.go`): a failed `SessionEnd(blocked)` is retried (pushing the local chain first) until aihub accepts it or verify shows the session is no longer held.
- **Blocked seal**: `End(blocked)` with `head_sha` stores `attrs.blocked_seal={head, machine}`; a claim by another machine whose clone head differs is refused `DAEMON-518` (`runtime.go` `verifySealedHead`; `v2-converge.md#§E`).
- **Single daemon per data dir**: `<data-dir>/daemon.lock` flock taken before the socket probe (`daemon/daemon.go` `acquireDataDirLock`); a live listener on the socket also refuses a second daemon.

## 4. aihub service <!-- §4 -->

### 4.1 HTTP routes <!-- §4.1 -->

Registered in `internal/aihub/server/server.go` `routes()` — 34 registrations. Auth: **key** = Bearer API
key (`s.auth`), **admin** = key of a `role=admin` user (`s.authAdmin`). Project access
(`canRead`/`canWrite`): any membership (`viewer`/`writer`/`maintainer`) reads, `writer`+ writes, a global
admin does both; a key scoped to one project is refused elsewhere (`AUTH-403`). No unauthenticated route, no
health route. All paths under `/api/v1` except `/git/`.

| Method | Path | Auth / gate | Notes |
|---|---|---|---|
| POST | `/work-items` | key + project writer | create (`bundle_name`, `repo_inventory`, `parent_id`, …) |
| GET | `/work-items` | key; rows filtered by read access | `limit` (default 500, max 1000), `project`, `status`, `assignee`, `priority`, `milestone` |
| GET | `/work-items/{id}` | key + read | `include=events,deps` (anything else `CORE-400`); `derived` block for parents |
| PATCH | `/work-items/{id}` | key + writer | `goal content labels priority milestone waiting_on clean_pause`, `bundle_name` in the R2 window (`patchableFields`; `attrs` only at create) |
| POST | `/work-items/{id}/cancel` | reporter/maintainer/admin | §2.1 |
| POST | `/work-items/{id}/claim` | writer | §3.2 |
| POST | `/work-items/{id}/session-verify`, `/session-attach`, `/session-end` | key (scope check) + secret | §3.3 |
| POST | `/work-items/{id}/force-end-session` | admin/maintainer | §2.1 |
| POST | `/work-items/{id}/resume-blocked` | reporter/maintainer/admin | §2.1 |
| POST, GET | `/work-items/{id}/dependencies` | add: writer; list: read | add / list edges; an unreadable blocker is `CORE-404` |
| DELETE | `/work-items/{id}/dependencies/{blocking}/{kind}` | writer | physical delete |
| GET | `/projects/{project}/ready-queue` | key + read | §2.2 |
| POST | `/machines/heartbeat` | key; first registration needs admin or any-project maintainer | §4.3 |
| GET | `/machines` | admin | §4.3 |
| POST | `/memories` | key + writer (else `MEM-403`) | §9 |
| GET | `/memories/search` | key | `q` (required), `match` (`all` default: every term; `any`: ranked any-term; else `MEM-400`), `type` (exact or `X.*`), `tags`, `limit` (default 50; ≤0 or >200 → 50), `project`, `excerpt` (≤8192 runes → `content_excerpt`) |
| GET | `/memories/{id}` | key; denied = `MEM-404` | `format=content` → raw Markdown body |
| POST | `/memories/{id}/redact` | owner or admin | soft delete |
| POST, GET / GET, PATCH | `/users`, `/users/{id}` | admin | |
| POST, GET / DELETE | `/api-keys`, `/api-keys/{id}` | admin | |
| POST, GET / GET | `/projects`, `/projects/{id}` | admin | |
| PATCH | `/projects/{id}` | key + in-handler role gate | repos, membership, description, visible |
| POST | `/admin/backup` | admin | `VACUUM INTO <data-dir>/backups/aihub-<ts>.db` — **SQLite only**, no memory files, no git repos (`core/admin.go`) |
| ANY | `/git/` | session Basic auth (§3.4) | git smart-HTTP via `git http-backend` |

### 4.2 Process and data dir <!-- §4.2 -->

`cmd/aihub/main.go` flags: `-data-dir` (default `/var/lib/polyforge/aihub`), `-listen` (`:8420`),
`-git-janitor-period` (1h, gc/repack of grown bare repos, `git/janitor.go`), `-machines-stale-after`
(5m), `-version`. A fresh DB seeds admin `usr_root` and writes its key to a 0600 file in the data dir
(`bootstrapAdmin`). A second process on the same data dir exits `CORE-508` (`service.lock`). SIGTERM
drains for 10s. SQLite (`core/store.go` `Open`): WAL, `foreign_keys(1)`, `busy_timeout(5000)`,
`synchronous(FULL)`, one open connection (single writer). No startup schema migration: SQL moves only through `.polyforge/deploy.sh` with
`deploy/sql/<tag>/` (`docs/upgrade.md`). Data dir: `aihub.db`, `memory/<project>/<mem>/memory.md`
(+ `_redacted/`), `git/<wi>.git` (bare chain repos with the pre-receive hook, `git/repo.go`), `backups/`.
Startup runs the memory reconcile in the background (§9). In production aihub runs as a docker
compose service (`deploy/docker-compose.yml`, `restart: unless-stopped`).

### 4.3 Machines <!-- §4.3 -->

Heartbeat body `{machine_id, runtime_version, display_name, mode, live_wi_ids}` (`core/admin.go`);
server-stamped `last_heartbeat_at`. A new `machine_id` creates (one transaction) a `mac_` machine user,
the machines row and a first API key returned once; the daemon does not keep it. Daemon cadence
`-heartbeat-every` 60s (`daemon/heartbeat.go`), log-only failures with backoff. `GET /machines`
returns `{machines:[{id,user_id,runtime_version,mode,live_wi_ids,last_heartbeat_at,heartbeat_age_seconds,stale}], stale_after_seconds, count}`, oldest first.
The daemon id is the machine id: default `m-<hostname>`, persisted in `<data-dir>/daemon-id`
(`daemon/identity.go`, `v2-converge.md#§4`).

## 5. Error codes <!-- §5 -->

Registry `internal/common/api/errors.go`; HTTP status = numeric suffix. `core.Fail` adds `extra.reason`.

| Code | Meaning (as raised) |
|---|---|
| `CORE-400` | Validation. Reasons incl. `IMMUTABLE`, `HAS_CHILDREN_NO_BUNDLE`, `PARENT_*`, `INVENTORY_*`, `BLOCKER_*`, `EXECUTION_STATE_REJECTED` |
| `CORE-403` | `STALE_SESSION`: the presented session is not the holder (attach/end/pause/idempotent replay) |
| `CORE-404` | WI/project/user missing, or hidden by the read gate |
| `CORE-409` | State conflict: `ALREADY_HELD NOT_CLAIMABLE DEPENDENCY_BLOCKED RACE_LOST IDEMPOTENCY_KEY_REUSED NOT_ATTACHABLE NOT_HELD NOT_RUNNING NOT_BLOCKED SESSION_HELD DEPENDENCY_CYCLE INVENTORY_REPO_GONE`; without a reason: cancel of a running WI, project already exists, state changed concurrently |
| `CORE-410` | `TERMINAL_STATE` |
| `CORE-500` | Untyped server error (default mapping) |
| `CORE-507` | Admin backup `VACUUM INTO` failed |
| `CORE-508` | Another aihub holds `service.lock` |
| `MEM-400` | Bad memory input (empty `q`, bad frontmatter, client-supplied id) |
| `MEM-403` | No write role; redact by a non-owner |
| `MEM-404` | Memory missing or read-denied |
| `MEM-409` | Idempotency key reused; redact of a non-active memory |
| `MEM-413` | Content over 1 MiB |
| `MEM-500` | Memory fs/transaction failure |
| `MEM-502` | Startup reconcile failure (walk, sweep, row scan, per-entry, FTS, commit; the name says "upstream", nothing is) |
| `MEM-503` | FTS query/scan failure |
| `AUTH-401` | Missing/invalid API key |
| `AUTH-403` | Not admin; key scoped to another project; not a writer |
| `GIT-400` | Wrong smart-HTTP method/target |
| `GIT-401` | `NO_SESSION`, `BAD_CREDENTIAL`, `BAD_FINGERPRINT`; client side "no push remote" |
| `GIT-403` | `PAUSED`: any git request while the WI is not running |
| `GIT-404` | Bad path / traversal / repo missing |
| `GIT-412` | Local staged gitlink refused by `pf` before commit (the hook rejects gitlinks with text only) |
| `GIT-500` | CGI failure; push failure in `pf` |
| `DAEMON-400` | Bad UDS request |
| `DAEMON-403` | Mutating UDS request from a step process tree (SO_PEERCRED; fail closed) |
| `DAEMON-404` | No local job for the WI |
| `DAEMON-409` | Job-state conflict (e.g. wrap/fail of a parked WI, pause/wrap while recovering, resume-paused refused) |
| `DAEMON-410` | Daemon is shutting down; start deferred |
| `DAEMON-412` | WI's project not in the `[workspace]` mapping (pre-claim) |
| `DAEMON-414` | `.polyforge.yaml` invalid / unknown step key (pre-claim) |
| `DAEMON-500` | Daemon internal |
| `DAEMON-503` | aihub unreachable (pre-claim probe, journaled-claim verify, resume-paused); nothing claimed |
| `DAEMON-507` | Task-repo clone failure at claim (`runtime/repos.go`; the constant is named `SpawnFail`) |
| `DAEMON-516` | Re-claim capsule clone failed for a WI with published progress; local init refused |
| `DAEMON-518` | Blocked WI continued on another daemon and the aihub head ≠ the sealed head |

`CORE-507` (`ErrCoreStorage`): the admin backup's `VACUUM INTO` failed (`core/admin.go`).

## 6. Daemon <!-- §6 -->

### 6.1 UDS routes <!-- §6.1 -->

`internal/polyforge/daemon/routes.go` `routes()` — 17 routes, all wrapped by `refuseStepOrigin`: every
non-read request whose peer (or its ancestors / process-group leader) carries this daemon's
`PF_WI_ID`+`PF_DAEMON_ID` markers — or whose peer credential cannot be read — is refused `DAEMON-403`
(SO_PEERCRED capture `daemon/peercred.go`, check `runtime/stepproc.go` `StepProcessOrigin`).

| Method | Path | Does | Caller |
|---|---|---|---|
| POST | `/jobs/{wi}/claim` | enqueue + claim + start (priority, models, origin) | `pf submit` |
| POST | `/jobs/{wi}/answer` | deliver an answer to the driver bridge (no `question_id` → the single pending one) | `pf answer` |
| POST | `/jobs/{wi}/resume-blocked` | continue a blocked WI (hub door + re-claim) | `pf resume` |
| POST | `/jobs/{wi}/resume-paused` | live holder re-attaches a paused WI in place (`attached`/`already_live`/`closed`) | `pf resume` |
| POST | `/jobs/{wi}/pause` | operator pause (clean_pause) | `pf pause` |
| POST | `/jobs/{wi}/wrap` | terminal close with verdict | `pf wrap [--fail]` |
| GET | `/status[?harnesses=1]` | machine id, auto, max, workspaces, jobs, queue; harness probe | `pf ops status`, `pf doctor`, `pf daemon ensure` |
| POST | `/ops/config` | auto-pull switch, max concurrent jobs | `pf ops auto on|off`, `pf ops max N|--reset` |
| POST | `/shutdown` | async graceful shutdown | `pf ops shutdown` |
| POST | `/chat/init` | harness session → chat id | `pf chat init` |
| GET | `/jobs/{wi}/watch?after=N` | per-WI snapshot + events | `pf watch <wi> --once` |
| GET | `/chats/{id}/subscriptions` | list | `pf watch` |
| POST | `/chats/{id}/subscribe`, `/unsubscribe` | manage subscriptions | `pf watch subscribe|unsubscribe` |
| POST | `/chats/{id}/ack`, `/heartbeat` | cursor advance; watcher liveness (TTL 20s) | resident `pf watch --chat` |
| GET | `/chats/{id}/attention` | pending human decisions | resident `pf watch --chat` |

Socket: `--socket` > `PF_DAEMON_SOCKET` > cli.toml `daemon_socket` > `/run/polyforge.sock`
(`cli/config.go`, `daemon/main.go`).

### 6.2 Lifecycle <!-- §6.2 -->

- **Start = `pf daemon ensure` only** (`daemon/ensure.go`). Called by `tools/install.sh`, the Claude SessionStart hook (`--timeout 15s`) and the pi `pf-chat` extension. Socket answers → exit 0. Refuses inside a step (`PF_WI_ID` set). `<data-dir>/ensure.lock` (flock) serializes callers; losers wait and answer by the socket. Canonical argv: `--config`, `-machine-id`, `-data-dir`, `-socket`, `-workflows-dir <data-dir>/release/current/workflows`.
- **tmux loop** (loop script in `daemon/ensure.go`; `internal/polyforge/supervisor` only names the session and the systemd unit): session `polyforge-daemon`; loop runs `nohup pf daemon …`, appends to `<data-dir>/daemon.log` (rotated at 10 MiB); exit 0 → loop ends; **exit 78** → loop ends ("needs an operator fix"); anything else → restart after 5s. No tmux → one unsupervised start with a warning. `pf doctor` also accepts a self-managed `polyforge-daemon.service`.
- **Daemon environment = an allow-list** (`supervisor/env.go`, the one definition): ensure gives the daemon ONLY `PATH`, `HOME` (pinned), `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, the proxy vars (`http_proxy`/`https_proxy`/`no_proxy`/`all_proxy`, both cases), `SSL_CERT_FILE`/`SSL_CERT_DIR`, the XDG base dirs (`XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`), the Go toolchain (`GOPATH`, `GOROOT`, `GOPROXY`, `GOPRIVATE`, `GONOSUMDB`, `GONOPROXY`, `GOFLAGS`, `GOTOOLCHAIN`, `GO111MODULE`, `GOCACHE`, `GOMODCACHE`), `POLYFORGE_DATA_DIR`, `GOCOVERDIR`, the marker `POLYFORGE_SUPERVISOR=tmux:polyforge-daemon` and the loop shell's own `PWD` — values from the CALLER of ensure, never the tmux server (whose environment is whatever shell first started it). The loop script runs `exec /usr/bin/env -i <assignments> /bin/sh -c <loop>`; the no-tmux fallback sets the same list as `cmd.Env`. Names that are not `^[A-Za-z_][A-Za-z0-9_]*$` are never carried (they would be shell syntax in the script); empty values are kept. No `ANTHROPIC_*`, no `PF_*` (ensure's own config load still honours them; the daemon reads `cli.toml`), no retired `POLYFORGE_*`, no harness-session markers. Steps see only this environment: pi provider keys belong in `auth.json` / literal `models.json` keys. Adding a variable is one line there; never a credential. `GET /status` reports the daemon's env NAMES (`env_names`, never values); `pf doctor`'s `daemon env` row warns on any `ANTHROPIC_*` or credential-looking name (`…TOKEN`/`…KEY`/`…SECRET`/`PASSWORD`) outside the list, with the clean restart (`pf ops shutdown`, wait, `pf daemon ensure`), and on any `$NAME` the step agent's `models.json` interpolates (`pi_models_env_refs` in `/status`) that is off the list.
- **Exit 78** = `ExitNeedsOperator` (`daemon/main.go`): startup failure only an operator can fix — config, identity mismatch, no `[workspace]` mapping, workspace dirs, on-disk state, failed restart reconciliation. Fix, then `pf daemon ensure`. After a machine/container reboot the daemon is down until the next
  interactive harness session runs ensure (auto-pull idles meanwhile); the skill retries an
  execution-plane verb once after `pf daemon ensure` when the daemon is unreachable.
- **Graceful shutdown** (`daemon/shutdown.go`; `pf ops shutdown`, SIGTERM/SIGINT; a second signal aborts hard): latch shutting-down (new starts `DAEMON-410`) → stop observe → terminate every agent tree (10s) → snapshot task repos to `refs/pf-wip/<name>` (per-job slice, ≤2m) → persist state → close listener. Whole budget **5 min** (`Config.ShutdownBudget`). Running WIs are **not** session-ended: they stay `running` at aihub with `clean_pause=false` and are attached on the next start. Cause written to `last-shutdown.json`, read once by the next start (`daemon_restarted` event; absent = `unknown`, i.e. crash/SIGKILL). There is no `pf daemon restart`: restart = `pf ops shutdown` then `pf daemon ensure`.
- **Restart reconciliation** (`runtime/restart.go`): claiming-journal replay first; then per job record (orphans of that WI reaped inside each branch) a session verify — match+paused+`clean_pause=false` → attach and resume; match+running → recover-resume; mismatch → release (kill, quarantine, drop; `session_released`); transport error → keep, mark `recovering`, retry with backoff (`session_verify_deferred`); typed 4xx → blocked (`session_verify_outage`). Missing capsule / unmapped project blocks only that WI (`capsule_missing`, `project_unmapped`). Reaping (`runtime/reap.go`): spawn-journal pids + a `PF_WI_ID`+`PF_DAEMON_ID` environ sweep (layers in `runtime/cleanup.go`: PPID snapshot → process-group kill → (pid,starttime) re-kill → env-marker scan → cwd scan warns only). Decision record: `v2-converge.md#§6`.
- **Recovery engine** (`runtime/recover.go`): kill the tree, commit dirty state, then relaunch the in-process driver as a fresh derivation (spawn_n advanced; the driver continues from `execution.json`; the old resume→fresh cascade is gone, polyforge#106); after 2 consecutive failed recoveries (`MaxRecoveries`) the WI ends blocked. (Within a step, the driver's own retry shape still resumes the transcript first — §7.4.) A daemon restart re-dispatches the same model and is not counted as a failure.

### 6.3 Configuration <!-- §6.3 -->

- **cli.toml** `$XDG_CONFIG_HOME/polyforge/cli.toml` (default `~/.config/polyforge/cli.toml`; `cli/config.go`). Flat keys `server api_key daemon_socket user_id claude_bin`; env overrides `PF_SERVER PF_API_KEY PF_DAEMON_SOCKET PF_USER_ID PF_CHAT_ID`. Unknown sections / flat keys are load errors.
- **`[daemon]`** keys exactly `step_timeout stall_window harness_root operator_agent_dir max_concurrent_jobs` (quoted Go durations, absolute paths; `parseDaemonTable`). The daemon reads the table itself from `--config`; precedence explicit flag > table > default (`daemon/main.go` `applyDaemonTable`). `pf ops max N` persists in `daemon-state.json` and wins until `pf ops max --reset`.
- **`[workspace."<path>"]`** `projects = [...]` — the only source of capsule placement (no workspace flag/env). A project maps to exactly one workspace; zero sections → exit 78; each workspace dir must exist and `capsules/` be writable (`internal/polyforge/mapping` builds the plan, `daemon/daemon.go` checks the dirs at start). Migration runbook: `08-capsule-base-migration.md#§2`.
- **Daemon flags** (`daemon/main.go`): production-relevant `--config -socket -server -api-key -machine-id -data-dir (POLYFORGE_DATA_DIR, default ~/.local/share/polyforge) -workflows-dir -claude-bin -harness-root -operator-agent-dir (default ~/.pi/agent) -max-concurrent-jobs (4) -step-timeout (90m) -stall-window (10m)`; test/dev-only `-display-name -pi-bin -git-remote-base -recall-topk -auto -silence-window -tool-window -model-error-grace -revalidate-every -heartbeat-every`. Retired `POLYFORGE_HARNESS_ROOT` / `POLYFORGE_OPERATOR_AGENT_DIR` only warn.
- **`.polyforge.yaml`** at the workspace root (`internal/polyforge/workspace/workspace.go`): closed grammar `steps.<project>.<default|step-id>.models: [candidates]`. Strict: unknown keys, `harness` (`RetiredHarnessHint`), the old top-level `models:`, unmapped projects, and step keys that are not installed step ids are errors → `DAEMON-414` before claim; `pf doctor` reports the same. Read fresh before every claim.
- **Auto-pull**: `pf ops auto on|off` + `pf ops max N` (`v2-converge.md#§5`); with auto on the daemon pulls each mapped project's ready queue; accepted submits beyond the cap queue in `submit-queue.json` and replay after restart.

### 6.4 Data dir and events <!-- §6.4 -->

Default `~/.local/share/polyforge`. File inventory: `v2-converge.md#§E` (E.4), plus `git/askpass/`
(one-shot credential helpers, swept at start; `gitops/gitops.go`). Events: one JSONL line per event in
`events.log` (16 MiB rotation, 3 generations; per-WI seq monotonic across restarts via `events.seq`;
`observe/events.go`); kinds and emitters in E.1. Execution state authority is always the capsule's
`runtime-state/execution.json`; events are news.

## 7. Execution <!-- §7 -->

### 7.1 Bundles and materialization <!-- §7.1 -->

- Shipped bundles: `workflows/{feature-full,bug-fix,chore,autopilot,release}`; shared step files in `workflows/_shared/steps/` (today `deliver.md`). `_shared` is not a bundle (no `workflow.md`; claiming it is refused).
- Step file lookup `workflow.StepDefinitionPath` (`internal/common/workflow/workflow.go`): the bundle's own `steps/<id>.md` wins, else `_shared/steps/<id>.md`. Do not share by symlink — materialization skips symlinks.
- Source: the daemon's `-workflows-dir` (production `<data-dir>/release/current/workflows`). At claim the bundle frontmatter is read from that checkout; then `materializeBundle` (`runtime/materialize.go`) copies the bundle to `<capsule>/workflows/<bundle>/` and `_shared/` beside it via `sync/materialize-staging/`, marking completion in `sync/materialized.json`. A missing bundle warns and continues. No digest check (deleted, polyforge#149).
- The driver loads the materialized copy: `driver/workflow.go` `LoadWorkflow` → `validateWorkflow`. Authoring guide: `01-workflow-skills.md#§4`.

### 7.2 workflow.md frontmatter <!-- §7.2 -->

Parsed keys (`driver/workflow.go` `extDoc`/`extStep`); **unknown keys are silently ignored** (incl. the
retired step `kind:`, `interactive:`, `tools:`); `name`/`description` are unread.

| Key | Meaning |
|---|---|
| `defaults.model_candidates` | fallback candidate list (§7.5) |
| `rewind.max_total` / `rewind.max_per_step` | rewind bounds, default 4 / 2 |
| `steps[]` | declaration order = execution order |
| `steps[].id` | required, unique; names `steps/<id>.md` and the `.polyforge.yaml` key |
| `steps[].gate` | step yields a verdict; must be `context_policy: fresh` |
| `steps[].on_fail.target` | gates only; an earlier step; absent → `failed_gate` |
| `steps[].requires_human_answer` | `true` (needs a human answer in the current attempt chain) / `if_asked` (questions are human-only; zero questions → event `human_gate_unasked`) / `false` |
| `steps[].requires_command_evidence` | completed result must carry `command_evidence` |
| `steps[].context_policy` | `inherit` (default) / `fresh` |
| `steps[].handoff` | step writes `artifacts/handoff-<id>.md` for the next inherit step |
| `steps[].model_candidates` | step's own candidate list |
| `steps[].budget.wall_minutes` | estimate: T = max(3 × wall_minutes, `--step-timeout`) |

Load-time rules (`validateWorkflow`): non-empty unique ids; `on_fail` only on gates and pointing
earlier; `context_policy` ∈ {inherit, fresh}, gates fresh. `requires_human_answer` ∈ {true, false,
if_asked} is enforced by its YAML decoder; candidates are validated in `driver.New` and, before the
claim, against the daemon checkout (`common/workflow.CheckCandidates`: unprefixed candidates or a
`harness` key → `DAEMON-414`, WI stays queued). Rewind bounds ≤0 fall back to 4 / 2. Step file frontmatter: only `output:` (draft-07 JSON Schema in YAML) is
read — injected into the prompt and checked record-only (`driver/outputschema.go`,
`enforceOutputSchemas=false` → event `output_schema_violation`).

### 7.3 Step dispatch and the result envelope <!-- §7.3 -->

- One harness process per dispatch, cwd = capsule. The step writes **one** JSON envelope to `runtime-state/pending-results/<spawn_n>.json`: `{wi_id, step_id, spawn_n, outcome, output, question_id, question, command_evidence, submitted_at}`; outcomes `completed | gate_failed | needs_input | incomplete` (`driver/result.go`). The driver archives it to `artifacts/steps/<step>/s<spawn_n>/result.json`.
- Validation order (`ValidateStepResult`): shape; identity + spawn_n fence; needs_input question shape; gate verdict binding (completed with `verdict: fail` → `gate_failed`); `requires_human_answer`; release/deploy verdict binding; command evidence (argv non-empty, `duration_ms>0`); committed test files; output schema (record-only); authored tests unchanged. A rejection is retried in-process with the reason; the 4th consecutive rejection (`maxEnvelopeRejects`=3 retries) blocks the step `result_invalid`. Details: `v2-converge.md#§2.6`.
- `gate_failed` → bounded rewind (`driver/transition.go` `applyGateFailLocked`): no `on_fail` → `failed_gate`; a bound hit → `failed_rewind_exhausted`; else `[target, gate)` archived as `gate_fail_rewind` and reset, the verdict text delivered to the target as `pending_rewind_input`, candidate pointers reset.
- `needs_input` → question stored in `execution.json`; a human answers with `pf answer` → daemon `/jobs/{wi}/answer` → driver bridge (`applyAnswerLocked`). Never answered by an agent (UDS step-origin refusal).
- `incomplete` → workflow `blocked`. Workflow statuses: `running completed failed_gate failed_rewind_exhausted blocked`; step statuses `pending in_progress done` (`common/capsule/capsule.go`). A terminal workflow closes the WI (completed → `wrapped`, failed → `failed`, blocked → hub `blocked`) after a final seal push; if that push fails the close degrades to blocked (`v2-converge.md#§D`).
- `execution.json` (`capsule.Execution`, the driver is its single writer): `capsule_id`, `work_item_id`, `current_step`, `workflow_status`, `steps` (per id: `status`, `model`, `effort`, `output`, `step_started_at`, `budget_consumed`, `attempts[]` with `cause` `accepted`/`gate_fail_rewind`/`infra_failure` and `failure_cause`, `pending_question`, `answers`, `answer_base`, `pending_rewind_input`, `supervision` counters and relaxed bounds, `next_dispatch`), `review_rounds`, `rewind_stats`, `budget_consumed`, `updated_at`, `version` (bumped on every write; the push loop keys on it), `blocked_reason` (set when the driver blocks on its own authority: host-class fault, failure bound, `result_invalid`).

### 7.4 Context between steps <!-- §7.4 -->

`inherit` steps get the body of the nearest earlier `handoff: true` step's `artifacts/handoff-<id>.md`
(producer must be done; a missing file only omits the section); `fresh` steps get none
(`driver/spawn.go`). The claim-time memory snapshot `runtime-state/recalled-memories.json` is rendered
into every step prompt as non-binding history (§9) — only when it has items; at most 400 runes per
excerpt and 6000 bytes per section (`driver/spawn.go`). Retry shape: first retry resumes the transcript,
later retries start fresh with the old transcript as read-only reference (`v2-converge.md#§2.3`).

### 7.5 Model candidates <!-- §7.5 -->

- Format `<harness>:<model>[:<effort>]` (`internal/common/harness/harness.go` `ParseCandidate`): harness ∈ `pi`, `claude`, split at the first `:`; the last segment is an effort only if it is one of `off minimal low medium high xhigh max` (Claude accepts `low`…`max`), otherwise it belongs to the model id. Effort → pi `--thinking`, claude `--effort`.
- Precedence, first **present** level wins whole (no merge; `workspace/compose.go`): (1) `pf submit --models a,b` (this WI, every step; daemon-local `manual-models.json`, dropped at a terminal close, not carried to another machine) > (2) `.polyforge.yaml` `steps.<project>.<step-id>.models` > (3) bundle step `model_candidates` > (4) `.polyforge.yaml` `steps.<project>.default.models` > (5) bundle `defaults.model_candidates`. Composed once per claim; snapshot in the job record.
- Advancement (`driver/candidates.go`): the pointer counts model-class failures since the last rewind (`model_error`, `model_hung`, `step_timeout_slow_model`, `exit_model_class`), capped at the last entry; host-class failures block; other causes retry the same model; `rate_limited` / `upstream_transient` retry the same model with backoff. Full cause table: `v2-converge.md#§2.3`.

### 7.6 Harness adapters <!-- §7.6 -->

| | pi (`runtime/spawn.go`) | Claude Code (`runtime/claudespawn.go`) |
|---|---|---|
| argv | `pi --mode rpc --no-skills --no-context-files --no-prompt-templates [--session <transcript>] [--model] [--thinking]` | `claude -p --bare --output-format stream-json --verbose --include-partial-messages --session-id <uuid>`/`--resume <uuid> [--model] [--effort] --tools/--allowedTools Bash,Read,Edit,Write,Glob,Grep,TodoWrite,Agent --permission-mode dontAsk --strict-mcp-config --disable-slash-commands …`; prompt on stdin |
| env | `PF_WI_ID`, `PF_DAEMON_ID`, `PI_CODING_AGENT_DIR=<harness-root>/step-agent`, `PI_CODING_AGENT_SESSION_DIR=<harness-root>/step-agent-sessions`; all other `PF_*` stripped | `CLAUDE_CONFIG_DIR=<harness-root>/claude-config`, `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, `DISABLE_AUTOUPDATER=1`, …; no credential injected; every inherited `ANTHROPIC_*`, `CLAUDECODE`, `CLAUDE_CODE_*`, `CLAUDE_CONFIG_DIR` stripped (the settings.json `env` block is the only credential/endpoint source) |
| transcript | live file located in the session store, copied verbatim to `session/session-<token>.jsonl` at commit boundaries (`runtime/pitranscript.go`) | `<claude-config>/projects/<dashed cwd>/<uuid>.jsonl`; resume only if it exists |

Provider errors (pi, `supervise/pi.go`): a `message_end`/`turn_end` whose message has `stopReason=="error"` is an error signal; an `agent_end` whose last assistant message ended in error and `willRetry=false` ends the turn as failed (rule ① / D7 classification in `v2-converge.md#§2.3`); `willRetry=true` means pi retries itself and the turn is not judged.

`<harness-root>` = `[daemon] harness_root`, default the data dir. The pi step-agent dir is a minimal
copy of the operator agent dir (`auth.json`, `models.json`, `agents/`, `extensions/subagent/`; never
`settings.json`), refreshed before every spawn, pruned at start; a configured operator dir without
`auth.json` refuses start (`runtime/stepagentdir.go`; rationale `v2-converge.md#§2.7`).

### 7.7 Supervision bounds <!-- §7.7 -->

Rules and causes: `v2-converge.md#§2.3`, bounds `v2-converge.md#§2.4` (code `internal/polyforge/supervise`,
`driver/supervision.go`). Defaults: X silence 10m token-level / 30m message-level; Y tool 30m; T step
90m cumulative (`[daemon] step_timeout`); N driver stall 10m (`[daemon] stall_window`); model-error
grace 1m; per-step failure bound 3 (`DefaultMaxStepFailures`). Timeouts relax (double) and persist in
`execution.json`. The 10s / 5-steps / 30-min / same-error-5 numbers are the **push** loop (§8), not step bounds.

## 8. Capsule and git chain <!-- §8 -->

- Layout: `v2-converge.md#§E` (E.5), constants `internal/common/capsule/capsule.go`; `pending-results/` and `sync/` are defined in `driver/spawn.go` and `runtime/materialize.go`. `.gitignore`: `/repos/`, `/artifacts/release-*/`, `/sync/`. The capsule's git-dir is external (`<data-dir>/git/<wi>.git`, a `gitdir:` pointer file in the capsule): it is outside the capsule tree, but a step's own `git commit` in the capsule root still reaches it — hence the trailer fold below.
- Chain rules (FF-only, `Polyforge-Seq` trailer from 1, blob ≤16 MiB, no gitlinks), push fencing, push retry schedule, re-claim = clone + task-repo restore, `DAEMON-516/518`: `v2-converge.md#§14.1`. Carrier refs `refs/pf-task/<name>`: `v2-converge.md#§14.2`. Uncommitted-work snapshots `refs/pf-wip/<name>`: `v2-converge.md#§14.3`.
- Local invariants (`internal/polyforge/gitops/gitops.go`): one per-WI git mutex shared by runtime and observe; seq derived inside it; `index.lock` older than 10m removed at acquisition; local gitlink precheck (`GIT-412`); commits a step made on the chain branch without a trailer are folded back into the next chain commit (`untrailered_commits_folded`); an amended already-pushed commit is non-fast-forward and needs a human.
- Push loop (`observe/observe.go`, `observe/monitor.go`): event-triggered on each `execution.json` commit; retry every 10s; `push_backpressure` after 5 consecutive failed pushes or 30 min without success; 5 identical errors stop the round until the next boundary; 60s per push.

## 9. Memory and recall <!-- §9 -->

Detail: `06-memory-search.md#§1`. Summary (`internal/aihub/memory`): the Markdown file
`<data-dir>/memory/<project>/<mem>/memory.md` is the authority; SQLite holds metadata + an FTS5 trigram
index (`memory_fts`). Write = temp file + fsync → one transaction (row + FTS) → rename. A startup
background reconcile (`RebuildIndex`) makes rows/index match files (file wins). Search needs `q`;
`match=all` (default) ANDs every token (≥3 runes as FTS phrases, shorter ones as substrings);
`match=any` ORs up to 32 filtered terms (words, plus 3-rune shingles of CJK runs) ranked by bm25 and
examines at most max(100, 10×limit) candidates (rules in `06-memory-search.md#§5.1`); filters, limit
and the in-SQL `project=` narrowing are the same in both modes (single entry `Store.SearchMode`). CLI (`cli/verbs_aihub.go`): `pf recall --project P <query> [--any]` (project must
be mapped; `--any` = `match=any`), `pf remember --project P --type T <content|-|@file>`, `pf memory show <id>`. At claim the
daemon writes `runtime-state/recalled-memories.json` (query = goal + the first 1500 runes of content, `match=any`, project-scoped, top 10,
1200-rune excerpts; empty list + notice on error or no hits; never fails the claim — `runtime/recall.go`,
`daemon/memface.go`). Deploy aihub before daemons: an older aihub ignores `match` and ANDs, which
leaves the snapshot empty. `pf backup` is a SQLite-only snapshot (§4.1).

## 10. Install, release, deploy <!-- §10 -->

The script headers are the contract; this section only points.

- **Work machine**: `tools/install.sh` (header) — `curl … | bash -s -- <api-key> [server-host]`; stages the SHA-verified `pf`, checks out `<data-dir>/release/<ver>`, then under `ensure.lock` (waits up to 120s, then proceeds without it) shuts the daemon down gracefully when a new binary was staged (refused while jobs run unless `PF_INSTALL_FORCE=1`), swaps `/usr/local/bin/pf` and the `release/current` symlink, keeps an existing cli.toml that has an `api_key` (else writes it), installs the skill, runs `pf daemon ensure` (skipped when the daemon already answers or there is no `[workspace]` mapping / workflows dir), verifies. Upgrade = re-run it. Skill install: Claude `~/.claude/skills/polyforge` → symlink into `release/current` + one SessionStart hook in `~/.claude/settings.json`; pi one local `packages` entry naming `release/current` (`package.json` declares the skill and `extensions/pf-chat.ts`). `pf doctor` checks both (`cli/doctor_layout.go`).
- **Release**: `.polyforge/release.sh` (header) — `[major|minor|patch]` or `--verify <tag>`; clean tree at `origin/main`, PR-only history since the last tag, `make test`, `make release`, docker image `ghcr.io/silgrid/aihub:<tag>`, tag push (the version claim), assets + `gh release create`. Run by the `workflows/release` bundle (`release → verify → deploy`, no rewind).
- **Deploy (aihub host only)**: `.polyforge/deploy.sh` (header) — `<version> --confirm <version>`, server-side lock, image via `docker save | ssh docker load`, online backups, seconds-long switch window applying `deploy/sql/<tag>/{upgrade,rollback,probe}.sql`, automatic rollback (exit 10). Schema history and operator steps: `docs/upgrade.md`.
- Workspace layout helper: `pf init [<dir>]` (`cli/verbs_init.go`) — `scratch/` and the managed CLAUDE.md section; mapping stays a cli.toml edit.

## 11. CLI verbs and the routing skill <!-- §11 -->

`internal/polyforge/cli/cli.go` `commands`: aihub-direct `file queue cancel recall remember memory`;
daemon `submit answer pause resume wrap ops chat watch`; admin `users api-keys machines projects backup`;
`doctor`, `init`, `daemon` (`daemon ensure`; `cmd/pf/main.go` routes `pf daemon …` to
`daemon.Main`). Exit codes: 0 ok, 1 client-class (4xx, validation, transport, a sub-action's flag error), 2
server-class (5xx, unknown verb, a global or verb flag parse error). The skill (`skills/polyforge/SKILL.md`) routes the 13 verbs + doctor/help/init,
one fragment per verb under `verbs/` (`ship.md` only says delivery is the workflow's `deliver` step);
`pf help` is the quick reference. Progress/notification design: `v2-converge.md#§3`.

## 12. History <!-- §12 -->

Archived (`archive/`, 2026-10-16): `00-architecture`, `02-aihub-core`, `03-execution-capsule`,
`04-local-runtime`, `05-sync-revision-recovery`, `07-deployment-pilot`, `v1-architecture-spec`,
`v1-spec-full`, `v1.5-session-model`, `v1.5-api-contract`, `v1.5-change-coverage-rules` (its rules now
live in README "Contributing & filing work items"). They describe the v0/v1 design (WI Agent, owner
generations, Revision/CAS sync, interactive/batch/drain modes, `pf upgrade`, …). Live mechanisms they
held were moved here or into `v2-converge.md` §14 / Appendix E. Lessons: `v0-lessons-ledger.md`.
