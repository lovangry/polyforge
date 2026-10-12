# Polyforge v1

Work-item orchestration platform: an aihub server (WI store, memory, git
revision chain) and a pf work-machine binary (Runtime + Observe + CLI).

## Install(team machines — read this first)

One line, any machine, any harness(or none). Ask the admin for a personal
API key(`pf_k1_…`), then:

```
curl -s -H "Authorization: token $(gh auth token)" \
  https://raw.githubusercontent.com/silgrid/polyforge/main/tools/install.sh \
  | bash -s -- <your-api-key>
```

(requires `gh` authenticated — the repo is private. Override the
server host with `PF_HOST=<host>` if it ever moves.)

It installs the `pf` binary(SHA-verified from the release server), writes
`~/.config/polyforge/cli.toml`, installs the `skills/polyforge` routing
skill(pi harness → pi package;other harnesses with a skills dir →
directory copy;otherwise CLI-only), and verifies connectivity. Execution
machines(ones that run a pf daemon)add the daemon per `tools/install.sh` — the
admin registers the machine identity first. Daily use from a session is
natural language via the `polyforge` routing skill;from a terminal it's
`pf queue`, `pf submit`, `pf answer`, `pf ops status`, …

## Quickstart(developers — from-zero smoke run)

A from-zero smoke run on one machine: start a local aihub, set up a
project and a machine identity, file a minimal work item, and drive it
through file → submit → observe → wrap on a local `pf daemon`.

Prerequisites: a Go toolchain; `git` with a configured
`user.name`/ `user.email` (capsule commits need it); and the `pi` agent
harness on `PATH` with a model configured — the daemon's execution driver
spawns the real step processes through it. All commands below run from the repo root; everything
local lands under `/tmp/pf-quickstart` (substitute your own paths).

**1. Build the two binaries:**

```
go build -o bin/aihub ./cmd/aihub
go build -o bin/pf ./cmd/pf
```

**2. Start aihub on a scratch data dir and port.** On first start over an
empty data dir, aihub seeds a bootstrap admin (`usr_root`) and prints a
one-time API key to its log — copy it now, it is never shown again:

```
mkdir -p /tmp/pf-quickstart/aihub-data
bin/aihub --data-dir /tmp/pf-quickstart/aihub-data --listen 127.0.0.1:18420 \
  > /tmp/pf-quickstart/aihub.log 2>&1 &
grep "one-time api key" /tmp/pf-quickstart/aihub.log
```

**3. Point `pf` at the server with a config file** (the flat `cli.toml`
subset plus the `[workspace]` mapping; every connection entry can also be
given per-command via the global `--server` / `--api-key` / `--socket`
flags). The mapping below is what makes the daemon startable — one
section per workspace, listing the projects whose capsules live there:

```
mkdir -p /tmp/pf-quickstart/workspace
cat > /tmp/pf-quickstart/cli.toml <<'EOF'
server = "http://127.0.0.1:18420"
api_key = "<BOOTSTRAP_KEY>"
daemon_socket = "/tmp/pf-quickstart/pf.sock"

[workspace."/tmp/pf-quickstart/workspace"]
projects = ["demo"]
EOF
```

**4. Create the project and a machine identity.** The machine user's API
key is the daemon's credential below; its plaintext is shown exactly
once:

```
bin/pf --config /tmp/pf-quickstart/cli.toml projects create --name demo
bin/pf --config /tmp/pf-quickstart/cli.toml users create --display-name smoke-machine --user-type machine
bin/pf --config /tmp/pf-quickstart/cli.toml api-keys create --user <MACHINE_USER_ID>
```

**5. File a minimal smoke work item.** `--bundle` makes it an execution
ticket — only execution tickets can be claimed; `quick-fix` is one of
the workflow bundles shipped in `workflows/`:

```
bin/pf --config /tmp/pf-quickstart/cli.toml file create --project demo \
  --bundle quick-fix \
  --goal "Quickstart smoke: exercise the file -> submit -> observe -> wrap chain." \
  --content "Minimal smoke work item; no product change is expected." \
  --label smoke
```

**6. Start the local daemon** (Runtime + Observe over its own UDS
socket; keep it running in a second terminal). The `[workspace]` mapping
in the config is the daemon's only workspace source (polyforge#113): a
config with zero sections refuses startup, every mapped workspace
directory must pre-exist, and the daemon creates nothing but
`capsules/` inside each. Capsules route by the claimed work item's
project through the mapping — the `demo` section above sends `demo`
capsules to `/tmp/pf-quickstart/workspace/capsules/`:

```
bin/pf daemon --config /tmp/pf-quickstart/cli.toml \
  --api-key <MACHINE_KEY> \
  --data-dir /tmp/pf-quickstart/daemon-data \
  --machine-id m-smoke
```

If a workspace lives inside a git checkout, ignore the capsule
directory in its `.gitignore` (`capsules/`) — the daemon never touches
`.gitignore` itself. `pf doctor` checks the mapping and the
connectivity faces.

**7. Submit the work item and watch the agent work:**

```
bin/pf --config /tmp/pf-quickstart/cli.toml submit <WI_ID>
bin/pf --config /tmp/pf-quickstart/cli.toml watch <WI_ID> --once
```

The submit claims the work item and the daemon's **Go execution driver**
takes over: an in-daemon engine that reads and writes
`runtime-state/execution.json` (its single state authority) and spawns one
**step process** per workflow step — real `pi` harness sessions — collecting
each step's result envelope, validating it, and advancing the workflow. A
plain submit is the ordinary claim. A WI whose holder daemon is dead or
stuck is recovered in two ordinary steps: an admin or project maintainer runs
`pf ops end-session <id> --reason <text>` (the session is ended, the WI is
`queued` again, and the old holder is fenced — its secret no longer matches, so
its pushes and verdicts are refused and its own session revalidation makes it
release), then any daemon claims it with a plain `pf submit <id>`; the claim
restores progress from the aihub capsule revision chain (and the task repos'
`refs/pf-task` carrier refs) and the driver continues from the recorded current
step. `pf watch <id> --once` shows a
snapshot read from the capsule's `execution.json`: status, current step and
how long it has run, the pending question and the latest events;
`pf watch --chat "$PF_CHAT_ID" --wait-decision` is the resident per-chat
watcher that wakes a harness session only for human decisions (see
`skills/polyforge/SKILL.md`). `pf watch <id> --once` is the one-shot snapshot (status, step, ages,
pending question, latest events). Give the first step a
few minutes: once the driver commits the first `runtime-state/execution.json`
version, the snapshot shows a current step and a `published` event.

**8. Wrap the run.** Wrap seals the capsule's current state and ends
the session as terminal success:

```
bin/pf --config /tmp/pf-quickstart/cli.toml wrap <WI_ID>
bin/pf --config /tmp/pf-quickstart/cli.toml queue --project demo
```

The queue now lists the work item as `wrapped`; its SESSION column is
`-` (unheld — while a session is live it shows the holder's public
fingerprint). Note: wrapping before the
capsule's first commit lands leaves no revision to seal — the close
degrades to `blocked`, and the WI is not claimable again until
`pf resume <WI_ID>` continues it, or `pf cancel <WI_ID>` abandons it (the task's
reporter, a project maintainer or an admin; the same two moves apply to a WI the
daemon parked `repair_pending` after a restart — see the failure codes below;
`pf wrap <WI_ID> --fail` records the owner's failed verdict instead).
`pf pause <WI_ID>` sets a clean_pause (a daemon restart will not
auto-resume it); `pf resume <WI_ID>` clears the flag and the holder
daemon re-attaches on its next start — to move it elsewhere (or immediately),
`pf ops end-session <id> --reason <text>` then `pf submit <id>`.

**9. Tear down:**

```
bin/pf --config /tmp/pf-quickstart/cli.toml ops shutdown
kill <AIHUB_PID>
```

## Architecture

Design record: `docs/design/polyforge-vnext/v1-architecture-spec.md`, as
superseded in part by `docs/design/polyforge-vnext/v2-converge.md` (the
design authority; conflicts resolve in its favor)
(full design record: `v1-spec-full.md`; the frozen v0 pilot tree was removed from the working tree and lives in git history / the `v0-archive` tag; restore with `git checkout v0-archive -- v0`).
The subsections below describe the code as it actually exists in this tree —
the live code is the authority; design-doc references are for traceability.

### Modules

One row per sub-package, exactly as `internal/` exists today:

| Package | Role |
| --- | --- |
| `internal/common/api` | Shared business contract between aihub and pf: work items, projects, users, generations, memories, and the typed error-code registry (`CORE-*`, `DAEMON-*`, `GIT-*`). |
| `internal/common/capsule` | Execution-capsule contract shared by pf (runtime creates capsules, observe commits them) and aihub (serves the matching bare repos; git-dir lives outside the capsule). |
| `internal/common/protocol` | The in-machine contracts: Runtime↔Observe messages and the pf CLI↔daemon UDS HTTP API surface. |
| `internal/common/workflow` | Workflow-bundle digest contract shared by pf (bundle materialization with digest verification) and aihub (serves bundle bytes, validates digests at claim time). |
| `internal/aihub/core` | aihub persistent layer: SQLite store (single writer, WAL), the work-item state machine, claim/generation arbitration with attempt-scoped push credentials, the ready queue and the admin face. |
| `internal/aihub/git` | aihub git surface: per-WI bare-repo lifecycle, the git-http-backend smart-protocol proxy, attempt-scoped credential auth (generation fencing) and the pre-receive hook enforcing the capsule-chain invariants. |
| `internal/aihub/memory` | Core-side memory store: FTS5-trigram full-text search, authoritative `memory.md` files and the reconcile procedure. |
| `internal/aihub/server` | The v1 AIHub HTTP face: business endpoints (v0 openapi surface minus every SYNC-*/revision-chain route), the admin face, and the `/git/{wi}.git/...` proxy mount; Bearer API key auth, typed `api.Error` envelope. |
| `internal/polyforge/aihubclient` | Shared HTTP client for the aihub server, used by runtime, observe and the CLI verbs; decodes the typed error envelope back into `*api.Error`. |
| `internal/polyforge/cli` | The `pf` client CLI: the routing verbs (file, queue, cancel, recall, remember, submit, `watch`, answer, pause, resume, wrap, ops), `chat`, `ship`, the M1–M5 management face, the daemon client over the UDS socket, and config resolution. |
| `internal/polyforge/daemon` | Phase-3 assembly of the local work machine: the UDS HTTP face, Runtime + Observe wired over the five protocol messages, the auto switch, the pending-job dispatcher, the auto-pull loop, the ship delivery bridge and the graceful shutdown sequence. |
| `internal/polyforge/gitops` | Shared capsule-git operations layer used by Runtime (clone at start, dirty-commit at recover) and Observe (async commit+push); owns the per-WI git mutex and the `Polyforge-Seq` trailer derivation. |
| `internal/polyforge/observe` | Observe lane: the events log, per-WI monitor goroutines, live-transcript streaming, and the observe/answer HTTP face over the monitors. |
| `internal/polyforge/driver` | The Go execution driver: owns `runtime-state/execution.json`, spawns one step process per workflow step and validates each result envelope (needs_input rules, human gate, V4 command-evidence gate). |
| `internal/polyforge/watcher` | The `pf watch` engine: decision classification (fixed-template wake-up lines) and the resident per-chat watcher with its progress file. |
| `internal/polyforge/runtime` | Runtime lane: the WI lifecycle manager (Start/Stop/Recover/Restart), the pi-harness agent spawner, the six-layer process cleanup and the recovery engine. |

Binaries: `cmd/aihub` (server binary) and `cmd/pf` (work-machine binary:
CLI verbs + `pf daemon`).

### Daemon UDS routes

The routes mounted by the daemon's local Unix-socket HTTP face
(`internal/polyforge/daemon/routes.go`). **Face** marks who calls the route:
`user` = the operator's CLI / observe tooling. (The in-capsule agent's
lifeline proxies and the flush lane were retired with the WI-Agent lane,
polyforge#106; no step process reaches the daemon socket by design.)

| Method | Path | Role | Face |
| --- | --- | --- | --- |
| GET | `/jobs/{wi}/observe` | Live observe view of one job's transcript stream (mounted verbatim from the Observe handler). | user |
| POST | `/jobs/{wi}/answer` | Answer a pending agent question. | user |
| POST | `/jobs/{wi}/claim` | Submit family: enqueue a claim of a queued work item. | user |
| POST | `/jobs/{wi}/resume` | Submit family: enqueue a resume of a paused item; for a WI parked `repair_pending` it re-checks the capsule/mapping and resumes in place with the kept session (still broken → `DAEMON-517` / `DAEMON-412`). | user |
| POST | `/jobs/{wi}/resume-blocked` | The claim/start half of `pf resume` on a blocked WI (the CLI lands aihub's resume-blocked door with the human's own key first). | user |
| POST | `/jobs/{wi}/abandon-repair` | Give up a `repair_pending` WI (`pf cancel` → verdict `canceled`, `pf wrap --fail` → `failed`): ends the kept session with the stored secret, drops the parked record, writes `repair_abandoned`. The CLI passes aihub's `close-auth` probe with the human's key first. | user |
| POST | `/jobs/{wi}/pause` | CLI close path: seal the job with reason `paused`. | user |
| POST | `/jobs/{wi}/wrap` | CLI close path: seal the job with reason `wrap` or `fail`. | user |
| POST | `/jobs/{wi}/ship` | Delivery bridge: push the sealed capsule's task repos to their origins as branch `polyforge/<wi>` (+ PR when a credential exists). | user |
| GET | `/status` | Machine status: jobs (with the needs-input projection), queue stats, auto switch. | user |
| POST | `/ops/config` | `pf ops auto on\|off` / `pf ops max N`: body `{"auto":bool,"max":N}` (either or both). | user |
| POST | `/shutdown` | Trigger the bounded graceful shutdown sequence. | user |
| POST | `/chat/init` | `pf chat init`: the chat id of a harness session (created on first use). | user |
| GET | `/jobs/{wi}/watch` | `pf watch <wi> --once`: per-WI snapshot (status, step, pending question, events). | user |
| GET | `/chats/{id}/subscriptions` | A chat's daemon-held subscriptions and its progress-file path. | user |
| POST | `/chats/{id}/subscribe`, `/unsubscribe` | Subscribe / drop a work item for a chat. | user |
| POST | `/chats/{id}/ack` | Acknowledge consumed events / delivered decisions (advances the cursor). | user |
| POST | `/chats/{id}/heartbeat` | Resident-watcher liveness for a chat. | user |
| GET | `/chats/{id}/attention` | Pending decisions for a chat (and for work items nobody watches). | user |

Authoritative source for this table is the code itself — the
`mux.Handle`/`mux.HandleFunc` mount list in `internal/polyforge/daemon/routes.go`;
design-doc references (e.g. `04-local-runtime.md §5.1`) are for traceability only.

### Failure codes of the recovery / re-claim paths

| Code | Meaning | Operator move |
| --- | --- | --- |
| `DAEMON-516` | Re-claim clone refusal: the capsule clone failed for a WI with published progress (or its progress cannot be determined), so a local init — which would restart the workflow at step 1 — is refused. The claim is abandoned back to blocked. | Fix aihub reachability / credentials, then `pf resume <id>`. |
| `DAEMON-517` | The WI is parked `repair_pending` on this daemon (restart found its capsule missing or its project unmapped); the detail names the path. The session is kept; nothing is rebuilt from the remote. | Restore the capsule directory (or re-add the project to `[workspace]`) and `pf resume <id>`; or give up with `pf cancel <id>` / `pf wrap --fail <id>`. |
| `DAEMON-518` | A blocked WI continued on a different daemon than the one that sealed it, and the aihub capsule head is not the sealed head. | Continue it on the sealing machine, or abandon with `pf cancel <id>`. |

A missing capsule is no longer a startup failure: only the affected WI parks
`repair_pending` (listed by `pf ops status`, announced by a `capsule_missing` /
`project_unmapped` event); the daemon starts and every other WI recovers.

### Release flow

The platform ships with zero release-specific code: release logic lives in
the project bundle's release step (`steps/release.md`), and the platform's
own release is one instance of that same hook. Deploying to a work machine is
re-running `tools/install.sh`; it is a deployment pipeline, not a release
system (`07-deployment-pilot.md §1.1/§6.3`, `00-architecture.md §3.3`).

```
 RELEASE: bundle release-step hook (no platform release code)
 +-----------------------------------------------------------+
 | project bundle release step (`steps/release.md`);       |
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
 DEPLOYMENT: re-run `tools/install.sh` (pipeline, not a release system)
 +-----------------------------------------------------------+
 |  download -> sha256 verify -> stop daemon -> replace -> restart  |
 +-----------------------------------------------------------+
```

## Build & test

    make check     # build + vet + test, all packages
    go build ./...
    go test ./...

## `pf` commands

`pf [--json] [--config PATH] [--server URL] [--api-key KEY] [--socket PATH]
[--user ID] <verb> ...` (global flags before the verb; env `PF_SERVER`,
`PF_API_KEY`, `PF_DAEMON_SOCKET`, `PF_USER_ID`; `PF_CHAT_ID` is the
notification label a harness adapter sets for the chat). `pf help` prints the
registry; `pf <verb> -help` one verb's face. The routing skill's fragments
(`skills/polyforge/verbs/`) carry each verb's success and failure shapes.

| Face | Verbs |
| --- | --- |
| aihub (data plane, direct) | `file`, `queue`, `cancel`, `recall`, `remember` |
| daemon (UDS) | `submit`, `watch` (snapshot, resident watcher, `subscribe`/`unsubscribe`/`list`; the old `pf observe` verb is removed), `answer`, `pause`, `resume`, `wrap`, `ship`, `ops` (`status`, `auto`, `max`, `shutdown`, and `end-session` — aihub-direct, admin/maintainer), `chat init` |
| management | `users`, `api-keys`, `projects`, `machines`, `backup`, `rotate-secret` |
| local | `daemon` (run the work machine), `doctor` |

Human moves: continue = `pf resume <id>` (a paused, blocked or repair_pending
WI), abandon = `pf cancel <id>` (queued, paused, blocked, or repair_pending on
this daemon), owner's failed verdict = `pf wrap <id> --fail`. These run with the
human's own api key: aihub's door checks the WI's reporter, a project
maintainer or an admin. There is no `unblock` verb.

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
| `--machine-id ID` | `POLYFORGE_MACHINE_ID` | `m-<hostname>` | Machine identity claims and leases are recorded under; recorded in `<data-dir>/daemon-id` on first start, after which that file wins (a differing value refuses to start). |
| `--display-name NAME` | `POLYFORGE_DISPLAY_NAME` | hostname | Daemon display name shown by `pf ops status`. |
| `--data-dir DIR` | `POLYFORGE_DATA_DIR` | `~/.local/share/polyforge` | Daemon state, events log and derived data. |
| `--workflows-dir DIR` | — | `./workflows` | Workflow-bundle checkout the executor reads. |
| `--pi-bin PATH` | — | `pi` | Harness binary the agent processes are derived from. |
| `--harness-sessions-root DIR` | `POLYFORGE_HARNESS_SESSIONS_ROOT` | `~/.pi/agent/sessions` | Harness session store for the live-transcript sync. |
| `--cgroup-root DIR` | `POLYFORGE_CGROUP_ROOT` | — | cgroup v2 root used for layer-0 agent cleanup. |
| `--git-remote-base URL` | — | `<server>/git` | aihub bare-repo base URL for the capsule revision chain. |
| `--max-concurrent-jobs N` | — | `4` | Cap on concurrently running jobs (also `pf ops max N`). |
| `--recall-topk N` | `POLYFORGE_RECALL_TOPK` | `10` (max 50) | Claim-time memory snapshot hit count. |
| `--auto` | — | off | Auto-pull: the daemon also claims ready-queue work itself; an explicit `--auto` / `--auto=false` overrides the persisted switch (also `pf ops auto on\|off`). |
| `--heartbeat-every D` | — | `60s` | Machines heartbeat cadence (`0` disables). |

The workspace mapping (polyforge#113) replaces the retired workspace and
project launch surfaces: the `[workspace."<path>"]` sections of the
`--config` file are the ONLY workspace source. One section per
workspace, `projects = ["..."]` under each; `~` at the start of a key
expands to the home directory, keys must be absolute with no trailing
separator. Every mapped workspace directory must pre-exist; the daemon
creates and writes only `capsules/` inside each and refuses to start
when any is missing or unusable (or when the config carries zero
sections). Capsules route per claimed project through the inverse
mapping; the auto-pull covers all mapped projects. `pf doctor` checks
the mapping.

Supervision bounds (v2-converge 2.4) — Go duration strings (e.g. `700ms`,
`5m`), flags only (no environment fallbacks). Omitting a flag, or passing
`0`, keeps the default listed here. A workflow step overrides them in
`workflow.md`: `budget.wall_minutes` (an estimate: T becomes
`max(3 x wall_minutes, --step-timeout)`), `silence_minutes` (X),
`tool_minutes` (Y).

| Flag | Env | Default | Controls |
| --- | --- | --- | --- |
| `--silence-window D` | — | `10m` (token-level harness) / `30m` (message-level) | X: no real work signal after dispatch (`harness_silent`), or no model output while waiting for the model (`model_hung`). UI chatter never counts as work. |
| `--tool-window D` | — | `30m` | Y: one tool running longer than this is `tool_hung`. |
| `--step-timeout D` | — | `90m` | T: cumulative step runtime across all its dispatches (`step_timeout_long_task` / `step_timeout_slow_model`). |
| `--stall-window D` | — | `10m` | N: the driver between steps (not waiting on a human) for longer than this is `driver_stalled`; a negative value disables it. |
| `--model-error-grace D` | — | `1m` | Grace after a turn ENDS with no result file: ended on a model error is `model_error` (switch candidate); ended normally is `ended_without_result` (same-model retry). |
| `--revalidate-every D` | — | `5m` | M4 periodic session-revalidation cadence for RUNNING jobs; `0`/omitted keeps the 5m default, a negative value disables revalidation. |

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

`<query>` is required (positional or `--q`); so is `--project` — there
is no default project anymore (polyforge#113), and the value is
validated against the `[workspace]` mapping (an unmapped project fails
loudly). Flags:

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

## Contributing & filing work items

Change coverage is tiered by the change's **behavior face**, not by a
"e2e on every ticket" blanket rule (owner ruling 2026-10-02). The existing
40-scenario e2e suite (`go test ./test/`) is the regression net for every
change — each ticket's verification step runs it — and the three disciplines
below govern when **new** coverage must be added. The authoritative source is
`docs/design/polyforge-vnext/v1.5-change-coverage-rules.md` (Chinese); this
section is a faithful summary, and that doc remains the single authority.

**Discipline ① — changes observable behavior → file on an e2e-carrying
graph.** If a change alters observable behavior — a new endpoint, a
protocol/contract/semantics change, an ownership or fencing rule change, a
new external capability — the ticket must be filed with `bundle_name` set to
a graph that carries e2e-author / e2e-run steps (currently `feature-full`;
giving another graph those steps is zero code). Pure refactors, pure docs,
and obviously non-behavior chores keep the existing graphs (the existing
suite + verification is enough). Quick test: ask "after this merges, what
difference would users/callers perceive that the existing 40 scenarios can't
detect?" — if you can answer, new scenarios are required. Edge case: a small
ticket discovered mid-flight to touch the behavior face (at the grill/spec
stage) declares it in needs_input, and the owner decides between re-filing
on the e2e graph and human-authored scenarios.

**Discipline ② — bug fixes → the repro must land as a regression test.** The
bug-fix graph's `fix` step carries a **mandatory** `regression_test` field:
the repro must be pinned as a test that fails before the fix and passes
after, lands in-tree, is run by verification, and is watched by review.
Waivers must be explicit (`waived: <reason>`) and are review-visible and
challengeable. A bug fix without a test pinning it is an unfixed bug — the
next regression of the same bug is left to the naked eye.

**Discipline ③ — workflow-bundle / prompt-logic changes → real-WI observation
window before the next cutover-grade release.** Any merge touching
`workflows/*/steps/*.md` or
`workflows/*/workflow.md` requires a real-WI observation window and gate
readings before the next cutover-grade release: a clean rig, ≥10 real
tickets of mixed bundle types (including ≥1 exercising the changed
surface), owner-answered needs_input, and the readings (V4 evidence gate, V8
sync chain, recovery/blocked counts, orphan counts) compared against the
previous window — no regression passes, regression freezes merges until
fixed. The window ticket is registered at merge; it blocks cutover, not
merge. The report lands in `docs/pilot/` and readings never rewrite
history.

**Merge-review checklist** (compact form of the authority doc's 检查单):

- [ ] Behavior face touched? → filed on an e2e-carrying graph, or new
  scenarios human-authored
- [ ] Bug fix merged? → `regression_test` field non-empty, or a waiver
  reason accepted by review
- [ ] Skill face triggered? → observation-window ticket registered (blocks
  cutover-grade release, not merge)
