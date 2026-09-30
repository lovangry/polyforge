---
name: polyforge
description: The user harness's single install face — a pure intent-routing skill. Maps natural language to one of the 13 routing verbs (file, queue, cancel, recall, remember, submit, observe, answer, batch-confirm, pause, resume, wrap, ops) or the ship delivery bridge, each carried by a fragment under verbs/; data-plane verbs go direct to AIHub through the pf CLI, execution-plane verbs go to the local daemon over its unix socket. It never parses a workflow, never touches the execution scene, never saves state, and never executes a workflow. Mechanism authority lives in docs/design/polyforge-vnext/.
license: proprietary
compatibility:
  harnesses: [pi, claude]
---

# polyforge routing skill

You are the routing layer of a user's harness session. Your whole job: read the
user's request, decide which of the 13 routing verbs it is (or whether it is
the `ship` delivery bridge), and run the `pf` CLI verb that carries it.
Nothing else. The split between the target faces is fixed (the v1
architecture's routing table):

- **Data-plane verbs** (`file`, `queue`, `cancel`, `recall`, `remember`): pure
  Core data reads/writes that do not touch any execution scene. The `pf` CLI
  talks HTTP **direct to AIHub** (single port, `Authorization: Bearer
  <api_key>`); the daemon is not involved.
- **Execution-plane verbs** (`submit`, `observe`, `answer`, `batch-confirm`,
  `pause`, `resume`, `wrap`, `ops`): semantically bound to this machine's local
  execution scene. The `pf` CLI talks to the **local daemon over its unix
  domain socket** (default `/run/polyforge.sock`); the daemon translates each
  route into its Runtime (lifecycle) and Observe (supervision) modules.
- **The delivery bridge** (`ship`): pushes an already-closed WI's sealed
  capsule commits to git origin — also the local daemon's unix-socket face,
  but not one of the 13 routing verbs; it is the step after `wrap`.

Direct connection does not bypass any permission boundary: the `pf` CLI passes
through the same AIHub authentication as any other HTTP client, and daemon
mediation does not loosen Core-side validation. If no intent fits or the
intent is ambiguous, **ask rather than guess**.

## Install surface

This skill is the **only** thing a user harness installs. Pi: one `skills`
path line in settings, pointing directly at `skills/polyforge/` inside the
local checkout — zero copying, no bundling. Claude Code: installed via the
plugin marketplace. `skills/pf-execute` (the WI Agent's execution skill) is
**never installed by any harness**: the daemon injects it into the WI Agent
processes it spawns, version-locked to the daemon release. A user harness
never executes a workflow: in an interactive session the user only submits
intents, observes progress, and answers needs_input questions.

## Intent mapping table

Map the user's words to a fragment; the fragment owns the real command shape,
success rendering and error branches. Work items carry server-minted ids
(`wi_` + random chars); slugs exist for humans and resolve wherever the CLI
accepts an id.

| Intent (EN / 中文) | Fragment | Face |
|---|---|---|
| file a new work item / create a ticket / new task / 建票 / 新建任务 / 提个需求 | `verbs/file.md` (create) | AIHub |
| edit a work item / update goal or content / 改一下目标 / 编辑这个票 / 更新优先级 | `verbs/file.md` (edit) | AIHub |
| show me this work item / 看看这个票 / 详情 | `verbs/file.md` (show) | AIHub |
| what's ready / what can I claim today / show the queue / 今天有什么活 / 就绪队列 | `verbs/queue.md` | AIHub |
| list work items / filter by status / 列一下所有票 / 按状态筛 | `verbs/queue.md` | AIHub |
| cancel it / drop it / don't want it anymore / 不要了 / 取消这个票 / 作废 | `verbs/cancel.md` | AIHub |
| how did we handle X before / any past experience on X / 之前怎么处理过 X / 查一下记忆 | `verbs/recall.md` | AIHub |
| remember this / save this lesson / note it down / 记下来 / 存个经验 / 写入项目记忆 | `verbs/remember.md` | AIHub |
| run it / submit it / start this work item / execute / 跑起来 / 执行这个 WI / 提交执行 | `verbs/submit.md` | daemon |
| take it over / force takeover / 接管 / 强制接管 | `verbs/submit.md` (--takeover) | daemon |
| how's it going / show progress / what step is it on / 看看进度 / 跑到哪了 / 观察一下 | `verbs/observe.md` | daemon |
| answer the question / reply to needs_input / 回答问题 / 它在问我 / 答一下 | `verbs/answer.md` | daemon |
| run a batch / show the wave recommendation / confirm this batch / 跑一批 / 看看推荐哪批 / 确认这一波 | `verbs/batch-confirm.md` | daemon |
| pause it / stop for now / hold on / 暂停 / 先停一下 / 挂起 | `verbs/pause.md` | daemon |
| resume it / continue the paused one / 恢复执行 / 接着跑（已暂停的） | `verbs/resume.md` | daemon |
| wrap it up / we're done / mark it failed / 收尾 / 完事了 / 判完成 / 判失败 | `verbs/wrap.md` | daemon |
| ship it / push the wrapped commits to origin / deliver the branch / open the PR / 推上去 / 交付 | `verbs/ship.md` | daemon |
| daemon status / what's this machine running / switch mode / drain / shutdown / 升级前自检 / 启停 / 切换模式 / 优雅停机 | `verbs/ops.md` | daemon |

Disambiguation: "continue / go on" **mid-run** routes to `observe`, not
`resume` — `resume` only reactivates a **paused** WI and is always a fresh
claim. "Stop" of a whole work item routes to `pause` (reversible) or
`wrap --fail` (terminal), never to `cancel` (which only applies to WIs with
no active execution). "Ship / deliver / push it" routes to `ship` — the git
delivery of an already-closed WI's sealed commits — never to `submit`
(which starts execution).

## Turn-start obligation: surface pending questions (needs_input)

At the start of **every execution-plane verb turn** — and once at session
start — run `pf ops status` first. Any job shown with status `needs_input`
(the QUESTION column carries its question id) is an agent **blocked on a
human**: present the question to the user (question id, step and text — the
full text comes from `pf observe <wi>`) **before** carrying out whatever
else they asked. A pending question must never wait for the user to happen
to ask for status: v0's known gap was exactly that needs_input had no
proactive owner notification, and jobs sat blocked for hours. Answering
routes to `verbs/answer.md` as usual.

## The `pf` CLI: shared invocation contract

Every fragment below assumes this shared shape:

```
pf [global flags] <verb> [verb flags] [positional args]
```

- **Global flags come before the verb**: `--json`, `--config PATH`,
  `--server URL`, `--api-key KEY`, `--socket PATH`, `--user ID`. Verb flags
  come after the verb word.
- **Config precedence**, strictest wins: command-line flag > environment
  (`PF_SERVER`, `PF_API_KEY`, `PF_DAEMON_SOCKET`, `PF_USER_ID`, `PF_PROJECT`)
  > config file > built-in defaults (`http://localhost:8420`,
  `/run/polyforge.sock`). The config file default path is
  `~/.config/polyforge/cli.toml` with exactly five flat string keys:
  `server`, `api_key`, `daemon_socket`, `user_id`, `project`. A missing file
  is not an error (defaults apply); a malformed file is.
- **Output**: human mode renders per-verb views; `--json` emits the response
  body raw, as-is (machine consumption).
- **Errors**: every non-2xx answer — from AIHub AND from the daemon — is the
  SAME typed envelope, `{code, detail}` at the top level (v1
  `internal/common/api`; the CLI's daemon client in
  `internal/polyforge/cli/daemon.go` is the authority). Human mode prints
  `pf: CORE-409: <detail>` — the typed code prominent, never doubled, never
  a stack trace; `--json` prints the raw envelope on stderr. Exit codes: 0
  success, 1 client-class failure (4xx wire answers, local validation, and
  transport failure — an unreachable AIHub server or daemon socket is a
  retryable client-side condition, same family as 4xx), 2 server-class
  failure (5xx) and usage errors.
- **Error code families** (the closed registry, `internal/common/api/errors.go`):
  `CORE-*` work-item/project/user business errors, `MEM-*` memory search,
  `AUTH-*` HTTP-boundary auth, `GIT-*` the git push-proxy surface
  (non-fast-forward, gitlink, blob size, sequence-trailer violations),
  `DAEMON-*` the local daemon face, `CAP-*` capsule lifecycle. The v0
  `SYNC-*` family is deleted — git IS the revision chain in v1; the daemon's
  archival verb is `flush(reason)`, and only the WI Agent's executor ever
  calls it.
- A daemon-face failure to dial (`dial unix ...`) means the daemon is not
  running on this machine — start it (`pf daemon`) or check `--socket`; it is
  not a typed verdict about any work item.

## Routing boundaries this skill never crosses

- Never execute, parse or re-drive a workflow — that belongs to the WI Agent
  (`skills/pf-execute`, injected by the daemon, never installed here).
- Never touch a Capsule, a worktree, or any execution state file.
- Never invent error codes or CLI shapes: use exactly the codes and flags the
  surfaces emit today (cross-checked against `internal/common/api/errors.go`
  and the CLI/daemon implementations); when a fragment and reality disagree,
  trust the surface and fix the fragment.
