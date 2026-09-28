# 04 — Daemon：Job 排入、Interactive/Batch/Drain、持久 WI Agent 生命周期与全扫描发布的机械执行

| 权威项 | 值 |
|---|---|
| 文档定位 | Daemon 职责、Job 排入、Interactive/Batch/Drain 三模式的**实现**、持久化 Harness RPC WI Agent 的派生与监督、harness 适配器（Pi/Claude）的启动参数、watcher 驱动（dirty 标记）、全扫描发布的四类触发时机的**机械执行**、优雅停机（quiesce/shutdown，**实现**）、Daemon state 存储、命令执行入口的调用面、崩溃恢复 |
| 上游基线 | `00-architecture.md#§3.1`（Daemon 命名裁定）、§3.4（Interactive/Batch/Drain 权威定义）、§3.5（优雅停机权威定义）、§3.6/§3.6a（持久 Harness RPC WI Agent 与 harness 适配器最小契约权威定义）、§4.3（同步触发时机权威定义、INV-11）；`03-execution-capsule.md#§5`/§6/§7（Native runner、Capsule 会话目录、崩溃续跑规则）；`02-aihub-core.md#§2.3`/§4.10/§7（owner generation、机器注册与心跳、stale 检测）；`05`（Revision 发布协议） |
| 术语权威 | 本文负责：Daemon 内部职责实现（Job 队列、持久 WI Agent 生命周期管理、harness 适配器启动参数）、命令执行入口（调用面）、Daemon state 存储位置与内容、模式切换实现、全扫描发布的**机械执行**。**不**重新定义 Interactive/Batch/Drain、优雅停机或 harness 适配器的最小契约本身——权威定义在 `00-architecture.md#§3.4`/§3.5/§3.6a，本文只给实现。 |

## 1. 范围与非目标 <!-- §1 -->

### 1.1 范围 <!-- §1.1 -->

- Daemon（`pf-daemon`）的职责边界与操作面：Job 排入、按 `00-architecture.md#§3.4` 的 Interactive/Batch/Drain 判据创建/回收 Capsule、为每个执行中的 WI 派生并监督其**持久** Harness RPC WI Agent（wi agent；`00-architecture.md#§3.6`），选定并启动该 WI 的 harness 适配器（Pi 或 Claude，§6.1a）、驱动 watcher（只做 dirty 标记）、在收到 `00-architecture.md#§4.3` 定义的发布请求时**机械地**执行一次全扫描发布。
- 优雅停机（quiesce/shutdown）的**实现**（`00-architecture.md#§3.5` 权威定义的落地）：明确它不是 Drain，是 Daemon 进程生命周期的终止路径，并且是四类发布触发时机中唯一由 Daemon 自己发起请求的一类。
- Daemon state 的存储位置与内容：记录当前承载哪些 WI 的 Job、各自持久 WI Agent 的 pid、使用的 harness 适配器、当前模式；与 Capsule 的 `runtime-state/execution.json` 严格区分（Daemon state 不是执行状态，只是簿记）。
- 命令执行入口的**调用面**：Daemon 本身不直接执行 step 命令，命令执行由 WI Agent 经 Capsule 的 Native runner 完成（`03-execution-capsule.md#§5`）；Daemon 只做进程级别的派生与回收。
- 全扫描发布的**机械执行**：WI Agent（或代其行动的 Daemon，见优雅停机）向本机 Daemon 发起发布请求后，实际的扫描 Capsule 变更、打包、调用 `05` 发布接口、推进 HEAD 这一整套动作由 Daemon 执行；Daemon 是这一动作唯一的执行者（`00` INV-11）。
- Daemon 自身崩溃重启后的恢复：用 Daemon state 重新发现本机之前管理的 WI，检测其 WI Agent 是否存活（pid 存活 + 同进程校验），对已死的 WI Agent 按 `03-execution-capsule.md#§7` 的续跑规则重新派生。

### 1.2 非目标 <!-- §1.2 -->

- **不重新定义** Interactive/Batch/Drain 三模式的进入条件、判据与边界——权威定义在 `00-architecture.md#§3.4`，本文只给实现（§2.2、§6）。
- **不重新定义**优雅停机（quiesce/shutdown）的语义与其和 Drain 的区分——权威定义在 `00-architecture.md#§3.5`/INV-13，本文只给实现（§2.3、§6.5）。
- **不重新定义** harness 适配器的最小契约（进程持久性、稳定 session id、Capsule 本地会话目录、subagent introspection 为可选能力）——权威定义在 `00-architecture.md#§3.6a`，本文只给两条适配器各自的启动参数（§6.1a）。
- 不定义 Capsule 身份、目录布局、`runtime-state/execution.json` 格式、Capsule 本地会话目录的路径约定或 Native runner 的隔离机制（`03` 权威）。
- 不定义 workflow 执行语义（rewind/resume 判定，needs_input/interactive step 的判定层语义，`01-workflow-skills.md#§6` 权威）。
- 不定义 Revision 数据模型、Revision 文件格式或全扫描发布的**协议**细节（分块、去抖动、快照一致性，`05` 权威）；本文只定义「谁在什么时候把请求交给谁、谁实际调用该协议」。
- 不设计 K8s 内运行形态与多 Daemon 协同调度。

## 2. 术语与不变量 <!-- §2 -->

### 2.1 Daemon（本文只做实现层复述，权威定义见 `00-architecture.md#§3.1`） <!-- §2.1 -->

每台机器一个的守护进程（`pf-daemon`），职责恰好五项（`00-architecture.md#§3.1` 权威列举，本文逐项给实现）：

1. **Job 排入与 Capsule 创建/回收**：按 `00-architecture.md#§3.4` 的 Interactive/Batch/Drain 判据接受或拒绝一个 WI 的执行请求；创建 Job 时调用 `03-execution-capsule.md#§6.1` `capsule_create`；Job 结束（wrap/fail/优雅停机中断）时封存 Capsule（§6.2）。
2. **持久 Harness RPC WI Agent 的派生与监督**：每个正在执行的 WI 有且只有一个 WI Agent 进程，由 Daemon 按 §6.1a 选定 harness 适配器并派生，该进程持续存活直至该 WI 到达终态或被优雅停机中断（`00-architecture.md#§3.6`/INV-14）；Daemon 通过标准进程句柄（pid、退出码、stdout/stderr、进程组）观察其存活与输出。
3. **驱动 watcher**：watcher 只做 dirty 标记（`00-architecture.md#§4.3`/INV-11），Daemon 负责启动/停止 watcher 进程或协程，不赋予其发布权。
4. **机械执行全扫描发布**：收到发布请求（来自某个 WI 的 WI Agent，或优雅停机路径下 Daemon 自己发起）后，Daemon 对该 Capsule 执行一次实际的全量扫描并调用 `05` 的发布接口推进 HEAD（`00-architecture.md#§4.3`/INV-11，本文 §6.4）。
5. **执行优雅停机（quiesce/shutdown）**：`00-architecture.md#§3.5` 权威定义的落地序列（本文 §6.5）。

Daemon **不**重新实现 workflow 驱动逻辑——驱动 workflow 是 WI Agent 的职责（`00-architecture.md#§3.6`），Daemon 只负责 Job 的排入、WI Agent 的生命周期、三种模式的判据、以及发布请求的机械执行。Daemon 本身也不直接执行任何 step 命令（§6.6）。Daemon **不**为同一个 WI 的不同 step 分别启动独立的 headless agent 进程——这不是本文档的一个可选实现分支，而是 `00` INV-14 划定的边界：一个 WI 只对应一个持久 WI Agent 进程，全部 step 都在它内部完成。

### 2.2 Interactive / Batch / Drain：本文只引用判据，不重复定义 <!-- §2.2 -->

三种模式的定义、进入条件、执行机制统一性（持久 WI Agent + Capsule 完全相同，差异只在 Job 的来源与准入判据）均以 `00-architecture.md#§3.4` 为唯一权威，本文档不重复给出定义，只在 §4/§6 给出 Daemon 侧的状态存储与流程实现：

- **Interactive** 的 submit/observe/answer 三个动作在 Daemon 操作面上分别对应 §5.1 的 `claim`/`resume`、`observe`/`status`（两层观察协议见 §5.1a）、`answer` 端点。
- **Batch** 的「调度器推荐一波 → 人确认 → 成批提交」在 Daemon 侧体现为：一次 `mode(get_recommendation)` 只读调用取得 AIHub 调度器给出的候选集合，人确认（可增删）后再以一次 `batch_submit(wi_ids)` 显式提交，Daemon 不会把只读的推荐结果自动当作已确认的 wave（§6.1b）。
- **Drain** 的「自动连续执行 waves」在 Daemon 侧体现为一个内部循环：`mode(drain)` 开启后，Daemon 反复调用 AIHub 的 ready queue 取下一批可执行 WI 并直接提交，无需逐条 `batch_submit` 确认（§6.1c）。

### 2.3 优雅停机（quiesce/shutdown）：本文只引用定义，强调与 Drain 的区分（INV-13） <!-- §2.3 -->

优雅停机（亦称 quiesce 或 shutdown，三词同指一事）的权威定义在 `00-architecture.md#§3.5`；INV-13（`00-architecture.md#§2.3`）已裁定优雅停机与 Drain 是两个不同概念，命名与判据均不得混用：

- 优雅停机是 Daemon **进程生命周期的终止路径**，触发条件是收到终止信号或人工停止请求，**不是**「没有更多 WI 可做」。
- Drain（`00-architecture.md#§3.4c`）是一种**常驻的自主 WI Job 执行模式**，没有可执行 WI 时空闲等待、不退出进程；这是一种执行模式，不是任何形式的停机。
- 本文只在 §6.5 给优雅停机的实现序列，不在此处或任何其它章节把两者的判据合并表述；`mode(drain)` 与触发优雅停机是 Daemon 操作面上两个完全独立的入口（§5.1），互不复用彼此的状态机。
- **概念不合并，但停机一旦触发即对 Drain 循环生效**：优雅停机序列的第一步（§6.5 第 1 步）置位的 `shutting_down` 标志，同一时刻既拒绝外部提交也让 Drain 的内部取 wave 循环（§6.1c）停止取下一批——这是「优雅停机禁止一切来源创建新 Job」这条要求本身的一部分，不是把 Drain 重新定义成停机的一种；Drain 模式的定义、判据、`mode(abort-drain)` 撤销入口在这次修订后完全不变，唯一变化是它的内部循环像 claim/resume/takeover/batch_submit 一样，在停机标志置位后不再产生新 Job。

### 2.4 本文新增不变量 <!-- §2.4 -->

- **INV-D1**：Daemon state 文件不得记录任何 workflow 执行状态字段（`current_step`、`step_attempt`、`review_round` 等）——这些字段的唯一权威是 Capsule 的 `runtime-state/execution.json`（`00` INV-1）；Daemon state 只记录「Daemon 知道自己在管哪些 Job」这一簿记事实（§4.1）。
- **INV-D2**：Daemon 自身不执行任何 step 命令；一切命令执行经 `03-execution-capsule.md#§5.1` `RunCommand`，调用方是 WI Agent，不是 Daemon（§6.6）。
- **INV-D3**：全扫描发布的实际扫描、打包与 HEAD 推进只由 Daemon 执行；WI Agent 与其它任何角色只能向 Daemon 发起请求，不持有直接调用 `05` 发布接口的路径（`00` INV-11 在本文的落地）。
- **INV-D4**：Daemon 为一个 WI 派生的持久 WI Agent 进程在该 WI 的整个执行期内只存在一个；进程崩溃后的续跑是「重新派生同一角色的新实例」，不是「为下一个 step 另外派生一个新角色」（`00` INV-14 在本文的落地）。

## 3. 模块与代码目录 <!-- §3 -->

```
internal/daemon/
├── server/         # 操作面 HTTP/UDS（§5.1）
├── modes/          # Interactive/Batch/Drain 状态存储与切换（§2.2，判据引用 `00-architecture.md#§3.4`，本目录只存状态）
├── jobs/           # Job 排入、持久 WI Agent 派生/监督/回收、harness 适配器启动参数（§6.1/§6.1a/§6.2）
├── watcher/        # dirty 标记驱动（§6.3，协议权威在 `05`）
├── publish/        # 全扫描发布的机械执行（收发布请求 → 扫描 → 调用 `05` 发布接口，§6.4）
└── shutdown/       # 优雅停机序列（§6.5）
cmd/pf/             # 入口：`pf` CLI 动词 + `daemon` 子命令（单文件工作机二进制，`00-architecture.md#§3.3`）
```

## 4. 数据结构 <!-- §4 -->

### 4.1 Daemon state（存储位置与内容，权威） <!-- §4.1 -->

存储于 **Daemon 数据目录**（缺省 `$POLYFORGE_HOME/daemon/`，本机路径，**不在任何 Capsule 内**）：

```
daemon/state.json:
{
  "machine_id": "m-01",
  "mode": "interactive|batch|drain",
  "version": "...",
  "workspace_root": "/home/svc/pf-workspace",
  "started_at": "2026-09-20T08:00:00Z",
  "jobs": [
    { "work_item_id": "wi_793",
      "capsule_id": "cap_wi_793",
      "generation": 3,
      "harness_adapter": "pi|claude",
      "harness_session_id": "sess_wi793_g3",
      "main_agent_pid": 12345,
      "main_agent_started_at": "2026-09-20T08:00:03Z",
      "status": "running|interrupting|sealing" }
  ]
}
daemon/credentials/     # AIHub api_key 与机器注册材料（0600）
```

`harness_session_id` 是该 WI 在其持久 WI Agent 生命周期内使用的稳定 session id（`00-architecture.md#§3.6a`），
崩溃续跑时按 §7.1 的规则复用同一个值，不重新分配；session 数据本身落在该 WI 的 Capsule 内
（Capsule 本地会话目录，`03` 权威布局），Daemon state 这里只记录 session id 这一标识，不复制
session 数据。

**与 Capsule 的 `runtime-state/execution.json` 严格区分（INV-D1）**：

| | Daemon state | `runtime-state/execution.json` |
|---|---|---|
| 归属 | Daemon（每机一份） | Capsule（每 WI 一份） |
| 内容 | 「本机在管哪些 Job、各自持久 WI Agent 的 pid/harness 适配器/session id、当前模式」的簿记 | `current_step`、step 尝试历史、review 轮次、预算消耗——workflow 执行的唯一权威（`03-execution-capsule.md#§4.2`） |
| 写者 | Daemon 进程 | 该 WI 的 WI Agent（`00` INV-6） |
| 是否为「执行状态」 | **否**——丢失后可从 AIHub（`02-aihub-core.md#§5.1` owner generation 查询）+ 本机进程表重新发现，不影响 workflow 执行进度 | **是**——INV-1 唯一权威，丢失则该 WI 的执行进度不可恢复，只能退回最近发布 Revision |
| 崩溃恢复角色 | §7 崩溃恢复的输入（找回「管什么」） | `03-execution-capsule.md#§7` 崩溃恢复的输入（找回「跑到哪」） |

`daemon/state.json` **不进入任何 Capsule 的可见路径**（`03-execution-capsule.md#§5.2`/§6.2 面表第 3 行）——subagent 对其不可见，这是 INV-6 实现层的一部分（Daemon 从不把该路径放进 subagent 的 env/cwd/argv）。

`main_agent_started_at` 是 pid 复用防护所需的进程创建时间戳（§7 崩溃恢复用它区分「同一个 WI Agent 还活着」与「pid 被无关新进程复用」）。

## 5. API 与错误码 <!-- §5 -->

### 5.1 Daemon 操作面（路由 Skill / CLI → Daemon；本机 Unix domain socket `/run/polyforge.sock`） <!-- §5.1 -->

| 操作 | 语义 | 错误码 |
|---|---|---|
| `claim(wi_id)` | Interactive submit：预检 → `02-aihub-core.md#§5.1` claim → `03-execution-capsule.md#§6.1` 创建 Capsule → 选定 harness 适配器 → 派生持久 WI Agent | `DAEMON-507` / `CORE-*` 透传 |
| `resume(wi_id)` | Interactive submit：`pause` 已结束该 WI 当前活跃的 generation，`resume` 恒为一次新 claim（generation+1，`02-aihub-core.md#§2.3`/§2.5）；因此走与 claim 相同的预检+派生序列，并按新 generation 分配**全新**的 harness session id（§6.1a）——**不**复用旧 generation 的 session：换 generation 即不延续旧 session（`00-architecture.md#§3.6a`、`03-execution-capsule.md#§4.4`）。只有同一 generation、同一台机器上的 Daemon/WI Agent **崩溃续跑**（§7.1）才复用同一个 session id；resume 不属于这一场景 | `DAEMON-507` / `CORE-*` 透传 |
| `takeover(wi_id)` | 与 claim 同一道预检 → `02-aihub-core.md#§5.1` force-takeover（admin）→ `03-execution-capsule.md#§6.1` 创建/隔离重建 Capsule → 派生持久 WI Agent；预检失败时**不**调用 force-takeover，Core 侧 owner generation 保持不变 | `DAEMON-507` / `CORE-*` 透传 |
| `observe(wi_id)` / `status()` | Interactive observe，**两层观察协议**（权威定义 §5.1a）：本机拥有该 WI Capsule 时走事件流端点 `GET /jobs/{wi_id}/observe?after=<cursor>`（Daemon 内存 ring buffer 的增量事件）；本机无该 WI Capsule 时 read-through 同样由 Daemon 经其 UDS 面提供（响应携带 `read_through:true` 与 freshness 标注，渲染 `02-aihub-core.md#§5.2` read-through 的最新已发布 HEAD `execution.json`），`DAEMON-404` 仅在两层都无结果时返回；CLI 直走 Core read-through 的三条触发路径（显式 `--read-through` 覆盖、可达 Daemon 答复 `DAEMON-404` 后的落回、Daemon 面不可用时的回退）见 §5.1a 第二层。无 SSE、无推送、无长连接。`status()` 返回 §4.1 `jobs[]` 的本机簿记列表（`main_agent_pid`、`harness_adapter`、`harness_session_id`、`status`）——只是「本机在管什么」的事实，不构成第二个 workflow 执行进度来源（`00` INV-1 在本文的落地） | `DAEMON-404` |
| `answer(wi_id, payload)` | Interactive answer：把用户对处于 needs_input 状态的 step 的回答转发给该 step（needs_input 的判定层语义见 `01-workflow-skills.md` 的 interactive step / grill）。`payload` 的 schema **单一权威在 `00-architecture.md#§3.4a`**（answer 中继报文五字段 `question_id`/`answer_id`/`answer_text`/`answered_at`/`answered_by`，及配套的 needs_input 观察报文 schema 与其在 observe 流的产生时机；本文不另设本地变体），Daemon 原样转发、不解析、不改写。回答**按 `question_id` 绑定**：必须等于该 step 在 `03-execution-capsule.md#§4.2` 的 `steps.<id>.pending_question` 中持久化的那个 `question_id`（比对依据的唯一持久化位置），不匹配或该 `question_id` 已被消费（step 已续跑、`pending_question` 已清空）拒绝为 `DAEMON-409`——比对/去重/过期规则的权威在 `00-architecture.md#§3.4a`。同一 `answer_id` 的重复提交按 `answer_id` **幂等**：不重复应用、不重复消费，返回与首次提交一致的结果；`answered_by` 与请求认证身份不一致拒绝为 `DAEMON-403` | `DAEMON-409` / `DAEMON-403` |
| `POST /jobs/{id}/pause` | Interactive pause（`00-architecture.md#§5.1` 分流表：依赖本机执行现场的动词打 Daemon 本机面）：暂停该 WI 的本机执行面——Daemon 以 `pause` 结束其当前活跃 generation，收尾所需 final flush 与 `flush_evidence` 由 Daemon 生成（与 §6.2 A 路径第 2–3 步、§6.5 第 4 步同一机械序列；end-generation 调用必须携带该证据，`02-aihub-core.md#§6.4`）；该 WI 在 Core 侧保持 claimed、不释放、不回到就绪队列，恢复恒为 `resume` 的一次新 claim（本表）；本机 Job 按 §6.2 A 第 4–6 步回收、Capsule 封存为只读现场，其后进度观察走 `observe`/`status()` 两层观察协议（§5.1a，无本地 Capsule 时由 read-through 层渲染最近已发布 HEAD） | `DAEMON-404` / `CORE-*` 透传 |
| `POST /jobs/{id}/wrap {reason: wrap|fail}` | Interactive 收尾（`00-architecture.md#§5.1` 分流表，与 `pause` 同一 flush 理由）：结束路径——Daemon 为该 WI 机械执行 final flush 并**自己**生成最终 `flush_evidence`（与 §6.5 第 4 步 / §6.2 A 路径第 2–3 步同一机械序列，证据字段定义 `02-aihub-core.md#§6.4`），随后以持有的 owner generation token 直接调用 Core end-generation（`reason ∈ {wrap, fail}`，`02-aihub-core.md#§6.4`）——发起者与证据生成者是同一个 Daemon 进程，不经 §5.3 代理往返；成功后按 §6.2 A 第 4–6 步 seal 该 Job。**不自动 re-claim**：收尾后不为该 WI 派生新 Job；若持有凭证已被 `mode="rollback"` 立即作废（`CORE-403` `secret_rolled_back`，`02-aihub-core.md#§6.6`），按 §7.3 隔离现场、不重试收尾，回滚恢复出的活跃 generation 行唯一仲裁路径是 force-takeover | `DAEMON-404` / `DAEMON-503` / `CORE-*` 透传 |
| `ship(wi_id)` | **wrap→git 交付桥**（基线修订 polyforge#37，方案 B 裁定；缺口实测：polyforge#2 全程 wrap 落定后，其 git commit 只存在于封存 capsule（detached HEAD）与 Revision 链，原有 12 个动词无一能把它推送至 git origin，人工桥接（fetch 封存 capsule → 临时分支 → PR → merge）验证可行后收编为一等动词）。**预检**（不通过则推送不发起）：本机持有该 WI 的封存 capsule（只读现场，`03-execution-capsule.md#§2.1`——封存对 ship 保持只读可达，写禁令不变）→ 否则 `DAEMON-404`；本机 git push 身份可用 → 否则 `DAEMON-512`。**语义**：从封存 capsule 各 `repos/<name>` 工作树读取封存时 HEAD 的字节，把固定名分支 `polyforge/<wi-slug>` 推送到该仓 origin；**永不推送 `main`/`master`**（保留名目标 → `DAEMON-403`）。**幂等**：远端同名分支已在目标 sha → 复用该分支、重开/复用既有 PR，返回与首次一致；远端同名分支已前移（≠ 封存 commit）→ 拒绝为 `DAEMON-409`，**绝不 force-push 覆盖已移动分支**。**PR 分层**：环境中存在 PR 创建凭证（gh CLI 或 token）→ 开 PR；无凭证 → 响应携带 compare URL 且**成功返回**——机器不因缺 PR 凭证而失败（PR 凭证与 push 凭证是两个独立前提，只有后者是 `DAEMON-512` 的判据）。**安全面**：git push 凭证**复用 capsule 检出时即已要求的本机 git 身份**（SSH/credential helper），不新增任何凭证存储、不落任何凭证字节、不持久化任何机密；ship 只读封存 capsule、只推固定名分支，不触碰 aihub 凭证域（§5.3/§8）。发起者身份与 `pause`/`wrap` 同一裁定（`X-PF-User`）。CLI 入口 `pf ship <wi>` 转发本端点（分流表 `00-architecture.md#§5.1`） | `DAEMON-404` / `DAEMON-512` / `DAEMON-403` / `DAEMON-409` |
| `mode(get_recommendation)` | Batch 只读调用：取 AIHub 调度器给出的候选 WI 集合，供人确认，不产生任何 Job（`00-architecture.md#§3.4b`） | — |
| `batch_submit(wi_ids)` | Batch 提交：人确认（可增删）后的 WI 集合一次性创建 Job 并派生持久 WI Agent（`00-architecture.md#§3.4b`） | `DAEMON-507` / `CORE-*` 透传 |
| `mode(set/drain/abort-drain)` | 模式查询与切换（判据引用 `00-architecture.md#§3.4`，本文只做状态存取，Drain 的连续取批循环见 §6.1c） | `DAEMON-409` |
| `publish_request(wi_id, reason)` | 内部调用：WI Agent 在 `00-architecture.md#§4.3` 的某个触发时机上向本机 Daemon 发起一次全扫描发布请求；Daemon 机械执行（§6.4），响应携带该次发布的落定结果（成功的新 `seq` / 幂等命中的既有 HEAD）。`reason` 为收尾类型（pause/block/wrap/fail，`05-sync-revision-recovery.md#§6.1` 第 1 步的 final flush）时，响应同时携带 Daemon 本次生成的 `flush_evidence`，由 WI Agent 随收尾调用提交给 Core（§6.2 A 路径第 3 步、`02-aihub-core.md#§6.4`） | `DAEMON-503` / `SYNC-*` 透传 |
| `shutdown()` | 触发优雅停机（quiesce/shutdown，§6.5） | — |

`observe` 的两层协议（§5.1a）不构成第二个执行进度权威：事件流层的 `state_snapshot` 与事件都只是**活的视图**（事件不持久化，状态的唯一权威仍是 Capsule `runtime-state/execution.json`，经 §5.3 白名单或 Capsule 本地读取渲染），read-through 层渲染的永远是最新已发布 HEAD——这正是 `aihub#773`（R9，`07-deployment-pilot.md#§10.2`）修复的裁定在 Daemon 操作面上的具体体现。

**命令执行不在此列**：Daemon 操作面不提供任何「执行一条 step 命令」的端点——命令执行的唯一入口是 `03-execution-capsule.md#§5.1` `RunCommand`，调用方是 WI Agent，不是 Daemon（§2.4 INV-D2，§6.6 展开）。

**`pause`/`wrap` 的发起者身份**：与 `answer` 中继共用同一裁定——本机 UDS 操作面没有已认证用户层，发起者身份经自报的 `X-PF-User` 头传递（头缺省时降级为信任报文内自报身份，v1 已知的降级本地信任形态），收尾动作按该身份记录；本文不另设本地变体。

### 5.1a observe：两层观察协议 <!-- §5.1a -->

观察（observe）按「本机是否拥有该 WI 的 Capsule」分两层；两层都是短请求/响应，**无 SSE、无推送、无长连接**。

**第一层：本机拥有该 WI Capsule 的 WI（本机正在执行）——事件流。**

- Daemon 为每个 Job 维护一个**内存 ring buffer**：每 Job 上限 500 条事件，事件在各 Job 内**单调编号**（事件序号即 cursor 的推进依据）。
- 观察端点：`GET /jobs/{wi_id}/observe?after=<cursor>`（`after` 为观察者已收到的最后一条事件序号，首次传 0）。响应体固定三段 `{events, state_snapshot, next_cursor}`：`events` 为序号严格大于 `after` 的增量事件列表；`state_snapshot` 为该 WI 当前 workflow 执行状态的快照渲染（来源 Capsule `runtime-state/execution.json`）；`next_cursor` 为本次推进到的事件序号，观察者以其继续增量拉取。
- **cursor 掉出 ring buffer**（观察者落后超过 500 条，或 Daemon 重启后 ring buffer 已清空）：返回**不携带 `events`**、只含当前 `state_snapshot` 的响应并置 `compacted=true` 标记——观察者据快照重建全量视图，再以返回的 `next_cursor` 继续增量观察。
- **九种粗事件类型**（封闭集）：`job_started` / `wi_agent_rederived` / `step_started` / `step_completed` / `needs_input` / `answered` / `published` / `job_stopped` / `sync_failed`。其中 `needs_input` / `answered` 事件的载荷沿用 `00-architecture.md#§3.4a` 定义的中继/观察报文形态，本文不另设本地变体。
- **事件不持久化**：ring buffer 是「活的视图」，仅服务于增量观察；workflow 执行状态的唯一权威始终是 Capsule 的 `runtime-state/execution.json`（`00` INV-1、本文 INV-D1 同口径），事件丢失或截断不损失任何执行状态。

**第二层：本机无该 WI Capsule 的 WI（在别机执行或已结束）——read-through。**read-through 由 Daemon 经其 UDS 面提供：`pf observe` 调用 Daemon 面的 read-through 端点，响应携带 `read_through:true` 与 freshness 标注，渲染**最新已发布 HEAD** 的 `execution.json`（与 `02-aihub-core.md#§5.2` read-through 同口径，UI 必须标注「数据截至最近一次发布」）；`DAEMON-404` 仅在事件流层与本层都无结果时返回。**CLI 直走 `02` read-through（`02-aihub-core.md#§5.2`）有三条触发路径**：`pf observe --read-through` 显式跳过本机 Daemon 层、直接渲染已发布 HEAD；Daemon 面可达但对两层均无结果（答复 `DAEMON-404`）时 CLI 落回直走；Daemon 面不可达（本机未运行）时 CLI 回退直走。三条路径渲染的是同一已发布事实，直走不改变本层语义。执行中 WI 的实时增量走第一层，本层只呈现已发布事实，观察到的进度至多滞后一次发布时延。

**传输形态（两层共同）**：无 SSE、无服务端推送、无长连接——第一层是客户端驱动的单次 GET 增量拉取，第二层是单次 GET 渲染；Daemon 不为观察维持任何常开连接或后台推送任务。

### 5.2 错误码表（前缀 `DAEMON`，`00-architecture.md#§5` 分配） <!-- §5.2 -->

| 码 | 语义 |
|---|---|
| `DAEMON-404` | 对象不存在 |
| `DAEMON-403` | `proxy_call` 请求方身份与目标 WI 不符，或请求的 `operation` 不在 §5.3 白名单内（`PROXY_OPERATION_NOT_ALLOWED`）；或 `answer` 的 `answered_by` 与请求认证身份不一致（`00-architecture.md#§3.4a`） |
| `DAEMON-409` | 模式冲突（例如 Drain 中收到 `mode set` 之外的切换请求；或 Drain 模式下对单条 `claim`/`resume`/`takeover`/`batch_submit` 的拒绝——因为 Drain 自己的内部循环才是该模式下创建 Job 的唯一入口（§6.1c），这是模式互斥，不意味着 Drain 停止了创建 Job；或对不处于 needs_input 的 step 调用 `answer`，或 `answer` 的 `question_id` 不匹配/已被消费——比对/去重/过期规则权威在 `00-architecture.md#§3.4a`） |
| `DAEMON-410` | SHUTTING_DOWN：优雅停机期间拒绝一切外部提交调用（claim/resume/takeover/batch_submit 一律拒绝，§6.5 第 2 步）。Drain 模式的内部取 wave 循环不通过该错误码被拒绝——它在优雅停机触发的同一时刻由 `shutting_down` 标志直接停止取新的一批（§6.1c/§6.5 第 2 步），不再发起新的内部创建请求，因此不会经过这个面向外部调用者的错误响应；这不是「与 Drain 无关」，而是两种不同的阻断路径达成同一个结果：优雅停机期间任何来源都不再产生新 Job |
| `DAEMON-503` | WI Agent 派生失败（harness 适配器/进程不可用，转 `01-workflow-skills.md#§6.3` 基础设施失败），或收到的发布请求因本机资源问题（磁盘/句柄）无法进入扫描流程 |
| `DAEMON-507` | PREFLIGHT_FAILED：本机状态预检失败，**未发起 claim**（§6.1） |
| `DAEMON-511` | SHUTDOWN_FLUSH_FAILED：优雅停机时代 WI Agent 发起并执行全扫描发布多次重试仍不收敛，停机中止且不退出进程（§6.5），需人工介入排查同步障碍 |
| `DAEMON-512` | SHIP_PUSH_PREFLIGHT_FAILED（基线修订 polyforge#37）：`ship`（§5.1）的本机推送预检失败——无可用 git push 身份（SSH/credential helper）或 origin 不可达；推送未发起。与 PR 凭证缺失严格区分：后者不是错误（compare URL 回退后成功返回，见 §5.1 `ship` 行） |

### 5.3 WI Agent → Daemon authenticated local proxy（凭证最小暴露，呼应 `00-architecture.md#§5`/`#§8`、`01-workflow-skills.md#§5.1`） <!-- §5.3 -->

**存在理由**：WI Agent 自始至终不持有 AIHub API key 或 Revision 发布凭证（与 §8 一致），但 `pf-execute`
主循环需要调用一组有限的 WI 生命周期/只读操作（读取该 WI 的 workflow 引用与预算（`bundle_name`/
`bundle_digest`/budget 基线与有效上界）、写 step 结果到 Capsule、读写 Memory）。这些调用均经本地代理中转，WI Agent 本身从不持有任何可直接呼叫 AIHub
的凭证。

**接口**：`POST /run/polyforge.sock` 上的 `proxy_call(work_item_id, operation, payload)`，仅接受本机 Unix domain
套接字连接，连接方身份由内核 `SO_PEERCRED`（Linux）/`LOCAL_PEERCRED`（macOS）验证为本机上由
本 Daemon 派生的那个 `main_agent_pid`。

**允许操作集（白名单，不可扩展，不含 pin/发布）**：

| `operation` | 转发目标 | 说明 |
|---|---|---|
| `read_workflow_ref` | `02-aihub-core.md#§5.1` 的 WI 详情读取（GET）与 `GET /api/v1/work-items/{id}/budget` | 只读：返回该 WI 的 `bundle_name`/`bundle_digest`（WI 详情）与预算基线/各维度有效上界（budget）；Core 不存储 resolved steps（`00-architecture.md#§2.3` INV-8），不存在对它的读取变体，也不存在写入变体 |
| `report_step_result` | `02-aihub-core.md#§5.1` WI 生命周期写入（pause/block/unblock/wrap/fail 等）；其中收尾变体（`02-aihub-core.md#§6.4` 的四个结束 generation 的端点）必须携带 Daemon 经 §6.2 A / `05-sync-revision-recovery.md#§6.1` 生成的 `flush_evidence`，代理仅签名转发、不构造不校验证据内容 | 仅限发起方自己的 WI，参数 `work_item_id` 必须与本次连接对应的 Job 一致 |
| `memory_read` / `memory_write` | `06-memory-search.md#§5.1` | 仅限本 WI 上下文内的调用 |
| `publish_request` | 本文 §6.4 | 已在 §5.1 列为独立操作，此处仅列出以说明它同样不需要 AIHub 凭证 |

不在上表中的任何操作（包括 `01-workflow-skills.md#§4.6` 的创建/固化路径——它是 AIHub 创建事务内的
服务端内部步骤，本就没有可经代理转发的操作面、`05` 的
发布接口、`02` 的 secret 轮换/reauthorize）均被代理拒绝（`DAEMON-403 PROXY_OPERATION_NOT_ALLOWED`），
不存在“未列出则透传”的默认行为。`save_artifact`（方法论 artifact 写入）**不在 v1 白名单内**：
step 产物是 Capsule 内的文件，经 Revision 同步（`05`）到达 Core 可见面，v1 不存在经代理向 Core
转发 artifact 写入的面。

**认证与转发**：代理收到请求后，从 §4.1 的 `jobs[]` 中根据对端 pid 找到对应条目，取其
`work_item_id`/`generation`，用 Daemon 自己在 `daemon/credentials/` 中的 api_key 以及该 WI 当前有效的
owner generation token（`02-aihub-core.md#§5.1`）向 AIHub 发起实际调用；请求体中若携带与代理身份不符的 `work_item_id`，
拒绝为 `DAEMON-403`。响应内容逐字转发给 WI Agent，代理自己不缓存、不修改。

## 6. 核心流程（编号步骤，非伪代码） <!-- §6 -->

### 6.1 claim / resume / takeover：预检 → workflow 引用读取 → 意图持久化 → Job 创建 → 持久 WI Agent 派生与 pf-execute 注入 <!-- §6.1 -->

下列每一步与其紧跟的持久化动作都写入 §4.1 的 `daemon/state.json`，实现 spec 「不可逆动作前持久化」要求（claim idempotency
key、claim 响应、Job 创建意图、Harness session identity 均先于对应的不可逆外部动作落盘）；本文对
`daemon/state.json` 的一切写入（含 `pending_jobs[]` 的追加/回写/删除）一律走 tmp+fsync+rename 原子写（与
`03-execution-capsule.md#§4.2` 同一原子写模式）——「落盘」指 fsync 成功返回、rename 完成后才算完成，任一步
未完成即视为未写入：

1. Daemon 收到 `claim(wi_id)` / `resume(wi_id)` / `takeover(wi_id)` 请求，先做本机预检：workspace 根存在且可写、Daemon 数据目录可写、Capsule 根可写、磁盘余量高于阈值、AIHub 可达。任一项失败立即返回 `DAEMON-507 PREFLIGHT_FAILED`，**不发起**后续任何服务端调用——不产生「claim 成功却无本地现场」的情形。
2. 预检通过后，按 `00-architecture.md#§3.4` 的 Interactive/Batch/Drain 判据确认本次请求在当前模式下是否被接受（例如 Drain 模式下对单条 `claim` 一律 `DAEMON-409`，因为 Drain 只接受来自 §6.1c 内部循环的提交，不接受外部单条 claim；这与优雅停机的 `DAEMON-410 SHUTTING_DOWN` 拒绝外部提交是两回事——前者是模式互斥，Drain 本身仍在持续创建 Job，后者才是真正停止创建新 Job）。
3. **读取 workflow 引用（pin 已随创建事务完成，无独立确认步骤）**：经 `02-aihub-core.md#§5.1` 的 WI
   详情读取（GET，只读、不消耗任何 owner generation）取得该 WI 的 `bundle_name` 与 `bundle_digest`，供
   第 9 步作为 WI Agent 启动参数（可随第 4/5 步的 `pending_jobs[]` 记录与 claim 响应回写一并留存）；
   预算基线经同一端点表的 `GET /api/v1/work-items/{id}/budget` 按需读取。workflow 的校验与固化是 WI
   创建事务内的服务端内部步骤（`01-workflow-skills.md#§4.6`、`02-aihub-core.md#§4.5`），创建必先于
   claim，三列为 NOT NULL 且写入后不可变（`02-aihub-core.md#§2.6` INV-2e）——不存在「未完成固定的 WI
   可被 claim」的路径，因此本步**没有**任何 resolved-steps 存在性查询，也不存在对应的拒绝分支（原
   `DAEMON-425 WORKFLOW_NOT_PINNED` 已随之删除）；WI 不存在时本步以透传的 `CORE-404` 终止，不继续
   后续步骤。
4. **持久化 Job 创建意图**：生成本次 claim 的 idempotency key（不存在则新生，存在则复用同一 key 重试），
   向 `daemon/state.json.pending_jobs[]` 追加一条记录：`{work_item_id, idempotency_key, intent:
   "claim"|"resume"|"takeover", created_at, status: "intent_persisted"}`，**写磁盘完成后才进入下一步**。若
   Daemon 在本步之后、完成 claim 之前崩溃，重启后可从这条 `pending_jobs[]` 记录识别出未完成的 claim 意图，
   用同一个 `idempotency_key` 重试（呼应 `01-workflow-skills.md` 「claim 响应丢失」场景）。
5. 调用 `02-aihub-core.md#§5.1` 的 claim / resume（或 admin force-takeover）端点，携带第 4 步的 `idempotency_key`，取得本次的
   owner generation。**成功后立即把完整响应（含 `generation`/`generation_token`）回写到同一条
   `pending_jobs[]` 记录**，`status` 改为 `claim_confirmed`，**并等到这次写入 fsync 完成后才进入第 6
   步及之后的创建/派生序列**（D4：claim 事实先于一切派生动作落盘）。若响应在网络中丢失（Daemon 未收到但 Core 已提交），
   重试同一 `idempotency_key` 会命中 Core 侧的幂等缓存，恰好得到同一个 attempt，不产生重复 claim。
6. 调用 `03-execution-capsule.md#§6.1` `capsule_create(wi_id, generation)` 创建/复用/隔离重建 Capsule。
7. 按 §6.1a 的规则确定本次的 `harness_session_id`（新分配或崩溃续跑复用）。**先将它写入 `pending_jobs[]` 同一条
   记录**（`status` 改为 `session_persisted`，新增 `harness_session_id`/`harness_adapter` 字段）并
   **fsync 落盘完成后才派生进程**：
   这保证若派生成功但落盘 `jobs[]` 正式记录前 Daemon 崩溃，重启后能从 `pending_jobs[]` 识别出这个孤儿
   意图（带着已知的 session id）并按 §7.1 的存活检测重新连接或回收，不产生无人认领的进程。
8. **读取并注入 `pf-execute` 初始指令（注入协议的来源侧）**：Daemon 从**自身 checkout/发行物**读取
   `skills/pf-execute/SKILL.md`（`00-architecture.md#§3.1`/§3.6、`01-workflow-skills.md#§2.1`），作为即将
   派生的 WI Agent RPC 会话的初始指令；注入时机即第 9 步的派生时刻。其版本标识与 Daemon 发行版
   绑定——`pf-execute` 与 Daemon 同一份发行物交付，升级 Daemon 即升级 executor，不存在独立于
   Daemon 的 pf-execute 版本协商面。任何 harness（Pi/Claude）都**不安装** `pf-execute`：二者在 vNext
   中仅作为宿主适配器承载该初始指令（注入方式见 §6.1a）。本步**不经 §5.3 本地代理**——该代理的对端
   是已派生的 WI Agent（`SO_PEERCRED` 校验 `main_agent_pid`，§5.3），而此刻 WI Agent 尚未派生。
9. 按第 7 步确定的 harness 适配器和 session id 派生**持久** Harness RPC WI Agent 进程（`00-architecture.md#§3.6`），把
   Capsule 路径、**claim 响应携带的** `bundle_name`+`bundle_digest`+预算基线（Core 在 claim 事务内随 generation/token 一并返回——因此 Daemon 与 WI Agent 都**不需要任何数据面详情读取**即可完成执行路径：机器的执行面授权由 key scope 与 claim 端点承载，数据面 membership 不可见性不构成障碍，`02-aihub-core.md#§8` 执行面规则；运行期 WI Agent 经 §5.3 白名单的 `read_workflow_ref` 按需读取仍可用）、harness session id 作为启动参数传入，并按
   §6.1a 的注入方式把第 8 步读取的 `pf-execute` 初始指令注入该 RPC 会话；WI Agent 启动后运行同一份
   `pf-execute` 主循环（不因 claim/resume 还是 takeover 而分叉），其**第 0 步**从本机 checkout 物化
   `workflows/<bundle_name>/` 并按 `bundle_digest` 校验（`03-execution-capsule.md#§4.5`）后加载
   `workflow.md` 正文驱动执行，并在其整个生命周期内持续存活、驱动全部 step（不因单个 step 结束而
   退出，INV-D4）。Daemon 不向 WI Agent 注入任何步骤集——Core 不存储 resolved steps
   （`00-architecture.md#§2.3` INV-8），步骤图的唯一来源是 WI Agent 第 0 步按 digest 校验过的本机
   物化产物。
10. 在 §4.1 的 Daemon state 中追加一条正式 `jobs[]` 记录：`{work_item_id, capsule_id, generation,
    harness_adapter, harness_session_id, main_agent_pid, main_agent_started_at, status: "running"}`，
    并删除对应的 `pending_jobs[]` 记录（意图已完成，不再需要）。
11. 启动该 Capsule 的 watcher（§6.3）。

**孤儿回收**：若重启后发现 `pending_jobs[]` 中存在 `status="session_persisted"` 但无对应 `jobs[]` 条目的记录，
按其 `harness_session_id` 尝试在该 Capsule 内查找具有相同 session 标识的持久进程（通过 §7.1 的 pid+创建时间戳比
对）：找到则补写 `jobs[]` 并清理 `pending_jobs[]`；未找到则按 §7.1 的“已死”分支重新派生（复用同一 session
   id）。

### 6.1a Harness 适配器的启动参数（实现细节，最小契约权威在 `00-architecture.md#§3.6a`） <!-- §6.1a -->

Daemon 在第 6 步派生持久 WI Agent 时，按该 WI 配置选定的 harness 决定启动方式：

- **Pi 适配器**：以 `pi --mode rpc` 启动，进程在整个 WI 生命周期内不退出；`harness_session_id`
  在 claim（或 resume——见下）时生成一次（例如 `sess_<wi_id>_<generation>`），此后**仅当同一
  generation、同一台机器发生 Daemon/WI Agent 崩溃续跑（§7.1）时**复用同一个值；`resume` 因
  `pause` 已结束该 generation（`02-aihub-core.md#§2.3`/§2.5），恒为新一次 claim，须按新 generation **重新
  分配**一个全新的 `harness_session_id`，不延续旧值；`--mode rpc` 进程的 session 目录参数指向
  该 WI Capsule 内的会话目录（Capsule-local session-dir，路径约定权威在
  `03-execution-capsule.md`），不指向 Capsule 之外的共享目录。
- **Claude 适配器**：以 `claude -p` 起持久会话，崩溃续跑时以 `claude --resume` 接续同一 session（`harness_session_id`
  同样在 claim（或 resume——按新 generation 重新分配）时生成一次，**仅在同一 generation、同一台
  机器的崩溃续跑之间复用**（§7.1）；resume 视为新 generation，不延续旧 session。该适配器是否
  暴露对其派生 step agent 的 introspection 接口取决于所用 harness 版本——Daemon 的派生/监督/回收
  逻辑（§6.2/§7）不依赖该接口存在，只依赖标准进程句柄（pid、退出码、stdout/stderr、进程组）。
- 两条适配器共用同一套 Daemon state 记录结构（§4.1）与同一套崩溃续跑规则（§7.1）；新增第三条适配器时只需在此新增一个分支，不改变 §6.1/§6.2/§7 的流程结构。
- **`pf-execute` 初始指令的注入方式（注入时机 = §6.1 第 9 步派生时）**：第 8 步读取的
  `skills/pf-execute/SKILL.md` 内容在两条适配器上分别经以下通道成为 WI Agent RPC 会话的初始
  指令——**Pi 适配器**：`pi --mode rpc` 起持久会话（稳定 session id、Capsule 内 session dir，见上）后，以**首条 prompt** 注入，先于该会话内任何其它消息；注入内容 = `pf-execute` 全文 + bundle 引用（`bundle_name`、`bundle_digest`、预算基线）+ 本 capsule 路径；崩溃重派时以同一 session id 重新起会话并按同一协议重注入（§7.1）；
  **Claude 适配器**：以**追加进 system prompt** 的方式注入 `pf-execute`（不是一条会被后续消息冲刷的 user
  message），bundle 引用与本 capsule 路径随 §6.1 第 9 步的启动参数传入。两条通道都只在派生时注入一次；注入内容的版本标识与 Daemon 发行版绑定（升级 Daemon
  即升级 executor），任何 harness 不安装 `pf-execute`——Pi/Claude 仅作为宿主适配器，其自身的 skill
  安装面与本注入无关（`00-architecture.md#§3.1` 安装面裁定）。§7.1 崩溃续跑重新派生时按同一协议从
  Daemon 当时发行物重读并再次注入。

### 6.1b Batch：调度器推荐 + 人确认 + 成批提交 <!-- §6.1b -->

1. 路由 Skill/CLI 调用 `mode(get_recommendation)`：Daemon 转发到 AIHub 现有调度器（`02`/ready
   queue）取一批候选 WI，作为**推荐**返回给人，此步骤**不创建任何 Job**，Daemon 不据此自行派生
   WI Agent。
2. 人对该推荐集合确认（可增删），得到最终要提交的 WI 集合。
3. 路由 Skill/CLI 调用 `batch_submit(wi_ids)`：Daemon 把确认后的整批 WI **全部入 §6.1d 的 pending**
   （不拒绝整批，也不按 `wi_ids` 传入顺序例外派工），派生一律按 §6.1d 的 priority 降序 + 同级 FIFO
   与并发上限进行，空出配额按同一排序自动递补——每个被派生的 Job 各走一遍 §6.1 的
   claim→Capsule→持久 WI Agent 序列；wave 的边界即人确认后传入的 `wi_ids`，Daemon 不自行扩大或缩小。

### 6.1c Drain：自动连续取批 <!-- §6.1c -->

`mode(drain)` 开启后，Daemon 进入一个内部循环（不是对外暴露的轮询端点）：反复调用 AIHub 现有
调度器的 ready queue 取下一批可执行 WI，取到后直接对每个 WI 走 §6.1 的 claim→Capsule→持久
WI Agent 序列，无需逐条人工确认；取不到候选时空闲等待并再次轮询（这是 Drain 模式本身的空闲
状态，不是优雅停机，`00` INV-13）。**该内部循环在每次取批前都会检查 Daemon 的 `shutting_down`
标志**（§6.5 第 1 步置位）：一旦该标志置位，循环立即停止取下一批，不再向 AIHub 调度器发起新的
ready queue 请求，也不再创建新 Job——这是优雅停机对「禁止一切来源创建新 Job」判据的一部分（§6.5），
与 `mode(abort-drain)` 是两个不同的停止入口（前者是外部显式撤销 Drain 模式本身；后者是优雅停机
触发的、对 Drain 循环同样生效的强制停止），概念仍然不合并：Drain 模式本身、以及它是否处于优雅
停机进程中，是两个独立的状态维度。`mode(abort-drain)` 结束该循环，不影响已经在跑的 Job；`mode
(set)` 切到其它模式对已跑 Job 同样没有追溯效力（§6.7）。

### 6.1d Job 队列（jobq）：簿记、并发上限、pending 排序与补位 <!-- §6.1d -->

- **jobs[] 是簿记不是执行状态**（INV-D1 同口径）：§4.1 `daemon/state.json` 的 `jobs[]` 记录「本机当前在管哪些 Job」；已接受、尚未派生的排队条目记录在 pending 部分，同样只是簿记，不含任何 workflow 执行状态字段。
- **并发上限**：`max_concurrent_jobs` 默认 4（同一机器，跨 Interactive/Batch/Drain 共享一个计数器，即同时在跑的 Job 数上限），可通过 `mode(set, max_concurrent_jobs=<n>)` 覆盖（§12 第 0 项）。
- **pending 排序**：按 WI priority 降序——`urgent > high > normal > low`；同一 priority 内 FIFO（按入队时间先后）。
- **无抢占**：已在跑的 Job 不会被后到的高优先级 WI 挤掉或降级；上限只约束**同时派生**的 Job 数，对已派生的 Job 没有追溯效力。
- **补位**：一个在跑 Job 到达终态（释放配额）后，Daemon 从 pending 按上述排序取下一个补位派生（走 §6.1 同一序列），无需人重新确认（Batch wave 的内容边界仍由人给定，§6.1b；本项只决定「wave 内谁先派生、空出配额谁递补」）。
- **三模式同一规则**：Interactive（单条 `claim`/`resume`/`takeover`）、Batch（`batch_submit` wave，§6.1b）与 Drain（内部取批，§6.1c）三种来源产生的排队条目共用同一 pending 队列与同一套排序/补位规则，不存在任何按来源或按传入顺序的例外（§12 第 0 项）。

### 6.2 Job 回收（seal）：正常结束与 stale/takeover 是两条不同路径 <!-- §6.2 -->

Job 回收的触发原因分两类，机械动作**不同**，不共用同一套「等待发布」步骤：

**A. 正常结束**（WI 到达 pause / wrap，或优雅停机中断收尾——即 §6.5 第 4 步已经完成过一次代发起的边界发布之后的收尾）：此时该 WI 的 owner generation 仍然有效（没有被别的角色夺走），发布仍然是在这个仍然 active 的 generation 下完成的，因此顺序是「先边界发布、再结束 generation」：

1. 停止该 Capsule 的 watcher。
2. 等待（或确认已完成）该 WI 的边界全扫描发布（`00-architecture.md#§4.3`；发起者是该 WI 的 WI Agent 本身，或 §6.5 第 4 步下由 Daemon 代其发起并直接机械执行）——这一步必须先于下一步：generation 此时还没有结束，发布仍然发生在同一个 owner generation 之内。
   收尾边界上的这一次发布即 `05-sync-revision-recovery.md#§6.1` 定义的 **final flush**：Daemon 按 `05-sync-revision-recovery.md#§6.1` 的生成序列获取
   write freeze、执行冻结扫描、比对 HEAD（一致则本 generation 无需新 Revision，不一致则先完成一次
   真实发布推进 HEAD），并**据此生成 `flush_evidence`**——D6 裁定：final flush 证据由 Daemon 生成，
   WI Agent 只负责发起收尾本身，证据的事实性字段全部来自 Daemon 侧写冻结后的一次真实扫描，不允许
   调用方自报了事。证据字段（`published_generation`/`published_seq`/`manifest_sha`/
   `frozen_checkpoint`，含无 HEAD 空链情形的哨兵形态）与 `02-aihub-core.md#§6.4` 的权威定义逐字一致，生成协议的细节
   本文不重复（`05-sync-revision-recovery.md#§6.1` 权威）；正常路径下它作为 `publish_request` 响应的一部分交回发起的 WI Agent
   （§5.1），优雅停机路径下由 Daemon 自己持有。
3. 边界发布确认、`flush_evidence` 生成完成后，结束该 WI 的 generation：收尾调用（`02-aihub-core.md#§5.1` 的
   `pause`/`block`/`wrap`/`fail`）**必须携带该 `flush_evidence`**（`02-aihub-core.md#§6.4`：缺失或校验不过被 Core 拒为
   `CORE-425 FLUSH_REQUIRED`，Daemon 侧不重试、不代报自造证据）。正常路径下由 WI Agent 经 §5.3 本地
   代理发出（代理用 Daemon 持有的 owner generation token 签名，§8），优雅停机路径下由 Daemon 直接发出——
   无论哪条路径，证据都由 Daemon 生成；只有到这一步之后，该 generation 才算真正结束。
4. 从 Daemon state 的 `jobs[]` 中移除该 WI 的记录。
5. 回收 WI Agent 进程（此时通常已随 §6.5 第 3 步的边界通知自然退出；若未退出，按超时策略终止其进程组）。
6. Capsule 目录本身不删除——按 `03-execution-capsule.md#§2.1` 封存为只读现场，等待恢复或走 `05-sync-revision-recovery.md#§2.10` 删除保护流程清理。

**B. cancel / stale（被 force-takeover 夺走）**：这条路径的前提就是旧的 owner generation 已经、或即将变得不再权威——force-takeover 已经在 Core 侧把 owner generation 交给了新的一方（`02-aihub-core.md#§5.1`），cancel 同样是把该 WI 从当前 owner 手中收回；因此这条路径**不等待、不重试**旧 generation 名下的任何全扫描发布，也**不声称**该收尾动作是在一个仍然 active 的 generation 下完成的发布——旧 generation 在这条路径进入的那一刻已经视为失去权威，不给它补发布的机会：

1. 停止该 Capsule 的 watcher。
2. **直接终止**该 Job 原有的 WI Agent 进程及其整个进程组（不等待其到达任何「安全边界」，不像 §6.5 第 3 步那样发送停止推进通知再等待）——旧 generation 已经/即将失效，继续跑下去的产出不再具备发布资格。
3. 对该 WI 的本地 Capsule 目录做**本地 quarantine**（标记为隔离现场，不同于 `03-execution-capsule.md#§6.1` 为新 generation 重新创建的干净 Capsule；隔离目录保留供人工排查，不参与后续任何自动续跑或发布路径）。
4. 从 Daemon state 的 `jobs[]` 中移除该 WI 的记录。
5. 若后续有新的 WI Agent 为该 WI 的新 generation（takeover 场景）派生，走 §6.1 的正常 claim/takeover 序列，从最近一次已发布 Revision 继续，与被 quarantine 的旧现场无关；cancel 场景则该 WI 到达终态，不再派生新 Job。

两条路径共用的不变量：无论走哪一条，Daemon 都是「机械执行发布」的唯一角色（INV-D3）；A 路径下它执行的是一次真实发布（或一次确认无需发布的冻结扫描）并生成 `flush_evidence` 随收尾调用提交（`02-aihub-core.md#§6.4` 要求的正是这份证据），B 路径下它明确地**不**执行任何发布、也**不**生成/携带任何 `flush_evidence`（`02-aihub-core.md#§6.4` 只约束 end-generation 端点的 pause/block/wrap/fail 四个 reason；force-takeover 不需要证据，`02-aihub-core.md#§6.2`，cancel 则只作用于无活跃 generation 的 WI，`02-aihub-core.md#§6.5`）——不存在「默认仍然发布」的第三种模糊路径，也不存在「无发布却补证据」的通道。

### 6.3 驱动 watcher（只做 dirty 标记，`00-architecture.md#§4.3`/INV-11 的实现） <!-- §6.3 -->

Daemon 为每个活跃 Capsule 启动一个 watcher（进程内协程或子进程均可，实现细节不影响协议）：watcher 监视 Capsule 目录（尤其 `runtime-state/`、`repos/`、`artifacts/`）的文件系统变化，检测到变化时**只**把该 Capsule 标记为 dirty（一个布尔位 + 时间戳），写入 `05` 定义的暂存结构。watcher **不**调用发布路径，也**不**定时轮询后自主发布——它不构成 `00-architecture.md#§4.3` 四类触发时机之外的第五种触发时机；是否需要发布、何时发布，只在下面四类事件发生时由 WI Agent（或代其行动的 Daemon）发起请求，再由 Daemon 机械执行（详细协议见 `05-sync-revision-recovery.md#§2.4`/§6）。

### 6.4 全扫描发布的机械执行（`00-architecture.md#§4.3`/INV-11/INV-D3 的实现） <!-- §6.4 -->

`00-architecture.md#§4.3` 定义的四类触发时机（+可选 milestone）中，「谁发起请求」与「谁机械执行」是两件事，本文
只定义后者：

| 触发时机 | 请求发起者 | Daemon 收到请求后做什么 |
|---|---|---|
| step 完成 | WI Agent | 调用 `publish_request(wi_id, reason="step_completed")`；Daemon 对该 Capsule 执行一次实际扫描（比对 dirty 标记与 Capsule 当前内容）、打包变更、调用 `05-sync-revision-recovery.md#§5` 发布接口推进 HEAD |
| step 中断 | WI Agent | 同上，`reason="step_interrupted"` |
| WI 出最终结果 | WI Agent | 同上，`reason="wi_outcome"` |
| Daemon 优雅停机 | Daemon（自己） | 不经操作面调用，直接在停机序列内部对每个仍活跃的 Capsule 触发并执行一次（§6.5 第 4 步） |
| milestone（可选） | WI Agent | 同上，`reason="milestone"` |

Daemon 收到一次发布请求后的机械动作恒定不变，与触发时机无关：扫描该 Capsule 自上次已发布
Revision 以来的全部变更（不仅是 dirty 标记覆盖的部分——dirty 标记只是「值得扫」的提示，扫描本身
仍是全量的，`05-sync-revision-recovery.md#§2.4` 权威协议）、打包为一个新 Revision、调用 `05-sync-revision-recovery.md#§5.1` 的发布接口尝试推进 HEAD
（受 fencing 约束，`05-sync-revision-recovery.md#§2.9`）。WI Agent 本身不持有直接调用 `05` 发布接口的凭证或路径——它能
做的只是「发起请求」，实际写 Revision/HEAD 一定经过 Daemon（INV-D3）。这一收敛点是 vNext 系统中
WI Agent 与 Daemon 之间唯一的同步接缝，其余时间两者各自独立运作。

### 6.5 优雅停机（quiesce/shutdown）：实现序列（`00-architecture.md#§3.5` 权威定义的落地，与 Drain 严格区分，INV-13） <!-- §6.5 -->

1. Daemon 进程收到终止信号（SIGTERM/SIGINT）或人工发起的 `shutdown()` 请求后，**第一时间**（先于任何其它动作）将进程内的 `shutting_down` 标志置位。
2. 该标志置位的同一时刻产生两个直接后果，二者一起构成「禁止一切来源创建新 Job」：
   a. 停止接受外部提交：新的 claim/resume/takeover/batch_submit 请求一律拒绝（`DAEMON-410
      SHUTTING_DOWN`）。
   b. **立即停止 Drain 的内部取 wave 循环**（§6.1c）：该循环在每次取批前检查 `shutting_down`，标志
      置位后不再向 AIHub 调度器发起新的 ready queue 请求，不再取新的一批，也就不再创建新
      Job；这不经过 `DAEMON-410`（Drain 循环不是外部调用者，没有响应体可拒绝），是同一标志
      驱动的另一条独立阻断路径。
   这一步与 Drain 概念上仍然是两回事：Drain（`00-architecture.md#§3.4c`）作为一种执行模式的定义、判据、
   `mode(abort-drain)` 撤销入口都不变；优雅停机只是在自己的收尾序列里，把「阻止新 Job」这件事
   同时套用到外部提交和 Drain 内部循环两个来源上，不把两者合并成同一个状态机（§2.3、§6.1c）。
3. 对每个当前活跃的 Job（此时已不会再有新 Job 加入，集合从这一刻起只减不增）：向其 WI Agent 发送停止推进通知（Daemon 本身不是 step 派发者——step
   之间的推进由该 WI Agent 自己的 `pf-execute` 主循环驱动，`00-architecture.md#§3.6`；Daemon 能做的只是通知
   它在下一个安全边界停止继续推进，不能代它选择或跳过某个 step）；等待该 WI Agent 到达一个可安全
   中断的边界。

   **“安全边界”的精确定义（权威裁定，修复 B14 OQ6：同时包含 step 边界与同步边界，不是二选一）**：
   安全边界是下列两个条件均满足的时刻（两者递进，不是任选其一）：
   a. **step 边界**：当前 step 已到达 `done`（业务判定完成），或已因 `01-workflow-skills.md#§6.3` 定义的基础设施
      失败中断（`in_progress` 但无活跃子进程）；此时 WI Agent 不再派发新的 step 或新的
      step agent。
   b. **同步边界**：上一次发布请求（若有）已完全结束（HEAD 成功推进或失败消歧已完成，不存在悬而
      未决的 publish mutex）；若此时存在一个尚未完成的发布，必须先按 `05-sync-revision-recovery.md#§2.8` 的幂等消歧
      规则完成它才能进入下一步。

   两者都满足后，才能进入本节第 4 步的“代发 final flush”；仅满足 step 边界而不满足同步边界
   （例如上一次发布仍在等待 AIHub 响应），优雅停机序列在该 Job 上继续等待，不提前发起新一轮
final flush（避免与尚未消歧的发布竞争同一个 publish mutex）。
4. 每个 Job 到达边界后，Daemon **代**该 WI 的 WI Agent 发起一次 `00-architecture.md#§4.3` 定义的发布请求，
   并**直接机械执行**（不经过 §5.1 的操作面往返，因为发起者与执行者是同一个 Daemon 进程）——这
   是优雅停机对四类触发时机中「Daemon 优雅停机」一类的具体实现：由 Daemon 代替仍在进行的 WI
   Agent 各触发并执行一次，不要求 WI Agent 自己还活着才能发布。本步机械执行的前提是先获得 `05-sync-revision-recovery.md#§2.7` 定义的 write freeze：只有当该 Job 的 WI Agent 本身、它派生的**全部** step agent、它曾启动的全部**命令子进程**，以及整个 **Native runner 进程组**（`03-execution-capsule.md#§5.1`/§7）都
   已经全部停止产生新写入（本步 3 的安全边界确保了 WI Agent 自身已到达可安全中断点，但 write
   freeze 的获得仍须按 `05-sync-revision-recovery.md#§2.7` 的确认/进程组全部终止判定执行，不得仅凭顶层 WI Agent 已退出就
   假设安全），且代该 Job 行事的新 WI Agent（若已在被派生）尚未开始产生任何写入；满足这两个
   条件后 Daemon 才开始扫描该 Capsule 的同步根并把字节固化进 `05-sync-revision-recovery.md#§2.7` 定义的不可变 staging；
   整个发布过程（获取 write freeze → 扫描 → staging 固化 → 释放 write freeze → 块上传 →
   manifest 提交 → HEAD 推进的成功或失败处理全部完成）自始至终持有该 WI 的 **per-WI publish
   mutex**（`05-sync-revision-recovery.md#§2.7`），不与同一 WI 的其它发布请求并发。这一次代发的 final flush 同样按 `05-sync-revision-recovery.md#§6.1`
   生成 `flush_evidence`（字段与 `02-aihub-core.md#§6.4` 一致；证据的事实性字段来自本次写冻结后的扫描，不是
   WI Agent 自报），随后的 generation 结束按 §6.2 A 路径第 3 步执行——收尾调用由 Daemon 直接发出
   （它本就持有该 WI 的 owner generation token，§8），此后才算完成 §6.2 A 路径的收尾。
5. 全部活跃 Job 完成上述收尾后，Daemon 进程退出。
6. 优雅停机之后，未完成的 WI 保持其 Capsule 现场不变；该 WI 的 generation 已随 §6.2 A 路径的
   收尾流程结束（`02-aihub-core.md#§2.3`/§2.5：优雅停机中断收尾与 pause/wrap 同属正常结束，均结束当前活跃
   generation），因此下次**任意机器（含原机器）**上的 Daemon 为该 WI 派生新的持久 WI Agent
   时，走的是 §6.1 的正常 claim/resume 序列（**新 generation**），按新 generation 分配**全新**
   的 harness session id，再按 `05-sync-revision-recovery.md` 的 takeover 恢复语义从最近发布的
   Revision 继续——**不**延续、也不跨机复用优雅停机前的 session；这与 §7.1 描述的「同一台机器、
   同一 generation 内 Daemon/WI Agent 崩溃续跑复用同一 session」是两种不同的场景，本文不重复
   定义 Revision 恢复协议本身。

**与 Drain 的区分在实现层的体现**：Drain（`00-architecture.md#§3.4c`）没有可执行 WI 时**空闲轮询**、不退出进程；
优雅停机没有「空闲」状态——它从收到终止信号那一刻起就是一次性的收尾+退出序列，序列本身不可逆。
Drain 有「撤销」操作（`mode(abort-drain)`，§6.1c）；优雅停机没有对应的「撤销」概念，因为它是
进程终止，不是可恢复的模式。

### 6.6 命令执行入口：Daemon 不执行 step 命令（INV-D2 的实现） <!-- §6.6 -->

Daemon 在整个 Job 生命周期中**不直接执行任何 step 命令**。命令执行的唯一入口是 `03-execution-capsule.md#§5.1` 的 `RunCommand`，调用方是该 WI 的持久 WI Agent；Daemon 的职责止步于：

1. 派生持久 WI Agent 进程（§6.1 第 6 步）——此后 WI Agent 自行决定何时对哪个 step 调用 `RunCommand`，全部在同一个进程内完成。
2. 通过标准进程句柄观察 WI Agent 的存活与输出（`00-architecture.md#§3.6`），不解析其内部对 `RunCommand` 的调用细节。
3. WI Agent 进程退出（正常或崩溃）时按 §7 处理回收或续跑。

这一分工的直接后果：Daemon 崩溃重启不会丢失「命令正在执行到哪一步」的信息——那份信息只存在于 Capsule 的 `runtime-state/execution.json`（`03-execution-capsule.md#§4.2`），不存在于 Daemon 侧；Daemon 重启后只需要重新发现「哪些 WI Agent 还活着/已死」（§7），不需要重放任何命令执行现场。

### 6.7 模式存储与切换（实现层；判据权威在 `00-architecture.md#§3.4`） <!-- §6.7 -->

- 当前模式存储在 §4.1 Daemon state 的 `mode` 字段，Daemon 进程内存中同时持有一份供快速判定。
- 模式切换请求（`mode(set/drain/abort-drain)`，§5.1）先由 `00-architecture.md#§3.4` 的判据决定是否合法（例如 Drain 期间只接受「撤销 Drain」），合法后原子更新 `daemon/state.json` 的 `mode` 字段（tmp+fsync+rename，与 `03-execution-capsule.md#§4.2` 同一原子写模式）。
- 模式切换**不**触发任何 Capsule 级别的操作（不重建、不封存）——它只改变「后续新请求按哪套判据准入」，对已经在跑的 Job 没有追溯效力（`00-architecture.md#§3.4` 已声明的既有规则，本文不重复其判据本身，只说明存储层不引入额外副作用）。优雅停机的「拒绝新 Job」开关（§6.5 第 2 步）与 `mode` 字段是两个独立状态，不互相覆盖。

## 7. 并发与崩溃恢复 <!-- §7 -->

### 7.1 Daemon 自身崩溃重启后的恢复（权威流程） <!-- §7.1 -->

1. Daemon 进程重启，读取 `daemon/state.json`（§4.1），得到重启前记录的 `jobs[]` 列表——每条含 `work_item_id`、`capsule_id`、`generation`、`harness_adapter`、`harness_session_id`、`main_agent_pid`、`main_agent_started_at`。
2. 对 `jobs[]` 中的每一条，做**存活检测**：
   a. 检查 `main_agent_pid` 对应的操作系统进程是否仍存在（`kill -0` 等价检查）。
   b. **pid 复用防护**：若进程存在，进一步比对该 pid 当前的进程创建时间戳与记录的 `main_agent_started_at` 是否一致（Linux 可读 `/proc/<pid>/stat` 的启动时间，macOS 可用 `ps -o lstart= -p <pid>`）。时间戳不一致 ⇒ 该 pid 已被无关的新进程复用，视为 WI Agent 已死，不得当作存活处理。
3. **存活**（pid 存在且创建时间戳匹配）：不重新派生，**重接**该 WI Agent——Daemon 崩溃重启丢失的只是自己进程内的监督连接与 RPC 通道，不是该 WI Agent 本身（它是长驻进程，`00-architecture.md#§3.6`，从未随 Daemon 一起退出）；按记录的 `harness_session_id` 与 `harness_adapter` 重建对它的 RPC 连接与标准进程句柄观察（pid、退出码、stdout/stderr、进程组），重接不打扰其运行、不重置其 session、不重复派发任何 step；把该 Job 保留在内存中的活跃集合，`harness_session_id` 不变。
4. **已死**（pid 不存在，或存在但创建时间戳不匹配）：**先回收残留进程组再重派**——WI Agent 进程已死，但其进程组内可能仍有孤儿存活（它派生的 step agent、经 `03-execution-capsule.md#§5.1` 启动的命令子进程），继续写入会违反 `03-execution-capsule.md#§7` 的单写者域并与新进程的写入短暂并存；Daemon 先向该 Job 记录的进程组（以 `main_agent_pid` 为组长派生，§6.1a）发 `SIGTERM`、超时仍存活则 `SIGKILL`（与 `03-execution-capsule.md#§6`/§7 的进程组回收口径一致），确认无残留写入者后，才按 `03-execution-capsule.md#§7` 定义的 WI Agent 崩溃续跑规则、按 §6.1a 记录的同一 `harness_adapter`，用**同一个** `harness_session_id`（不重新分配）重新派生一个新的持久 WI Agent 进程——重新派生走 §6.1 第 8/9 步的同一注入与参数序列（从 Daemon 自身 checkout/发行物重读并注入 `pf-execute` 初始指令，传入该 WI 的 `bundle_name`+`bundle_digest` 与同一 session id）；新 WI Agent 的第 0 步**不重新读取本机 checkout**（同代快照只读，`01-workflow-skills.md#§2.2` INV-1a/`03-execution-capsule.md#§4.5`）：同 generation 崩溃续跑**复用本机已物化的快照**（Capsule 内 `.pi/agents/` 与 `workflow.md` 副本仍在上次派生时物化的原样），仅当快照**缺失**（上次物化从未成功落盘）才按 `03-execution-capsule.md#§4.5` 从 checkout 物化——快照存在而与 pin digest 不一致时**告警、继续用既有快照**（该快照是本代已被接受的执行基准，不因 checkout 后续漂移而更换；跨代/跨机重建才按 pin digest 重新物化），随后读取该 Capsule 的 `runtime-state/execution.json`，从 `current_step`/`steps.<current_step>.status` 决定原地重派当前 step 还是推进到下一个 step（`03-execution-capsule.md#§7` 权威流程，本文不重复其判定细节），并更新 `daemon/state.json` 中该条目的 `main_agent_pid`/`main_agent_started_at`。
5. 若某条 `jobs[]` 记录对应的 `capsule_id` 目录已不存在（被外部误删）：记为异常，不自动重建，转人工介入（不同于「WI Agent 已死但 Capsule 现场完好」的正常续跑路径）。
6. 恢复完成后，Daemon 按正常职责继续驱动 watcher 与后续触发时机的机械发布。

### 7.2 其它并发面 <!-- §7.2 -->

- 心跳：Daemon 每 60s 调用 `POST /api/v1/machines/heartbeat`（`02-aihub-core.md#§4.10`/§5.1 行 21）：心跳 body 携带该 Daemon 当前的 Job 执行模式（`mode`）字段，Core 以同一 upsert 把机器模式随 `last_heartbeat_at`/`runtime_version` 一并同步（注册即首次心跳，`02-aihub-core.md#§4.10`）；心跳停止超过阈值即被 Core 标记 stale（`02-aihub-core.md#§7`），由人决策是否 force-takeover——Daemon 侧**不**自动重新认领本机以外机器上的 WI，避免与 `05-sync-revision-recovery.md#§2.9` 接管窗竞态。
- 双 Daemon 同机：本机 Unix domain socket（`/run/polyforge.sock`）绑定失败即退出（单实例锁），不存在两个 Daemon 同时管理同一机器的情形。
- 同 Capsule 并发：`03-execution-capsule.md#§7` 已保证同一时刻至多一个 step agent 在跑；Daemon 侧不引入第二套并发控制。
- 发布请求的并发：同一 Capsule 的两次发布请求（例如 step 完成的请求与随后紧接的优雅停机代发请求）由 Daemon 串行处理，不并发扫描同一 Capsule，避免产生互相覆盖的 Revision。

### 7.3 rollback 专用现场隔离与重建（与 §7.1 崩溃续跑为两条独立路径，不共享判据） <!-- §7.3 -->

触发条件：本机 Daemon 持有的某 WI token 的写请求收到 `CORE-403`，且响应体携带回滚作废专用的
`reason=secret_rolled_back`（`02-aihub-core.md#§5.3`：区别于 `mode="grace"` 轮换宽限期过后的
`reason=secret_rotated_expired`）——即 `02-aihub-core.md#§6.6` 的
`mode="rollback"`：epoch `retired_at` 无宽限；不存在独立 whoami 端点可查，
判定材料全部来自失败写请求的 403 响应体。与 §7.1 的普通崩溃、与 `mode="grace"` 的宽限期轮换均不同）。命中时：

1. **MUST NOT** 调用 `02-aihub-core.md#§6.6` 的 `reauthorize`（那是宽限期内的重新签发，不适用于立即作废的
   rollback），也 
**MUST NOT** 把该 token 失效当作普通接管竞态转发给其他机器。
2. 立即向该 WI 的持久 WI Agent 发送终止信号，等待其退出后向整个进程组发 `SIGTERM`，超时仍存活则
   `SIGKILL`（与 `03-execution-capsule.md#§6` 进程组终止规则一致）。
3. 将该 Capsule 目录**本地 quarantine**（重命名为 `capsules/<capsule_id>.quarantined.<ts>/`，不删除，俛信审计），
   从 `daemon/state.json` 的 `jobs[]` 中移除对应条目。
4. 不主动对该 WI 发起任何重新 claim/resume（**明确不自动重 claim**）：是否重新执行由人决定——若该 WI 在
   Core 侧仍有活跃 generation 行（回滚备份恢复所致：其原持有方的 token 已被立即作废，无法再调用
   `end-generation` 收尾），该行的**唯一仲裁路径是 force-takeover**（`02-aihub-core.md#§5.1` 行 8，
   `07-deployment-pilot.md#§6.4` 与 `02-aihub-core.md#§6.6` 的同款裁定）。**恢复序（与 `02-aihub-core.md#§6.2` 一致）**：`force-takeover` 调用本身即结束旧代并**开启 generation+1 且在响应中返回新代凭证**——恢复方（发起接管的机器）**消费该响应中的新代凭证直接创建现场**（capsule_create 的 generation 变化分支 + 派生新 harness session，§6.1 takeover 序列），**不再发起第二次 claim**（旧代已由 takeover 结束，再 claim 会被 `02-aihub-core.md#§6.1` 以 `ALREADY_CLAIMED` 拒绝）。
5. 与 §7.1 的区别：§7.1 处理的是同一 generation 下的意外进程死亡，目标是**继续**同一执行现场；本节
   处理的是服务端回滚导致的**身份不再有效**，目标是**隔离**该现场，两者 MUST NOT 共用同一判据或
   代码路径。

## 8. 安全边界 <!-- §8 -->

- **凭证隔离**：AIHub 凭证只存在于 `daemon/credentials/`（0600），从不进入任何 Capsule 目录（含 Capsule 本地会话目录）或 WI Agent/subagent 的 env/cwd/argv（`03-execution-capsule.md#§8`）。
- **路径安全**：Daemon 派生 WI Agent 时只传入该 WI 自己的 Capsule 路径与 harness session id；不同 WI 的 Capsule 路径与会话目录互不可见；harness-native subagent 的可写路径集合由 WI Agent 侧限定为 worktree 与 artifacts 目录，不含 `runtime-state/`、Daemon state 或 Daemon 凭证目录（INV-6）。
- **操作面仅本机（无网络面）**：Daemon 只监听本机 Unix domain socket（`/run/polyforge.sock`），**不开任何内网/网络监听**（不存在 TCP 监听面）；路由 Skill、CLI 与本机 `pf observe` 是唯一调用方；AIHub 不直接连 Daemon（方向单一，缩小暴露面）。**Daemon 之间零通信**：不存在任何 Daemon-to-Daemon 协议、直连或互推通道，一切跨机信息经 AIHub 中转——各 Daemon 自行向 AIHub claim/取 ready queue 获得自己的 Job（§6.1c），无需 Daemon 间协调；本机观察不到的 WI 走 §5.1a 第二层 read-through，同样不产生任何 Daemon 间连接。
- **预检即安全边界的一部分**：§6.1 的预检失败即拒绝服务（宁可拒绝也不带伤 claim），本机环境劣化（磁盘满、目录被删/只读）不会导致「本地无现场却对外报告已认领」的不一致状态。
- **Daemon 不放大权限**：Daemon 派生持久 WI Agent 时不注入任何凭证；**WI Agent 自始至终不持有 AIHub API key、WI 级 owner generation 凭证或 Revision 发布凭证**（与上一条「AIHub 凭证从不进入 WI Agent env/cwd/argv」一致，无例外）；它对 AIHub 的访问边界完全由 §5.3 的 Daemon 本地代理的允许操作集定义，不存在另一个绕过代理的直达通道。真正持有 `02-aihub-core.md#§5.1` WI 级 owner generation 凭证的是 Daemon，代理转发时用它代 WI Agent 签名；Daemon 本身不持有可代任意 WI 操作的万能凭证之外的放大面。发布请求的机械执行同样只对发起请求的那个 WI 的 Capsule 生效，不产生跨 WI 的写权限。

## 9. 监控 <!-- §9 -->

- 模式与切换次数；当前承载 Job 数（`daemon/state.json.jobs` 长度）；持久 WI Agent 存活数 / 派生失败率（`DAEMON-503`）；各 harness 适配器的启动成功率；watcher→发布请求→机械发布完成的端到端延迟（`05-sync-revision-recovery.md#§9` 联动，Daemon 是机械执行者，指标口径以本文档与 `05` 共同定义）；优雅停机时长；心跳年龄；预检失败率（`DAEMON-507` 尖峰 = 本机环境劣化）。
- Daemon 重启后的恢复耗时与「续跑 vs 存活复用」的比例（后者应占绝大多数，前者尖峰意味着 WI Agent 崩溃率异常）。
- Batch 推荐命中率（`get_recommendation` 返回的候选中有多少最终被人确认提交）；Drain 循环的取批频率与空闲占比。

## 10. 测试与故障注入 <!-- §10 -->

- 预检：只读 Daemon 数据目录 / 删 Capsule 根 / 磁盘满 → 断言 `DAEMON-507` 且**未调用** claim（AIHub 侧无事件为证）。
- **模式回归测试**：Interactive 下对处于 needs_input 的 step 调用 `answer` 成功、对未处于该状态的 step 调用 `answer` 返回 `DAEMON-409`；Batch 的 `get_recommendation` 不产生任何 Job，只有显式 `batch_submit` 才创建 Job；Drain 的内部循环在无候选时空闲而不退出进程。
- **answer 幂等/绑定回归测试（D9）**：①同一 `answer_id` 重复提交 → 断言返回与首次一致的结果，且 step 不被二次消费（`03-execution-capsule.md#§4.2` 的 `pending_answer` 只落一次、续跑只发生一次）；②`question_id` 已被消费（`pending_question` 已清空）后的再次提交拒绝为 `DAEMON-409`，不会被应用到后续新产生的问题（`question_id` 每次重新生成）；③`answered_by` 与请求认证身份不一致拒绝为 `DAEMON-403`；④比对与去重规则逐字以 `00-architecture.md#§3.4a` 为准，不另设本地变体。
- **claim 意图持久化回归测试（D4）**：①在 §6.1 第 4–5 步之间 `kill -9` Daemon → 重启后断言其从 `pending_jobs[]` 识别出 `intent_persisted` 意图并以同一 `idempotency_key` 重试，Core 侧恰好一次 claim（幂等缓存命中），不产生重复 attempt；②在第 7 步写入 `session_persisted` 后、落 `jobs[]` 前 `kill -9` → 断言按孤儿回收规则认领已派生的进程，不产生无人认领的进程；③断言第 5 步的 `claim_confirmed` 回写先于任何 Capsule 创建/派生调用完成 fsync（崩溃注入后无「已 claim 无本地现场」或「已派生无意图记录」的中间态）。
- **已死 WI Agent 的残留进程组回收测试（D4）**：杀死 WI Agent 但保留其 step agent/命令子进程存活 → 重启/检测后断言 Daemon 先以 `SIGTERM`→超时 `SIGKILL` 回收该进程组、确认无残留写入者，**之后**才按 `03-execution-capsule.md#§7` 用**同一个** `harness_session_id` 重新派生；存活分支则断言只重接 RPC 通道、不重新派生、session 不变。
- **flush 证据回归测试（D6）**：①正常 wrap/pause 前断言收尾调用携带的 `flush_evidence` 由 Daemon 生成（字段与 `02-aihub-core.md#§6.4` 权威定义一致：`published_generation`/`published_seq`/`manifest_sha`/`frozen_checkpoint`），WI Agent 侧不存在自行构造或改写证据的路径；②证据缺失/字段不符的收尾被 Core 拒为 `CORE-425`，Daemon 不重试代报；③无 HEAD 空链情形只允许 `published_seq=0`+`frozen_checkpoint.bytes_digest == EMPTY_SYNC_BYTES_DIGEST`（且本 generation 从未产生同步面写入）这一种形态（`05-sync-revision-recovery.md#§6.1`）；④B 路径断言不生成、不携带任何 `flush_evidence`。
- **持久 WI Agent 不逐 step 重启回归测试**：断言同一个 WI 从 claim 到 wrap 期间，`main_agent_pid` 全程不变（除非发生崩溃续跑）；断言不存在「每个 step 结束后 pid 变化」的行为（防止退化为逐 step 起新 headless agent）。
- **优雅停机测试**：向 Daemon 发送 SIGTERM，断言：`shutting_down` 标志在处理任何其它动作之前先被置位；不再接受新 claim/resume/takeover/batch_submit（`DAEMON-410`）；若该机处于 Drain 模式，其内部取 wave 循环在同一时刻立即停止取下一批（无新的 ready queue 请求、无新 Job 创建，§6.1c）；进行中 step 跑到检查点；每个活跃 Capsule 触发恰好一次全扫描发布，且该次发布由 Daemon 直接机械执行（不经过 `publish_request` 操作面往返），并按 `05-sync-revision-recovery.md#§6.1` 生成 `flush_evidence` 随收尾调用提交（`02-aihub-core.md#§6.4`）；进程最终退出；退出前 `daemon/state.json` 反映的 Job 集合已清空。
- **优雅停机与 Drain 不可混淆的回归测试**：断言 Drain 模式下发 SIGTERM 与非 Drain 模式下发 SIGTERM 都走同一套 §6.5 序列（触发原因不同不应导致序列分叉）；断言 Drain 模式空闲时不会自行退出进程，只有显式终止信号才会。
- **发布请求机械执行测试**：断言 WI Agent 侧不存在任何直接调用 `05-sync-revision-recovery.md#§5.1` 发布接口的代码路径——所有发布都经过一次 `publish_request` 到 Daemon 的往返（优雅停机路径除外，因为发起者与执行者都是 Daemon 自己）。
- **崩溃恢复测试（pid 复用防护）**：①记录 Job 的 `main_agent_pid=P` 与 `main_agent_started_at=T`；②杀死该进程；③让一个无关新进程复用 pid `P`；④重启 Daemon；⑤断言 Daemon 通过创建时间戳比对识别出该 WI Agent 已死（不因 pid 存在而误判存活），并按 `03-execution-capsule.md#§7` 用**同一个** `harness_session_id` 重新派生。
- **可注入故障**：`kill -9` Daemon（预期重启后按 §7.1 恢复，存活 WI Agent 不受影响，已死的按 `03-execution-capsule.md#§7` 续跑）；AIHub 不可达 30 分钟（预期发布请求积压后由 Daemon 侧的下一次触发时机补齐扫描，WI 不中断）；本机 Unix domain socket 端口冲突（预期第二个 Daemon 实例启动失败并退出）。

## 11. 迁移与 rollout <!-- §11 -->

- 无存量迁移（新系统）。单机引入顺序：操作面 + 模式存储 → claim/预检 → harness 适配器接入
  （Pi、Claude）与持久 WI Agent 派生（`00-architecture.md#§3.6`/§3.6a）→ watcher 与全扫描发布的机械执行联调
  （`05`）→ 优雅停机序列联调 → 崩溃恢复（pid 复用防护、session id 复用）联调。总 rollout 序见
  `07-deployment-pilot.md#§11`。
- Daemon state 格式变更（如新增字段，例如 `harness_adapter`/`harness_session_id`）需保证向后
  兼容读取：旧格式文件被识别为「无该字段」而非解析失败，避免升级后无法恢复在跑的 Job。

## 12. 未决问题（OQ2/OQ5 已在本次修订中裁定，不再是开放项） <!-- §12 -->

0. **（已裁定，OQ2；B12 复审统一入队规则）并发上限默认值与 `batch_submit` 入队规则**：缺省
   `max_concurrent_jobs = 4`（同一机器，跨 Interactive/Batch/Drain 共享一个计数器），可通过
   `mode(set, max_concurrent_jobs=<n>)` 覆盖（`n ≥ 1`，非正整数拒绝为 `DAEMON-400`）。
   `batch_submit(wi_ids)` 若 `len(wi_ids)` 超过当前上限，**不拒绝整批：整批全部入 pending**（§6.1d），
   派工一律按 §6.1d 的 priority 降序 + 同级 FIFO 与并发上限进行，**不存在「先按 `wi_ids` 传入顺序
   派前 N 个」的例外**；空出配额后按同一排序自动递补（不需要人重新确认）；Drain 的内部循环
   同样受此上限约束（当前在跑 Job 数达上限时暂停取新一批，不等于收敛）。Interactive/Batch/Drain
   三模式共用同一条入队与派工规则（§6.1d），差异只在 Job 的来源与准入判据。这不违反
   `00-architecture.md#§3.4b`「wave 边界由人给定」：wave 的**内容**仍由人给定，本项只约束 wave 内
   **同时派生**的 Job 数与派生顺序；队列的簿记、pending 排序（priority 降序 + 同级 FIFO）、无抢占与按序补位规则见 §6.1d。
1. **Batch 模式自动认领的默认并发上限**：已在上方第 0 项裁定，本条删除不再开放。
2. **优雅停机等待可中断边界的超时上限**：一期无强制超时（等到边界为止），是否需要一个「等待过久则强制中断」的兜底策略开放。
3. **多 Daemon 同机的资源配额**（并发 Capsule 上限）：一期人工约定，自动配额开放。
4. **第三条 harness 适配器接入时的启动参数抽象层**：一期只固定 Pi/Claude 两条具体实现（§6.1a），是否需要在此之上抽出一层通用「适配器插件」接口待定；不得因此推迟 v1 的两条既定适配器交付。
