# Polyforge

Work-item orchestration platform: an aihub server (WI store, memory, git
revision chain) and a pf work-machine binary (CLI + daemon with the Go
execution driver).

## Install (team machines — read this first)

One line, any machine, any harness (or none). Ask the admin for a personal
API key (`pf_k1_…`), then:

```
curl -s -H "Authorization: token $(gh auth token)" \
  https://raw.githubusercontent.com/silgrid/polyforge/main/tools/install.sh \
  | bash -s -- <your-api-key>
```

(requires `gh` authenticated — the repo is private. Override the
server host with `PF_HOST=<host>` if it ever moves.)

It installs the `pf` binary (downloaded from the GitHub Release with `gh release download` and checked against its `SHA256SUMS`), writes
`~/.config/polyforge/cli.toml` (never rewritten), and keeps its own checkout
of the release tag in `~/.local/share/polyforge/release/<tag>` (stable
symlink `release/current`, previous version kept). From that checkout it
wires the `skills/polyforge` routing skill into each harness it finds,
independently, by REFERENCE (nothing copied, nothing cloned twice):
Claude Code (`~/.claude` or `claude` on PATH) gets `~/.claude/skills/polyforge`
as a symlink to `release/current/skills/polyforge` plus a SessionStart hook
registered once in `~/.claude/settings.json`; pi (on PATH, or its agent dir
present) gets one local package entry naming `release/current` in its
settings (replacing an older `git:...silgrid/polyforge.git@<tag>` entry).
It then reports
whether each harness will display polyforge progress (the `pf doctor
--harness` check: Claude Code's statusLine running `pf watch --statusline`,
pi's pf-chat extension) and prints a copyable snippet when not — it never
edits your statusLine — plus the install-layout and v1-leftover checks.
Then it starts the daemon through `pf daemon ensure` (once a `[workspace]`
mapping exists): a restart loop in tmux session `polyforge-daemon` runs it
with `-workflows-dir release/current/workflows` and revives it within ~5s
after a crash; after a container/machine restart (tmux does not survive
one) the next pi or Claude Code session runs the same ensure. An upgrade
shuts the daemon down gracefully (the loop ends with that clean exit —
`pf ops shutdown` always stops for good, only failures are restarted),
swaps the binary, then ensures again; meanwhile it holds the ensure lock,
so a harness session starting mid-upgrade starts nothing. A daemon already running with a different workflows
dir, or without its loop, is reported, never auto-restarted. Finally it
verifies connectivity. If the machine's daemon id is new to aihub, an admin
registers it afterwards — the installer prints the id (`pf ops status` shows
it) when the daemon does not answer or a heartbeat fails. Re-running the same command upgrades
and is idempotent. Daily use from a session is natural language via the
`polyforge` routing skill; from a terminal it's
`pf queue`, `pf submit`, `pf answer`, `pf ops status`, …

## Quickstart (developers — from-zero smoke run)

A from-zero smoke run on one machine: start a local aihub, set up a
project and a machine identity, file a minimal work item, and drive it
through file → submit → watch → wrap on a local `pf daemon`.

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
  --goal "Quickstart smoke: exercise the file -> submit -> watch -> wrap chain." \
  --content "Minimal smoke work item; no product change is expected." \
  --label smoke
```

**6. Start the local daemon** (runtime, driver and watcher behind its own UDS
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

**7. Submit the work item and watch it run:**

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
`skills/polyforge/SKILL.md`). Give the first step a
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
daemon set `blocked` at restart for a missing capsule / unmapped project — see
below; `pf wrap` / `pf wrap --fail` do not apply to a blocked WI — there is no
completed run to wrap or fail, and the daemon refuses with DAEMON-409).
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

Design authority: `docs/design/polyforge-vnext/v2-converge.md` (with
`wi-kind-contract.md` and `08-capsule-base-migration.md`). The numbered
docs 00–07 and the v1 specs are largely archived history — sections that
describe deleted mechanisms carry an ARCHIVED banner; do not build from them.
The subsections below describe the code as it actually exists in this tree —
the live code is the authority; design-doc references are for traceability.

### Modules

One row per sub-package, exactly as `internal/` exists today:

| Package | Role |
| --- | --- |
| `internal/common/api` | Shared business contract between aihub and pf: work items, projects, users, sessions, memories, and the typed error-code registry (`CORE-*`, `MEM-*`, `AUTH-*`, `DAEMON-*`, `GIT-*`). |
| `internal/common/atomicfile` | The one crash-safe "replace this file" primitive every state writer uses. |
| `internal/common/capsule` | Execution-capsule contract shared by pf (runtime creates capsules, the driver writes `execution.json`, the observe lane commits them) and aihub (serves the matching bare repos; git-dir lives outside the capsule). |
| `internal/common/protocol` | The in-machine contracts: Runtime↔Observe messages and the pf CLI↔daemon UDS HTTP API surface. |
| `internal/common/harness` | The closed step-harness set (`pi` \| `claude`) and the one model-candidate format `<harness>:<model>`. |
| `internal/common/spawn` | Process-derivation contract shared by the runtime (daemon assembly) and the driver (step-process spawning). |
| `internal/common/workflow` | The minimal workflow-bundle frontmatter read surface (defaults + per-step `model_candidates`) that drives model and harness selection. No digest contract: the bundle digest chain was deleted (polyforge#149). |
| `internal/aihub/core` | aihub persistent layer: SQLite store (single writer, WAL), the work-item state machine, claim/session arbitration (one live session per WI, its hashed push secret and public fingerprint), the ready queue and the admin face. |
| `internal/aihub/git` | aihub git surface: per-WI bare-repo lifecycle, the git-http-backend smart-protocol proxy, session-secret auth (only the WI's live session secret pushes; session-fingerprint fencing) and the pre-receive hook enforcing the capsule-chain invariants. |
| `internal/aihub/memory` | Core-side memory store: FTS5-trigram full-text search, authoritative `memory.md` files and the reconcile procedure. |
| `internal/aihub/server` | The AIHub HTTP face: business endpoints, the admin face, and the `/git/{wi}.git/...` proxy mount; Bearer API key auth, typed `api.Error` envelope. |
| `internal/polyforge/aihubclient` | Shared HTTP client for the aihub server, used by runtime, observe and the CLI verbs; decodes the typed error envelope back into `*api.Error`. |
| `internal/polyforge/cli` | The `pf` client CLI: the routing verbs (file, queue, cancel, recall, remember, memory, submit, `watch`, answer, pause, resume, wrap, ops), `chat`, the management face (users, api-keys, projects, machines, backup), the daemon client over the UDS socket, and config resolution. |
| `internal/polyforge/daemon` | Assembly of the local work machine: the UDS HTTP face, runtime, observe lane and driver wired together, the auto switch, the pending-job dispatcher, the auto-pull loop and the graceful shutdown sequence. |
| `internal/polyforge/gitops` | Shared capsule-git operations layer used by Runtime (clone at start, dirty-commit at recover) and Observe (async commit+push); owns the per-WI git mutex and the `Polyforge-Seq` trailer derivation. |
| `internal/polyforge/bridge` | Connects the runtime's job machinery to the Go driver: a running `Driver.Run` is the job's agent handle for supervision. |
| `internal/polyforge/mapping` | The workspace→projects plan read from cli.toml's `[workspace]` sections, and its validated inverse (project → workspace). |
| `internal/polyforge/sessionjsonl` | The single parser of the harness session-jsonl format (live transcript tail extraction). |
| `internal/polyforge/supervisor` | The daemon's own supervision on a work machine: the `polyforge-daemon` tmux restart loop shared by `pf daemon ensure` and `pf doctor`. |
| `internal/polyforge/supervise` | The driver↔supervisor contract: facts, verdicts and the per-WI link (v2-converge §2.2–2.5). |
| `internal/polyforge/workspace` | The `.polyforge.yaml` loader (`steps.<project>.<default\|step-id>.models`), read and validated by the daemon before every claim and by `pf doctor` (an invalid file refuses the claim without claiming). |
| `internal/polyforge/observe` | Observe lane: the events log, per-WI monitor goroutines, live-transcript streaming, session verification, and the live activity / pending-question / progress views the daemon reads. |
| `internal/polyforge/driver` | The Go execution driver: owns `runtime-state/execution.json`, spawns one step process per workflow step and validates each result envelope (needs_input rules, human gate, command-evidence gate). |
| `internal/polyforge/watcher` | The `pf watch` engine: decision classification (fixed-template wake-up lines) and the resident per-chat watcher with its progress file. |
| `internal/polyforge/runtime` | Runtime lane: the WI lifecycle manager (Start/Stop/Recover/Restart), the pi-harness agent spawner, the layered (L1–L5) process cleanup and the recovery engine. |

Binaries: `cmd/aihub` (server binary) and `cmd/pf` (work-machine binary:
CLI verbs + `pf daemon`).

### Daemon UDS routes

The routes mounted by the daemon's local Unix-socket HTTP face
(`internal/polyforge/daemon/routes.go`). **Face** marks who calls the route:
`user` = the operator's CLI / watch tooling. Every mutating (non-GET) route
refuses a caller that belongs to a step agent's process tree, or whose UDS peer
credential is unavailable, with `DAEMON-403` (polyforge#122 accident guard).

| Method | Path | Role | Face |
| --- | --- | --- | --- |
| POST | `/jobs/{wi}/answer` | Answer a pending agent question. | user |
| POST | `/jobs/{wi}/claim` | Submit family: enqueue a claim of a queued work item (optional body `models`: the manual candidate list of `pf submit --models`). | user |
| POST | `/jobs/{wi}/resume-blocked` | The claim/start half of `pf resume` on a blocked WI (the CLI lands aihub's resume-blocked door with the human's own key first). | user |
| POST | `/jobs/{wi}/resume-paused` | The live-holder half of `pf resume` on a paused WI (the CLI clears `clean_pause` at aihub first; this makes the online holder re-attach). | user |
| POST | `/jobs/{wi}/pause` | CLI close path: seal the job with reason `paused`. | user |
| POST | `/jobs/{wi}/wrap` | CLI close path: seal the job with reason `wrap` or `fail`. | user |
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
| `DAEMON-518` | A blocked WI continued on a different daemon than the one that sealed it, and the aihub capsule head is not the sealed head. | Continue it on the sealing machine, or abandon with `pf cancel <id>`. |

A missing capsule is no longer a startup failure: only the affected WI is set to
the ordinary `blocked` status at restart (the reason note names the cause —
"capsule missing at <path>" or "project <p> is not in the daemon's [workspace]
mapping"; a `capsule_missing` / `project_unmapped` event shows in `pf watch`);
nothing is parked locally, the daemon starts and every other WI recovers. Restore
the mapping if that was the cause, then `pf resume <id>`: a fresh re-claim from
the aihub capsule chain plus the task-repo carrier refs (`refs/pf-task/<name>`);
the dead scene is quarantined, completed steps are not re-run, and work published
after the last push is lost. A blocked WI has exactly two exits: `pf resume <id>`
(continue) or `pf cancel <id>` (abandon).

### Release flow

The platform ships with zero release-specific code in its engine: a release is a
work item filed against the project-agnostic `release` workflow bundle
(`workflows/release/`), and the delivery lanes (`feature-full`, `bug-fix`,
`quick-fix`, `chore`) never release or deploy. Trigger it by filing a release WI
(`pf file --bundle release ...`, or ask the routing skill: "release" / "deploy" /
"发版" files that WI). If the goal names a bump (`major`, `minor`, `patch`) it is
passed verbatim to the repo's procedure; otherwise the repo default applies.

The bundle has three steps, each `gate: true`, fresh context, with command
evidence required; a failure terminates the WI (published tags, releases and
images are immutable, so there is no rewind into a re-release):

1. `release` - fetches and resets the capsule clone to origin's default-branch
   head, records the pre-release remote tag list, checks the procedure supports
   `--verify`, then runs `.polyforge/release.sh [bump]` (which runs the repo's
   test gate itself). No procedure in the capsule is a FAIL; two repos carrying
   one is `PF-RELEASE-AMBIGUOUS_OWNER`.
2. `verify` - a fresh judge that trusts nothing from step 1: release commit vs
   origin head, the new tag exists and points at it, every older tag is
   unchanged, release notes are non-empty, then `.polyforge/release.sh --verify
   <version>`. Only a pass writes the verified version that `deploy` reads.
3. `deploy` - runs `.polyforge/deploy.sh <version> --confirm <version>` for the
   verified version only (never "latest"). A project with no `deploy.sh`
   defers (it releases without deploying). The procedure ends with production
   on the new version or rolled back; a rollback is a step FAIL.

A failed `deploy` ends the WI failed with production on the old version (rolled
back, or refused before touching it). Redeploy the already-verified version by
running `.polyforge/deploy.sh <version> --confirm <version>` directly (a human or
an agent); never by filing a new release WI, which would cut a duplicate version
of the same commit (this repo's `release.sh` refuses that).

Per-repo procedure contract (the repo owns the procedure; the bundle only
dispatches):

- `.polyforge/release.sh [bump]` releases origin's default-branch head, exits
  non-zero on any failure, and prints `version=<tag>`, `commit=<sha>`,
  `notes=<location>` lines at the end.
- `.polyforge/release.sh --verify <version>` is a read-only audit of a published
  release (format, ordering, artifacts, integrity); exit 0 only if all pass.
- `.polyforge/deploy.sh <version> --confirm <version>` deploys exactly that
  version; production ends on it or rolled back, rollback exits non-zero.

This repository's own procedures: `release.sh` (vX.Y.Z, bump `major|minor|patch`,
default `minor`) refuses a dirty tree or a HEAD that is not the fetched
`origin/main` or a HEAD that already carries a release tag on origin, refuses unless
every commit that entered `main` since the previous release tag came through a merged
PR (main's first-parent history must be merge commits that `gh api
repos/<repo>/commits/<sha>/pulls` names as a merged PR's merge commit — the shape
the workflows' `deliver` step produces with `gh pr merge --merge`), and takes the
version floor from the highest semver among the ORIGIN tags only (an unreadable
`ls-remote` is fatal) — these cheap refusals run first; only then does it run
`make test` (including the e2e suite, ~20 minutes), build the two
binaries and the `ghcr.io/silgrid/aihub:<tag>` image, claims the version by
pushing the git tag (fails if it exists: nothing is published), pushes the
image, writes `aihub-image.digest` after the push, then `SHA256SUMS` and
`gh release create` with the four assets (`aihub-linux-amd64`, `pf-linux-amd64`,
`aihub-image.digest`, `SHA256SUMS`). `aihub-image.digest` holds the bare image digest
`sha256:<64 hex>`; `deploy.sh` looks for `ghcr.io/silgrid/aihub@<that digest>` among
the pulled image's repo digests. `release.sh --verify <version>` also requires the
version to be strictly greater than every other tag on origin and runs the same
PR-only check over previous-tag..version. It never deletes or
overwrites a tag, release or image; a failure after the tag push burns the version
(cut the next one).

`deploy.sh` deploys the aihub service host: it requires `--confirm <version>`
equal to the version argument (no prompt), takes a server-side lock for the
whole mutating part (fails fast if another deploy holds it), runs on a machine
with `gh` and `docker` logged in to the private `ghcr.io/silgrid/aihub` - the
server never holds GitHub credentials. Steps: ONLINE verified full backup; pull
`ghcr.io/silgrid/aihub:<tag>`, check the repo digest against the release's
`aihub-image.digest` (itself checked via `SHA256SUMS`), `docker save | ssh
docker load`, verify the image ID on the server and tag it
`polyforge/aihub:<tag>`; edit only the aihub service of the server's compose
file; then the switch window — the old container stops (graceful drain), the
versions' upgrade SQL under `deploy/sql/` runs (one transaction per script;
there is no startup auto-migration, see [docs/upgrade.md](docs/upgrade.md)),
and just that container is recreated (seconds of downtime, measured and
printed); post-swap checks. If post-swap verification fails or the service does
not come back, it AUTOMATICALLY rolls the compose image back to
`polyforge/aihub:<old>`, recreates, verifies the old version answers and prints
`ROLLED BACK` (the applied versions' `rollback.sql` runs first, in descending
order, so the old binary never sees the new schema; distinct non-zero exit; if the rollback itself fails it prints
loud manual instructions). Data restore stays manual (it needs a stop).
`--dry-run` shows the plan and the compose diff; `--force` is a human-only
override. Deploying a work machine is re-running `tools/install.sh`. It is a
deployment pipeline, not a release system (`07-deployment-pilot.md §1.1/§6.3`,
`00-architecture.md §3.3`).

```
 RELEASE: a WI on the `release` bundle (no platform release code)
 +-----------------------------------------------------------+
 | release (.polyforge/release.sh) -> verify (fresh judge,   |
 | release.sh --verify) -> deploy (.polyforge/deploy.sh)     |
 +----------------------------+----------------------------+
                              |
                              v
               mono-repo git tag (single semver per tag)
                              |
                              v
        GitHub release: aihub-linux-amd64, pf-linux-amd64,
        aihub-image.digest, SHA256SUMS  +  ghcr.io image <tag>
                              |
                              v
 DEPLOYMENT: `deploy.sh` for the aihub host; re-run `tools/install.sh` on work machines
 +-----------------------------------------------------------+
 |  fetch+verify -> refuse if jobs run -> stop -> swap -> ensure    |
 +-----------------------------------------------------------+
```

## Build & test

    make check     # build + vet + test, all packages
    go build ./...
    go test ./...

Opt-in tests (skipped with a clear message unless their variable is set):
`POLYFORGE_TEST_SYSTEM_INSTALL=1` runs
the installer scenarios, which write `/usr/local/bin/pf` (disposable hosts only).

## `pf` commands

`pf [--json] [--config PATH] [--server URL] [--api-key KEY] [--socket PATH]
[--user ID] <verb> ...` (or `pf --version`) (global flags before the verb; env `PF_SERVER`,
`PF_API_KEY`, `PF_DAEMON_SOCKET`, `PF_USER_ID`; `PF_CHAT_ID` is the
notification label a harness adapter sets for the chat). `pf help` prints the
registry; `pf help <verb>` one verb's usage line. Exit codes: 0 success; 1
client-class (4xx, transport, local validation, and a sub-action's
flag/argument mistake); 2 server-class (5xx), an unknown verb, or a flag
error caught by the global / verb-level flag parse. The routing skill's fragments
(`skills/polyforge/verbs/`) carry each verb's success and failure shapes.

| `pf help` group | Verbs |
| --- | --- |
| Data-plane verbs (direct to aihub) | `file`, `queue`, `cancel`, `recall`, `remember`, `memory` |
| Execution-plane verbs (local daemon UDS face) | `submit`, `watch` (snapshot, resident watcher, `subscribe`/`unsubscribe`/`list`), `answer`, `pause`, `resume`, `wrap`, `ops` (`status`, `auto`, `max`, `shutdown`, and `end-session` — aihub-direct, admin/maintainer), `chat init` |
| Management face (M1–M5) | `users`, `api-keys`, `projects`, `machines`, `backup` (DB-only snapshot, see below) |
| Diagnostics | `doctor` |
| Process | `daemon` (run the work machine; `pf daemon ensure` starts it) |

`pf backup` writes a consistent DB-only snapshot (`VACUUM INTO`) under the
server's data dir `backups/`, on the same disk. It does NOT include memory
content (memory text lives only as files under `memory/`; the DB holds only its
sha/length) nor the git repos (`git/`). For a full backup take the
sqlite backup-API snapshot of `aihub.db` (not a plain file copy of a live WAL
database) and rsync `memory/` and `git/` alongside it, as `deploy.sh` does. There
is no backup-only mode: `deploy.sh`'s online backup (snapshot, rsync catch-up,
consistency check, off-host copy) runs only as part of a deploy, so an ad-hoc full
backup is these manual steps (or a copy of the whole data dir while aihub is stopped).

Human moves: continue = `pf resume <id>` (a paused or blocked
WI), abandon = `pf cancel <id>` (queued, paused or blocked), owner's failed verdict on a live run = `pf wrap <id> --fail` (refused on a blocked WI). These run with the
human's own api key: aihub's door checks the WI's reporter, a project
maintainer or an admin.

## `pf daemon` flags

`pf daemon` runs the local work machine (runtime, driver and watcher over
the UDS face). Every setting is a flag; where an environment variable is listed
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
| `--workflows-dir DIR` | — | `./workflows` | Workflow-bundle checkout the in-process Go driver loads step definitions from (`pf daemon ensure` passes `<data-dir>/release/current/workflows`). |
| `--pi-bin PATH` | — | `pi` | Harness binary the agent processes are derived from. |
| `--claude-bin PATH` | — (config `claude_bin`) | `claude` | Claude Code binary for steps whose harness is `claude` (run as `claude -p --bare`). The daemon never manages its credentials: the `<harness-root>/claude-config/settings.json` env block is the authority layer. |
| `--operator-agent-dir DIR` | `POLYFORGE_OPERATOR_AGENT_DIR` | `~/.pi/agent` | SOURCE of the dedicated step agent dir: the daemon copies the minimal set (`auth.json`, `models.json`, `agents/`, `extensions/subagent/`) from here into `<harness-root>/step-agent`, and every pi step runs against that copy — the operator's own packages and settings never reach a step. A configured source without a readable `auth.json` refuses the start. |
| `--harness-root DIR` | `POLYFORGE_HARNESS_ROOT` | `<data-dir>` | Parent root of the step harness dirs: `<DIR>/step-agent` (dedicated agent dir), `/step-agent-sessions` (pi sessions) and `/claude-config` (claude config dir; its settings.json env block is the credential authority layer). The former `--harness-sessions-root` was renamed — old deployments fail loudly at startup. |
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

Per-project model candidates. A workspace root may carry a
`.polyforge.yaml` whose only section is `steps` (strict: unknown keys at
any level, duplicate keys, type mismatches, empty model ids and a
per-step key that is no step id of the daemon's installed workflows, a
project the cli.toml mapping does not route to this workspace, and
duplicate model ids in one list are load errors; a step entry's only key is `models`; the pre-#169
top-level `models:` section and the old `projects:` / `workflow_defaults:`
shapes fail loudly):

```yaml
# Precedence: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
steps:
  polyforge:                                # a project of this workspace
    default: { models: ["pi:prov/model-a", "pi:prov/model-b"] }  # optional: steps without a list of their own
    fix: { models: ["claude:claude-opus-5-5", "pi:prov/model-c"] }  # optional: one step id (fix, code,
                                            # review, deliver, ...) — the step
                                            # with that id in every bundle
```

Every candidate — here, in a bundle's `model_candidates` and in
`pf submit --models` — is `<harness>:<model>` with harness `pi` or
`claude` (polyforge#175; split on the first `:`, the prefix mandatory). A
chain may mix harnesses; a switch onto a candidate of another harness
starts a fresh session with the previous transcript as read-only
reference.

For a step of the claimed project the list is, highest first: the work
item's manual `pf submit <id> --models pi:a,claude:b` list (every step; kept by this
daemon only — across its restarts and `pf resume` — and a re-claim on
another machine uses that machine's settings), else the
yaml list keyed by the step's id, else the workflow bundle's step-level candidates, else
the project's `default`, else the bundle's `defaults` (a
declared-but-empty yaml list governs: no switchable chain). A project not
listed, or an absent or comments-only file, keeps the bundle's. The
daemon reads and validates the file BEFORE the aihub claim: an invalid
file refuses the work item with DAEMON-414 (`pf submit` shows it; auto-pull
skips that project and logs once) and nothing is claimed, so the work item
stays queued for other machines. Repo lists and clone URLs always come from
the aihub project registry. `pf doctor` reports the same refusal (an
unknown per-step key and a leftover `models:` section included) before
any claim. Upgrading from the pre-#169 shape: see
[docs/upgrade.md](docs/upgrade.md).

Supervision bounds (v2-converge 2.4) — Go duration strings (e.g. `700ms`,
`5m`), flags only (no environment fallbacks). Omitting a flag, or passing
`0`, keeps the default listed here. The only per-step override in
`workflow.md` is `budget.wall_minutes` (an estimate: T becomes
`max(3 x wall_minutes, --step-timeout)`); X and Y are global only — the
former `silence_minutes` / `tool_minutes` keys were deleted (polyforge#147)
and an old bundle that still carries them has them silently ignored.

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

Minimal external dependencies: pure-Go SQLite (modernc.org/sqlite), yaml.v3, goldmark, jsonschema, klauspost/compress, x/sys.

## `pf recall` — memory search

Searches a project's memories on the aihub memory face (trigram-indexed
full-text search):

    pf recall --project P <query> [--type T] [--tags a,b] [--limit N]

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
  rejected locally with no request sent. The
  `GET /api/v1/memories/search` face interprets a trailing `.*` the same way.
- `--tags a,b` — comma-separated tag filter (a hit carries all of them).

Examples:

    pf recall --project polyforge flaky build
    pf recall --project polyforge --type 'experience.*' --limit 50 flaky build
    pf recall --project polyforge --type fact.note --tags postgres,backoff "retry ladder"

## Contributing & filing work items

Change coverage is tiered by the change's **behavior face**, not by a
"e2e on every ticket" blanket rule (owner ruling 2026-10-02). The existing
e2e suite (`go test ./test/`) is the regression net for every
change — each ticket's verification step runs it — and the three disciplines
below govern when **new** coverage must be added. The authoritative source is
`docs/design/polyforge-vnext/v1.5-change-coverage-rules.md` (Chinese); this
section is a faithful summary, and that doc remains the single authority.

**Discipline ① — changes observable behavior → file on an e2e-carrying
graph.** If a change alters observable behavior — a new endpoint, a
protocol/contract/semantics change, an ownership or fencing rule change, a
new external capability — the ticket must be filed with `bundle_name` set to
a graph that carries an e2e-author step (currently `feature-full`, whose
verification also runs the authored scenarios; giving another graph that
step is zero code). Pure refactors, pure docs,
and obviously non-behavior chores keep the existing graphs (the existing
suite + verification is enough). Quick test: ask "after this merges, what
difference would users/callers perceive that the existing scenarios can't
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
surface), owner-answered needs_input, and the readings (command-evidence gate,
revision chain, recovery/blocked counts, orphan counts) compared against the
previous window — no regression passes, regression freezes merges until
fixed. The window ticket is registered at merge; it blocks cutover, not
merge. The report is filed on the window ticket and readings never
rewrite history.

**Merge-review checklist** (compact form of the authority doc's 检查单):

- [ ] Behavior face touched? → filed on an e2e-carrying graph, or new
  scenarios human-authored
- [ ] Bug fix merged? → `regression_test` field non-empty, or a waiver
  reason accepted by review
- [ ] Skill face triggered? → observation-window ticket registered (blocks
  cutover-grade release, not merge)
